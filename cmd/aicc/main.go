// SPDX-License-Identifier: Apache-2.0

// Command aicc is the AI Native Call Center server: one executable serving the
// REST API, the event stream and the embedded single-page application.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/aicall"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/seed"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
	"github.com/rasonyang/ai-native-callcenter/internal/streamin"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
	"github.com/rasonyang/ai-native-callcenter/web"
)

func main() {
	// Subcommands come before the server so an operator can bootstrap the
	// first administrator against an empty database.
	if len(os.Args) > 1 {
		var handler func([]string) error
		switch os.Args[1] {
		case "useradd":
			handler = runUserAdd
		case "passwd":
			handler = runPasswd
		case "flowadd":
			handler = runFlowAdd
		case "version", "-version", "--version":
			handler = runVersion
		}
		if handler != nil {
			if err := handler(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providers, err := obs.Setup(ctx, cfg.ServiceName, cfg.LogLevel,
		cfg.OTLPEndpoint, cfg.LogDir, cfg.IsDev())
	if err != nil {
		return fmt.Errorf("setup observability: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := providers.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown observability", "error", err)
		}
	}()

	slog.Info("starting", "service", cfg.ServiceName, "env", cfg.Env, "httpAddr", cfg.HTTPAddr)

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	lock, err := st.AcquireInstanceLock(ctx)
	if err != nil {
		return fmt.Errorf("acquire instance lock: %w", err)
	}
	defer lock.Release(context.Background())

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	slog.Info("database ready")

	switch cfg.Seed {
	case "demo":
		if err := seed.Demo(ctx, st, slog.Default()); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
	case "fresh":
		if err := seed.Fresh(ctx, st, slog.Default()); err != nil {
			return fmt.Errorf("reset demo data: %w", err)
		}
	}

	authSvc := auth.NewService(st.Queries, cfg.SessionTTL)
	hub := events.NewHub(events.NewSequence(seqReserver{st}, "events"))
	hub.OnDropped = func(sub events.Subscriber) {
		slog.Warn("sse subscriber dropped", "userId", sub.UserID)
	}
	// One transcript actor per call owns the order of what was said, whichever
	// phase said it, so the bot and the human path cannot allocate a colliding
	// seq for one conversation.
	transcripts := transcript.NewRegistry(st.Ledger(), hub, slog.Default())

	// Telephony: one link to the switch, a command adapter over it, the live
	// call registry, and the agent presence service.
	link := esl.NewLink(cfg.ESLAddr, cfg.ESLPassword, telephony.Subscriptions)
	adapter := telephony.NewAdapter(link, cfg.SwitchDomain)
	registry := telephony.NewRegistry(hub)
	defer registry.Shutdown()

	agentSvc := agents.NewService(st.Agents(), adapter, hub)
	if err := agentSvc.Restore(ctx); err != nil {
		slog.Warn("could not restore agent presence", "error", err)
	}

	coordinator := telephony.NewCoordinator(registry, adapter, agentSvc, hub)
	catalogSvc := catalog.NewService(st.Catalog(), adapter, st.Catalog())

	// Live transcription of the human phase. The tap goes on the agent's own
	// leg at the bridge, so it exists for exactly as long as the human part of
	// the conversation does.
	// tap stays nil when transcription is disabled, which is the one
	// conditional connection the composition has.
	var tap telephony.Tapper
	if cfg.IsTranscriptionEnabled {
		profile, err := transcribe.ProfileFor(cfg.TranscribeProviderName(), transcribe.Override{
			Endpoint: cfg.TranscribeEndpoint,
			Model:    cfg.TranscribeModel,
		})
		if err != nil {
			return fmt.Errorf("transcription: %w", err)
		}
		apiKey := os.Getenv(profile.APIKeyEnv)
		if apiKey == "" {
			return fmt.Errorf("transcription: %s is required for the %s recogniser",
				profile.APIKeyEnv, profile.Name)
		}

		ingest, err := streamin.New(streamin.Config{
			Addr:    cfg.StreamAddr,
			Secret:  []byte(cfg.StreamSecret),
			Profile: profile,
			NewSession: func() (transcribe.Session, error) {
				return transcribe.New(profile, apiKey, slog.Default())
			},
			Transcripts: transcripts,
			Logger:      slog.Default(),
		})
		if err != nil {
			return fmt.Errorf("transcription ingest: %w", err)
		}
		if err := ingest.Start(); err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = ingest.Stop(shutdownCtx)
		}()

		// The rate is the recogniser's, and the switch resamples to it. That
		// is why no resampler exists in this process.
		tap = streamin.NewTap(ingest, adapter,
			streamin.NormalizePublicURL(cfg.StreamPublicURL),
			profile.SampleRate, 60*time.Second, slog.Default())
		slog.Info("live transcription enabled",
			"provider", profile.Name, "model", profile.Model, "rateHz", profile.SampleRate)
	}

	// Recording storage: nil when no directory is configured, which disables
	// ingestion without disabling anything else.
	var recordings recording.Storage
	// The filesystem backend needs the directory the switch records into; the
	// S3 backend works with the endpoint alone when the switch uploads
	// directly (mod_http_cache), the spool directory being optional.
	if cfg.RecordingDir != "" || (cfg.RecordingBackend == "S3" && cfg.S3Endpoint != "") {
		recordings, err = recording.New(recording.Config{
			Backend: cfg.RecordingBackend, Dir: cfg.RecordingDir,
			S3Endpoint: cfg.S3Endpoint, S3AccessKey: cfg.S3AccessKey,
			S3SecretKey: cfg.S3SecretKey, S3Bucket: cfg.S3Bucket, S3IsSSL: cfg.S3IsSSL,
		})
		if err != nil {
			return fmt.Errorf("recording storage: %w", err)
		}
		if s3, ok := recordings.(interface{ EnsureBucket(context.Context) error }); ok {
			if err := s3.EnsureBucket(ctx); err != nil {
				return fmt.Errorf("recording storage: %w", err)
			}
		}
		slog.Info("recording storage ready", "backend", recordings.Backend(),
			"dir", cfg.RecordingDir, "endpoint", cfg.S3Endpoint)
	}

	var recordingStorage telephony.RecordingStorage
	if recordings != nil {
		recordingStorage = recordings
	}
	// Every connection between the parts, in one place that a test can drive
	// with fakes. Nothing above this line joins two services together.
	composition{
		Registry:    registry,
		Coordinator: coordinator,
		Agents:      agentSvc,
		Catalog:     catalogSvc,
		Link:        link,
		CDR:         telephony.NewCDRAssembler(st.Ledger(), catalogSvc, recordingStorage, slog.Default()),
		Queues:      queueCatalog{catalogSvc},
		WrapUps:     st.Ledger(),
		Transcripts: transcripts,
		Audiences:   transcripts,
		Taps:        tap,
		Registrations: func() ([]telephony.Registration, error) {
			return adapter.Registrations(cfg.SIPProfile)
		},
		Events: hub,
		Log:    slog.Default(),
	}.connect()

	// Outbound: click-to-dial and the AI outbound leg share one originator.
	outboundSvc := outbound.New(outbound.Config{
		EndpointFormat: cfg.OutboundEndpoint,
		CallerID:       cfg.OutboundCallerID,
	}, adapter, catalogSvc,
		st.Ledger().HasCDR,
		func(callID uuid.UUID) bool {
			_, err := registry.Snapshot(callID)
			return err == nil
		}, slog.Default())

	// Business data a request attached to a call reaches the call itself here,
	// and the bot's own session below. It deliberately never goes through the
	// switch, so these two readers are the only ways it travels.
	coordinator.AttachCallData(outboundSvc.Data())

	go link.Run(ctx)
	go dispatchSwitchEvents(ctx, link, coordinator, agentSvc, outboundSvc)

	// The AI voice leg: a SIP server the switch bridges bot calls to, and the
	// orchestration that runs a conversation on each.
	if cfg.IsBotEnabled {
		// One provider answers every call in this deployment; an unknown name
		// is a startup failure, not a surprise on the first call.
		transcribeOff := strings.EqualFold(cfg.ProviderTranscribeModel, "off")
		transcribeModel := cfg.ProviderTranscribeModel
		if transcribeOff {
			transcribeModel = ""
		}
		profile, err := provider.ProfileFor(cfg.Provider, provider.Override{
			Endpoint:        cfg.ProviderEndpoint,
			Model:           cfg.ProviderModel,
			TranscribeModel: transcribeModel,
			TranscribeOff:   transcribeOff,
		})
		if err != nil {
			return fmt.Errorf("AICC_PROVIDER: %w", err)
		}
		slog.Info("voice provider selected",
			"provider", profile.Name, "model", profile.Model, "endpoint", profile.Endpoint,
			// Whether the caller's own words will be in the bot phase's
			// transcript at all, said at startup rather than discovered from
			// an empty column afterwards. Empty on a vendor that transcribes
			// unasked is not the same as off.
			"transcribesCaller", profile.TranscribeModel)

		orchestrator, err := aicall.NewOrchestrator(botConfig(
			botUAS(cfg),
			catalogSvc,
			st.Flows(),
			adapter,
			st.Ledger(),
			transcripts,
			cfg.BotBackendBase,
			profile,
			announceCallback(ctx, hub),
			outboundSvc.Data(),
		))
		if err != nil {
			return fmt.Errorf("build ai voice leg: %w", err)
		}
		if err := orchestrator.Start(); err != nil {
			return fmt.Errorf("start ai voice leg: %w", err)
		}
		defer orchestrator.Stop()
		slog.Info("ai voice leg listening",
			"sipPort", cfg.BotSIPPort, "maxCalls", cfg.BotMaxCalls)
	}

	var spa http.Handler
	if dist, err := web.Dist(); err == nil {
		spa = httpapi.SPAHandler(dist)
		slog.Info("serving embedded spa")
	} else {
		slog.Warn("no embedded spa; run the vite dev server", "error", err)
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(cfg, apiDeps(
			authSvc,
			hub,
			agentSvc,
			agentDirectory{st},
			coordinator,
			transcripts,
			catalogSvc,
			st.Contacts(),
			st.Ledger(),
			recordings,
			st.Ledger(),
			outboundSvc,
			spa,
		)).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the event stream is long-lived.
		IdleTimeout: 120 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           httpapi.MetricsHandler(providers.MetricsHandler, func() error { return st.Pool.Ping(ctx) }),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go serve(metricsSrv, "metrics")
	go serve(srv, "http")

	go purgeSessions(ctx, authSvc)

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown http", "error", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown metrics", "error", err)
	}
	return nil
}

