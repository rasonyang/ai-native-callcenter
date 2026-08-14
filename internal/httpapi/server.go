// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// AgentService is the agent presence surface used by the API.
type AgentService interface {
	Login(ctx context.Context, agentID uuid.UUID, extensionNumber string) (agents.Presence, error)
	Logout(ctx context.Context, agentID uuid.UUID) (agents.Presence, error)
	Ready(ctx context.Context, agentID uuid.UUID) (agents.Presence, error)
	NotReady(ctx context.Context, agentID uuid.UUID, reason agents.Reason) (agents.Presence, error)
	Presence(agentID uuid.UUID) agents.Presence
	Roster(ctx context.Context) ([]agents.RosterEntry, error)
}

// Server owns the HTTP surface: REST, SSE and the embedded SPA.
type Server struct {
	cfg        config.Config
	auth       *auth.Service
	hub        *events.Hub
	agents     AgentService
	agentDir   AgentDirectory
	calls      CallService
	catalog    CatalogService
	ledger     *store.LedgerStore
	recordings RecordingStreamer
	auditor    Auditor
	spa        http.Handler
}

// Deps are the services the API exposes.
type Deps struct {
	Auth     *auth.Service
	Hub      *events.Hub
	Agents   AgentService
	AgentDir AgentDirectory
	Calls    CallService
	Catalog  CatalogService
	// Ledger serves finished calls: CDRs, transcripts, recordings, reviews.
	Ledger *store.LedgerStore
	// Recordings streams stored call audio; nil disables playback.
	Recordings RecordingStreamer
	// Auditor records mutating requests; nil disables the trail.
	Auditor Auditor
	// SPA may be nil during development, when the Vite dev server serves the
	// frontend instead.
	SPA http.Handler
}

// New builds the server.
func New(cfg config.Config, deps Deps) *Server {
	return &Server{
		cfg:        cfg,
		auth:       deps.Auth,
		hub:        deps.Hub,
		agents:     deps.Agents,
		agentDir:   deps.AgentDir,
		calls:      deps.Calls,
		catalog:    deps.Catalog,
		ledger:     deps.Ledger,
		recordings: deps.Recordings,
		auditor:    deps.Auditor,
		spa:        deps.SPA,
	}
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	return otelhttp.NewHandler(s.router(), "aicc")
}

// router builds the routing table. It is split from Handler so tests can walk
// the tree — the otel wrapper hides it from chi.Walk.
func (s *Server) router() chi.Router {
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
				private.Use(s.auditTrail)
				private.Post("/auth/logout", s.handleLogout)
				private.Get("/auth/me", s.handleMe)

				if s.agents != nil {
					// An agent drives only their own presence.
					private.Group(func(agent chi.Router) {
						agent.Use(requireAgentRole)
						agent.Get("/agent/presence", s.handleAgentPresence)
						agent.Post("/agent/login", s.handleAgentLogin)
						agent.Post("/agent/logout", s.handleAgentLogout)
						agent.Post("/agent/ready", s.handleAgentReady)
						agent.Post("/agent/not-ready", s.handleAgentNotReady)
					})
					// The roster and force-logout belong to supervision.
					private.Group(func(sup chi.Router) {
						sup.Use(requireRole(auth.RoleSupervisor))
						sup.Get("/agents", s.handleAgentRoster)
						sup.Post("/agents/{agentId}/force-logout", s.handleAgentForceLogout)
					})
				}

				if s.calls != nil {
					// Call control acts through the caller's own agent
					// identity: a call id in the path is never authority on
					// its own.
					private.Group(func(call chi.Router) {
						call.Use(requireAgentRole)
						call.Get("/calls/mine", s.handleMyCalls)
						call.Post("/calls/{callId}/answer", s.handleCallAnswer)
						call.Post("/calls/{callId}/hold", s.handleCallHold)
						call.Post("/calls/{callId}/retrieve", s.handleCallRetrieve)
						call.Post("/calls/{callId}/hangup", s.handleCallHangup)
						call.Post("/calls/{callId}/transfer", s.handleCallTransfer)
					})
					private.With(requireSupervisorRole).Get("/calls", s.handleAllCalls)
				}

				if s.catalog != nil {
					// Configuration is administration: changing who can
					// register, which queues exist and which numbers reach
					// them is not a supervision task.
					private.Group(func(admin chi.Router) {
						admin.Use(requireRole(auth.RoleAdmin))

						admin.Get("/extensions", s.handleListExtensions)
						admin.Post("/extensions", s.handleCreateExtension)
						admin.Put("/extensions/{extensionId}", s.handleUpdateExtension)
						admin.Delete("/extensions/{extensionId}", s.handleDeleteExtension)

						admin.Post("/queues", s.handleCreateQueue)
						admin.Put("/queues/{queueId}", s.handleUpdateQueue)
						admin.Delete("/queues/{queueId}", s.handleDeleteQueue)
						admin.Put("/queues/{queueId}/agents", s.handleStaffQueue)
						admin.Delete("/queues/{queueId}/agents/{agentId}", s.handleUnstaffQueue)

						admin.Get("/dids", s.handleListDIDs)
						admin.Post("/dids", s.handleCreateDID)
						admin.Put("/dids/{didId}", s.handleUpdateDID)
						admin.Delete("/dids/{didId}", s.handleDeleteDID)
					})

					// Reading the queues and their staffing is supervision:
					// it answers who is covering what right now.
					private.Group(func(sup chi.Router) {
						sup.Use(requireSupervisorRole)
						sup.Get("/queues", s.handleListQueues)
						sup.Get("/queues/{queueId}/agents", s.handleListQueueAgents)
					})
				}

				if s.ledger != nil {
					// Finished calls and their artifacts are supervision:
					// reviewing what happened is not an agent task.
					private.Group(func(sup chi.Router) {
						sup.Use(requireSupervisorRole)
						sup.Get("/cdrs", s.handleListCDRs)
						sup.Get("/cdrs/{callId}", s.handleGetCDR)
						sup.Get("/calls/{callId}/recordings", s.handleCallRecordings)
						sup.Get("/calls/{callId}/reviews", s.handleCallReviews)
						sup.Get("/recordings/{recordingId}/audio", s.handleRecordingAudio)
						sup.Post("/recordings/{recordingId}/reviews", s.handleCreateReview)
						sup.Get("/reports/overview", s.handleReportOverview)
						sup.Get("/reports/queues", s.handleReportQueues)
						sup.Get("/reports/daily", s.handleReportDaily)
					})

					// Callbacks are agent work: any signed-in agent may claim
					// and keep a promise; supervisors see the same queue.
					private.Group(func(anyRole chi.Router) {
						anyRole.Get("/callbacks", s.handleListCallbacks)
						anyRole.Post("/callbacks/{callbackId}/claim", s.handleClaimCallback)
						anyRole.Post("/callbacks/{callbackId}/complete", s.handleCompleteCallback)
					})
				}

				private.With(requireRole(auth.RoleAdmin)).Get("/system/health", s.handleHealth)
			})
		})

		// Long-lived stream: no timeout, it ends with the client connection.
		api.With(s.requireSession).Get("/events", s.handleEvents)
	})

	if s.spa != nil {
		r.NotFound(s.spa.ServeHTTP)
	}

	return r
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
