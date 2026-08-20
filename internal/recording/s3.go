// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Storage keeps recordings in any S3-compatible store.
//
// Two ways in, one way out. With a spool directory, the switch records
// locally and ingestion is the upload that makes the file durable, after
// which the spool copy is deleted. Without one, the switch uploads directly —
// mod_http_cache PUTs the finished file over HTTP (on SeaweedFS the filer
// path /buckets/<bucket>/<key> and the S3 object <bucket>/<key> are the same
// file) — and ingestion only confirms the object arrived. The client library
// speaks plain S3, so AWS, SeaweedFS, MinIO and the rest are all the same
// backend with a different endpoint.
type s3Storage struct {
	client *minio.Client
	bucket string
	spool  *fsStorage
}

func newS3Storage(cfg Config) (*s3Storage, error) {
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" {
		return nil, fmt.Errorf("recording: the S3 backend needs an endpoint and a bucket")
	}
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3IsSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("recording: s3 client: %w", err)
	}
	s := &s3Storage{client: client, bucket: cfg.S3Bucket}
	// No spool means the switch uploads directly — mod_http_cache PUTs the
	// finished file into the store itself — and ingestion only confirms the
	// object exists.
	if cfg.Dir != "" {
		s.spool = newFSStorage(cfg.Dir)
	}
	return s, nil
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
	if s.spool != nil {
		local, err := s.spool.resolve(key)
		if err != nil {
			return 0, err
		}
		info, statErr := os.Stat(local)
		if statErr == nil {
			if _, err := s.client.FPutObject(ctx, s.bucket, key, local,
				minio.PutObjectOptions{ContentType: "audio/wav"}); err != nil {
				return 0, fmt.Errorf("recording: upload %s: %w", key, err)
			}
			// The spool copy has served its purpose. Failing to remove it
			// costs disk, not correctness, so it is logged by the caller
			// rather than fatal here.
			_ = os.Remove(local)
			_ = removeEmptyParents(local, s.spool.root)
			return info.Size(), nil
		}
		if !os.IsNotExist(statErr) {
			return 0, statErr
		}
		// No spool file: the switch may have uploaded directly. Fall through
		// to confirming the object in the store.
	}
	return s.statUploaded(ctx, key)
}

// statUploaded confirms an object the switch PUT into the store itself.
//
// mod_http_cache uploads when the recording file closes, which races the
// hangup events that trigger ingestion; a short retry covers an upload still
// in flight without inventing a recording that was never made.
func (s *s3Storage) statUploaded(ctx context.Context, key string) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
		if err == nil {
			return info.Size, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("recording: not in the store: %w", lastErr)
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
