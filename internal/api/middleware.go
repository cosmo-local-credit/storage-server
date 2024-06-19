package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rs/cors"
	"github.com/uptrace/bunrouter"
)

type (
	middleware struct {
		errorProvider *errorProvider
		logg          *slog.Logger
	}
)

func (m *middleware) maxUploadSizeMiddleware(next bunrouter.HandlerFunc) bunrouter.HandlerFunc {
	return func(w http.ResponseWriter, r bunrouter.Request) error {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		return next(w, r)
	}
}

func (m *middleware) errorMiddleware(next bunrouter.HandlerFunc) bunrouter.HandlerFunc {
	return func(w http.ResponseWriter, r bunrouter.Request) error {
		err := next(w, r)
		if err == nil {
			return nil
		}

		httpErr := m.errorProvider.from(err)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpErr.HTTPStatusCode())

		enc := json.NewEncoder(w)
		if err := enc.Encode(bunrouter.H{
			"ok":   false,
			"code": httpErr.Error(),
		}); err != nil {
			return err
		}

		return err
	}
}

func newCorsMiddleware(allowedOrigins []string) bunrouter.MiddlewareFunc {
	corsHandler := cors.New(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowCredentials: true,
	})

	return func(next bunrouter.HandlerFunc) bunrouter.HandlerFunc {
		return bunrouter.HTTPHandler(corsHandler.Handler(next))
	}
}
