package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http/httpproxy"
)

// dnsStub is the resolver the dialer uses in tests. It speaks real DNS to Go's
// resolver, so resolution still happens inside net.Dialer and every address it
// returns still meets the Control hook; only the answers are scripted. answer
// gets the 1-based count of A queries seen so far.
type dnsStub struct {
	mu      sync.Mutex
	queries int
	answer  func(name string, n int) []netip.Addr
}

func answerAlways(addrs ...string) func(string, int) []netip.Addr {
	parsed := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	return func(string, int) []netip.Addr { return parsed }
}

func (s *dnsStub) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go s.serve(server)
			return client, nil
		},
	}
}

// serve answers length-prefixed queries; a net.Pipe is not a PacketConn, so
// Go's resolver frames it as DNS over TCP.
func (s *dnsStub) serve(conn net.Conn) {
	defer conn.Close()
	for {
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, msg); err != nil {
			return
		}
		out, err := s.reply(msg)
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(out)))
		if _, err := conn.Write(append(size[:], out...)); err != nil {
			return
		}
	}
}

func (s *dnsStub) reply(msg []byte) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}

	var addrs []netip.Addr
	if q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeAAAA {
		s.mu.Lock()
		if q.Type == dnsmessage.TypeA {
			s.queries++
		}
		n := max(s.queries, 1)
		s.mu.Unlock()
		addrs = s.answer(strings.TrimSuffix(q.Name.String(), "."), n)
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET}
	for _, a := range addrs {
		switch {
		case q.Type == dnsmessage.TypeA && a.Is4():
			err = b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		case q.Type == dnsmessage.TypeAAAA && a.Is6():
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
		if err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// addrLog records every address the dialer presented for checking.
type addrLog struct {
	mu   sync.Mutex
	seen []netip.Addr
}

func (l *addrLog) addrs() []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]netip.Addr(nil), l.seen...)
}

// allowLoopback is the production check with loopback admitted, so an httptest
// server is reachable while every other refusal stays in force.
func (l *addrLog) allowLoopback(a netip.Addr) error {
	l.mu.Lock()
	l.seen = append(l.seen, a)
	l.mu.Unlock()
	if a.Unmap().IsLoopback() {
		return nil
	}
	return checkAddr(a)
}

type importEnv struct {
	*testEnv
	store *recordingStorage
	srv   *httptest.Server
	hits  *atomic.Int64
	dns   *dnsStub
	addrs *addrLog
	// base is the server's URL under the allowlisted name example.com, which
	// the httptest certificate covers.
	base string
}

func newImportEnv(t *testing.T, handler http.HandlerFunc, overrides ...func(*APIOpts)) *importEnv {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	store := &recordingStorage{}
	dns := &dnsStub{answer: answerAlways("127.0.0.1")}
	addrs := &addrLog{}
	all := append([]func(*APIOpts){func(o *APIOpts) {
		o.Import = ImportOpts{
			AllowedHosts:  []string{"example.com"},
			FetchTimeout:  5 * time.Second,
			RatePerMinute: 0,
			resolver:      dns.resolver(),
			validateAddr:  addrs.allowLoopback,
			rootCAs:       pool,
		}
	}}, overrides...)

	return &importEnv{
		testEnv: newTestEnv(t, store, all...),
		store:   store,
		srv:     srv,
		hits:    hits,
		dns:     dns,
		addrs:   addrs,
		base:    "https://example.com:" + port,
	}
}

func (e *importEnv) post(t *testing.T, body any, claims tokenClaims) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token(t, claims))
	res := httptest.NewRecorder()
	e.api.Handler().ServeHTTP(res, req)
	return res
}

func importBody(rawURL string) map[string]any {
	return map[string]any{
		"url":      rawURL,
		"folder":   "voucher",
		"uploadId": "bd10fd365101425f8bafeb6adfe8007c",
		"width":    400,
		"aspect":   "1:1",
	}
}

func serveBytes(contentType string, b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(b)
	}
}

