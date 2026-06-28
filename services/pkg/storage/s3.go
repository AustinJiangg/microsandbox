package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is a StorageProvider over an S3-compatible object store -- MinIO in this repo, but the code
// speaks plain S3, so it would run unchanged against AWS S3 (one of E2B's own providers) or GCS's S3
// endpoint. It is Stage 15's running default. minio-go is pure Go, so the host binaries stay static
// (Decision 5), the same static-binary line pgx/go-redis/modernc held before it.
//
// *minio.Object (what GetObject returns) already implements io.ReaderAt + io.Closer, so it satisfies
// RangeReader / uffd.PageSource directly -- a memfile object opened here is the UFFD page source with
// no wrapping, which is the whole reason minio-go was chosen over aws-sdk-go-v2.
type S3 struct {
	client *minio.Client
	bucket string
}

var _ StorageProvider = (*S3)(nil)

// NewS3 connects to endpoint (host:port, no scheme; useSSL toggles https), ensures bucket exists
// (creating it if absent -- which doubles as the readiness probe conftest/dev-up rely on instead of
// a compose healthcheck), and returns the provider.
func NewS3(ctx context.Context, endpoint, accessKey, secretKey, bucket string, useSSL bool) (*S3, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client for %s: %w", endpoint, err)
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %q at %s: %w", bucket, endpoint, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("make bucket %q: %w", bucket, err)
		}
	}
	return &S3{client: client, bucket: bucket}, nil
}

func (s *S3) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy (no request until the first read); Stat now so a missing key fails here
	// rather than mid-copy, matching the os.Open error timing the Local impl gives.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	return obj, nil
}

func (s *S3) OpenReaderAt(ctx context.Context, key string) (RangeReader, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil { // surface a missing memfile up front, not on the first page fault
		obj.Close()
		return nil, fmt.Errorf("open %s for range reads: %w", key, err)
	}
	return obj, nil // *minio.Object is io.ReaderAt + io.Closer
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", key, err)
}