func serve(srv *http.Server, name string) {
	slog.Info("listening", "listener", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listener failed", "listener", name, "error", err)
	}
}

// purgeSessions removes expired sessions hourly.
func purgeSessions(ctx context.Context, svc *auth.Service) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := svc.PurgeExpiredSessions(ctx)
			if err != nil {
				slog.Error("purge sessions", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("purged expired sessions", "count", n)
			}
		}
	}
}

// dispatchSwitchEvents is the single consumer of the switch event stream. It
// normalizes each event once and hands it to whoever owns that fact: the call
// registry for channel lifecycle, the agent service for device reachability.
func dispatchSwitchEvents(ctx context.Context, link *esl.Link, coordinator *telephony.Coordinator, agentSvc *agents.Service, outboundSvc *outbound.Service) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, open := <-link.Events():
			if !open {
				return
			}
			ev, ok := telephony.Normalize(raw)
			if !ok {
				continue
			}

			switch ev.Kind {
			case telephony.KindDeviceRegistered, telephony.KindDeviceUnregistered:
				agentSvc.ObserveDevice(ctx, ev.Extension, ev.Registered, ev.Registered)
			case telephony.KindDeviceState:
				// A phone that stops answering keepalives is still registered:
				// this is the signal that separates a live agent from a
				// crashed browser tab.
				agentSvc.ObserveDevice(ctx, ev.Extension, true, ev.Registered)
			default:
				coordinator.Handle(ctx, ev)
			}
			// Outbound follows its originated legs on the same feed.
			outboundSvc.HandleSwitchEvent(ev)
		}
	}
}

