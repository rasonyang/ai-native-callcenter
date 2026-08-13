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

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
	"github.com/rasonyang/ai-native-callcenter/web"
)

func main() {
	// Subcommands come before the server so an operator can bootstrap the
	// first administrator against an empty database.
	if len(os.Args) > 1 && os.Args[1] == "useradd" {
		if err := runUserAdd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
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

	providers, err := obs.Setup(ctx, cfg.ServiceName, cfg.LogLevel, cfg.OTLPEndpoint, cfg.IsDev())
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

	authSvc := auth.NewService(st.Queries, cfg.SessionTTL)
	hub := events.NewHub(events.NewSequence(seqReserver{st}, "events"))
	hub.OnDropped = func(sub events.Subscriber) {
		slog.Warn("sse subscriber dropped", "userId", sub.UserID)
	}

	var spa http.Handler
	if dist, err := web.Dist(); err == nil {
		spa = httpapi.SPAHandler(dist)
		slog.Info("serving embedded spa")
	} else {
		slog.Warn("no embedded spa; run the vite dev server", "error", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(cfg, authSvc, hub, spa).Handler(),
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

// seqReserver adapts the store to the events package's reserver interface.
type seqReserver struct{ st *store.Store }

func (r seqReserver) ReserveSeqBlock(ctx context.Context, name string, size int64) (int64, error) {
	return r.st.Queries.ReserveSeqBlock(ctx, queries.ReserveSeqBlockParams{
		BlockSize: size,
		Name:      name,
	})
}