func wantCode(t *testing.T, res *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status = %d, want %d body=%s", res.Code, status, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["ok"] != false || got["code"] != code {
		t.Fatalf("body = %v, want ok=false code=%s", got, code)
	}
}

func TestCheckAddr(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		refused bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
		{"::ffff:8.8.8.8", false},
		{"64:ff9b::808:808", false},

		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"169.254.169.254", true},
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"255.255.255.255", true},
		{"::", true},
		{"::1", true},
		{"fe80::1", true},
		{"fe80::1%eth0", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"ff02::1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::a9fe:a9fe", true},
		{"64:ff9b::c0a8:101", true},
		{"64:ff9b:1::808:808", true},
		{"64:ff9b:1::a00:1", true},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			err := checkAddr(netip.MustParseAddr(tc.addr))
			if tc.refused && !errors.Is(err, errRefusedAddress) {
				t.Fatalf("err = %v, want refused", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("err = %v, want allowed", err)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"images.example.org":    "images.example.org",
		"Images.Example.ORG":    "images.example.org",
		"images.example.org.":   "images.example.org",
		"BÜCHER.example":        "xn--bcher-kva.example",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "under_score.example", "::1"} {
		if got, err := NormalizeHost(in); err == nil {
			t.Fatalf("NormalizeHost(%q) = %q, want an error", in, got)
		}
	}
	if _, err := NormalizeHosts([]string{"ok.example", "bad_host.example"}); err == nil {
		t.Fatal("NormalizeHosts accepted an invalid entry")
	}
}

func TestImportStoresAndRepeatsOnTheSameKey(t *testing.T) {
	// A text/html header on a real JPEG: the bytes decide, not the header.
	env := newImportEnv(t, serveBytes("text/html; charset=utf-8", photoJPEG(t, 1200, 800, 3)))

	post := func() map[string]any {
		t.Helper()
		res := env.post(t, importBody(env.base+"/v/123.jpg"), userClaims("USER"))
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 body=%s", res.Code, res.Body.String())
		}
		got := decodeJSON(t, res)
		if got["ok"] != true {
			t.Fatalf("ok = %v", got["ok"])
		}
		payload, _ := got["payload"].(map[string]any)
		return payload
	}

	first := post()
	again := post()

	s3URL, _ := first["s3"].(string)
	if !keyPattern.MatchString(s3URL) ||
		!strings.HasPrefix(s3URL, "https://content.sarafu.network/voucher/bd10fd365101425f8bafeb6adfe8007c_400_") {
		t.Fatalf("s3 = %q, want the upload key form", s3URL)
	}
	if again["s3"] != s3URL {
		t.Fatalf("repeat import stored %v, want %v", again["s3"], s3URL)
	}
	up := env.store.last()
	if up.path != "voucher" || !strings.HasSuffix(s3URL, "/voucher/"+up.name) {
		t.Fatalf("stored %s/%s, url %s", up.path, up.name, s3URL)
	}
	if first["width"] != float64(400) || first["height"] != float64(400) {
		t.Fatalf("dimensions = %vx%v, want a 400x400 crop", first["width"], first["height"])
	}
	if first["byteSize"] != float64(len(up.data)) || first["contentType"] != up.contentType {
		t.Fatalf("payload %v does not describe stored %s (%d bytes)", first, up.contentType, len(up.data))
	}
}

func TestImportCropsToSixteenNine(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 1000, 1000, 4)))
	body := importBody(env.base + "/v/square.jpg")
	body["aspect"] = "16:9"
	res := env.post(t, body, userClaims("USER"))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	payload, _ := decodeJSON(t, res)["payload"].(map[string]any)
	if payload["width"] != float64(400) || payload["height"] != float64(225) {
		t.Fatalf("dimensions = %vx%v, want 400x225", payload["width"], payload["height"])
	}
}

