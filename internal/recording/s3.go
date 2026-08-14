// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Storage keeps recordings in any S3-compatible store.
//
// The switch still records to the local spool — it knows nothing about object
// stores — and ingestion is the upload that makes the file durable, after
// which the spool copy is deleted. The client library speaks plain S3, so
// AWS, SeaweedFS, MinIO and the rest are all the same backend with a
// different endpoint.
type s3Storage struct {
	client *minio.Client
	bucket string
	spool  *fsStorage
}

func newS3Storage(cfg Config) (*s3Storage, error) {
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" {
		return nil, fmt.Errorf("recording: the S3 backend needs an endpoint and a bucket")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("recording: the S3 backend needs the spool directory the switch records into")
	}
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3IsSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("recording: s3 client: %w", err)
	}
	return &s3Storage{
		client: client,
		bucket: cfg.S3Bucket,
		spool:  newFSStorage(cfg.Dir),
	}, nil
}

func (s *s3Storage) Backend() string { return "S3" }
func (s *s3Storage) Bucket() string  { return s.bucket }

// EnsureBucket creates the bucket when it does not exist, so first start
// against a fresh store needs no manual step.
func (s *s3Storage) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("recording: check bucket %s: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("recording: create bucket %s: %w", s.bucket, err)
	}
	return nil
}

func (s *s3Storage) Ingest(ctx context.Context, key string) (int64, error) {
	local, err := s.spool.resolve(key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(local)
	if err != nil {
		return 0, err
	}

	if _, err := s.client.FPutObject(ctx, s.bucket, key, local,
		minio.PutObjectOptions{ContentType: "audio/wav"}); err != nil {
		return 0, fmt.Errorf("recording: upload %s: %w", key, err)
	}
	// The spool copy has served its purpose. Failing to remove it costs disk,
	// not correctness, so it is logged by the caller rather than fatal here.
	_ = os.Remove(local)
	_ = removeEmptyParents(local, s.spool.root)
	return info.Size(), nil
}

// Open downloads to a seekable temporary file.
//
// Streaming straight from the store cannot honour range requests without one
// request per range; a recording is a few megabytes, and playback scrubbing
// wants cheap seeks. The temporary file is unlinked immediately, so it lives
// exactly as long as the response.
func (s *s3Storage) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("recording: get %s: %w", key, err)
	}
	defer object.Close()

	tmp, err := os.CreateTemp("", "aicc-recording-*.wav")
	if err != nil {
		return nil, 0, err
	}
	// Unlink now: the descriptor keeps the bytes alive until Close.
	_ = os.Remove(tmp.Name())

	size, err := io.Copy(tmp, object)
	if err != nil {
		tmp.Close()
		return nil, 0, fmt.Errorf("recording: download %s: %w", key, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return nil, 0, err
	}
	return tmp, size, nil
}

func (s *s3Storage) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// removeEmptyParents tidies the dated spool directories a removed file leaves
// behind, stopping at the root.
func removeEmptyParents(file, root string) error {
	dir := filepath.Dir(file)
	rootClean := filepath.Clean(root)
	for dir != rootClean && len(dir) > len(rootClean) {
		if err := os.Remove(dir); err != nil {
			return nil // not empty or not removable: done tidying
		}
		dir = filepath.Dir(dir)
	}
	return nil
}
