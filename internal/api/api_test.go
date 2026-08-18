package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	img "github.com/grassrootseconomics/storage-server/internal/image"
	"github.com/grassrootseconomics/storage-server/internal/storage"
)

type testEnv struct {
	api  *API
	priv ed25519.PrivateKey
}

type storedObject struct {
	name        string
	path        string
	contentType string
	size        int64
	data        []byte
}

type recordingStorage struct {
	mu      sync.Mutex
	uploads []storedObject
	err     error
}

func (s *recordingStorage) Upload(_ context.Context, fileName, path string, r io.Reader, size int64, contentType string) error {
	if s.err != nil {
		return s.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, storedObject{
		name:        fileName,
		path:        path,
		contentType: contentType,
		size:        size,
		data:        data,
	})
	return nil
}

func (s *recordingStorage) last() storedObject {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.uploads) == 0 {
		return storedObject{}
	}
	return s.uploads[len(s.uploads)-1]
}

func newTestEnv(t *testing.T, store storage.Storage, overrides ...func(*APIOpts)) *testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	o := APIOpts{
		EnableMetrics:        true,
		ListenAddress:        "127.0.0.1:0",
		MaxBodySize:          1 << 20,
		MaxPixels:            12_500_000,
		NormalizeConcurrency: 4,
		CORS:                 []string{"https://sarafu.network", "http://localhost:3000"},
		AllowedFolders:       []string{"voucher", "profile"},
		AllowedWidths:        []int{400, 800, 1280},
		CDNBaseURL:           "https://content.sarafu.network",
		UploadTimeout:        5 * time.Second,
		Image:                img.DefaultOpts(),
		ClockSkew:            30 * time.Second,
		VerifyingKey:         pub,
		StorageProvider:      store,
		Logg:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, fn := range overrides {
		fn(&o)
	}
	return &testEnv{api: New(o), priv: priv}
}

