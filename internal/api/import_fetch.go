package api

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/idna"
)

// ImportOpts configures /v1/import. AllowedHosts must already be normalized,
// see NormalizeHosts; an entry that is not simply never matches.
type ImportOpts struct {
	AllowedHosts  []string
	FetchTimeout  time.Duration
	RatePerMinute int

	// Test seams, unexported so production wiring cannot reach them: tests serve
	// from httptest TLS servers on loopback, which validateAddr refuses.
	resolver     *net.Resolver
	validateAddr func(netip.Addr) error
	rootCAs      *x509.CertPool
}

const (
	defaultFetchTimeout = 15 * time.Second
	importUserAgent     = "storage-server-import"
)

var (
	errRefusedAddress = errors.New("refused address")
	errRedirect       = errors.New("redirects are not followed")
	errEmptyHost      = errors.New("empty host")
)

// NormalizeHost is the one form both configured and requested hosts are
// compared in: IDNA ASCII, lower-case, no trailing dot. Comparing any other way
// lets a mixed-case or Unicode spelling of a listed host miss the list, or an
// unlisted one hit it.
func NormalizeHost(host string) (string, error) {
	h := strings.TrimSuffix(host, ".")
	if h == "" {
		return "", errEmptyHost
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", err
	}
	return strings.ToLower(ascii), nil
}

// NormalizeHosts normalizes a configured allowlist, failing on the first entry
// that cannot be a hostname so a typo stops startup instead of disabling a host.
func NormalizeHosts(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		n, err := NormalizeHost(h)
		if err != nil {
			return nil, fmt.Errorf("import: allowed host %q: %w", h, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// refusedPrefixes are never dialled, whatever host resolved to them. Beyond the
// private, loopback and link-local ranges this covers 0.0.0.0/8, which Linux
// routes to the local host, and the IPv4 ranges that are never public. The
// deprecated IPv4-compatible block ::/96 also holds :: and ::1. 64:ff9b:1::/48 is
// the local-use NAT64 prefix: where the IPv4 address sits in it depends on the
// operator's prefix length, so it is refused whole rather than unwrapped.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// nat64WellKnown embeds the IPv4 address in its last 32 bits (RFC 6052).
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// checkAddr refuses any address that is not publicly routable. IPv4-mapped and
// well-known NAT64 addresses reach their embedded IPv4 address, so they are
// judged as that address.
func checkAddr(addr netip.Addr) error {
	addr = addr.WithZone("").Unmap()
	if nat64WellKnown.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte(b[12:]))
	}
	if !addr.IsValid() {
		return errRefusedAddress
	}
	for _, p := range refusedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s", errRefusedAddress, addr)
		}
	}
	return nil
}

// newImportClient builds the only client import fetches through.
//
// The address check is the dialer's Control hook, which runs on the socket about
// to connect, once for every address tried. Resolution, Happy Eyeballs fallback
// and every reconnect the transport makes all pass through it, so there is no
// lookup to race: a name that rebinds between two connections is judged again on
// the second. The URL keeps its hostname, so SNI and certificate verification are
// against the host that was allowed.
func newImportClient(o ImportOpts) *http.Client {
	validate := o.validateAddr
	if validate == nil {
		validate = checkAddr
	}

	dialer := &net.Dialer{
		Timeout:  o.FetchTimeout,
		Resolver: o.resolver,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errRefusedAddress
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return errRefusedAddress
			}
			return validate(ip)
		},
	}

	transport := &http.Transport{
		// Explicit: a proxy from the environment would make the proxy's address
		// the one dialled, and the check would never see the target's.
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{RootCAs: o.rootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    o.FetchTimeout,
		ResponseHeaderTimeout:  o.FetchTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           16,
		IdleConnTimeout:        90 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   o.FetchTimeout,
		// A redirect would leave the allowlist; the source must answer directly.
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
}

// fetchImport reads at most maxBytes of the response. The returned errors carry
// only an API sentinel in their chain, with the cause as text for the log, so the
// error provider's sentinel match stays unambiguous.
func fetchImport(req *http.Request, client *http.Client, maxBytes int64) ([]byte, error) {
	req.Header.Set("Accept", "image/jpeg, image/png, image/webp")
	req.Header.Set("User-Agent", importUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errRefusedAddress) {
			return nil, fmt.Errorf("%w: %v", ErrImportHostNotAllowed, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrImportFetchFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: status %d", ErrImportFetchFailed, resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w: content length %d over %d", ErrImportFetchFailed, resp.ContentLength, maxBytes)
	}

	// One byte past the cap tells an exactly-full body from an oversized one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImportFetchFailed, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w: body over %d bytes", ErrImportFetchFailed, maxBytes)
	}
	return body, nil
}
