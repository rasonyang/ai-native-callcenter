// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeReleaser struct {
	keys []string
	err  error
}

func (f *fakeReleaser) ReleaseSpoolFile(key string) error {
	f.keys = append(f.keys, key)
	return f.err
}

// lockedSpool is a spool holding one recording in a directory its owner
// cannot unlink in, as the switch's 0750 directories are to the application.
func lockedSpool(t *testing.T) (s *s3Storage, key, local string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can unlink anywhere")
	}
	spool := t.TempDir()
	var err error
	s, err = newS3Storage(Config{Backend: "S3", Dir: spool, S3Endpoint: "127.0.0.1:1", S3Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	key = Key(time.Now(), uuid.Must(uuid.NewV7()).String())
	local = writeSpoolFile(t, spool, key, 100)
	dir := filepath.Dir(local)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return s, key, local
}

func TestReleaseAsksTheSwitchWhenTheApplicationMayNotRemove(t *testing.T) {
	t.Parallel()
	s, key, _ := lockedSpool(t)
	r := &fakeReleaser{}
	s.SetSpoolReleaser(r)
	if err := s.Release(t.Context(), key); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(r.keys) != 1 || r.keys[0] != key {
		t.Errorf("the switch was asked for %v, want [%s]", r.keys, key)
	}
}

func TestReleaseNamesBothCausesWhenTheSwitchFailsToo(t *testing.T) {
	t.Parallel()
	s, key, _ := lockedSpool(t)
	s.SetSpoolReleaser(&fakeReleaser{err: errors.New("REMOVE_FAILED Permission denied")})
	err := s.Release(t.Context(), key)
	if err == nil {
		t.Fatal("a failed release reported success")
	}
	for _, want := range []string{"permission denied", "REMOVE_FAILED"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the local cause is lost: %v", err)
	}
}

func TestReleaseWithoutAReleaserReturnsTheLocalError(t *testing.T) {
	t.Parallel()
	s, key, _ := lockedSpool(t)
	err := s.Release(t.Context(), key)
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want the local permission error", err)
	}
}

func TestReleaseDoesNotAskTheSwitchWhatTheApplicationRemoved(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	s, err := newS3Storage(Config{Backend: "S3", Dir: spool, S3Endpoint: "127.0.0.1:1", S3Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeReleaser{err: errors.New("must not be asked")}
	s.SetSpoolReleaser(r)
	key := Key(time.Now(), uuid.Must(uuid.NewV7()).String())
	local := writeSpoolFile(t, spool, key, 100)
	if err := s.Release(t.Context(), key); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(local)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the empty dated directory was left")
	}
	// Already gone is success and short-circuits too.
	if err := s.Release(t.Context(), key); err != nil {
		t.Fatalf("release of a missing copy: %v", err)
	}
	if len(r.keys) != 0 {
		t.Errorf("the switch was asked for %v", r.keys)
	}
}

func TestAttachSpoolReleaserOnlyWhereThereIsASpool(t *testing.T) {
	t.Parallel()
	withSpool, err := newS3Storage(Config{Backend: "S3", Dir: t.TempDir(), S3Endpoint: "127.0.0.1:1", S3Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	direct := &s3Storage{bucket: "bkt"}
	r := &fakeReleaser{}
	if !AttachSpoolReleaser(withSpool, r) || withSpool.releaser == nil {
		t.Error("the spool storage did not take the releaser")
	}
	if AttachSpoolReleaser(direct, r) || direct.releaser != nil {
		t.Error("S3 without a spool took a releaser")
	}
	if AttachSpoolReleaser(newFSStorage(t.TempDir()), r) {
		t.Error("the filesystem storage took a releaser")
	}
}

// The probe file the switch writes at the spool root is not a recording key,
// so the sweep never lists it.
func TestThePendingSweepIgnoresTheSpoolProbeFile(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	s, err := newS3Storage(Config{Backend: "S3", Dir: spool, S3Endpoint: "127.0.0.1:1", S3Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	probe := writeSpoolFile(t, spool, ".aicc-spool-probe-0123456789abcdef", 10)
	ageFile(t, probe, time.Hour)
	got, err := s.Pending(t.Context(), time.Now())
	if err != nil || len(got) != 0 {
		t.Errorf("pending = %v, %v", got, err)
	}
}
