// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// Server owns the HTTP surface: REST, SSE and the embedded SPA.
type Server struct {
	cfg  config.Config
	auth *auth.Service
	hub  *events.Hub
	spa  http.Handler
}

// New builds the server. spa may be nil during development, when the Vite dev
// server serves the frontend instead.
func New(cfg config.Config, authSvc *auth.Service, hub *events.Hub, spa http.Handler) *Server {
	return &Server{cfg: cfg, auth: authSvc, hub: hub, spa: spa}
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// middleware.RealIP is deliberately not used: it trusts client-supplied
	// forwarding headers. Deployments behind a proxy configure it there.
	r.Use(middleware.Recoverer)

	r.Route("/api/v1", func(api chi.Router) {
		// Request-scoped routes carry a timeout; the event stream must not.
		api.Group(func(short chi.Router) {
			short.Use(middleware.Timeout(30 * time.Second))

			short.Post("/auth/login", s.handleLogin)

			short.Group(func(private chi.Router) {
				private.Use(s.requireSession)
				private.Post("/auth/logout", s.handleLogout)
				private.Get("/auth/me", s.handleMe)
				private.With(requireRole(auth.RoleAdmin)).Get("/system/health", s.handleHealth)
			})
		})

		// Long-lived stream: no timeout, it ends with the client connection.
		api.With(s.requireSession).Get("/events", s.handleEvents)
	})

	if s.spa != nil {
		r.NotFound(s.spa.ServeHTTP)
	}

	return otelhttp.NewHandler(r, "aicc")
}

// MetricsHandler builds the unauthenticated ops listener: metrics, liveness
// and readiness. It is served on a separate address.
func MetricsHandler(metrics http.Handler, ready func() error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}
