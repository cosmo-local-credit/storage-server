package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/uptrace/bunrouter"
	"github.com/uptrace/bunrouter/extra/reqlog"
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
		logg   *slog.Logger
		server *http.Server
	}
)

const (
	apiVersion = "/v1"
	s3CDNPath  = "https://content.sarafu.network"
)

var maxUploadSize int64

func New(o APIOpts) *API {
	errorProvider := &errorProvider{
		logg: o.Logg,
	}
	middleware := &middleware{
		errorProvider: errorProvider,
		logg:          o.Logg,
	}
	maxUploadSize = o.MaxBodySize

	router := bunrouter.New(
		bunrouter.Use(middleware.errorMiddleware),
		bunrouter.Use(reqlog.NewMiddleware(
			reqlog.WithEnabled(false),
			reqlog.WithVerbose(true),
			reqlog.FromEnv("BUNDEBUG"),
		)),
	)

	metricsHandler := newMetricshandler(o.EnableMetrics)
	router.GET("/metrics", metricsHandler.metrics)

	apiGroup := router.NewGroup(apiVersion,
		bunrouter.Use(middleware.maxUploadSizeMiddleware),
		bunrouter.Use(newCorsMiddleware(o.CORS)),
	)

	uploadHandler := &uploadHandler{
		storage: o.StorageProvider,
	}
	apiGroup.POST("/upload", uploadHandler.upload)

	return &API{
		logg: o.Logg,
		server: &http.Server{
			ReadTimeout: o.UploadTimeout,
			Addr:        o.ListenAddress,
			Handler:     router,
		},
	}
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
