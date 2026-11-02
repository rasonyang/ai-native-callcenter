// SPDX-License-Identifier: Apache-2.0

// Package good holds the shapes the gate must accept.
package good

import (
	"log/slog"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

type logger interface {
	Info(msg string, kv ...any)
}

func fine(l *slog.Logger, w logger, id uuid.UUID, s string) {
	l.Info("a", "callId", id)
	l.Info("b", "callId", id.String())
	l.Info("c", slog.Any("callId", id))
	l.Info("d", slog.String("callId", id.String()))
	w.Info("e", "callId", id)
	_ = []any{"callId", id}
	_ = map[string]any{"callId": s}
	_ = "callId=%s"
	_ = attribute.String("callId", id.String())
	_ = attribute.Key("callId").String(id.String())
	l.Info("sip", "sipCallId", s)
}

type wire struct {
	CallID string `json:"callId"`
}
