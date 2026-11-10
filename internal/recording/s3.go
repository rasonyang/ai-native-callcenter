// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
// file) — and ingestion only confirms the object arrived.
//
// The client library speaks plain S3, and S3-compatible stores agree on the
// basics, but not on how a PUT body is signed. The one behaviour this
// backend relies on is a single PUT carrying the payload's SHA-256 in
// X-Amz-Content-Sha256, signed as such. The library's default differs by
// transport: over plain HTTP it streams the body as aws-chunked with a
// per-chunk signature, which some stores refuse ("Aws MultiChunkedEncoding is
// not supported"), while over HTTPS it sends UNSIGNED-PAYLOAD. upload pins
// one request shape for both, and SelfCheck proves the store takes it.
type s3Storage struct {
	client   *minio.Client
	bucket   string
	spool    *fsStorage
	endpoint string
}

func newS3Storage(cfg Config) (*s3Storage, error) {
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" {
		return nil, fmt.Errorf("recording: the S3 backend needs an endpoint and a bucket")
	}
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure:    cfg.S3IsSSL,
		Transport: cfg.s3Transport,
	})
	if err != nil {
		return nil, fmt.Errorf("recording: s3 client: %w", err)
	}
	s := &s3Storage{client: client, bucket: cfg.S3Bucket, endpoint: cfg.S3Endpoint}
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

// CheckBucket reports whether the bucket exists, creating nothing. Doctor
// uses it where EnsureBucket's side effect is unwanted.
func (s *s3Storage) CheckBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("recording: check bucket %s: %w", s.bucket, err)
	}
	if !exists {
		return fmt.Errorf("recording: bucket %s does not exist at %s", s.bucket, s.endpoint)
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
			if err := s.upload(ctx, key, local); err != nil {
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

// upload stores the file at path under key as one PUT whose
// X-Amz-Content-Sha256 is the file's SHA-256.
//
// FPutObject is avoided on purpose: over plain HTTP it streams the body as
// aws-chunked, and above 16 MiB it switches to multipart. Core.PutObject with
// DisableContentSha256 skips the streaming signature, and the non-empty hash
// is then signed into the request as the ordinary payload hash, the same on
// HTTP and HTTPS. The file is hashed by streaming it, then rewound; the body
// stays seekable, so the library can retry the PUT.
func (s *s3Storage) upload(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = minio.Core{Client: s.client}.PutObject(ctx, s.bucket, key, f, size, "",
		hex.EncodeToString(h.Sum(nil)),
		minio.PutObjectOptions{ContentType: "audio/wav", DisableContentSha256: true})
	return err
}

// selfCheckPrefix holds the probe objects. Key never produces it (a dated
// path starting with a digit), so a probe cannot collide with a recording.
const selfCheckPrefix = "_aicc/selfcheck/"

// SelfCheck proves the store takes what Ingest sends: it uploads a few bytes
// through the same upload path, reads them back, and deletes them. It checks
// the upload path of a spool deployment, so without a spool (the switch
// uploads directly and this process never PUTs) it does nothing.
func (s *s3Storage) SelfCheck(ctx context.Context) (err error) {
	if s.spool == nil {
		return nil
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	key := selfCheckPrefix + hex.EncodeToString(rnd[:]) + ".bin"
	defer func() {
		// Best effort on every path: a probe left behind is clutter, and a
		// failing delete must not mask the failure being reported.
		if rmErr := s.client.RemoveObject(context.WithoutCancel(ctx), s.bucket, key,
			minio.RemoveObjectOptions{}); rmErr != nil && err == nil {
			err = fmt.Errorf("recording self-check: delete probe %s: %w", key, rmErr)
		}
	}()

	want := []byte("aicc recording storage self-check\n")
	dir, mkErr := os.MkdirTemp("", "aicc-selfcheck-*")
	if mkErr != nil {
		return mkErr
	}
	defer os.RemoveAll(dir)
	probe := filepath.Join(dir, "probe.bin")
	if wErr := os.WriteFile(probe, want, 0o600); wErr != nil {
		return wErr
	}
	if upErr := s.upload(ctx, key, probe); upErr != nil {
		return fmt.Errorf("recording self-check: upload probe %s: %w", key, upErr)
	}
	obj, getErr := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if getErr != nil {
		return fmt.Errorf("recording self-check: read probe back: %w", getErr)
	}
	defer obj.Close()
	got, readErr := io.ReadAll(obj)
	if readErr != nil {
		return fmt.Errorf("recording self-check: read probe back: %w", readErr)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("recording self-check: the store returned %d bytes that differ from the %d uploaded", len(got), len(want))
	}
	return nil
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
