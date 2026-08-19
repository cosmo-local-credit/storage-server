package api

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"time"

	"github.com/grassrootseconomics/storage-server/internal/image"
	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

type (
	APIOpts struct {
		EnableMetrics        bool
		ListenAddress        string
		MaxBodySize          int64
		MaxPixels            int
		NormalizeConcurrency int
		CORS                 []string
		AllowedFolders       []string
		AllowedWidths        []int
		CDNBaseURL           string
		UploadTimeout        time.Duration
		ClockSkew            time.Duration
		Image                image.Opts
		VerifyingKey         ed25519.PublicKey
		StorageProvider      storage.Storage
		Logg                 *slog.Logger
	}

	API struct {
		logg          *slog.Logger
		errorProvider *errorProvider
		verifyingKey  ed25519.PublicKey
		clockSkew     time.Duration
		server        *http.Server
	}
)

const apiVersion = "/v1"

// The read deadline is configured, since a slow uploader needs it; these bound
// idle and stalled-write connections.
const (
	defaultWriteTimeout = 30 * time.Second
	defaultIdleTimeout  = 60 * time.Second
)

func New(o APIOpts) *API {
	errorProvider := &errorProvider{
		logg: o.Logg,
	}

	api := &API{
		logg:          o.Logg,
		errorProvider: errorProvider,
		verifyingKey:  o.VerifyingKey,
		clockSkew:     o.ClockSkew,
	}

	router := echo.New()
	router.HTTPErrorHandler = api.customHTTPErrorHandler

	router.Use(middleware.Recover())
	router.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:   true,
		LogURI:      true,
		HandleError: true,
		LogValuesFunc: func(_ *echo.Context, v middleware.RequestLoggerValues) error {
			errMsg := ""
			if v.Error != nil {
				errMsg = v.Error.Error()
			}
			switch {
			case v.Status >= http.StatusInternalServerError:
				o.Logg.LogAttrs(context.Background(), slog.LevelError, http.StatusText(v.Status),
					slog.String("uri", v.URI),
					slog.Int("status", v.Status),
					slog.String("err", errMsg),
				)
			default:
				o.Logg.LogAttrs(context.Background(), slog.LevelInfo, http.StatusText(v.Status),
					slog.String("uri", v.URI),
					slog.Int("status", v.Status),
				)
			}
			return nil
		},
	}))
	router.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     o.CORS,
		AllowCredentials: true,
		AllowMethods:     []string{http.MethodGet, http.MethodHead, http.MethodPost},
		AllowHeaders:     []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization},
	}))

	// Grouped by guard: anything added under /v1 is authenticated and body-limited
	// by construction. The monitoring group is deliberately open for scrapers.
	if o.EnableMetrics {
		monitoring := router.Group("")
		monitoring.GET("/metrics", newMetricsHandler().metrics)
	}

	uploadHandler := &uploadHandler{
		storage:        o.StorageProvider,
		maxPixels:      o.MaxPixels,
		allowedFolders: o.AllowedFolders,
		allowedWidths:  o.AllowedWidths,
		cdnBaseURL:     o.CDNBaseURL,
		imageOpts:      o.Image,
		sem:            make(chan struct{}, max(o.NormalizeConcurrency, 1)),
	}

	v1 := router.Group(apiVersion, middleware.BodyLimit(o.MaxBodySize), api.authMiddleware())
	v1.POST("/upload", uploadHandler.upload)

	api.server = &http.Server{
		Addr:         o.ListenAddress,
		Handler:      router,
		ReadTimeout:  o.UploadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}
	return api
}

func (a *API) Handler() http.Handler {
	return a.server.Handler
}

func (a *API) Start() error {
	a.logg.Info("starting API HTTP server", "listen_address", a.server.Addr)
	return a.server.ListenAndServe()
}

func (a *API) Stop(ctx context.Context) error {
	a.logg.Info("shutting down API server")
	return a.server.Shutdown(ctx)
}

func (a *API) customHTTPErrorHandler(c *echo.Context, err error) {
	if r, rErr := echo.UnwrapResponse(c.Response()); rErr == nil && r.Committed {
		return
	}

	httpErr := a.errorProvider.from(err)
	_ = c.JSON(httpErr.HTTPStatusCode(), map[string]any{
		"ok":   false,
		"code": httpErr.Error(),
	})
}
