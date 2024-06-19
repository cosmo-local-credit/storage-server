package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
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
	ErrFormFolderKeyNotFound = errors.New("form folder key not found")
	ErrNotImageFile          = errors.New("uploaded file is not an image")
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
	if errors.Is(err, ErrNotImageFile) {
		return newError(http.StatusBadRequest, "UNSUPPORTED_FILE_EXTENSION")

	}

	e.logg.Error("internal server error", "error", err)
	return newError(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR")
}
