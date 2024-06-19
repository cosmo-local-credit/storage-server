package storage

import (
	"context"
	"io"
)

type Storage interface {
	Upload(
		ctx context.Context,
		fileName string,
		path string,
		ioReader io.Reader,
		size int64,
		contentType string,
	) error
}