// agentDirectory resolves the agent behind a signed-in user.
type agentDirectory struct{ st *store.Store }

func (d agentDirectory) AgentIDForUser(r *http.Request, userID uuid.UUID) (uuid.UUID, error) {
	agent, err := d.st.Queries.GetAgentByUserID(r.Context(), userID)
	if err != nil {
		return uuid.Nil, err
	}
	return agent.ID, nil
}

func (d agentDirectory) QueuesForAgent(r *http.Request, agentID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.st.Queries.ListQueuesForAgent(r.Context(), agentID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids, nil
}

// queueCatalog turns a configured queue into what the waiting line needs, so
// call control can render a queue without depending on the catalog's types.
type queueCatalog struct{ catalog *catalog.Service }

func (q queueCatalog) QueueByName(ctx context.Context, name string) (telephony.QueueSummary, bool) {
	queue, ok := q.catalog.QueueByName(ctx, name)
	if !ok {
		return telephony.QueueSummary{}, false
	}
	return telephony.QueueSummary{
		ID:              queue.ID,
		Name:            queue.Name,
		DisplayName:     queue.DisplayName,
		SLAThresholdSec: queue.SLAThresholdSec,
	}, true
}

func (q queueCatalog) Queues(ctx context.Context) ([]telephony.QueueSummary, error) {
	queues, err := q.catalog.Queues(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]telephony.QueueSummary, 0, len(queues))
	for _, queue := range queues {
		out = append(out, telephony.QueueSummary{
			ID:              queue.ID,
			Name:            queue.Name,
			DisplayName:     queue.DisplayName,
			SLAThresholdSec: queue.SLAThresholdSec,
		})
	}
	return out, nil
}

// seqReserver adapts the store to the events package's reserver interface.
type seqReserver struct{ st *store.Store }

func (r seqReserver) ReserveSeqBlock(ctx context.Context, name string, size int64) (int64, error) {
	return r.st.Queries.ReserveSeqBlock(ctx, queries.ReserveSeqBlockParams{
		BlockSize: size,
		Name:      name,
	})
}
