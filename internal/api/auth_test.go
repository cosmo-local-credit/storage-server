package api

import (
	"crypto/ed25519"
	"crypto/rand"
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
	file := jpegBytes(t, 8, 8)

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
	file := jpegBytes(t, 8, 8)
	bodyFor := func() (*strings.Reader, string) {
		b, ctype := multipartBody(t, map[string]string{
			"folder": "voucher",
			"name":   "authfail",
			"width":  "400",
		}, "photo.jpg", file)
		return strings.NewReader(b.String()), ctype
	}

	otherPub, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = otherPub
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