// Requested hosts are normalized the same way as the list, so a case or
// trailing-dot spelling of a listed host still matches.
func TestImportNormalizesTheRequestedHost(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 600, 600, 5)))
	u := strings.Replace(env.base, "example.com", "EXAMPLE.com.", 1) + "/v/1.jpg"
	if res := env.post(t, importBody(u), userClaims("USER")); res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestImportRefusesTheURL(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)))
	port := strings.TrimPrefix(env.base, "https://example.com:")

	for name, tc := range map[string]struct {
		url  string
		code string
	}{
		"absent":         {"", "INVALID_URL"},
		"unparseable":    {"https://example.com:" + port + "/%zz", "INVALID_URL"},
		"bad port":       {"https://example.com:x/a.jpg", "INVALID_URL"},
		"userinfo":       {"https://user:pw@example.com:" + port + "/a.jpg", "INVALID_URL"},
		"bare userinfo":  {"https://user@example.com:" + port + "/a.jpg", "INVALID_URL"},
		"no host":        {"https:///a.jpg", "INVALID_URL"},
		"opaque":         {"https:example.com/a.jpg", "INVALID_URL"},
		"http":           {"http://example.com:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"other scheme":   {"ftp://example.com/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"relative":       {"/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"unlisted host":  {"https://other.example:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"suffix of host": {"https://evilexample.com:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"subdomain":      {"https://cdn.example.com:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"ip literal":     {"https://127.0.0.1:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"ipv6 literal":   {"https://[::1]:" + port + "/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
		"invalid label":  {"https://bad_host.example/a.jpg", "IMPORT_HOST_NOT_ALLOWED"},
	} {
		t.Run(name, func(t *testing.T) {
			res := env.post(t, importBody(tc.url), userClaims("USER"))
			status := http.StatusBadRequest
			wantCode(t, res, status, tc.code)
		})
	}
	if n := env.hits.Load(); n != 0 {
		t.Fatalf("source was fetched %d times, want none", n)
	}
}

func TestImportDisabledByAnEmptyList(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
		o.Import.AllowedHosts = nil
	})
	wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadRequest, "IMPORT_HOST_NOT_ALLOWED")
	if n := env.hits.Load(); n != 0 {
		t.Fatalf("source was fetched %d times, want none", n)
	}
}

func TestImportValidatesTheRequest(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)))
	for name, tc := range map[string]struct {
		edit func(map[string]any)
		raw  string
		code string
	}{
		"unknown folder":    {edit: func(b map[string]any) { b["folder"] = "secret" }, code: "INVALID_FOLDER"},
		"missing folder":    {edit: func(b map[string]any) { delete(b, "folder") }, code: "INVALID_FOLDER"},
		"bad upload id":     {edit: func(b map[string]any) { b["uploadId"] = "../etc/passwd" }, code: "INVALID_NAME"},
		"missing upload id": {edit: func(b map[string]any) { delete(b, "uploadId") }, code: "INVALID_NAME"},
		"long upload id":    {edit: func(b map[string]any) { b["uploadId"] = strings.Repeat("a", 65) }, code: "INVALID_NAME"},
		"unlisted width":    {edit: func(b map[string]any) { b["width"] = 123 }, code: "INVALID_WIDTH"},
		"missing width":     {edit: func(b map[string]any) { delete(b, "width") }, code: "INVALID_WIDTH"},
		"string width":      {edit: func(b map[string]any) { b["width"] = "400" }, code: "INVALID_WIDTH"},
		"unlisted aspect":   {edit: func(b map[string]any) { b["aspect"] = "4:3" }, code: "INVALID_ASPECT"},
		"reversed aspect":   {edit: func(b map[string]any) { b["aspect"] = "9:16" }, code: "INVALID_ASPECT"},
		"missing aspect":    {edit: func(b map[string]any) { delete(b, "aspect") }, code: "INVALID_ASPECT"},
		"malformed json":    {raw: `{"url":`, code: "BAD_REQUEST"},
	} {
		t.Run(name, func(t *testing.T) {
			var body any = tc.raw
			if tc.edit != nil {
				b := importBody(env.base + "/a.jpg")
				tc.edit(b)
				body = b
			}
			wantCode(t, env.post(t, body, userClaims("USER")), http.StatusBadRequest, tc.code)
		})
	}
	if n := env.hits.Load(); n != 0 {
		t.Fatalf("source was fetched %d times, want none", n)
	}
}

