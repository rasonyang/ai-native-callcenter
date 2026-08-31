// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
)

// stubSPA stands in for the embedded application: it answers anything with
// index.html and a 200, which is exactly what makes it dangerous under /api.
func stubSPA() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><title>SPA</title>"))
	})
}

// The SPA fallback must not answer for the API.
//
// chi propagates a parent's NotFound into every subrouter that has none of its
// own, so mounting the application at the root once meant an unknown /api/v1
// path came back as index.html with a 200. A client library cannot tell that
// from success: it either dies parsing HTML as JSON, with an error naming
// nothing real, or believes the call worked. The contract says a path it does
// not declare is 404 in the error envelope, and this is what proves it.
func TestTheAPIAnswersItsOwnMisses(t *testing.T) {
	s := New(config.Config{SessionCookie: "aicc_session"}, Deps{SPA: stubSPA()})
	router := s.router()

	unknown := []string{
		"/api/v1/nope",
		"/api/v1/calls/mine/extra",
		"/api/v1/auth/login/extra",
		"/api/v1",
	}
	for _, path := range unknown {
		t.Run("GET "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want %d (body %.60q)", w.Code, http.StatusNotFound, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", got)
			}
			if code := errorCodeOf(t, w); code != string(CodeNotFound) {
				t.Errorf("code = %q, want %q", code, CodeNotFound)
			}
		})
	}
}

// A method the contract does not declare is refused in the envelope too.
// chi's default answers 405 with an empty body and no content type, which
// makes the contract's promise that errors *always* use the envelope false.
func TestAWrongMethodIsRefusedInTheEnvelope(t *testing.T) {
	s := New(config.Config{SessionCookie: "aicc_session"}, Deps{SPA: stubSPA()})

	w := httptest.NewRecorder()
	s.router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/auth/login", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
	if code := errorCodeOf(t, w); code != string(CodeMethodNotAllowed) {
		t.Errorf("code = %q, want %q", code, CodeMethodNotAllowed)
	}
}

// Everything outside /api/v1 is still the application's, so a deep link the
// client-side router owns keeps reloading into the SPA.
func TestTheApplicationStillOwnsEveryPathOutsideTheAPI(t *testing.T) {
	s := New(config.Config{SessionCookie: "aicc_session"}, Deps{SPA: stubSPA()})

	for _, path := range []string{"/admin/users", "/agent", "/"} {
		w := httptest.NewRecorder()
		s.router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s = %d %.40q, want the SPA", path, w.Code, w.Body.String())
		}
	}
}
