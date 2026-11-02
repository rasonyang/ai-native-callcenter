// SPDX-License-Identifier: Apache-2.0

// Package bad plants every shape of offence the gate must find.
package bad

import (
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
)

type dialog struct{ SIPCallID string }

type logger interface {
	Info(msg string, kv ...any)
}

const callIDKey = "callId"

func plant(l *slog.Logger, w logger, d dialog, s string) {
	l.Info("plain", "callId", s)
	l.Info("attr", slog.String("callId", d.SIPCallID))
	_ = []any{"callId", s}
	var attrs []any
	attrs = append(attrs, "callId", s)
	l.Info("const", callIDKey, s)
	w.Info("iface", "callId", s)
	l.Info("trailing", "callId")
	_ = attribute.String("callId", s)
	_ = attribute.Key("callId").String(s)
	_ = attrs
}
