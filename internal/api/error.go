package api

import (
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
		StatusCode int    `json:"status"`
		Message    string `json:"message"`
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

func (e *httpError) HTTPStatusCode() int {
	return e.StatusCode
}

func (e *httpError) Error() string {
	return e.Message
}

func newError(status int, message string) Error {
	return &httpError{
		StatusCode: status,
		Message:    message,
	}
}

func (e *errorProvider) from(err error) Error {
	switch err := err.(type) {
	case Error:
		return err
	case *http.MaxBytesError:
		return newError(http.StatusRequestEntityTooLarge, "FILE_SIZE_LIMIT_EXCEEDED")
	}

	if errors.Is(err, echo.ErrStatusRequestEntityTooLarge) {
		return newError(http.StatusRequestEntityTooLarge, "FILE_SIZE_LIMIT_EXCEEDED")
	}
	var sc echo.HTTPStatusCoder
	if errors.As(err, &sc) {
		switch sc.StatusCode() {
		case http.StatusRequestEntityTooLarge:
			return newError(http.StatusRequestEntityTooLarge, "FILE_SIZE_LIMIT_EXCEEDED")
		case http.StatusNotFound:
			return newError(http.StatusNotFound, "NOT_FOUND")
		}
	}

	if errors.Is(err, io.EOF) {
		return newError(http.StatusBadRequest, "EOF")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return newError(http.StatusRequestTimeout, "DEADLINE_EXCEEDED")
	}
	if errors.Is(err, http.ErrNotMultipart) {
		return newError(http.StatusBadRequest, "NOT_MULTIPART")
	}
	if errors.Is(err, http.ErrMissingFile) {
		return newError(http.StatusBadRequest, "MISSING_FILE")
	}
	if errors.Is(err, ErrFormFolderKeyNotFound) {
		return newError(http.StatusBadRequest, "MISSING_FOLDER")
	}
	if errors.Is(err, ErrFormFileNameKeyNotFound) {
		return newError(http.StatusBadRequest, "MISSING_NAME")
	}
	if errors.Is(err, ErrInvalidFolder) {
		return newError(http.StatusBadRequest, "INVALID_FOLDER")
	}
	if errors.Is(err, ErrInvalidName) {
		return newError(http.StatusBadRequest, "INVALID_NAME")
	}
	if errors.Is(err, ErrImageTooLarge) {
		return newError(http.StatusBadRequest, "IMAGE_TOO_LARGE")
	}
	if errors.Is(err, ErrInvalidWidth) {
		return newError(http.StatusBadRequest, "INVALID_WIDTH")
	}
	if errors.Is(err, ErrNotImageFile) {
		return newError(http.StatusBadRequest, "UNSUPPORTED_FILE_EXTENSION")
	}
	if errors.Is(err, ErrUnauthorized) {
		return newError(http.StatusUnauthorized, "UNAUTHORIZED")
	}

	e.logg.Error("internal server error", "error", err)
	return newError(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR")
}
