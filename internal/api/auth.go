package api

import (
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
)

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

func (a *API) authenticate(header string) error {
	tokenStr, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || tokenStr == "" {
		return ErrUnauthorized
	}

	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(t *jwt.Token) (any, error) {
			if t.Method.Alg() != jwt.SigningMethodEdDSA.Alg() {
				return nil, jwt.ErrTokenSignatureInvalid
			}
			return a.verifyingKey, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(a.clockSkew),
	)
	if err != nil || !token.Valid || claims.IssuedAt == nil || !claims.hasIdentity() {
		return ErrUnauthorized
	}
	return nil
}

func isAllowedFolder(folder string, allowed []string) bool {
	for _, f := range allowed {
		if f == folder {
			return true
		}
	}
	return false
}
