// SPDX-License-Identifier: Apache-2.0

// Package recording stores and serves call audio. The switch writes files;
// this package decides where they live — the local filesystem, or any
// S3-compatible store — and hands them back as streams.
package recording

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/google/uuid"
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
	// it belongs. On S3 it is an upload. It never removes the local spool
	// copy: that is Release, called once the ledger holds the row, because a
	// spool copy deleted before the row exists is a recording nothing can
	// ever book. An error for a file that is not there satisfies
	// errors.Is(err, fs.ErrNotExist); one the store or the disk refused for
	// lack of rights satisfies fs.ErrPermission.
	Ingest(ctx context.Context, key string) (sizeBytes int64, err error)

	// Release removes the local spool copy of a recording whose row is booked
	// and tidies the empty dated directories it leaves. Only the S3 backend
	// with a spool has anything to remove; the filesystem backend, whose
	// directory is the store itself, and S3 without a spool do nothing. A
	// copy already gone is not an error.
	Release(ctx context.Context, key string) error

	// Pending lists the keys of recordings the switch wrote that are older
	// than writtenBefore (by modification time) and may not be booked: the
	// S3 spool's files, or on the filesystem the files of today and
	// yesterday (UTC), which bounds a pass over a directory that keeps every
	// recording for good. Anything not named like a recording key is
	// skipped. S3 without a spool has nothing local and lists nothing.
	Pending(ctx context.Context, writtenBefore time.Time) ([]string, error)

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

// keyPattern is the shape Key produces: UTC date directories, then the call id.
var keyPattern = regexp.MustCompile(
	`^\d{4}/\d{2}/\d{2}/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.wav$`)

// CallIDOf is the call a key belongs to, and false for a name that is not a
// recording key (the stock dialplan's recordings at the root of the directory,
// for one).
func CallIDOf(key string) (uuid.UUID, bool) {
	m := keyPattern.FindStringSubmatch(key)
	if m == nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(m[1])
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
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
	// upload; empty means the switch uploads directly (mod_http_cache) and
	// there is nothing local to spool.
	Dir string

	// S3 settings; endpoint host:port, credentials, bucket.
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3IsSSL     bool

	// s3Transport replaces the HTTP transport of the S3 client; tests use it
	// to trust a TLS stub's certificate.
	s3Transport http.RoundTripper
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
