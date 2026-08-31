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
	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// AgentService is the agent presence surface used by the API.
type AgentService interface {
	Login(ctx context.Context, agentID uuid.UUID, extensionNumber string) (agents.Presence, error)
	Logout(ctx context.Context, agentID uuid.UUID) (agents.Presence, error)
	Ready(ctx context.Context, agentID uuid.UUID) (agents.Presence, error)
	NotReady(ctx context.Context, agentID uuid.UUID, reason agents.Reason) (agents.Presence, error)
	Presence(agentID uuid.UUID) agents.Presence
	// CallcenterNameFor is the switch's own name for an agent, for the
	// commands that have to name them the way mod_callcenter knows them.
	CallcenterNameFor(ctx context.Context, agentID uuid.UUID) string
	// BoundExtensionFor is the phone configuration binds to an agent identity.
	// A supervisor is not signed in anywhere, so this is the only phone that
	// is theirs to be reached at.
	BoundExtensionFor(ctx context.Context, agentID uuid.UUID) string
	// DeviceAtExtension and AgentAtExtension ask about a phone rather than
	// about a person. Registration is a fact about the phone and outlives its
	// agent logging out, which is what lets a call be placed for somebody who
	// is on the floor but never signed into this application; who is signed in
	// there, if anybody, is the separate question.
	DeviceAtExtension(extensionNumber string) (isRegistered, isInService, isKnown bool)
	AgentAtExtension(extensionNumber string) (agentID uuid.UUID, ok bool)
	Roster(ctx context.Context) ([]agents.RosterEntry, error)

	// EndWrapUp completes after-call work; WrapUpCall names the call it was
	// for, which is what a filing is recorded against.
	EndWrapUp(ctx context.Context, agentID uuid.UUID) (agents.Presence, error)
	WrapUpCall(agentID uuid.UUID) (uuid.UUID, bool)

	// EndWrapUp completes after-call work; WrapUpCall names the call it was
	// for, which is what a filing is recorded against.
	// Configuration, as administration edits it.
	CreateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error)
	// MirrorAgent tells the switch about an agent provisioned around this
	// service, in the one transaction that writes account, identity and phone.
	MirrorAgent(ctx context.Context, agentID uuid.UUID)
	UpdateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error)
	DeleteAgent(ctx context.Context, agentID uuid.UUID) error
}

// TrunkReader reports the gateways the switch holds. Read-only by design: a
// gateway is defined in the switch's own configuration, not here.
type TrunkReader interface {
	Trunks() ([]telephony.Trunk, error)
}

// Server owns the HTTP surface: REST, SSE and the embedded SPA.
type Server struct {
	cfg         config.Config
	auth        *auth.Service
	hub         *events.Hub
	agents      AgentService
	agentDir    AgentDirectory
	calls       CallService
	transcripts TranscriptStates
	catalog     CatalogService
	contacts    ContactService
	ledger      *store.LedgerStore
	flows       FlowService
	accounts    AccountService
	trunks      TrunkReader
	recordings  RecordingStreamer
	auditor     Auditor
	outbound    OutboundService
	webhooks    WebhookService
	keys        KeyService
	spa         http.Handler
}

// Deps are the services the API exposes.
type Deps struct {
	Auth     *auth.Service
	Hub      *events.Hub
	Agents   AgentService
	AgentDir AgentDirectory
	Calls    CallService
	// Transcripts answers what state a call's transcription is in. Nil means
	// the snapshot cannot say, which is honest rather than invented.
	Transcripts TranscriptStates
	Catalog     CatalogService
	// Contacts is the customer record book; nil hides the contact endpoints.
	Contacts ContactService
	// Ledger serves finished calls: CDRs, transcripts, recordings, reviews.
	Ledger *store.LedgerStore
	// Flows are the conversations the bot runs; nil hides the flow endpoints
	// and leaves `aicc flowadd` as the only way in.
	Flows FlowService
	// Accounts provisions people; nil leaves `aicc useradd` as the only way in.
	Accounts AccountService
	// Trunks reports the gateways the switch holds; nil leaves the card empty.
	Trunks TrunkReader
	// Recordings streams stored call audio; nil disables playback.
	Recordings RecordingStreamer
	// Auditor records mutating requests; nil disables the trail.
	Auditor Auditor
	// Outbound places calls; nil hides the dial endpoints.
	Outbound OutboundService
	// Webhooks tells a customer's own system about finished calls; nil hides
	// the subscription endpoints.
	Webhooks WebhookService
	// Keys authenticates bearer API keys and owns the key management
	// endpoints. Nil means the deployment issues none, every bearer is
	// answered with the same 401, and the /api-keys operations are not
	// mounted — the same shape every other optional dependency here has.
	Keys KeyService
	// SPA may be nil during development, when the Vite dev server serves the
	// frontend instead.
	SPA http.Handler
}

