package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/metrics"
	img "github.com/grassrootseconomics/storage-server/internal/image"
	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/labstack/echo/v5"
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
	if !slices.Contains(u.allowedFolders, folder) {
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

	// Validate the container and the pixel count from the header before taking a
	// slot, so a junk or oversized upload is refused without waiting behind the
	// images that are actually being encoded.
	info, err := img.Inspect(src, u.maxPixels)
	if err != nil {
		return imageError(err)
	}

	result, err := u.normalize(ctx, src, info, requestedWidth)
	if err != nil {
		return imageError(err)
	}

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

// normalize encodes one image while holding a slot from the concurrency gate.
//
// Normalize allocates on the order of the source's pixel count, so the gate
// bounds memory rather than CPU and is sized independently of core count. The
// slot is taken with the request context in play so a burst does not queue
// behind clients that have already gone away, and released with defer so a panic
// recovered by the router cannot retire a slot for the lifetime of the process.
// It is given up before the upload starts; waiting on object storage does not
// need a decode budget.
func (u *uploadHandler) normalize(ctx context.Context, src []byte, info img.Info, width int) (img.Result, error) {
	select {
	case u.sem <- struct{}{}:
	case <-ctx.Done():
		return img.Result{}, ctx.Err()
	}
	defer func() { <-u.sem }()

	start := time.Now()
	result, err := img.Normalize(src, info, width, u.imageOpts)
	if err != nil {
		return img.Result{}, err
	}
	recordUploadMetrics(info.Format, result, width, len(src), start)
	return result, nil
}

// imageError maps the image package's sentinels onto the API's error vocabulary.
func imageError(err error) error {
	switch {
	case errors.Is(err, img.ErrTooManyPixels):
		return ErrImageTooLarge
	case errors.Is(err, img.ErrInvalidWidth):
		return ErrInvalidWidth
	case errors.Is(err, img.ErrUnsupported):
		return ErrNotImageFile
	default:
		return err
	}
}

func parseWidth(values []string, allowed []int) (int, error) {
	if len(values) != 1 {
		return 0, ErrInvalidWidth
	}
	width, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, ErrInvalidWidth
	}
	if !slices.Contains(allowed, width) {
		return 0, ErrInvalidWidth
	}
	return width, nil
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
		`storage_upload_format_total{source=%q,output=%q,lossless="%t"}`,
		source, result.Extension, result.Lossless,
	)).Inc()
}
