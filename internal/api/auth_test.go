package api

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestUploadAcceptsKnownIdentities(t *testing.T) {
	store := &recordingStorage{}
	env := newTestEnv(t, store)
	file := photoJPEG(t, 8, 8, 1)

	for _, claims := range []jwt.Claims{
		userClaims("USER"),
		userClaims("SUPERADMIN"),
		serviceClaims(),
	} {
		body, ctype := multipartBody(t, map[string]string{
			"folder": "voucher",
			"name":   "oktoken",
			"width":  "400",
		}, "photo.jpg", file)
		req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+env.token(t, claims))
		res := httptest.NewRecorder()
		env.api.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("claims=%#v status=%d body=%s", claims, res.Code, res.Body.String())
		}
	}
	if len(store.uploads) != 3 {
		t.Fatalf("uploads = %d, want 3", len(store.uploads))
	}
}

func TestUploadAuthFailures(t *testing.T) {
	env := newTestEnv(t, &recordingStorage{})
	file := photoJPEG(t, 8, 8, 1)
	bodyFor := func() (*strings.Reader, string) {
		b, ctype := multipartBody(t, map[string]string{
			"folder": "voucher",
			"name":   "authfail",
			"width":  "400",
		}, "photo.jpg", file)
		return strings.NewReader(b.String()), ctype
	}

	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongSig, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, userClaims("USER")).SignedString(otherPriv)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := jwt.NewWithClaims(jwt.SigningMethodHS256, userClaims("USER")).SignedString([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	expired := userClaims("USER")
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))

	missingExp := userClaims("USER")
	missingExp.ExpiresAt = nil

	missingIat := userClaims("USER")
	missingIat.IssuedAt = nil

	noIdentity := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}

	cases := []struct {
		name   string
		header string
	}{
		{name: "missing header"},
		{name: "malformed bearer", header: "Bearer"},
		{name: "basic scheme", header: "Basic abc"},
		{name: "bearer with no space", header: "Bearer" + env.token(t, userClaims("USER"))},
		{name: "empty bearer token", header: "Bearer "},
		{name: "invalid signature", header: "Bearer " + wrongSig},
		{name: "expired", header: "Bearer " + env.token(t, expired)},
		{name: "missing exp", header: "Bearer " + env.token(t, missingExp)},
		{name: "missing iat", header: "Bearer " + env.token(t, missingIat)},
		{name: "wrong algorithm", header: "Bearer " + hs},
		{name: "unknown identity", header: "Bearer " + env.token(t, noIdentity)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ctype := bodyFor()
			req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
			req.Header.Set("Content-Type", ctype)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			res := httptest.NewRecorder()
			env.api.Handler().ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 body=%s", res.Code, res.Body.String())
			}
			got := decodeJSON(t, res)
			if got["code"] != "UNAUTHORIZED" {
				t.Fatalf("code = %v, want UNAUTHORIZED", got["code"])
			}
		})
	}
}

func TestUploadAcceptsAnyCaseBearerScheme(t *testing.T) {
	// RFC 7235 makes the auth scheme a case-insensitive token, so a client that
	// sends it lowercase must not be turned away.
	store := &recordingStorage{}
	env := newTestEnv(t, store)
	file := photoJPEG(t, 64, 48, 1)

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		body, ctype := multipartBody(t, map[string]string{
			"folder": "voucher",
			"name":   "anycase",
			"width":  "400",
		}, "photo.jpg", file)
		req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", scheme+" "+env.token(t, userClaims("USER")))
		res := httptest.NewRecorder()
		env.api.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("scheme %q: status = %d, want 200 body=%s", scheme, res.Code, res.Body.String())
		}
	}
}

func TestUploadRejectsBeforeParsingBody(t *testing.T) {
	// An unauthenticated request must cost nothing beyond the header check: the
	// body here is not valid multipart at all, and a 400 would prove it was read.
	env := newTestEnv(t, &recordingStorage{})
	req := httptest.NewRequest(http.MethodPost, "/v1/upload", strings.NewReader("not multipart at all"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	res := httptest.NewRecorder()
	env.api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before the body is parsed body=%s", res.Code, res.Body.String())
	}
	if got := decodeJSON(t, res)["code"]; got != "UNAUTHORIZED" {
		t.Fatalf("code = %v, want UNAUTHORIZED", got)
	}
}

func pemBlock(t *testing.T, kind string, der []byte) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

func TestLoadVerifyingKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	got, err := LoadVerifyingKey(pemBlock(t, "PUBLIC KEY", pubDER))
	if err != nil {
		t.Fatalf("valid public key rejected: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatal("loaded key does not match the generated key")
	}

	for _, tc := range []struct {
		name string
		pem  string
	}{
		// This service verifies; it must never be handed the signing key, so a
		// private-key PEM has to be refused rather than quietly used.
		{"ed25519 private key", pemBlock(t, "PRIVATE KEY", privDER)},
		{"non-ed25519 public key", pemBlock(t, "PUBLIC KEY", ecDER)},
		{"empty", ""},
		{"not pem", "MCowBQYDK2VwAyEAd5osN95MX4qOplzCcXvWggSe79YKbJLJbQWEaL0sRnE="},
		{"truncated key", pemBlock(t, "PUBLIC KEY", pubDER[:len(pubDER)-4])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadVerifyingKey(tc.pem); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestLoadedKeyVerifiesRealTokens(t *testing.T) {
	// Close the loop: a key that came through PEM must accept a token signed by
	// its private half and reject one signed by any other.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadVerifyingKey(pemBlock(t, "PUBLIC KEY", pubDER))
	if err != nil {
		t.Fatal(err)
	}

	store := &recordingStorage{}
	env := newTestEnv(t, store, func(o *APIOpts) {
		o.VerifyingKey = loaded
	})
	file := photoJPEG(t, 64, 48, 1)

	sign := func(key ed25519.PrivateKey) int {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, userClaims("USER")).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		body, ctype := multipartBody(t, map[string]string{
			"folder": "voucher", "name": "pemloaded", "width": "400",
		}, "photo.jpg", file)
		req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		env.api.Handler().ServeHTTP(res, req)
		return res.Code
	}

	if code := sign(priv); code != http.StatusOK {
		t.Fatalf("token from the matching private key: status = %d, want 200", code)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if code := sign(otherPriv); code != http.StatusUnauthorized {
		t.Fatalf("token from a foreign key: status = %d, want 401", code)
	}
}
