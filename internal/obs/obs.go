// SPDX-License-Identifier: Apache-2.0

// Package obs wires structured logging, tracing and metrics.
//
// Logs are slog JSON records; every record produced inside a traced context
// carries traceId/spanId so a log line can be joined to its span. Metrics are
// exposed in Prometheus format on the separate metrics listener.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexp "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	metricsdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"net/http"
)

// Providers holds the OpenTelemetry providers owned by the process.
type Providers struct {
	tracer *tracesdk.TracerProvider
	meter  *metricsdk.MeterProvider
	// MetricsHandler serves the Prometheus exposition format.
	MetricsHandler http.Handler
}

// Setup installs the global logger and OpenTelemetry providers.
//
// otlpEndpoint may be empty, in which case no trace exporter is installed and
// tracing becomes a no-op with negligible cost.
func Setup(ctx context.Context, serviceName, logLevel, otlpEndpoint string, dev bool) (*Providers, error) {
	slog.SetDefault(newLogger(logLevel, dev))

	// NewSchemaless: merging a pinned semconv schema with the SDK's own
	// default resource fails whenever the two schema versions differ.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, err
	}

	traceOpts := []tracesdk.TracerProviderOption{tracesdk.WithResource(res)}
	if otlpEndpoint != "" {
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(otlpEndpoint))
		if err != nil {
			return nil, err
		}
		traceOpts = append(traceOpts, tracesdk.WithBatcher(exp))
	}
	tp := tracesdk.NewTracerProvider(traceOpts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	promExporter, err := promexp.New()
	if err != nil {
		return nil, err
	}
	mp := metricsdk.NewMeterProvider(metricsdk.WithResource(res), metricsdk.WithReader(promExporter))
	otel.SetMeterProvider(mp)

	return &Providers{tracer: tp, meter: mp, MetricsHandler: promhttp.Handler()}, nil
}

// Shutdown flushes and stops the providers.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var first error
	if err := p.tracer.Shutdown(ctx); err != nil {
		first = err
	}
	if err := p.meter.Shutdown(ctx); err != nil && first == nil {
		first = err
	}
	return first
}

// newLogger builds the process logger: JSON in production, text in dev, with
// trace correlation applied to every record.
func newLogger(level string, dev bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if dev {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(&traceHandler{Handler: h})
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// traceHandler adds traceId and spanId to records emitted within a span.
type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("traceId", sc.TraceID().String()),
			slog.String("spanId", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
