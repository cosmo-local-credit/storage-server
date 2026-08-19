package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/labstack/echo/v5"
)

type (
	Error interface {
		error
		HTTPStatusCode() int
	}

	httpError struct {
		statusCode int
		code       string
	}

	errorProvider struct {
		logg *slog.Logger
	}
)

var (
	ErrFormFolderKeyNotFound   = errors.New("form folder key not found")
	ErrFormFileNameKeyNotFound = errors.New("form file name key not found")
	ErrNotImageFile            = errors.New("uploaded file is not an image")
	ErrUnauthorized            = errors.New("unauthorized")
	ErrInvalidFolder           = errors.New("invalid folder")
	ErrInvalidName             = errors.New("invalid name")
	ErrImageTooLarge           = errors.New("image exceeds max pixels")
	ErrInvalidWidth            = errors.New("invalid width")
)

// Distinct sentinels, so order does not matter.
var sentinelErrors = []struct {
	err    error
	status int
	code   string
}{
	{ErrUnauthorized, http.StatusUnauthorized, "UNAUTHORIZED"},
	{ErrFormFolderKeyNotFound, http.StatusBadRequest, "MISSING_FOLDER"},
	{ErrFormFileNameKeyNotFound, http.StatusBadRequest, "MISSING_NAME"},
	{ErrInvalidFolder, http.StatusBadRequest, "INVALID_FOLDER"},
	{ErrInvalidName, http.StatusBadRequest, "INVALID_NAME"},
	{ErrInvalidWidth, http.StatusBadRequest, "INVALID_WIDTH"},
	{ErrImageTooLarge, http.StatusBadRequest, "IMAGE_TOO_LARGE"},
	{ErrNotImageFile, http.StatusBadRequest, "UNSUPPORTED_FILE_EXTENSION"},
	{http.ErrNotMultipart, http.StatusBadRequest, "NOT_MULTIPART"},
	{http.ErrMissingFile, http.StatusBadRequest, "MISSING_FILE"},
	{io.EOF, http.StatusBadRequest, "EOF"},
	{os.ErrDeadlineExceeded, http.StatusRequestTimeout, "DEADLINE_EXCEEDED"},
	{context.DeadlineExceeded, http.StatusRequestTimeout, "DEADLINE_EXCEEDED"},
	{context.Canceled, http.StatusRequestTimeout, "REQUEST_CANCELED"},
}

// Statuses Echo raises itself; without an entry a router refusal reads as a 500.
var echoStatusCodes = map[int]string{
	http.StatusBadRequest:            "BAD_REQUEST",
	http.StatusNotFound:              "NOT_FOUND",
	http.StatusMethodNotAllowed:      "METHOD_NOT_ALLOWED",
	http.StatusRequestEntityTooLarge: "FILE_SIZE_LIMIT_EXCEEDED",
	http.StatusUnsupportedMediaType:  "UNSUPPORTED_MEDIA_TYPE",
}

func (e *httpError) HTTPStatusCode() int {
	return e.statusCode
}

func (e *httpError) Error() string {
	return e.code
}

func newError(status int, code string) Error {
	return &httpError{
		statusCode: status,
		code:       code,
	}
}

func (e *errorProvider) from(err error) Error {
	var apiErr Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	for _, s := range sentinelErrors {
		if errors.Is(err, s.err) {
			return newError(s.status, s.code)
		}
	}

	// Carries the limit, so it is not a sentinel.
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return newError(http.StatusRequestEntityTooLarge, "FILE_SIZE_LIMIT_EXCEEDED")
	}

	var coder echo.HTTPStatusCoder
	if errors.As(err, &coder) {
		if code, ok := echoStatusCodes[coder.StatusCode()]; ok {
			return newError(coder.StatusCode(), code)
		}
	}

	e.logg.Error("internal server error", "error", err)
	return newError(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR")
}
