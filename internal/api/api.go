package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

type (
	APIOpts struct {
		EnableMetrics   bool
		ListenAddress   string
		MaxBodySize     int64
		CORS            []string
		UploadTimeout   time.Duration
		StorageProvider storage.Storage
		Logg            *slog.Logger
	}

	API struct {
		logg          *slog.Logger
		errorProvider *errorProvider
		server        *http.Server
	}
)

const (
	apiVersion = "/v1"
	s3CDNPath  = "https://content.sarafu.network"
)

func New(o APIOpts) *API {
	errorProvider := &errorProvider{
		logg: o.Logg,
	}

	api := &API{
		logg:          o.Logg,
		errorProvider: errorProvider,
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
		AllowHeaders:     []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderXRequestedWith},
	}))

	metricsHandler := newMetricshandler(o.EnableMetrics)
	router.GET("/metrics", metricsHandler.metrics)

	uploadHandler := &uploadHandler{
		storage:     o.StorageProvider,
		maxBodySize: o.MaxBodySize,
	}
	v1 := router.Group(apiVersion)
	v1.Use(middleware.BodyLimit(o.MaxBodySize))
	v1.POST("/upload", uploadHandler.upload)

	api.server = &http.Server{
		ReadTimeout: o.UploadTimeout,
		Addr:        o.ListenAddress,
		Handler:     router,
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
