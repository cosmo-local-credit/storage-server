package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/VictoriaMetrics/metrics"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
)

// bearerPrefix is matched case-insensitively: RFC 7235 defines the auth scheme
// as a case-insensitive token.
const bearerPrefix = "bearer "

// tokenClaims covers both clc-core identity shapes. Only the claim names are
// shared with clc-core; role, service and permission level are read to confirm
// that a token is one of the known shapes and never to make a decision.
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

func (a *API) authMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if err := a.authenticate(c.Request().Header.Get(echo.HeaderAuthorization)); err != nil {
				return err
			}
			return next(c)
		}
	}
}

// authenticate accepts any current clc-core user or service token. Every failure
// is reported as the same bare error so nothing about the token reaches the
// client; the reason is recorded as a metric label instead, because logging it
// alongside the request would risk carrying the credential into the log.
func (a *API) authenticate(header string) error {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return authFailure("malformed_header")
	}
	tokenStr := header[len(bearerPrefix):]
	if tokenStr == "" {
		return authFailure("malformed_header")
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
		return authFailure(failureReason(err))
	}

	// WithIssuedAt only validates iat when it is present, and clc-core always
	// sends one, so require it explicitly.
	if claims.IssuedAt == nil {
		return authFailure("missing_iat")
	}
	if !claims.hasIdentity() {
		return authFailure("unknown_identity")
	}

	metrics.GetOrCreateCounter(`storage_auth_total{result="accepted"}`).Inc()
	return nil
}

func authFailure(reason string) error {
	metrics.GetOrCreateCounter(fmt.Sprintf(
		`storage_auth_total{result="rejected",reason=%q}`, reason,
	)).Inc()
	return ErrUnauthorized
}

// failureReason buckets a parse failure into one of a fixed set of labels, so the
// metric cannot grow a new time series per malformed token.
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