func TestImportRequiresAToken(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)))
	raw, _ := json.Marshal(importBody(env.base + "/a.jpg"))
	req := httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)
	wantCode(t, res, http.StatusUnauthorized, "UNAUTHORIZED")
}

// The production check, not the test one: a listed host resolving to loopback is
// refused at the socket, before the server sees a connection.
func TestImportRefusesAListedHostOnAPrivateAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1"} {
		t.Run(addr, func(t *testing.T) {
			env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
				o.Import.validateAddr = nil
			})
			env.dns.answer = answerAlways(addr)
			wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadRequest, "IMPORT_HOST_NOT_ALLOWED")
			if n := env.hits.Load(); n != 0 {
				t.Fatalf("server saw %d requests, want none", n)
			}
		})
	}
}

// DNS rebinding: the name answers an allowed address, then a private one. There
// is no lookup ahead of the fetch to fool; the second connection's own address
// reaches the Control hook and is refused there, before connect.
func TestImportRebindingNeverConnectsToThePrivateAnswer(t *testing.T) {
	photo := photoJPEG(t, 300, 300, 6)
	env := newImportEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		// No keep-alive, so the second import has to dial again.
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(photo)
	}, func(o *APIOpts) {
		o.Import.FetchTimeout = 2 * time.Second
	})
	metadata := netip.MustParseAddr("169.254.169.254")
	env.dns.answer = func(_ string, n int) []netip.Addr {
		if n == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}
		}
		return []netip.Addr{metadata}
	}

	if res := env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")); res.Code != http.StatusOK {
		t.Fatalf("first import status = %d body=%s", res.Code, res.Body.String())
	}
	wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadRequest, "IMPORT_HOST_NOT_ALLOWED")

	seen := env.addrs.addrs()
	if len(seen) == 0 || seen[len(seen)-1] != metadata {
		t.Fatalf("checked addresses = %v, want the rebound %v presented to Control last", seen, metadata)
	}
	if n := env.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want only the first", n)
	}
}

func TestImportFetchFailures(t *testing.T) {
	small := photoJPEG(t, 64, 64, 1)
	large := photoJPEG(t, 400, 400, 2)
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		opts    func(*APIOpts)
	}{
		"not found": {handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "missing", http.StatusNotFound)
		}},
		"server error": {handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		"found redirect": {handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				_, _ = w.Write(small)
				return
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}},
		"permanent redirect": {handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data", http.StatusPermanentRedirect)
		}},
		"not modified": {handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		}},
		"declared over cap": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/jpeg")
				w.Header().Set("Content-Length", strconv.Itoa(len(large)))
				_, _ = w.Write(large)
			},
			opts: func(o *APIOpts) { o.MaxBodySize = 4 << 10 },
		},
		"streamed over cap": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/jpeg")
				body := large
				for len(body) > 0 {
					n := min(1024, len(body))
					_, _ = w.Write(body[:n])
					w.(http.Flusher).Flush()
					body = body[n:]
				}
			},
			opts: func(o *APIOpts) { o.MaxBodySize = 4 << 10 },
		},
		"timeout": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			},
			opts: func(o *APIOpts) { o.Import.FetchTimeout = 200 * time.Millisecond },
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := []func(*APIOpts){}
			if tc.opts != nil {
				opts = append(opts, tc.opts)
			}
			env := newImportEnv(t, tc.handler, opts...)
			wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadGateway, "IMPORT_FETCH_FAILED")
			if len(env.store.uploads) != 0 {
				t.Fatalf("stored %d objects, want none", len(env.store.uploads))
			}
		})
	}
}

// Redirects are refused, not followed: the target is never requested.
func TestImportDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	env := newImportEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadGateway, "IMPORT_FETCH_FAILED")
	if followed.Load() {
		t.Fatal("redirect was followed")
	}
}

