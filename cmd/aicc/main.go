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
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/seed"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
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

	if cfg.Seed == "demo" {
		if err := seed.Demo(ctx, st, slog.Default()); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
	}

	authSvc := auth.NewService(st.Queries, cfg.SessionTTL)
	hub := events.NewHub(events.NewSequence(seqReserver{st}, "events"))
	hub.OnDropped = func(sub events.Subscriber) {
		slog.Warn("sse subscriber dropped", "userId", sub.UserID)
	}

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

	// The switch forgets its agents when it restarts, and we are the source of
	// truth, so every reconnect rebuilds its view. In the other direction the
	// switch knows which phones are registered, which live events alone never
	// tell us: a phone that registered before this process started would
	// otherwise look missing and its agent unroutable.
	link.OnConnect(func(ctx context.Context) {
		agentSvc.SyncSwitch(ctx)

		regs, err := adapter.Registrations(cfg.SIPProfile)
		if err != nil {
			slog.WarnContext(ctx, "could not read registrations", "error", err)
			return
		}
		for _, reg := range regs {
			agentSvc.ObserveDevice(ctx, reg.Extension, true, reg.IsReachable)
		}
		slog.InfoContext(ctx, "registrations reconciled", "endpoints", len(regs))
	})

	coordinator := telephony.NewCoordinator(registry, adapter, agentSvc, hub)
	catalogSvc := catalog.NewService(st.Catalog(), adapter, st.Catalog())

	// Recording storage: nil when no directory is configured, which disables
	// ingestion without disabling anything else.
	var recordings recording.Storage
	if cfg.RecordingDir != "" {
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
		slog.Info("recording storage ready", "backend", recordings.Backend(), "dir", cfg.RecordingDir)
	}

	var recordingStorage telephony.RecordingStorage
	if recordings != nil {
		recordingStorage = recordings
	}
	coordinator.AttachCDR(telephony.NewCDRAssembler(st.Ledger(), catalogSvc, recordingStorage, slog.Default()))

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

	go link.Run(ctx)
	go dispatchSwitchEvents(ctx, link, coordinator, agentSvc, outboundSvc)

	// The AI voice leg: a SIP server the switch bridges bot calls to, and the
	// orchestration that runs a conversation on each.
	if cfg.IsBotEnabled {
		orchestrator, err := aicall.NewOrchestrator(aicall.OrchestratorConfig{
			UAS: voice.Config{
				SIPHost:          cfg.BotSIPHost,
				SIPPort:          cfg.BotSIPPort,
				AdvertiseIP:      cfg.BotAdvertiseIP,
				RTPPortRange:     [2]int{cfg.BotRTPPortLow, cfg.BotRTPPortHigh},
				CodecPreferences: voice.DefaultConfig().CodecPreferences,
				RTPDeadTimeout:   voice.DefaultConfig().RTPDeadTimeout,
				AckTimeout:       voice.DefaultConfig().AckTimeout,
				MaxCalls:         cfg.BotMaxCalls,
				IsDTMFEnabled:    true,
				RTCPInterval:     voice.DefaultConfig().RTCPInterval,
			},
			Catalog:     catalogSvc,
			Flows:       st.Flows(),
			Switch:      adapter,
			Ledger:      st.Ledger(),
			BackendBase: cfg.BotBackendBase,
			AnnounceCallback: func(callback store.Callback) {
				hub.Publish(ctx, events.Event{
					Type:    events.TypeCallbackCreated,
					CallID:  callback.CallID,
					Payload: map[string]any{"callback": callback},
				}, events.Scope{})
			},
		})
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
		Handler: httpapi.New(cfg, httpapi.Deps{
			Auth:       authSvc,
			Hub:        hub,
			Agents:     agentSvc,
			AgentDir:   agentDirectory{st},
			Calls:      coordinator,
			Catalog:    catalogSvc,
			Ledger:     st.Ledger(),
			Recordings: recordings,
			Auditor:    st.Ledger(),
			Outbound:   outboundSvc,
			SPA:        spa,
		}).Handler(),
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

// seqReserver adapts the store to the events package's reserver interface.
type seqReserver struct{ st *store.Store }

func (r seqReserver) ReserveSeqBlock(ctx context.Context, name string, size int64) (int64, error) {
	return r.st.Queries.ReserveSeqBlock(ctx, queries.ReserveSeqBlockParams{
		BlockSize: size,
		Name:      name,
	})
}
