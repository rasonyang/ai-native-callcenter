// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeClock is a scheduler whose time only moves when a test says so.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Duration
	tasks []*fakeTask
}

type fakeTask struct {
	at      time.Duration
	f       func()
	stopped bool
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTask{at: c.now + d, f: f}
	c.tasks = append(c.tasks, t)
	return func() bool { c.mu.Lock(); defer c.mu.Unlock(); was := !t.stopped; t.stopped = true; return was }
}

// advance moves time forward, running what falls due in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now + d
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.tasks, func(i, j int) bool { return c.tasks[i].at < c.tasks[j].at })
		var next *fakeTask
		for _, t := range c.tasks {
			if !t.stopped && t.at <= target {
				next = t
				break
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.stopped = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

type reportRig struct {
	clock *fakeClock
	id    uuid.UUID
	mu    sync.Mutex
	cam   Campaign
	got   []Snapshot
	rep   *Reporter
}

func newRig() *reportRig {
	r := &reportRig{clock: &fakeClock{}, id: uuid.Must(uuid.NewV7())}
	r.cam = Campaign{ID: r.id, Status: StatusRunning, Progress: Progress{Activity: ActivityDialing, InFlight: 1}}
	r.rep = NewReporter(func(context.Context, uuid.UUID) (Campaign, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.cam, nil
	}, func(s Snapshot) { r.mu.Lock(); r.got = append(r.got, s); r.mu.Unlock() }, r.clock, nil)
	return r
}

func (r *reportRig) set(f func(c *Campaign)) {
	r.mu.Lock()
	f(&r.cam)
	r.mu.Unlock()
	r.rep.Touch(r.id)
}

func (r *reportRig) events() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Snapshot(nil), r.got...)
}

func TestABurstOfChangesIsOneReportWithTheLastState(t *testing.T) {
	r := newRig()
	for i := 1; i <= 10; i++ {
		r.set(func(c *Campaign) { c.Progress.InFlight = i; c.Contacts.Done = i })
		r.clock.advance(30 * time.Millisecond)
	}
	if got := r.events(); len(got) != 0 {
		t.Fatalf("reported inside the window: %+v", got)
	}
	r.clock.advance(ReportInterval)
	got := r.events()
	if len(got) != 1 || got[0].InFlight != 10 || got[0].Contacts.Done != 10 {
		t.Fatalf("reports = %+v, want one carrying the tenth state", got)
	}
}

func TestChangesFurtherApartThanTheWindowAreReportedEach(t *testing.T) {
	r := newRig()
	r.set(func(c *Campaign) { c.Contacts.Done = 1 })
	r.clock.advance(1200 * time.Millisecond)
	r.set(func(c *Campaign) { c.Contacts.Done = 2 })
	r.clock.advance(1200 * time.Millisecond)
	got := r.events()
	if len(got) != 2 || got[0].Contacts.Done != 1 || got[1].Contacts.Done != 2 {
		t.Fatalf("reports = %+v, want two", got)
	}
}

func TestNothingIsReportedWhenNothingMoved(t *testing.T) {
	r := newRig()
	r.set(func(c *Campaign) {})
	r.clock.advance(2 * time.Second)
	if len(r.events()) != 1 {
		t.Fatalf("the first look must report the state once, got %+v", r.events())
	}
	// A touch with the same state, and a change that reverts inside a window.
	r.set(func(c *Campaign) {})
	r.clock.advance(2 * time.Second)
	r.set(func(c *Campaign) { c.Progress.InFlight = 5 })
	r.set(func(c *Campaign) { c.Progress.InFlight = 1 })
	r.clock.advance(2 * time.Second)
	if got := r.events(); len(got) != 1 {
		t.Fatalf("reports = %+v, want just the first", got)
	}
}

func TestAFinalStatusIsNeverDropped(t *testing.T) {
	r := newRig()
	r.set(func(c *Campaign) { c.Contacts.Done = 3 })
	r.clock.advance(ReportInterval)
	r.set(func(c *Campaign) { c.Status = StatusCompleted; c.Progress = Progress{} })
	// Shutdown inside the window: Close reports what waits.
	r.rep.Close()
	got := r.events()
	if len(got) != 2 || got[1].Status != StatusCompleted || got[1].Activity != "" || got[1].InFlight != 0 {
		t.Fatalf("reports = %+v, want the completed state last", got)
	}
}

func TestOnlyARunningCampaignCarriesTheDialersAccount(t *testing.T) {
	c := Campaign{ID: uuid.Must(uuid.NewV7()), Status: StatusPaused, PauseReason: PauseManual,
		Progress: Progress{Activity: ActivityDialing, InFlight: 2}}
	s := SnapshotOf(c)
	if s.Activity != "" || s.InFlight != 0 || s.PauseReason != PauseManual {
		t.Errorf("snapshot = %+v", s)
	}
}
