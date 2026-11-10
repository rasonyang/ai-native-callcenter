// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubRequest is what the stub saw of one request.
type stubRequest struct {
	Method, Path, RawQuery string
	ContentEncoding        string
	ContentSHA256          string
	BodyLen                int
}

// s3Stub is an in-memory S3 that is as strict as the stores behind #520: it
// refuses an aws-chunked or streaming-signed PUT ("Aws MultiChunkedEncoding is
// not supported."), a PUT with no X-Amz-Content-Sha256, a PUT whose header is
// not the SHA-256 of the body it received, and every multipart call. It does
// not verify signatures.
type s3Stub struct {
	bucket string
	// rejectPuts makes every PUT fail, a store that takes nothing.
	rejectPuts bool

	mu       sync.Mutex
	objects  map[string][]byte
	requests []stubRequest
}

const chunkedRefusal = "Aws MultiChunkedEncoding is not supported."

func newS3Stub(bucket string) *s3Stub {
	return &s3Stub{bucket: bucket, objects: map[string][]byte{}}
}

func (s *s3Stub) start(t *testing.T, tls bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(s)
	} else {
		srv = httptest.NewServer(s)
	}
	t.Cleanup(srv.Close)
	return srv
}

func (s *s3Stub) puts() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubRequest
	for _, r := range s.requests {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func (s *s3Stub) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

func (s *s3Stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func s3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>stub</RequestId></Error>`, code, msg)
}

func (s *s3Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := stubRequest{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		ContentEncoding: r.Header.Get("Content-Encoding"),
		ContentSHA256:   r.Header.Get("X-Amz-Content-Sha256"),
		BodyLen:         len(body),
	}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.mu.Unlock()

	q := r.URL.Query()
	if _, ok := q["uploads"]; ok || q.Get("uploadId") != "" {
		s3Error(w, http.StatusBadRequest, "NotImplemented", "multipart is not accepted by this stub")
		return
	}
	prefix := "/" + s.bucket
	if r.URL.Path == prefix || r.URL.Path == prefix+"/" {
		if _, ok := q["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
			return
		}
		w.WriteHeader(http.StatusOK) // HEAD bucket
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, prefix+"/")
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}

	switch r.Method {
	case http.MethodPut:
		if s.rejectPuts {
			s3Error(w, http.StatusForbidden, "AccessDenied", "this stub takes no objects")
			return
		}
		if strings.Contains(rec.ContentEncoding, "aws-chunked") || strings.HasPrefix(rec.ContentSHA256, "STREAMING-") {
			s3Error(w, http.StatusBadRequest, "NotImplemented", chunkedRefusal)
			return
		}
		if rec.ContentSHA256 == "" {
			s3Error(w, http.StatusBadRequest, "InvalidRequest", "Missing required header for this request: x-amz-content-sha256")
			return
		}
		sum := sha256.Sum256(body)
		if rec.ContentSHA256 != hex.EncodeToString(sum[:]) {
			s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
			return
		}
		s.mu.Lock()
		s.objects[key] = body
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		data, ok := s.object(key)
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("ETag", `"stub"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "not supported")
	}
}
