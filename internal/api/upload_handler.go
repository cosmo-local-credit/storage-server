package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/h2non/filetype"
	"github.com/uptrace/bunrouter"
)

type uploadHandler struct {
	storage storage.Storage
}

func newUploadHandler(storage storage.Storage) *uploadHandler {
	return &uploadHandler{
		storage: storage,
	}
}

func (u *uploadHandler) upload(w http.ResponseWriter, r bunrouter.Request) error {
	ctx := r.Context()

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		return err
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return err
	}
	defer file.Close()

	folder := r.FormValue("folder")
	if folder == "" {
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

	if kind.Extension == "jpg" || kind.Extension == "png" || kind.Extension == "pdf" {
		newFileID := strings.Replace(uuid.NewString(), "-", "", -1)
		filePath := fmt.Sprintf("%s.%s", newFileID, kind.Extension)

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

		return bunrouter.JSON(w, bunrouter.H{
			"ok": true,
			"payload": bunrouter.H{
				"s3": fmt.Sprintf("%s/%s/%s", s3CDNPath, folder, filePath),
			},
		})
	} else {
		return ErrNotImageFile
	}
}
