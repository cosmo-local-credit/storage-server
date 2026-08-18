package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/metrics"
	img "github.com/grassrootseconomics/storage-server/internal/image"
	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/h2non/filetype"
	"github.com/labstack/echo/v5"
	_ "golang.org/x/image/webp"
)

type uploadHandler struct {
	storage        storage.Storage
	maxBodySize    int64
	maxPixels      int
	allowedFolders []string
	allowedWidths  []int
	cdnBaseURL     string
	imageOpts      img.Opts
	sem            chan struct{}
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (u *uploadHandler) upload(c *echo.Context) error {
	ctx := c.Request().Context()

	if err := c.Request().ParseMultipartForm(u.maxBodySize); err != nil {
		return err
	}

	file, _, err := c.Request().FormFile("file")
	if err != nil {
		return err
	}
	defer file.Close()

	folder := c.FormValue("folder")
	if folder == "" {
		return ErrFormFolderKeyNotFound
	}
	if !isAllowedFolder(folder, u.allowedFolders) {
		return ErrInvalidFolder
	}

	fileName := c.FormValue("name")
	if fileName == "" {
		return ErrFormFileNameKeyNotFound
	}
	if !namePattern.MatchString(fileName) {
		return ErrInvalidName
	}

	requestedWidth, err := parseWidth(c.Request().MultipartForm.Value["width"], u.allowedWidths)
	if err != nil {
		return err
	}

	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, file); err != nil {
		return err
	}
	src := buffer.Bytes()
	kind, err := filetype.Match(src)
	if err != nil {
		return err
	}
	if kind.Extension != "jpg" && kind.Extension != "png" && kind.Extension != "webp" {
		return ErrNotImageFile
	}
	if err := rejectOversized(src, u.maxPixels); err != nil {
		return err
	}

	u.sem <- struct{}{}
	start := time.Now()
	result, err := img.Normalize(src, requestedWidth, u.imageOpts)
	<-u.sem
	if err != nil {
		if errors.Is(err, img.ErrTooManyPixels) {
			return ErrImageTooLarge
		}
		if errors.Is(err, img.ErrInvalidWidth) {
			return ErrInvalidWidth
		}
		if errors.Is(err, img.ErrUnsupported) {
			return ErrNotImageFile
		}
		return err
	}

	recordUploadMetrics(kind.Extension, result, requestedWidth, len(src), start)

	filePath := fmt.Sprintf("%s_%d.%s", fileName, result.Width, result.Extension)
	if err := u.storage.Upload(
		ctx,
		filePath,
		folder,
		bytes.NewReader(result.Bytes),
		int64(len(result.Bytes)),
		result.ContentType,
	); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ok": true,
		"payload": map[string]any{
			"s3": fmt.Sprintf("%s/%s/%s", u.cdnBaseURL, folder, filePath),
		},
	})
}

func parseWidth(values []string, allowed []int) (int, error) {
	if len(values) != 1 {
		return 0, ErrInvalidWidth
	}
	width, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, ErrInvalidWidth
	}
	for _, w := range allowed {
		if w == width {
			return width, nil
		}
	}
	return 0, ErrInvalidWidth
}

func rejectOversized(src []byte, maxPixels int) error {
	if maxPixels <= 0 {
		return nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return err
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return ErrImageTooLarge
	}
	return nil
}

func recordUploadMetrics(source string, result img.Result, requested, bytesIn int, start time.Time) {
	metrics.GetOrCreateCounter(`storage_uploads_total`).Inc()
	metrics.GetOrCreateCounter(`storage_upload_bytes_in_total`).Add(bytesIn)
	metrics.GetOrCreateCounter(`storage_upload_bytes_out_total`).Add(len(result.Bytes))
	metrics.GetOrCreateHistogram(`storage_normalize_duration_seconds`).UpdateDuration(start)
	metrics.GetOrCreateCounter(fmt.Sprintf(
		`storage_upload_width_total{requested="%d",actual="%d"}`,
		requested, result.Width,
	)).Inc()
	metrics.GetOrCreateCounter(fmt.Sprintf(
		`storage_upload_format_total{source=%q,output=%q}`,
		source, result.Extension,
	)).Inc()
}