func (e *testEnv) token(t *testing.T, claims jwt.Claims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(e.priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func userClaims(role string) tokenClaims {
	return tokenClaims{
		UserID:          1,
		EthereumAddress: "0x1111111111111111111111111111111111111111",
		Role:            role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
}

func serviceClaims() tokenClaims {
	return tokenClaims{
		Service:   true,
		PublicKey: "0x0000000000000000000000000000000000000000",
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartBody(t *testing.T, fields map[string]string, filename string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, w.FormDataContentType()
}

func decodeJSON(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode json: %v body=%q", err, res.Body.String())
	}
	return got
}

func TestUploadSuccessJSON(t *testing.T) {
	store := &recordingStorage{}
	env := newTestEnv(t, store)
	file := jpegBytes(t, 800, 500)
	body, ctype := multipartBody(t, map[string]string{
		"folder": "voucher",
		"name":   "bd10fd365101425f8bafeb6adfe8007c",
		"width":  "400",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["ok"] != true {
		t.Fatalf("ok = %v, want true", got["ok"])
	}
	payload, _ := got["payload"].(map[string]any)
	s3URL, _ := payload["s3"].(string)
	if !strings.HasPrefix(s3URL, "https://content.sarafu.network/voucher/bd10fd365101425f8bafeb6adfe8007c_400.") {
		t.Fatalf("s3 = %v", s3URL)
	}
	if len(store.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(store.uploads))
	}
	up := store.last()
	if up.path != "voucher" || !strings.HasPrefix(up.name, "bd10fd365101425f8bafeb6adfe8007c_400.") {
		t.Fatalf("stored key = %s/%s", up.path, up.name)
	}
	if up.size != int64(len(up.data)) {
		t.Fatalf("stored size = %d, data = %d", up.size, len(up.data))
	}
	if !strings.HasSuffix(s3URL, "/"+up.path+"/"+up.name) {
		t.Fatalf("url %s does not match stored object %s/%s", s3URL, up.path, up.name)
	}
}

func TestUploadErrorEnvelopes(t *testing.T) {
	file := jpegBytes(t, 4, 4)

	cases := []struct {
		name   string
		fields map[string]string
		file   string
		data   []byte
		ctype  string
		body   io.Reader
		code   int
		err    string
	}{
		{
			name: "missing folder",
			fields: map[string]string{
				"name":  "abc",
				"width": "400",
			},
			file: "p.jpg",
			data: file,
			code: http.StatusBadRequest,
			err:  "MISSING_FOLDER",
		},
		{
			name: "missing name",
			fields: map[string]string{
				"folder": "voucher",
				"width":  "400",
			},
			file: "p.jpg",
			data: file,
			code: http.StatusBadRequest,
			err:  "MISSING_NAME",
		},
		{
			name:   "invalid folder",
			fields: map[string]string{"folder": "secret", "name": "abc", "width": "400"},
			file:   "p.jpg",
			data:   file,
			code:   http.StatusBadRequest,
			err:    "INVALID_FOLDER",
		},
		{
			name:   "invalid name",
			fields: map[string]string{"folder": "voucher", "name": "../etc/passwd", "width": "400"},
			file:   "p.jpg",
			data:   file,
			code:   http.StatusBadRequest,
			err:    "INVALID_NAME",
		},
		{
			name:   "missing width",
			fields: map[string]string{"folder": "voucher", "name": "abc"},
			file:   "p.jpg",
			data:   file,
			code:   http.StatusBadRequest,
			err:    "INVALID_WIDTH",
		},
		{
			name:   "invalid width",
			fields: map[string]string{"folder": "voucher", "name": "abc", "width": "123"},
			file:   "p.jpg",
			data:   file,
			code:   http.StatusBadRequest,
			err:    "INVALID_WIDTH",
		},
		{
			name:   "missing file",
			fields: map[string]string{"folder": "voucher", "name": "abc", "width": "400"},
			code:   http.StatusBadRequest,
			err:    "MISSING_FILE",
		},
		{
			name:  "not multipart",
			ctype: "application/json",
			body:  strings.NewReader(`{}`),
			code:  http.StatusBadRequest,
			err:   "NOT_MULTIPART",
		},
		{
			name:   "unsupported type",
			fields: map[string]string{"folder": "voucher", "name": "abc", "width": "400"},
			file:   "note.txt",
			data:   []byte("hello world this is not an image"),
			code:   http.StatusBadRequest,
			err:    "UNSUPPORTED_FILE_EXTENSION",
		},
		{
			name:   "pdf rejected",
			fields: map[string]string{"folder": "voucher", "name": "abc", "width": "400"},
			file:   "doc.pdf",
			data:   []byte("%PDF-1.1\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF"),
			code:   http.StatusBadRequest,
			err:    "UNSUPPORTED_FILE_EXTENSION",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, &recordingStorage{})
			var (
				body  io.Reader
				ctype string
			)
			if tc.body != nil {
				body, ctype = tc.body, tc.ctype
			} else {
				b, ct := multipartBody(t, tc.fields, tc.file, tc.data)
				body, ctype = b, ct
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
			req.Header.Set("Content-Type", ctype)
			req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
			res := httptest.NewRecorder()
			env.api.Handler().ServeHTTP(res, req)

			if res.Code != tc.code {
				t.Fatalf("status = %d, want %d body=%s", res.Code, tc.code, res.Body.String())
			}
			got := decodeJSON(t, res)
			if got["ok"] != false {
				t.Fatalf("ok = %v, want false", got["ok"])
			}
			if got["code"] != tc.err {
				t.Fatalf("code = %v, want %s", got["code"], tc.err)
			}
		})
	}
}

func TestUploadBodyLimit(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{}, func(o *APIOpts) {
		o.MaxBodySize = 256
	})
	file := jpegBytes(t, 64, 64)
	if len(file) < 256 {
		t.Fatalf("fixture too small to exceed limit: %d", len(file))
	}
	body, ctype := multipartBody(t, map[string]string{
		"folder": "voucher",
		"name":   "too-big",
		"width":  "400",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["code"] != "FILE_SIZE_LIMIT_EXCEEDED" {
		t.Fatalf("code = %v, want FILE_SIZE_LIMIT_EXCEEDED", got["code"])
	}
}

func TestCORSPreflightConfiguredOrigin(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/upload", nil)
	req.Header.Set("Origin", "https://sarafu.network")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://sarafu.network" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow credentials = %q", got)
	}
	allowMethods := res.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(allowMethods, http.MethodPost) {
		t.Fatalf("allow methods = %q, want POST", allowMethods)
	}
	allowHeaders := strings.ToLower(res.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowHeaders, "authorization") || !strings.Contains(allowHeaders, "content-type") {
		t.Fatalf("allow headers = %q", allowHeaders)
	}
}

func TestCORSPreflightRejectsUnknownOrigin(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/upload", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow origin = %q, want empty", got)
	}
}

func TestMetricsRouteEnabled(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if !strings.Contains(res.Body.String(), "go_") && !strings.Contains(res.Body.String(), "process_") {
		t.Fatalf("expected prometheus metrics, got %q", res.Body.String())
	}
}

func TestMetricsRouteDisabled(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{}, func(o *APIOpts) {
		o.EnableMetrics = false
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", res.Code, res.Body.String())
	}
}

func TestGracefulShutdown(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- env.api.Start()
	}()

	select {
	case err := <-errCh:
		t.Fatalf("server exited early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.api.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("start err = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestUploadNarrowerSourceUsesActualWidth(t *testing.T) {
	store := &recordingStorage{}
	env := newTestEnv(t, store)
	file := jpegBytes(t, 200, 120)
	body, ctype := multipartBody(t, map[string]string{
		"folder": "profile",
		"name":   "smallsrc",
		"width":  "400",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	payload, _ := got["payload"].(map[string]any)
	s3URL, _ := payload["s3"].(string)
	if !strings.Contains(s3URL, "/profile/smallsrc_200.") {
		t.Fatalf("s3 = %v, want actual width 200", s3URL)
	}
	if len(store.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(store.uploads))
	}
}

func TestUploadRejectsMultipleWidths(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	file := jpegBytes(t, 8, 8)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("folder", "voucher")
	_ = w.WriteField("name", "abc")
	_ = w.WriteField("width", "400")
	_ = w.WriteField("width", "800")
	fw, err := w.CreateFormFile("file", "p.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["code"] != "INVALID_WIDTH" {
		t.Fatalf("code = %v, want INVALID_WIDTH", got["code"])
	}
}

func TestUploadRejectsOversizedPixels(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{}, func(o *APIOpts) {
		o.MaxPixels = 4
	})
	file := jpegBytes(t, 8, 8)
	body, ctype := multipartBody(t, map[string]string{
		"folder": "voucher",
		"name":   "huge",
		"width":  "400",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["code"] != "IMAGE_TOO_LARGE" {
		t.Fatalf("code = %v, want IMAGE_TOO_LARGE", got["code"])
	}
}

func TestUploadConcurrencyGateReleasesSlots(t *testing.T) {
	// A single slot, more requests than slots, and every one must still complete:
	// a slot that is not returned would hang the rest of the run.
	store := &recordingStorage{}
	env := newTestEnv(t, store, func(o *APIOpts) {
		o.NormalizeConcurrency = 1
	})
	file := jpegBytes(t, 200, 150)

	const requests = 8
	var wg sync.WaitGroup
	codes := make([]int, requests)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, ctype := multipartBody(t, map[string]string{
				"folder": "voucher",
				"name":   fmt.Sprintf("concurrent%d", i),
				"width":  "400",
			}, "photo.jpg", file)
			req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
			req.Header.Set("Content-Type", ctype)
			req.Header.Set("Authorization", "Bearer "+env.token(t, userClaims("USER")))
			res := httptest.NewRecorder()
			env.api.Handler().ServeHTTP(res, req)
			codes[i] = res.Code
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("uploads did not finish: a concurrency slot was not released")
	}

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, code)
		}
	}
	if len(store.uploads) != requests {
		t.Fatalf("uploads = %d, want %d", len(store.uploads), requests)
	}
}

func TestNormalizeGivesUpWhenRequestIsAbandoned(t *testing.T) {
	// The gate is filled by hand so the only ready case is the dead context;
	// racing a real upload against it would leave the outcome to the scheduler.
	u := &uploadHandler{sem: make(chan struct{}, 1), imageOpts: img.DefaultOpts()}
	u.sem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := u.normalize(ctx, nil, img.Info{Format: "jpg"}, 400)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked on the concurrency gate instead of giving up")
	}
}

func TestNormalizeReleasesSlotOnError(t *testing.T) {
	u := &uploadHandler{sem: make(chan struct{}, 1), imageOpts: img.DefaultOpts()}

	if _, err := u.normalize(context.Background(), []byte("not an image"), img.Info{Format: "jpg"}, 400); err == nil {
		t.Fatal("want an error from a non-image source")
	}

	select {
	case u.sem <- struct{}{}:
	default:
		t.Fatal("slot was not released after a failed encode")
	}
}
