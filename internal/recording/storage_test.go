// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func writeSpoolFile(t *testing.T, root, key string, size int) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, size)
	copy(payload, "RIFF----WAVEfmt ")
	for i := wavHeaderSize; i < size; i++ {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(full, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestKeyIsDatedAndNamedByCall(t *testing.T) {
	at := time.Date(2026, 8, 14, 23, 59, 0, 0, time.FixedZone("cst", 8*3600))
	// The date is UTC: a call at 23:59 Beijing time belongs to that UTC day.
	if got := Key(at, "abc"); got != "2026/08/14/abc.wav" {
		t.Errorf("key = %q", got)
	}
}

func TestDurationEstimate(t *testing.T) {
	// One second of stereo 16-bit telephone audio plus the header.
	if got := DurationSec(wavHeaderSize + 32000); got != 1 {
		t.Errorf("duration = %d, want 1", got)
	}
	if got := DurationSec(10); got != 0 {
		t.Errorf("a header-less file has duration %d, want 0", got)
	}
}

func TestFSIngestOpenDelete(t *testing.T) {
	root := t.TempDir()
	storage, err := New(Config{Backend: "FS", Dir: root})
	if err != nil {
		t.Fatal(err)
	}

	key := Key(time.Now(), "call-1")
	writeSpoolFile(t, root, key, 1000)

	size, err := storage.Ingest(t.Context(), key)
	if err != nil || size != 1000 {
		t.Fatalf("ingest: size=%d err=%v", size, err)
	}

	reader, size, err := storage.Open(t.Context(), key)
	if err != nil || size != 1000 {
		t.Fatalf("open: size=%d err=%v", size, err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(reader, head); err != nil || string(head) != "RIFF" {
		t.Errorf("head = %q err=%v", head, err)
	}
	// Seeking is what makes range requests work.
	if _, err := reader.Seek(8, io.SeekStart); err != nil {
		t.Errorf("seek: %v", err)
	}
	if _, err := io.ReadFull(reader, head); err != nil || string(head) != "WAVE" {
		t.Errorf("after seek head = %q", head)
	}
	reader.Close()

	if err := storage.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.Open(t.Context(), key); err == nil {
		t.Error("opened a deleted recording")
	}
	// Deleting what is already gone is the outcome deletion wanted.
	if err := storage.Delete(t.Context(), key); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// Keys come from the database, but escaping the recording directory must be
// impossible rather than merely unexpected.
func TestFSRefusesEscapingKeys(t *testing.T) {
	root := t.TempDir()
	storage, _ := New(Config{Backend: "FS", Dir: root})

	for _, key := range []string{"../secrets.wav", "a/../../etc/passwd", "/etc/passwd"} {
		if _, _, err := storage.Open(t.Context(), key); err == nil {
			t.Errorf("key %q opened", key)
		}
	}
}

func TestUnknownBackendIsRejected(t *testing.T) {
	if _, err := New(Config{Backend: "FTP", Dir: "x"}); err == nil {
		t.Error("an unknown backend was accepted")
	}
	if _, err := New(Config{Backend: "FS"}); err == nil {
		t.Error("a filesystem backend with no directory was accepted")
	}
	if _, err := New(Config{Backend: "S3", Dir: "x"}); err == nil {
		t.Error("an S3 backend with no endpoint was accepted")
	}
}

// TestS3RoundTrip drives the real S3 backend against a local SeaweedFS.
//
// It starts `weed mini` itself; when the binary is missing the test skips, and
// AICC_S3_TEST=1 turns a skip into a failure so the gate cannot rot silently.
func TestS3RoundTrip(t *testing.T) {
	weedPath, err := exec.LookPath("weed")
	if err != nil {
		if os.Getenv("AICC_S3_TEST") != "" {
			t.Fatal("AICC_S3_TEST is set but the weed binary is not installed")
		}
		t.Skip("weed is not installed; install seaweedfs to run the S3 round-trip")
	}

	port := freePort(t)
	dataDir := t.TempDir()

	// SeaweedFS rejects signed requests unless identities are configured, so
	// the test runs with real credentials — the same shape production uses.
	s3Config := filepath.Join(t.TempDir(), "s3.json")
	if err := os.WriteFile(s3Config, []byte(`{
		"identities": [{
			"name": "aicc",
			"credentials": [{"accessKey": "aicc-test-key", "secretKey": "aicc-test-secret"}],
			"actions": ["Admin", "Read", "Write", "List", "Tagging"]
		}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(weedPath, "mini",
		"-dir", dataDir,
		"-s3.port", fmt.Sprint(port),
		"-s3.config", s3Config,
		"-master.port", fmt.Sprint(freePort(t)),
		"-volume.port", fmt.Sprint(freePort(t)),
		"-filer.port", fmt.Sprint(freePort(t)),
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start weed mini: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	awaitTCP(t, endpoint, 30*time.Second)

	spool := t.TempDir()
	storage, err := New(Config{
		Backend: "S3", Dir: spool,
		S3Endpoint: endpoint, S3Bucket: "aicc-recordings",
		S3AccessKey: "aicc-test-key", S3SecretKey: "aicc-test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	s3, ok := storage.(*s3Storage)
	if !ok {
		t.Fatalf("backend is %T", storage)
	}
	awaitBucket(t, s3, 30*time.Second)

	// The switch wrote a spool file; ingestion uploads it and clears the spool.
	key := Key(time.Now(), "call-s3")
	local := writeSpoolFile(t, spool, key, wavHeaderSize+32000)

	size, err := storage.Ingest(t.Context(), key)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if size != wavHeaderSize+32000 {
		t.Errorf("size = %d", size)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Error("the spool copy survived the upload")
	}

	// The audio comes back byte-identical and seekable.
	reader, gotSize, err := storage.Open(t.Context(), key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer reader.Close()
	if gotSize != size {
		t.Errorf("downloaded %d bytes, uploaded %d", gotSize, size)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(reader, head); err != nil || string(head) != "RIFF" {
		t.Errorf("head = %q err=%v", head, err)
	}
	if _, err := reader.Seek(-4, io.SeekEnd); err != nil {
		t.Errorf("seek: %v", err)
	}
	tail := make([]byte, 4)
	if _, err := io.ReadFull(reader, tail); err != nil {
		t.Errorf("read tail: %v", err)
	}
	want := byte((wavHeaderSize + 32000 - 4) % 251)
	if tail[0] != want {
		t.Errorf("tail byte = %d, want %d — the object round-tripped corrupted", tail[0], want)
	}

	if err := storage.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if got := DurationSec(size); got != 1 {
		t.Errorf("duration = %d", got)
	}

	// Direct upload: the switch PUTs the file into the store itself
	// (mod_http_cache), so there is no spool and ingestion only confirms the
	// object. A spool-less backend is legal, and ingesting a key the switch
	// never uploaded reports the absence instead of inventing a recording.
	direct, err := New(Config{
		Backend:    "S3",
		S3Endpoint: endpoint, S3Bucket: "aicc-recordings",
		S3AccessKey: "aicc-test-key", S3SecretKey: "aicc-test-secret",
	})
	if err != nil {
		t.Fatalf("spool-less S3 backend: %v", err)
	}
	directKey := Key(time.Now(), "call-direct")
	uploaded := writeSpoolFile(t, t.TempDir(), directKey, wavHeaderSize+16000)
	if _, err := direct.(*s3Storage).client.FPutObject(t.Context(), "aicc-recordings",
		directKey, uploaded, minio.PutObjectOptions{ContentType: "audio/wav"}); err != nil {
		t.Fatalf("simulate the switch's PUT: %v", err)
	}
	directSize, err := direct.Ingest(t.Context(), directKey)
	if err != nil {
		t.Fatalf("direct ingest: %v", err)
	}
	if directSize != wavHeaderSize+16000 {
		t.Errorf("direct size = %d", directSize)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := direct.Ingest(ctx, Key(time.Now(), "never-recorded")); err == nil {
		t.Error("ingested a recording the switch never made")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func awaitTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("nothing listening at %s after %s", addr, timeout)
}

func awaitBucket(t *testing.T, s3 *s3Storage, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = s3.EnsureBucket(t.Context()); lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("bucket never became available: %v", lastErr)
}
