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

	// Configuration, as administration edits it.
	CreateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error)
	UpdateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error)
	DeleteAgent(ctx context.Context, agentID uuid.UUID) error
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
	outbound   OutboundService
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
	// Outbound places calls; nil hides the dial endpoints.
	Outbound OutboundService
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
		outbound:   deps.Outbound,
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

	// Every operation is mounted through the generated wrapper, which binds
	// and validates the contract's path, query and header parameters before
	// the handler sees them. Nothing parses a parameter twice.
	op := s.apiWrapper()

	r.Route("/api/v1", func(v1 chi.Router) {
		// Request-scoped routes carry a timeout; the event stream must not.
		v1.Group(func(short chi.Router) {
			short.Use(middleware.Timeout(30 * time.Second))

			short.Post("/auth/login", op.Login)

			short.Group(func(private chi.Router) {
				private.Use(s.requireSession)
				private.Use(s.auditTrail)
				private.Post("/auth/logout", op.Logout)
				private.Get("/auth/me", op.GetMe)

				if s.agents != nil {
					// An agent drives only their own presence.
					private.Group(func(agent chi.Router) {
						agent.Use(requireAgentRole)
						agent.Get("/agent/presence", op.GetAgentPresence)
						agent.Post("/agent/login", op.AgentLogin)
						agent.Post("/agent/logout", op.AgentLogout)
						agent.Post("/agent/ready", op.AgentReady)
						agent.Post("/agent/not-ready", op.AgentNotReady)
					})
					// The roster and force-logout belong to supervision.
					private.Group(func(sup chi.Router) {
						sup.Use(requireSupervisorRole)
						sup.Get("/agents", op.ListAgents)
						sup.Post("/agents/{agentId}/force-logout", op.ForceLogoutAgent)
					})
					// Who is an agent, and which phone they are bound to, is
					// configuration rather than supervision.
					private.Group(func(admin chi.Router) {
						admin.Use(requireRole(auth.RoleAdmin))
						admin.Post("/agents", op.CreateAgent)
						admin.Put("/agents/{agentId}", op.UpdateAgent)
						admin.Delete("/agents/{agentId}", op.DeleteAgent)
						admin.Get("/users", op.ListUsers)
					})
				}

				if s.calls != nil {
					// Call control acts through the caller's own agent
					// identity: a call id in the path is never authority on
					// its own.
					private.Group(func(call chi.Router) {
						call.Use(requireAgentRole)
						call.Get("/calls/mine", op.ListMyCalls)
						call.Post("/calls/{callId}/answer", op.AnswerCall)
						call.Post("/calls/{callId}/hold", op.HoldCall)
						call.Post("/calls/{callId}/retrieve", op.RetrieveCall)
						call.Post("/calls/{callId}/mute", op.MuteCall)
						call.Post("/calls/{callId}/unmute", op.UnmuteCall)
						call.Post("/calls/{callId}/hangup", op.HangupCall)
						call.Post("/calls/{callId}/transfer", op.TransferCall)
						call.Post("/calls/{callId}/dtmf", op.SendCallDTMF)
					})
					private.With(requireSupervisorRole).Get("/calls", op.ListCalls)
				}

				if s.catalog != nil {
					// Configuration is administration: changing who can
					// register, which queues exist and which numbers reach
					// them is not a supervision task.
					private.Group(func(admin chi.Router) {
						admin.Use(requireRole(auth.RoleAdmin))

						admin.Get("/extensions", op.ListExtensions)
						admin.Post("/extensions", op.CreateExtension)
						admin.Put("/extensions/{extensionId}", op.UpdateExtension)
						admin.Delete("/extensions/{extensionId}", op.DeleteExtension)

						admin.Post("/queues", op.CreateQueue)
						admin.Put("/queues/{queueId}", op.UpdateQueue)
						admin.Delete("/queues/{queueId}", op.DeleteQueue)
						admin.Put("/queues/{queueId}/agents", op.StaffQueue)
						admin.Delete("/queues/{queueId}/agents/{agentId}", op.UnstaffQueue)

						admin.Get("/dids", op.ListDIDs)
						admin.Post("/dids", op.CreateDID)
						admin.Put("/dids/{didId}", op.UpdateDID)
						admin.Delete("/dids/{didId}", op.DeleteDID)
					})

					// Reading the queues and their staffing is supervision:
					// it answers who is covering what right now.
					private.Group(func(sup chi.Router) {
						sup.Use(requireSupervisorRole)
						sup.Get("/queues", op.ListQueues)
						sup.Get("/queues/{queueId}/agents", op.ListQueueAgents)
					})
				}

				if s.ledger != nil {
					// Finished calls and their artifacts are supervision:
					// reviewing what happened is not an agent task.
					private.Group(func(sup chi.Router) {
						sup.Use(requireSupervisorRole)
						sup.Get("/cdrs", op.ListCDRs)
						sup.Get("/cdrs/{callId}", op.GetCDR)
						sup.Get("/calls/{callId}/recordings", op.ListCallRecordings)
						sup.Get("/calls/{callId}/reviews", op.ListCallReviews)
						sup.Get("/recordings/{recordingId}/audio", op.GetRecordingAudio)
						sup.Post("/recordings/{recordingId}/reviews", op.CreateRecordingReview)
						sup.Get("/reports/overview", op.GetReportOverview)
						sup.Get("/reports/queues", op.GetReportQueues)
						sup.Get("/reports/daily", op.GetReportDaily)
					})

					// Callbacks are agent work: any signed-in agent may claim
					// and keep a promise; supervisors see the same queue.
					private.Group(func(anyRole chi.Router) {
						anyRole.Get("/callbacks", op.ListCallbacks)
						anyRole.Post("/callbacks/{callbackId}/claim", op.ClaimCallback)
						anyRole.Post("/callbacks/{callbackId}/complete", op.CompleteCallback)
					})
				}

				if s.outbound != nil {
					// An agent dials out as themselves; placing an AI call
					// is an operations decision.
					private.With(requireAgentRole).Post("/calls/dial", op.DialCall)
					private.With(requireSupervisorRole).Post("/calls", op.CreateCall)
				}

				private.With(requireRole(auth.RoleAdmin)).Get("/system/health", op.GetSystemHealth)
			})
		})

		// Long-lived stream: no timeout, it ends with the client connection.
		v1.With(s.requireSession).Get("/events", op.StreamEvents)
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