// New builds the server.
func New(cfg config.Config, deps Deps) *Server {
	return &Server{
		cfg:         cfg,
		auth:        deps.Auth,
		hub:         deps.Hub,
		agents:      deps.Agents,
		agentDir:    deps.AgentDir,
		calls:       deps.Calls,
		transcripts: deps.Transcripts,
		catalog:     deps.Catalog,
		contacts:    deps.Contacts,
		ledger:      deps.Ledger,
		flows:       deps.Flows,
		accounts:    deps.Accounts,
		trunks:      deps.Trunks,
		recordings:  deps.Recordings,
		auditor:     deps.Auditor,
		outbound:    deps.Outbound,
		webhooks:    deps.Webhooks,
		keys:        deps.Keys,
		spa:         deps.SPA,
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
		// The API answers its own misses, in the envelope, before the SPA
		// fallback can.
		//
		// chi propagates a parent's NotFound into every subrouter that has
		// none of its own (Mux.updateSubRoutes), so the r.NotFound(spa) at
		// the bottom of this function would otherwise answer an unknown
		// /api/v1 path with index.html and a 200 — which a client library
		// cannot tell from success: it either dies parsing HTML as JSON,
		// with an error that names nothing real, or believes the call
		// worked. This repository has been bitten once already, by a
		// contract operation that had no route (see routes_test.go).
		// Claiming the handlers here means the propagation skips this
		// subtree, and the SPA keeps every path outside it.
		v1.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, CodeNotFound,
				"no such endpoint", map[string]any{"path": r.URL.Path})
		})
		v1.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"the endpoint does not take this method",
				map[string]any{"method": r.Method, "path": r.URL.Path})
		})

		// Request-scoped routes carry a timeout; the event stream must not.
		v1.Group(func(short chi.Router) {
			short.Use(middleware.Timeout(30 * time.Second))

			// Signing in is the one operation with nothing to authenticate.
			short.Post("/auth/login", op.Login)
			// The contract serves itself, to anyone. A deployment that cannot
			// hand out its own API description is asking every integrator to
			// trust a file they found somewhere else.
			short.Get("/openapi.json", op.GetOpenAPI)

			// Everything else authenticates. Which credential reaches which
			// operation, and what it must carry, is the contract's answer and
			// is applied by enforceContract inside the generated wrapper — so
			// this routing table says only what exists, never who may use it.
			// That is why there is one group here where there used to be a
			// nest of role groups: a route's authorization is no longer a
			// property of where it is mounted.
			short.Group(func(private chi.Router) {
				private.Use(s.authenticate)
				private.Use(s.auditTrail)

				private.Post("/auth/logout", op.Logout)
				private.Get("/auth/me", op.GetMe)

				if s.agents != nil {
					private.Get("/agent/presence", op.GetAgentPresence)
					private.Post("/agent/login", op.AgentLogin)
					private.Post("/agent/logout", op.AgentLogout)
					private.Post("/agent/ready", op.AgentReady)
					private.Post("/agent/not-ready", op.AgentNotReady)
					private.Get("/agent/wrap-up", op.GetAgentWrapUp)
					private.Post("/agent/wrap-up", op.AgentWrapUp)

					private.Get("/agents", op.ListAgents)
					private.Post("/agents/{agentId}/force-logout", op.ForceLogoutAgent)

					private.Post("/agents", op.CreateAgent)
					private.Put("/agents/{agentId}", op.UpdateAgent)
					private.Delete("/agents/{agentId}", op.DeleteAgent)
					private.Get("/users", op.ListUsers)
				}

				if s.calls != nil {
					private.Get("/calls/mine", op.ListMyCalls)
					private.Get("/calls/waiting", op.ListWaitingCalls)
					private.Post("/calls/{callId}/answer", op.AnswerCall)
					private.Post("/calls/{callId}/hold", op.HoldCall)
					private.Post("/calls/{callId}/retrieve", op.RetrieveCall)
					private.Post("/calls/{callId}/mute", op.MuteCall)
					private.Post("/calls/{callId}/unmute", op.UnmuteCall)
					private.Post("/calls/{callId}/transfer", op.TransferCall)
					private.Post("/calls/{callId}/dtmf", op.SendCallDTMF)
					private.Patch("/calls/{callId}/user-data", op.PatchUserData)
					private.Get("/calls", op.ListCalls)
					private.Post("/calls/{callId}/monitor", op.MonitorCall)
					// Ending a call: a system that dials must be able to stop
					// what it started, and the call it placed has no agent on
					// it for an agent's rule to reach. The handler decides
					// which leg goes, because an agent ends their own and
					// everybody else ends the call.
					private.Post("/calls/{callId}/hangup", op.HangupCall)
				}

				private.Get("/calls/{callId}/transcript", op.GetCallTranscript)

				if s.catalog != nil {
					private.Get("/extensions", op.ListExtensions)
					private.Post("/extensions", op.CreateExtension)
					private.Put("/extensions/{extensionId}", op.UpdateExtension)
					private.Delete("/extensions/{extensionId}", op.DeleteExtension)
					// The one way to learn a phone's credential, and the one
					// place that reading it is recorded.
					private.Get("/extensions/{extensionId}/password", op.RevealExtensionPassword)

					private.Get("/queues", op.ListQueues)
					private.Post("/queues", op.CreateQueue)
					private.Put("/queues/{queueId}", op.UpdateQueue)
					private.Delete("/queues/{queueId}", op.DeleteQueue)
					private.Get("/queues/{queueId}/agents", op.ListQueueAgents)
					private.Put("/queues/{queueId}/agents", op.StaffQueue)
					private.Delete("/queues/{queueId}/agents/{agentId}", op.UnstaffQueue)

					// Who changed what: the trail names accounts and carries
					// what their requests contained.
					private.Get("/audit-logs", op.ListAuditLogs)

					private.Post("/users", op.CreateUser)
					private.Put("/users/{userId}", op.UpdateUser)
					private.Post("/users/{userId}/password", op.ResetUserPassword)

					private.Get("/dids", op.ListDIDs)
					private.Post("/dids", op.CreateDID)
					private.Put("/dids/{didId}", op.UpdateDID)
					private.Delete("/dids/{didId}", op.DeleteDID)
				}

				if s.flows != nil {
					private.Get("/flows", op.ListFlows)
					private.Post("/flows", op.CreateFlow)
					private.Get("/flows/{flowId}", op.GetFlow)
					private.Put("/flows/{flowId}", op.UpdateFlowDraft)
					private.Post("/flows/{flowId}/publish", op.PublishFlow)
				}

				if s.ledger != nil {
					private.Get("/cdrs/mine", op.ListMyCDRs)
					private.Get("/dispositions", op.ListDispositions)
					private.Get("/reports/me", op.GetMyDay)

					// A recording is heard by whoever may hear the call. The
					// handlers draw that line against the call's agent list.
					private.Get("/calls/{callId}/recordings", op.ListCallRecordings)
					private.Get("/recordings/{recordingId}/audio", op.GetRecordingAudio)

					private.Get("/cdrs", op.ListCDRs)
					private.Get("/cdrs/{callId}", op.GetCDR)
					private.Get("/calls/{callId}/reviews", op.ListCallReviews)
					private.Get("/calls/{callId}/queue-events", op.ListCallQueueEvents)
					private.Post("/recordings/{recordingId}/reviews", op.CreateRecordingReview)
					private.Get("/reports/overview", op.GetReportOverview)
					private.Get("/reports/queues", op.GetReportQueues)
					private.Get("/reports/daily", op.GetReportDaily)

					private.Get("/callbacks", op.ListCallbacks)
					private.Post("/callbacks/{callbackId}/claim", op.ClaimCallback)
					private.Post("/callbacks/{callbackId}/release", op.ReleaseCallback)
					private.Post("/callbacks/{callbackId}/complete", op.CompleteCallback)
				}

				if s.contacts != nil {
					private.Get("/contacts", op.ListContacts)
					private.Post("/contacts", op.CreateContact)
					private.Put("/contacts/{contactId}", op.UpdateContact)
					private.Delete("/contacts/{contactId}", op.DeleteContact)
				}

				// Where finished calls are sent is configuration, the same kind
				// of decision as defining a queue or a number — and it is
				// reachable by a properly scoped key like every other
				// configuration operation. The old rule shut the API key out of
				// it entirely; that was a rule about credentials written where
				// the contract could not state it, and it is now config:write
				// like the rest (docs/auth/baseline.md P5).
				if s.webhooks != nil {
					private.Get("/webhook-subscriptions", op.ListWebhookSubscriptions)
					private.Post("/webhook-subscriptions", op.CreateWebhookSubscription)
					private.Get("/webhook-subscriptions/{subscriptionId}", op.GetWebhookSubscription)
					private.Put("/webhook-subscriptions/{subscriptionId}", op.UpdateWebhookSubscription)
					private.Delete("/webhook-subscriptions/{subscriptionId}", op.DeleteWebhookSubscription)
					private.Get("/webhook-subscriptions/{subscriptionId}/deliveries", op.ListWebhookDeliveries)
				}

				if s.outbound != nil {
					private.Post("/calls", op.CreateCall)
				}

				private.Get("/system/health", op.GetSystemHealth)

				if s.keys != nil {
					private.Get("/api-keys", op.ListAPIKeys)
					private.Post("/api-keys", op.CreateAPIKey)
					private.Get("/api-keys/{keyId}", op.GetAPIKey)
					private.Patch("/api-keys/{keyId}", op.UpdateAPIKey)
					private.Post("/api-keys/{keyId}/revoke", op.RevokeAPIKey)
				}
			})
		})

		// Long-lived stream: no timeout, it ends with the client connection.
		//
		// The one route that does not go through the wrapper. Binding the
		// contract's parameters here would reject a resume point it cannot
		// parse, and an EventSource retries a rejected request forever with
		// the same header — see StreamEvents for why that must degrade to a
		// fresh stream instead.
		//
		// Being outside the wrapper, it is also outside enforceContract, so it
		// applies the contract's rule for itself — the one place in this file
		// where authorization is written by hand, and it is written by asking
		// the same generated table.
		v1.With(s.authenticate, s.enforceContract).Get("/events", func(w http.ResponseWriter, r *http.Request) {
			s.StreamEvents(w, r, api.StreamEventsParams{})
		})
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
