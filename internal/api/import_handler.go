package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/VictoriaMetrics/metrics"
	img "github.com/cosmo-local-credit/storage-server/internal/image"
	"github.com/labstack/echo/v5"
)

type importHandler struct {
	upload       *uploadHandler
	allowedHosts map[string]struct{}
	client       *http.Client
	maxBytes     int64
	limiter      *rateLimiter
}

type importRequest struct {
	URL      string `json:"url"`
	Folder   string `json:"folder"`
	UploadID string `json:"uploadId"`
	Width    int    `json:"width"`
	Aspect   string `json:"aspect"`
}

// importAspects are the frames the client lays imported images out in. A closed
// set keeps the crop a product decision rather than a request parameter.
var importAspects = map[string]img.Aspect{
	"1:1":  {W: 1, H: 1},
	"16:9": {W: 16, H: 9},
}

// importTypes are what /upload accepts, keyed by the sniffed type. The response's
// Content-Type is the source's claim and is never consulted.
var importTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

func newImportHandler(upload *uploadHandler, o ImportOpts, maxBytes int64) *importHandler {
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = defaultFetchTimeout
	}
	hosts := make(map[string]struct{}, len(o.AllowedHosts))
	for _, h := range o.AllowedHosts {
		hosts[h] = struct{}{}
	}
	h := &importHandler{
		upload:       upload,
		allowedHosts: hosts,
		client:       newImportClient(o),
		maxBytes:     maxBytes,
	}
	if o.RatePerMinute > 0 {
		h.limiter = newRateLimiter(o.RatePerMinute)
	}
	return h
}

// importImage fetches an image from an allowlisted host and stores it exactly as
// /upload would, centre-cropped to the requested aspect first.
func (h *importHandler) importImage(c *echo.Context) (err error) {
	defer func() { recordImportResult(err) }()

	// An empty list is import switched off; nothing below should run.
	if len(h.allowedHosts) == 0 {
		return ErrImportHostNotAllowed
	}

	if h.limiter != nil {
		identity, _ := c.Get(identityKey).(string)
		if wait, ok := h.limiter.allow(identity); !ok {
			c.Response().Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			return ErrRateLimited
		}
	}

	var req importRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		// The body limit answers 413 through this read; keep its status.
		var coder echo.HTTPStatusCoder
		if errors.As(err, &coder) {
			return err
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field == "width" {
			return ErrInvalidWidth
		}
		return echo.ErrBadRequest.Wrap(err)
	}

	if !slices.Contains(h.upload.allowedFolders, req.Folder) {
		return ErrInvalidFolder
	}
	if !namePattern.MatchString(req.UploadID) {
		return ErrInvalidName
	}
	if !slices.Contains(h.upload.allowedWidths, req.Width) {
		return ErrInvalidWidth
	}
	aspect, ok := importAspects[req.Aspect]
	if !ok {
		return ErrInvalidAspect
	}

	target, err := h.target(req.URL)
	if err != nil {
		return err
	}

	ctx := c.Request().Context()
	fetchReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ErrInvalidURL
	}
	src, err := fetchImport(fetchReq, h.client, h.maxBytes)
	if err != nil {
		return err
	}

	if sniffed := http.DetectContentType(src); !importTypes[sniffed] {
		return fmt.Errorf("%w: sniffed %s", ErrImportContentRejected, sniffed)
	}
	info, err := img.Inspect(src, h.upload.maxPixels)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrImportContentRejected, err)
	}

	result, err := h.upload.normalize(ctx, src, info, req.Width, aspect)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Inspect read only the header; a body that fails to decode is still
		// the source's fault, not ours.
		return fmt.Errorf("%w: %v", ErrImportContentRejected, err)
	}

	return h.upload.store(c, req.Folder, req.UploadID, result)
}

// target validates raw and returns the URL to fetch, its host rewritten to the
// normalized form that matched the list so what is fetched is what was checked.
func (h *importHandler) target(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalidURL
	}
	// Credentials in the URL would be forwarded to the source.
	if u.User != nil {
		return nil, ErrInvalidURL
	}
	if u.Scheme != "https" {
		return nil, ErrImportHostNotAllowed
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return nil, ErrInvalidURL
	}

	host, err := NormalizeHost(u.Hostname())
	if err != nil {
		return nil, ErrImportHostNotAllowed
	}
	if _, ok := h.allowedHosts[host]; !ok {
		return nil, ErrImportHostNotAllowed
	}

	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// recordImportResult buckets outcomes into fixed labels; a refused address
// counts under host_not_allowed, which is the signal worth alerting on.
func recordImportResult(err error) {
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrImportHostNotAllowed):
		result = "host_not_allowed"
	case errors.Is(err, ErrImportFetchFailed):
		result = "fetch_failed"
	case errors.Is(err, ErrImportContentRejected):
		result = "content_rejected"
	case errors.Is(err, ErrRateLimited):
		result = "rate_limited"
	default:
		result = "error"
	}
	metrics.GetOrCreateCounter(fmt.Sprintf(`storage_import_total{result=%q}`, result)).Inc()
}
