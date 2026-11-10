// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestCallIDOfReadsOnlyRecordingKeys(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7())
	if got, ok := CallIDOf(Key(time.Now(), id.String())); !ok || got != id {
		t.Errorf("CallIDOf(Key) = %v, %v", got, ok)
	}
	for _, bad := range []string{
		"13800138000.1760000000.wav",
		"2026/08/14/not-a-uuid.wav",
		"2026/08/14/" + id.String() + ".mp3",
		id.String() + ".wav",
	} {
		if _, ok := CallIDOf(bad); ok {
			t.Errorf("%q was taken for a recording key", bad)
		}
	}
}

func TestS3PendingListsTheSpoolByPatternAndAge(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	s, err := newS3Storage(Config{Backend: "S3", Dir: spool, S3Endpoint: "127.0.0.1:1", S3Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	oldKey := Key(time.Now().AddDate(0, 0, -9), uuid.Must(uuid.NewV7()).String())
	youngKey := Key(time.Now(), uuid.Must(uuid.NewV7()).String())
	oldFile := writeSpoolFile(t, spool, oldKey, 100)
	youngFile := writeSpoolFile(t, spool, youngKey, 100)
	stray := writeSpoolFile(t, spool, "13800138000.1760000000.wav", 100)
	ageFile(t, oldFile, time.Hour)
	ageFile(t, stray, time.Hour)
	_ = youngFile

	got, err := s.Pending(t.Context(), time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{oldKey}; !reflect.DeepEqual(got, want) {
		t.Errorf("pending = %v, want %v (the young file and the stray name stay out)", got, want)
	}

	none, err := (&s3Storage{bucket: "bkt"}).Pending(t.Context(), time.Now())
	if err != nil || len(none) != 0 {
		t.Errorf("a spool-less backend listed %v, %v", none, err)
	}
}

func TestFSPendingWalksOnlyTodayAndYesterday(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fsStore := newFSStorage(root)
	now := time.Now().UTC()
	keys := map[string]time.Time{
		"today":     now,
		"yesterday": now.AddDate(0, 0, -1),
		"older":     now.AddDate(0, 0, -2),
	}
	made := map[string]string{}
	for name, day := range keys {
		key := Key(day, uuid.Must(uuid.NewV7()).String())
		ageFile(t, writeSpoolFile(t, root, key, 100), time.Hour)
		made[name] = key
	}
	youngKey := Key(now, uuid.Must(uuid.NewV7()).String())
	writeSpoolFile(t, root, youngKey, 100)
	ageFile(t, writeSpoolFile(t, root, "x.1.wav", 100), time.Hour)

	got, err := fsStore.Pending(t.Context(), now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{made["today"], made["yesterday"]}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pending = %v, want %v", got, want)
	}
	if err := fsStore.Release(t.Context(), made["today"]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(made["today"]))); err != nil {
		t.Error("the filesystem backend released a file that is the store itself")
	}
}

func TestS3IngestKeepsTheSpoolCopyUntilReleased(t *testing.T) {
	t.Parallel()
	stub := newS3Stub(stubBucket)
	s, _ := newStubStorage(t, stub, false, t.TempDir())
	key := Key(time.Now(), uuid.Must(uuid.NewV7()).String())
	local := writeSpoolFile(t, s.spool.root, key, wavHeaderSize+320)

	size, err := s.Ingest(context.Background(), key)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if size != wavHeaderSize+320 || len(stub.puts()) != 1 {
		t.Errorf("size %d, puts %d", size, len(stub.puts()))
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("ingest removed the spool copy")
	}
	if err := s.Release(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !errors.Is(err, fs.ErrNotExist) {
		t.Error("release left the spool copy")
	}
	if _, err := os.Stat(filepath.Dir(local)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("release left the empty dated directory")
	}
	if err := s.Release(context.Background(), key); err != nil {
		t.Errorf("releasing a copy that is gone failed: %v", err)
	}
}

func TestAStoreSaysAbsentAndForbiddenInTheStandardLibrarysWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resp   minio.ErrorResponse
		target error
	}{
		{minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}, fs.ErrNotExist},
		{minio.ErrorResponse{StatusCode: 404}, fs.ErrNotExist},
		{minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, fs.ErrPermission},
	} {
		if err := classify(tc.resp); !errors.Is(err, tc.target) {
			t.Errorf("%v is not %v", tc.resp.Code, tc.target)
		}
	}
	if err := classify(minio.ErrorResponse{Code: "InternalError", StatusCode: 500}); errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		t.Error("a server error was classified")
	}
}