// Exactly the cap is accepted; the cap is a limit, not an off-by-one.
func TestImportAcceptsABodyAtTheCap(t *testing.T) {
	body := photoJPEG(t, 200, 200, 2)
	env := newImportEnv(t, serveBytes("image/jpeg", body), func(o *APIOpts) {
		o.MaxBodySize = int64(len(body))
	})
	if res := env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")); res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestImportRejectsContent(t *testing.T) {
	var gifBuf bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, 8, 8), []color.Color{color.Black, color.White})
	if err := gif.Encode(&gifBuf, frame, nil); err != nil {
		t.Fatal(err)
	}
	jpeg := photoJPEG(t, 64, 64, 1)

	for name, tc := range map[string]struct {
		ctype string
		body  []byte
		opts  func(*APIOpts)
	}{
		"text claiming png": {ctype: "image/png", body: []byte("hello, this is plainly text")},
		"html":              {ctype: "image/jpeg", body: []byte("<!doctype html><html><body>hi</body></html>")},
		"pdf":               {ctype: "image/jpeg", body: []byte("%PDF-1.1\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF")},
		"gif":               {ctype: "image/gif", body: gifBuf.Bytes()},
		"truncated jpeg":    {ctype: "image/jpeg", body: jpeg[:len(jpeg)/3]},
		"too many pixels": {
			ctype: "image/jpeg", body: jpeg,
			opts: func(o *APIOpts) { o.MaxPixels = 16 },
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := []func(*APIOpts){}
			if tc.opts != nil {
				opts = append(opts, tc.opts)
			}
			env := newImportEnv(t, serveBytes(tc.ctype, tc.body), opts...)
			wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadRequest, "IMPORT_CONTENT_REJECTED")
			if len(env.store.uploads) != 0 {
				t.Fatalf("stored %d objects, want none", len(env.store.uploads))
			}
		})
	}
}

// A certificate is verified against the requested hostname: the httptest
// certificate covers example.com, not images.test.
func TestImportVerifiesTheCertificateHostname(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
		o.Import.AllowedHosts = []string{"images.test"}
	})
	u := strings.Replace(env.base, "example.com", "images.test", 1) + "/a.jpg"
	wantCode(t, env.post(t, importBody(u), userClaims("USER")), http.StatusBadGateway, "IMPORT_FETCH_FAILED")
	if n := env.hits.Load(); n != 0 {
		t.Fatalf("server answered %d requests over an unverified connection", n)
	}
}

// Without the test pool the httptest CA is unknown, so verification is on.
func TestImportVerifiesTheCertificateChain(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
		o.Import.rootCAs = nil
	})
	wantCode(t, env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")), http.StatusBadGateway, "IMPORT_FETCH_FAILED")
}

func TestImportIgnoresEnvironmentProxies(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var proxied atomic.Int64
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = conn.Close()
		}
	}()
	proxyURL := "http://" + proxy.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(k, proxyURL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)))
	if res := env.post(t, importBody(env.base+"/a.jpg"), userClaims("USER")); res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("proxy saw %d connections, want none", n)
	}

	transport, ok := newImportClient(ImportOpts{FetchTimeout: time.Second}).Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("import transport must not consult a proxy")
	}
	// Guards the test itself: the environment really does name a proxy for this
	// URL. httpproxy reads it afresh, unlike http.ProxyFromEnvironment's cache.
	target, _ := url.Parse("https://example.com/a.jpg")
	if u, _ := httpproxy.FromEnvironment().ProxyFunc()(target); u == nil {
		t.Fatal("the environment names no proxy for the target")
	}
}

