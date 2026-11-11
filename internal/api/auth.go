package api

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/VictoriaMetrics/metrics"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
)

// Matched case-insensitively; RFC 7235 makes the scheme a case-insensitive token.
const bearerPrefix = "bearer "

// LoadVerifyingKey parses clc-core's Ed25519 verification key from PEM. The
// concrete return type makes a missing or wrong-typed key a startup failure
// instead of an indistinguishable 401 per request. Only the public half belongs
// here, and a private-key PEM is PKCS#8 rather than PKIX, so it is refused.
func LoadVerifyingKey(publicKeyPem string) (ed25519.PublicKey, error) {
	parsed, err := jwt.ParseEdPublicKeyFromPEM([]byte(publicKeyPem))
	if err != nil {
		return nil, err
	}

	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth: want an ed25519 public key, got %T", parsed)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("auth: ed25519 public key is %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}
	return key, nil
}

// tokenClaims covers both clc-core identity shapes. role and service only confirm
// which shape a token is; they never affect the outcome.
type tokenClaims struct {
	jwt.RegisteredClaims

	UserID          uint64 `json:"userId"`
	EthereumAddress string `json:"ethereumAddress"`
	Role            string `json:"role"`
	Service         bool   `json:"service"`
	PublicKey       string `json:"publicKey"`
}

func (c tokenClaims) hasIdentity() bool {
	if c.Service && c.PublicKey != "" {
		return true
	}
	return c.UserID != 0 && c.EthereumAddress != "" && c.Role != ""
}

// identity names the token's subject, so per-caller limits follow the account
// rather than the token: a refreshed token is the same caller.
func (c tokenClaims) identity() string {
	if c.Service {
		return "service:" + c.PublicKey
	}
	return "user:" + strconv.FormatUint(c.UserID, 10)
}

// identityKey holds the authenticated subject on the echo context.
const identityKey = "storage.identity"

func (a *API) authMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			identity, err := a.authenticate(c.Request().Header.Get(echo.HeaderAuthorization))
			if err != nil {
				return err
			}
			c.Set(identityKey, identity)
			return next(c)
		}
	}
}

// authenticate accepts any current clc-core user or service token and returns
// its subject. Failures collapse to one bare error so nothing about the token
// reaches the client; the reason goes to a metric label rather than the log,
// which would carry the token.
func (a *API) authenticate(header string) (string, error) {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", authFailure("malformed_header")
	}
	tokenStr := header[len(bearerPrefix):]
	if tokenStr == "" {
		return "", authFailure("malformed_header")
	}

	claims := &tokenClaims{}
	if _, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(*jwt.Token) (any, error) { return a.verifyingKey, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(a.clockSkew),
	); err != nil {
		return "", authFailure(failureReason(err))
	}

	// WithIssuedAt only validates iat when present, so require it.
	if claims.IssuedAt == nil {
		return "", authFailure("missing_iat")
	}
	if !claims.hasIdentity() {
		return "", authFailure("unknown_identity")
	}

	metrics.GetOrCreateCounter(`storage_auth_total{result="accepted"}`).Inc()
	return claims.identity(), nil
}

func authFailure(reason string) error {
	metrics.GetOrCreateCounter(fmt.Sprintf(
		`storage_auth_total{result="rejected",reason=%q}`, reason,
	)).Inc()
	return ErrUnauthorized
}

// failureReason buckets failures into fixed labels so the metric cannot grow a
// series per malformed token.
func failureReason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "not_yet_valid"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "missing_claim"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "bad_signature"
	case errors.Is(err, jwt.ErrTokenUnverifiable), errors.Is(err, jwt.ErrTokenMalformed):
		return "unverifiable"
	default:
		return "invalid"
	}
}
