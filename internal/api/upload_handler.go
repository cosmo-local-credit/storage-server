package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/h2non/filetype"
	"github.com/labstack/echo/v5"
)

type uploadHandler struct {
	storage     storage.Storage
	maxBodySize int64
}

func newUploadHandler(storage storage.Storage) *uploadHandler {
	return &uploadHandler{
		storage: storage,
	}
}

func (u *uploadHandler) upload(c *echo.Context) error {
	ctx := c.Request().Context()

	if err := c.Request().ParseMultipartForm(u.maxBodySize); err != nil {
		return err
	}

	file, header, err := c.Request().FormFile("file")
	if err != nil {
		return err
	}
	defer file.Close()

	folder := c.FormValue("folder")
	if folder == "" {
		return ErrFormFolderKeyNotFound
	}

	fileName := c.FormValue("name")
	if fileName == "" {
		return ErrFormFolderKeyNotFound
	}

	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, file); err != nil {
		return err
	}
	kind, err := filetype.Match(buffer.Bytes())
	if err != nil {
		return err
	}

	if kind.Extension == "jpg" || kind.Extension == "png" || kind.Extension == "pdf" || kind.Extension == "webp" {
		filePath := fmt.Sprintf("%s.%s", fileName, kind.Extension)

		if err := u.storage.Upload(
			ctx,
			filePath,
			folder,
			&buffer,
			header.Size,
			kind.MIME.Value,
		); err != nil {
			return err
		}

		return c.JSON(http.StatusOK, map[string]any{
			"ok": true,
			"payload": map[string]any{
				"s3": fmt.Sprintf("%s/%s/%s", s3CDNPath, folder, filePath),
			},
		})
	}

	return ErrNotImageFile
}
