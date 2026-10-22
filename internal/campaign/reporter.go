// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ReportInterval is the shortest gap between two reports of one campaign
// (design 10 section 14: at most one CAMPAIGN_UPDATED per second).
const ReportInterval = time.Second

// Snapshot is what supervisors watch of a campaign: the payload of the
// CAMPAIGN_UPDATED event (SseCampaignUpdatedPayload).
type Snapshot struct {
	CampaignID  uuid.UUID
	Status      Status
	PauseReason PauseReason
	// Activity, NextDialAt and InFlight are the dialer's account and exist
	// only while the campaign is RUNNING.
	Activity   Activity
	NextDialAt *time.Time
	InFlight   int
	Contacts   ContactCounts
}

// SnapshotOf reads the watched part of a campaign. Progress is ignored unless
// the campaign is RUNNING, as in the campaign resource.
func SnapshotOf(c Campaign) Snapshot {
	s := Snapshot{CampaignID: c.ID, Status: c.Status, PauseReason: c.PauseReason, Contacts: c.Contacts}
	if c.Status == StatusRunning && c.Progress.Activity != "" {
		s.Activity = c.Progress.Activity
		s.NextDialAt = c.Progress.NextDialAt
		s.InFlight = c.Progress.InFlight
	}
	return s
}

// Equal reports whether nothing the snapshot carries differs.
func (s Snapshot) Equal(o Snapshot) bool {
	if s.CampaignID != o.CampaignID || s.Status != o.Status || s.PauseReason != o.PauseReason ||
		s.Activity != o.Activity || s.InFlight != o.InFlight || s.Contacts != o.Contacts {
		return false
	}
	if (s.NextDialAt == nil) != (o.NextDialAt == nil) {
		return false
	}
	return s.NextDialAt == nil || s.NextDialAt.Equal(*o.NextDialAt)
}

// Scheduler runs a function after a delay; the reporter's only use of time.
type Scheduler interface {
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realScheduler struct{}

func (realScheduler) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Reporter turns "this campaign may have changed" into at most one report per
// ReportInterval, and only when something a supervisor watches did change.
//
// Touch opens a window of ReportInterval for the campaign, unless one is open.
// When it closes the campaign is read afresh and compared with what was last
// reported: equal, nothing is emitted; different, one report carries the
// latest state. So a burst of changes inside a window yields one report with
// the last state, changes further apart than the window yield one each, and a
// final status is reported like any other (Close flushes the windows still
// open, so none is lost at shutdown). The compare happens when the window
// closes, which is also what keeps a change that reverted inside the window
// silent: a state event means a state changed.
type Reporter struct {
	load  func(ctx context.Context, id uuid.UUID) (Campaign, error)
	emit  func(Snapshot)
	sched Scheduler
	log   *slog.Logger

	mu     sync.Mutex
	states map[uuid.UUID]*reportState
	closed bool
}

type reportState struct {
	// mu serialises the closing of windows of one campaign, so two reads
	// cannot be emitted out of order.
	mu    sync.Mutex
	armed bool
	stop  func() bool
	last  *Snapshot
}

// NewReporter builds a reporter. load reads a campaign with the dialer's
// account attached (Service.Load, which leaves out the flow's field names: the
// reporter reads a campaign up to once a second per running campaign and never
// looks at them); emit publishes a snapshot and must not block. sched nil means
// the wall clock.
func NewReporter(load func(ctx context.Context, id uuid.UUID) (Campaign, error), emit func(Snapshot), sched Scheduler, log *slog.Logger) *Reporter {
	if sched == nil {
		sched = realScheduler{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reporter{load: load, emit: emit, sched: sched, log: log, states: map[uuid.UUID]*reportState{}}
}

// Touch says the campaign may have changed. It never blocks.
func (r *Reporter) Touch(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	st := r.states[id]
	if st == nil {
		st = &reportState{}
		r.states[id] = st
	}
	if st.armed {
		return
	}
	st.armed = true
	st.stop = r.sched.AfterFunc(ReportInterval, func() { r.fire(id, st) })
}

// Close stops the windows and reports what is still waiting.
func (r *Reporter) Close() {
	r.mu.Lock()
	r.closed = true
	var pending []uuid.UUID
	states := map[uuid.UUID]*reportState{}
	for id, st := range r.states {
		if st.armed {
			st.stop()
			pending = append(pending, id)
			states[id] = st
		}
	}
	r.mu.Unlock()
	for _, id := range pending {
		r.fire(id, states[id])
	}
}

func (r *Reporter) fire(id uuid.UUID, st *reportState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r.mu.Lock()
	st.armed = false
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := r.load(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		r.mu.Lock()
		delete(r.states, id)
		r.mu.Unlock()
		return
	case err != nil:
		// Try again a window later rather than lose the change.
		r.log.Warn("campaign report: cannot read the campaign", "campaignId", id, "error", err)
		r.Touch(id)
		return
	}
	snap := SnapshotOf(c)
	if st.last != nil && st.last.Equal(snap) {
		return
	}
	st.last = &snap
	r.emit(snap)
}
