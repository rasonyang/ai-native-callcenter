// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const stubBucket = "aicc-recordings"

// newStubStorage is an S3 backend with a spool against the stub, over plain
// HTTP or TLS (trusting the stub's certificate).
func newStubStorage(t *testing.T, stub *s3Stub, tls bool, spool string) (*s3Storage, *httptest.Server) {
	t.Helper()
	srv := stub.start(t, tls)
	cfg := Config{
		Backend: "S3", Dir: spool,
		S3Endpoint: strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"),
		S3Bucket:   stubBucket, S3AccessKey: "k", S3SecretKey: "s", S3IsSSL: tls,
	}
	if tls {
		cfg.s3Transport = srv.Client().Transport
	}
	st, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st.(*s3Storage), srv
}

// The stub is only worth anything if the library's default really trips it.
func TestTheStubRefusesWhatTheLibraryDefaultSendsOverPlainHTTP(t *testing.T) {
	t.Parallel()
	stub := newS3Stub(stubBucket)
	srv := stub.start(t, false)
	client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4("k", "s", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	file := writeSpoolFile(t, t.TempDir(), "2026/11/10/old.wav", 300<<10)
	_, err = client.FPutObject(t.Context(), stubBucket, "2026/11/10/old.wav", file,
		minio.PutObjectOptions{ContentType: "audio/wav"})
	if err == nil || !strings.Contains(err.Error(), chunkedRefusal) {
		t.Fatalf("FPutObject over HTTP = %v, want %q", err, chunkedRefusal)
	}
}

func TestIngestUploadsOneSignedPUTOverPlainHTTP(t *testing.T) {
	t.Parallel()
	for name, size := range map[string]int{"300 KB": 300 << 10, "20 MB": 20 << 20} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spool := t.TempDir()
			stub := newS3Stub(stubBucket)
			st, _ := newStubStorage(t, stub, false, spool)

			key := Key(time.Now(), "call-"+strings.ReplaceAll(name, " ", ""))
			local := writeSpoolFile(t, spool, key, size)
			want, err := os.ReadFile(local)
			if err != nil {
				t.Fatal(err)
			}

			got, err := st.Ingest(t.Context(), key)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if got != int64(size) {
				t.Errorf("size = %d, want %d", got, size)
			}
			stored, ok := stub.object(key)
			if !ok || !bytes.Equal(stored, want) {
				t.Error("the stored object is not the spool file")
			}
			if _, err := os.Stat(local); !os.IsNotExist(err) {
				t.Errorf("the spool copy is still there: %v", err)
			}
			entries, _ := os.ReadDir(spool)
			if len(entries) != 0 {
				t.Errorf("the spool keeps %d entries, want its dated directories gone", len(entries))
			}
			puts := stub.puts()
			if len(puts) != 1 {
				t.Fatalf("PUTs = %d, want one (no multipart)", len(puts))
			}
			assertPlainSignedPUT(t, puts[0], size)
		})
	}
}

func assertPlainSignedPUT(t *testing.T, r stubRequest, size int) {
	t.Helper()
	if r.ContentEncoding != "" {
		t.Errorf("Content-Encoding = %q, want none", r.ContentEncoding)
	}
	if len(r.ContentSHA256) != 64 {
		t.Errorf("X-Amz-Content-Sha256 = %q, want the body's hex SHA-256", r.ContentSHA256)
	}
	if r.RawQuery != "" {
		t.Errorf("query = %q, want a plain PUT", r.RawQuery)
	}
	if r.BodyLen != size {
		t.Errorf("body = %d bytes, want %d", r.BodyLen, size)
	}
}

// Over TLS the library's default is a different request; the backend pins one
// shape for both.
func TestIngestSendsTheSameRequestOverTLS(t *testing.T) {
	t.Parallel()
	const size = 300 << 10
	var seen []stubRequest
	for _, tls := range []bool{false, true} {
		spool := t.TempDir()
		stub := newS3Stub(stubBucket)
		st, _ := newStubStorage(t, stub, tls, spool)
		key := Key(time.Now(), "call-tls")
		local := writeSpoolFile(t, spool, key, size)
		want, _ := os.ReadFile(local)
		if _, err := st.Ingest(t.Context(), key); err != nil {
			t.Fatalf("ingest (tls=%v): %v", tls, err)
		}
		if stored, _ := stub.object(key); !bytes.Equal(stored, want) {
			t.Errorf("tls=%v: stored bytes differ", tls)
		}
		puts := stub.puts()
		if len(puts) != 1 {
			t.Fatalf("tls=%v: PUTs = %d, want 1", tls, len(puts))
		}
		assertPlainSignedPUT(t, puts[0], size)
		seen = append(seen, puts[0])
	}
	if seen[0].ContentSHA256 != seen[1].ContentSHA256 || seen[0].ContentEncoding != seen[1].ContentEncoding {
		t.Errorf("HTTP sent %+v, TLS sent %+v: want the same kind of request", seen[0], seen[1])
	}
}

func TestSelfCheckPassesAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	stub := newS3Stub(stubBucket)
	st, _ := newStubStorage(t, stub, false, t.TempDir())
	if err := st.SelfCheck(t.Context()); err != nil {
		t.Fatalf("self-check: %v", err)
	}
	if n := stub.count(); n != 0 {
		t.Errorf("%d objects left in the store", n)
	}
	puts := stub.puts()
	if len(puts) != 1 || !strings.HasPrefix(puts[0].Path, "/"+stubBucket+"/"+selfCheckPrefix) {
		t.Errorf("PUTs = %+v, want one under %s", puts, selfCheckPrefix)
	}
	if err := st.CheckBucket(t.Context()); err != nil {
		t.Errorf("CheckBucket: %v", err)
	}
}

func TestSelfCheckFailsWhenTheStoreRefusesTheUpload(t *testing.T) {
	t.Parallel()
	stub := newS3Stub(stubBucket)
	stub.rejectPuts = true
	st, _ := newStubStorage(t, stub, false, t.TempDir())
	err := st.SelfCheck(t.Context())
	if err == nil || !strings.Contains(err.Error(), "upload probe") {
		t.Fatalf("self-check = %v, want an upload failure", err)
	}
	if n := stub.count(); n != 0 {
		t.Errorf("%d objects left in the store", n)
	}
}

func TestSelfCheckWritesNothingWithoutASpool(t *testing.T) {
	t.Parallel()
	stub := newS3Stub(stubBucket)
	st, _ := newStubStorage(t, stub, false, "")
	if err := st.SelfCheck(t.Context()); err != nil {
		t.Fatalf("self-check: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.requests) != 0 {
		t.Errorf("a spool-less backend sent %d requests, want none", len(stub.requests))
	}
}
