package api

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"regexp"

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
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, file); err != nil {
		return err
	}
	kind, err := filetype.Match(buffer.Bytes())
	if err != nil {
		return err
	}

	if kind.Extension == "jpg" || kind.Extension == "png" || kind.Extension == "pdf" || kind.Extension == "webp" {
		if kind.Extension != "pdf" {
			if err := rejectOversized(buffer.Bytes(), u.maxPixels); err != nil {
				return err
			}
		}

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