func TestImportRateLimitsPerIdentity(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
		o.Import.RatePerMinute = 1
	})
	body := importBody(env.base + "/a.jpg")

	if res := env.post(t, body, userClaims("USER")); res.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", res.Code, res.Body.String())
	}
	res := env.post(t, body, userClaims("USER"))
	wantCode(t, res, http.StatusTooManyRequests, "RATE_LIMITED")
	retry, err := strconv.Atoi(res.Header().Get("Retry-After"))
	if err != nil || retry < 1 || retry > 60 {
		t.Fatalf("Retry-After = %q, want whole seconds in [1, 60]", res.Header().Get("Retry-After"))
	}

	// A fresh token for the same account is the same caller.
	other := userClaims("ADMIN")
	wantCode(t, env.post(t, body, other), http.StatusTooManyRequests, "RATE_LIMITED")

	// Another identity has its own bucket.
	second := userClaims("USER")
	second.UserID = 2
	if res := env.post(t, body, second); res.Code != http.StatusOK {
		t.Fatalf("other user status = %d body=%s", res.Code, res.Body.String())
	}
	if res := env.post(t, body, serviceClaims()); res.Code != http.StatusOK {
		t.Fatalf("service status = %d body=%s", res.Code, res.Body.String())
	}
	if n := env.hits.Load(); n != 3 {
		t.Fatalf("source fetched %d times, want 3: a limited call must not fetch", n)
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newRateLimiter(30)
	r.now = func() time.Time { return now }

	for i := range 30 {
		if _, ok := r.allow("a"); !ok {
			t.Fatalf("call %d refused inside the burst", i)
		}
	}
	wait, ok := r.allow("a")
	if ok || wait <= 0 || wait > 2*time.Second {
		t.Fatalf("allow = %v, %v; want a refusal of at most one refill interval", wait, ok)
	}
	if got := retryAfterSeconds(wait); got != 2 {
		t.Fatalf("Retry-After = %d, want 2", got)
	}
	// A refusal spends nothing, so retrying early does not extend the wait.
	if again, _ := r.allow("a"); again != wait {
		t.Fatalf("second refusal waits %v, want %v", again, wait)
	}

	now = now.Add(wait)
	if _, ok := r.allow("a"); !ok {
		t.Fatal("refused after waiting the advertised time")
	}

	// A bucket that has fully refilled is dropped by the next sweep.
	now = now.Add(2 * time.Minute)
	if _, ok := r.allow("b"); !ok {
		t.Fatal("new identity refused")
	}
	if _, kept := r.buckets["a"]; kept {
		t.Fatal("refilled bucket was not swept")
	}
	if got := retryAfterSeconds(10 * time.Millisecond); got != 1 {
		t.Fatalf("Retry-After for a sub-second wait = %d, want 1", got)
	}
}

// The handler's body parse keeps the 413 the body limit raises.
func TestImportBodyLimit(t *testing.T) {
	env := newImportEnv(t, serveBytes("image/jpeg", photoJPEG(t, 64, 64, 1)), func(o *APIOpts) {
		o.MaxBodySize = 64
	})
	body := importBody(env.base + "/" + strings.Repeat("a", 128) + ".jpg")
	wantCode(t, env.post(t, body, userClaims("USER")), http.StatusRequestEntityTooLarge, "FILE_SIZE_LIMIT_EXCEEDED")
}

// target rewrites the host to the form that matched, so what is fetched is
// exactly what was checked.
func TestImportTargetUsesTheNormalizedHost(t *testing.T) {
	h := &importHandler{allowedHosts: map[string]struct{}{"xn--bcher-kva.example": {}}}
	for in, want := range map[string]string{
		"https://BÜCHER.example./v/1.jpg#frag": "https://xn--bcher-kva.example/v/1.jpg",
		"https://bücher.example:8443/v/1.jpg":  "https://xn--bcher-kva.example:8443/v/1.jpg",
	} {
		u, err := h.target(in)
		if err != nil {
			t.Fatalf("target(%q): %v", in, err)
		}
		if got := u.String(); got != want {
			t.Fatalf("target(%q) = %q, want %q", in, got, want)
		}
		if _, err := url.Parse(u.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.target(fmt.Sprintf("https://%s/a", "other.example")); !errors.Is(err, ErrImportHostNotAllowed) {
		t.Fatalf("err = %v", err)
	}
}
