package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grassrootseconomics/storage-server/internal/api"
	"github.com/grassrootseconomics/storage-server/internal/image"
	"github.com/grassrootseconomics/storage-server/internal/s3"
	"github.com/knadh/koanf/v2"
)

const defaultGracefulShutdownPeriod = time.Second * 5

var (
	build = "dev"

	confFlag string

	lo *slog.Logger
	ko *koanf.Koanf
)

func init() {
	flag.StringVar(&confFlag, "config", "config.toml", "Config file location")
	flag.Parse()

	lo = initLogger()
	ko = initConfig()

	lo.Info("starting storage server", "build", build)
}

func main() {
	ctx, stop := notifyShutdown()
	defer stop()

	s3Uploader, err := s3.New(s3.S3Opts{
		Endpoint:        ko.MustString("s3.endpoint"),
		AccessKeyID:     ko.MustString("s3.access_key_id"),
		SecretAccessKey: ko.MustString("s3.secret_access_key"),
		BucketName:      ko.MustString("s3.bucket_name"),
		Logg:            lo,
	})
	if err != nil {
		lo.Error("could not load s3 storage provider", "error", err)
		os.Exit(1)
	}

	verifyingKey, err := jwt.ParseEdPublicKeyFromPEM([]byte(ko.MustString("auth.public_key")))
	if err != nil {
		lo.Error("could not parse auth public key", "error", err)
		os.Exit(1)
	}

	apiServer := api.New(api.APIOpts{
		EnableMetrics:        ko.Bool("metrics.enable"),
		ListenAddress:        ko.MustString("api.address"),
		MaxBodySize:          ko.MustInt64("api.max_body_size") << 20,
		MaxPixels:            ko.MustInt("image.max_pixels"),
		NormalizeConcurrency: ko.MustInt("image.normalize_concurrency"),
		CORS:                 ko.MustStrings("api.origin"),
		AllowedFolders:       ko.MustStrings("api.allowed_folders"),
		AllowedWidths:        ko.MustInts("image.allowed_widths"),
		CDNBaseURL:           ko.MustString("api.cdn_base_url"),
		UploadTimeout:        ko.MustDuration("api.upload_timeout"),
		ClockSkew:            ko.MustDuration("auth.clock_skew"),
		Image: image.Opts{
			Quality:                ko.MustInt("image.quality"),
			Method:                 ko.MustInt("image.method"),
			MateriallySmallerRatio: ko.MustFloat64("image.materially_smaller_ratio"),
			MinPSNR:                ko.MustFloat64("image.min_psnr"),
		},
		VerifyingKey:    verifyingKey,
		StorageProvider: s3Uploader,
		Logg:            lo,
	})

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- apiServer.Start()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			lo.Error("failed to start HTTP server", "error", err)
			os.Exit(1)
		}
		return
	case <-ctx.Done():
		lo.Info("shutdown signal received")
	}

	// Stop trapping signals so a second one can still kill the process while it
	// is draining.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultGracefulShutdownPeriod)
	defer cancel()

	if err := apiServer.Stop(shutdownCtx); err != nil {
		lo.Error("graceful shutdown period exceeded, forcefully shutting down", "error", err)
		os.Exit(1)
	}
	lo.Info("shutdown complete")
}

func notifyShutdown() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
}
