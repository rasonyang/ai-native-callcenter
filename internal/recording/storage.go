// SPDX-License-Identifier: Apache-2.0

// Package recording stores and serves call audio. The switch writes files;
// this package decides where they live — the local filesystem, or any
// S3-compatible store — and hands them back as streams.
package recording

import (
	"context"
	"fmt"
	"io"
	"path"
	"time"
)

// Storage is one place recordings live.
//
// The demo runs on the filesystem and a production deployment points the same
// configuration at an S3-compatible store; nothing else changes. That is the
// single-executable philosophy applied to artifacts: no object store ships
// with the product, one can be attached to it.
type Storage interface {
	// Backend names the implementation, matching the recordings.backend enum.
	Backend() string
	// Bucket is the S3 bucket, empty on the filesystem.
	Bucket() string

	// Ingest takes the file the switch wrote and makes it durable, returning
	// its size. On the filesystem that is a stat — the file is already where
	// it belongs. On S3 it is an upload followed by removing the local spool
	// copy.
	Ingest(ctx context.Context, key string) (sizeBytes int64, err error)

	// Open returns the audio for streaming. The reader seeks, which is what
	// lets playback honour range requests.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)

	// Delete removes the object, for retention.
	Delete(ctx context.Context, key string) error
}

// Key is the canonical object key for a call's audio: the UTC date the call
// started, then the call id. Both backends use it verbatim.
func Key(startedAt time.Time, callID string) string {
	return path.Join(startedAt.UTC().Format("2006/01/02"), callID+".wav")
}

// wavHeaderSize is what a canonical RIFF/WAVE header occupies before samples.
const wavHeaderSize = 44

// DurationSec estimates a recording's length from its byte size.
//
// The switch writes stereo 16-bit PCM at the telephone rate, so the arithmetic
// is exact for its own files; anything else gets a floor of zero rather than
// an invented number.
func DurationSec(sizeBytes int64) int {
	const bytesPerSecond = 8000 * 2 * 2 // rate × 16-bit × stereo
	if sizeBytes <= wavHeaderSize {
		return 0
	}
	return int((sizeBytes - wavHeaderSize) / bytesPerSecond)
}

// Config selects and parameterises a backend.
type Config struct {
	// Backend is FS or S3.
	Backend string
	// Dir is the filesystem root — the same directory the switch records
	// into. On the S3 backend it is the spool the switch writes before
	// upload.
	Dir string

	// S3 settings; endpoint host:port, credentials, bucket.
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3IsSSL     bool
}

// New builds the configured backend.
func New(cfg Config) (Storage, error) {
	switch cfg.Backend {
	case "", "FS":
		if cfg.Dir == "" {
			return nil, fmt.Errorf("recording: the filesystem backend needs a directory")
		}
		return newFSStorage(cfg.Dir), nil
	case "S3":
		return newS3Storage(cfg)
	default:
		return nil, fmt.Errorf("recording: unknown backend %q (FS, S3)", cfg.Backend)
	}
}
