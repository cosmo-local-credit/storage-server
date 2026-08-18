package s3

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/grassrootseconomics/storage-server/internal/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type (
	S3Opts struct {
		Endpoint        string
		AccessKeyID     string
		SecretAccessKey string
		BucketName      string
		Logg            *slog.Logger
	}

	S3 struct {
		bucketName string
		client     *minio.Client
		logg       *slog.Logger
	}
)

const bootstrapTimeout = 10 * time.Second

func New(o S3Opts) (storage.Storage, error) {
	minioClient, err := minio.New(o.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(o.AccessKeyID, o.SecretAccessKey, ""),
		Secure: true,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	exists, err := minioClient.BucketExists(ctx, o.BucketName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("bucket %s does not exist create it manually", o.BucketName)
	}
	o.Logg.Debug("successfully bootstrapped s3 uploader")

	return &S3{
		bucketName: o.BucketName,
		logg:       o.Logg,
		client:     minioClient,
	}, nil
}

func (s *S3) Upload(ctx context.Context, objectName string, path string, ioReader io.Reader, objectSize int64, contentType string) error {
	info, err := s.client.PutObject(ctx, s.bucketName, fmt.Sprintf("%s/%s", path, objectName), ioReader, objectSize, minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: "public, max-age=31536000, immutable",
	})
	if err != nil {
		return err
	}
	s.logg.Debug("s3 sucessfully uploaded file", "upload_key", info.Key, "etag", info.ETag, "size", info.Size)

	return nil
}
