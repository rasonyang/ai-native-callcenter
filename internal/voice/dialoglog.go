// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

// headerAICCCallID is the key under which customHeaders files the INVITE
// header that carries the domain call id (X-AICC-Call-ID on the wire).
const headerAICCCallID = "X-Aicc-Call-Id"

// parseCallID reads the domain call id off an INVITE header value. A value
// that is empty, is not a UUID or is the nil UUID is not an id: uuid.Nil.
func parseCallID(value string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return uuid.Nil
	}
	return id
}

// dialogLog is the identity one dialog's log lines carry: its SIP Call-ID
// always, and the domain call id from the moment it is known. The id can
// arrive after the dialog's media sessions were built (the orchestrator mints
// a fallback), so the handler reads it on every record instead of baking it
// into a derived logger; the RTP and RTCP sessions hold that one logger.
type dialogLog struct {
	sipCallID string
	callID    atomic.Pointer[uuid.UUID]
	logger    *slog.Logger
}

func newDialogLog(base *slog.Logger, sipCallID string, callID uuid.UUID) *dialogLog {
	if base == nil {
		base = slog.Default()
	}
	l := &dialogLog{sipCallID: sipCallID}
	if callID != uuid.Nil {
		l.callID.Store(&callID)
	}
	l.logger = slog.New(&dialogHandler{inner: base.Handler(), state: l})
	return l
}

func (l *dialogLog) id() uuid.UUID {
	if p := l.callID.Load(); p != nil {
		return *p
	}
	return uuid.Nil
}

// bind attaches id when none is known yet.
func (l *dialogLog) bind(id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	l.callID.CompareAndSwap(nil, &id)
}

type dialogHandler struct {
	inner slog.Handler
	state *dialogLog
}

func (h *dialogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *dialogHandler) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(slog.String("sipCallId", h.state.sipCallID))
	if id := h.state.id(); id != uuid.Nil {
		r.AddAttrs(slog.Any("callId", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *dialogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &dialogHandler{inner: h.inner.WithAttrs(attrs), state: h.state}
}

func (h *dialogHandler) WithGroup(name string) slog.Handler {
	return &dialogHandler{inner: h.inner.WithGroup(name), state: h.state}
}

// CallID is the domain call id this dialog belongs to: the X-AICC-Call-ID the
// INVITE carried, or the one BindCallID attached later. uuid.Nil until known.
// Not the SIP Call-ID, which is SIPCallID.
func (d *Dialog) CallID() uuid.UUID {
	if d.callLog != nil {
		return d.callLog.id()
	}
	return parseCallID(d.CustomHeaders[headerAICCCallID])
}

// BindCallID attaches a call id learned after the INVITE (the orchestrator's
// fallback mint) to every later line the dialog and its RTP and RTCP sessions
// log. It changes nothing when an id is already known, and ignores uuid.Nil.
// Safe from any goroutine.
func (d *Dialog) BindCallID(id uuid.UUID) {
	if d.callLog != nil {
		d.callLog.bind(id)
	}
}

// Logger is the dialog's logger: every record carries sipCallId, and callId
// once it is known. Derive from it with With; the derived logger keeps
// following a later BindCallID.
func (d *Dialog) Logger() *slog.Logger {
	if d.callLog != nil {
		return d.callLog.logger
	}
	return slog.Default().With("sipCallId", d.SIPCallID)
}
