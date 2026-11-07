// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A caller hanging up closes the session; an uplink send that is in flight
// then fails because the model was closed, which is not a failed call (#500).

var errModelClosed = errors.New("provider: session closed")

// blockingSendModel parks SendAudio until Close has run, then fails it as a
// real provider does, so the send error after Close happens on every run.
type blockingSendModel struct {
	*fakeModel
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingSendModel() *blockingSendModel {
	return &blockingSendModel{
		fakeModel: newFakeModel(),
		entered:   make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (m *blockingSendModel) SendAudio([]byte) error {
	m.once.Do(func() { close(m.entered) })
	<-m.closed
	return errModelClosed
}

func (m *blockingSendModel) Close(ctx context.Context) error {
	err := m.fakeModel.Close(ctx)
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return err
}

// tickedSession starts a session whose uplink clock moves only when the test
// ticks it, and returns a function that delivers one tick that owes a slot.
func tickedSession(t *testing.T, leg *fakeLeg, model provider.VoiceSession) (*Session, func()) {
	t.Helper()
	session, err := New(leg, model, provider.OpenAIProfile(), Config{BargeGuard: -1, NoInput: -1})
	if err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(1_000_000, 0)
	var offset time.Duration
	var mu sync.Mutex
	started := make(chan struct{})
	var startOnce sync.Once
	session.uplinkNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		startOnce.Do(func() { close(started) })
		return epoch.Add(offset)
	}
	ticks := make(chan time.Time)
	session.uplinkTicks = ticks
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the uplink pump never started its clock")
	}
	tick := func() {
		mu.Lock()
		offset += 2 * uplinkSlot
		at := epoch.Add(offset)
		mu.Unlock()
		select {
		case ticks <- at:
		case <-time.After(10 * time.Second):
			t.Fatal("the uplink pump did not take its tick")
		}
	}
	return session, tick
}

// drainEvents reads the stream to its end and returns the event types.
func drainEvents(t *testing.T, session *Session) []EventType {
	t.Helper()
	var seen []EventType
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				return seen
			}
			seen = append(seen, event.Type)
		case <-deadline:
			t.Fatalf("the stream never closed (saw %v)", seen)
		}
	}
}

func countOf(seen []EventType, want EventType) int {
	n := 0
	for _, typ := range seen {
		if typ == want {
			n++
		}
	}
	return n
}

func TestACallerHangingUpDuringASendIsNotAFailure(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := newBlockingSendModel()
	session, tick := tickedSession(t, leg, model)

	tick()
	select {
	case <-model.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the uplink never sent")
	}
	leg.Stop() // the caller hangs up while the send is in flight

	seen := drainEvents(t, session)
	if n := countOf(seen, EventTypeFailed); n != 0 {
		t.Fatalf("a hang-up was reported as FAILED %d times: %v", n, seen)
	}
	if len(seen) == 0 || seen[len(seen)-1] != EventTypeEnded {
		t.Fatalf("the stream did not end with ENDED: %v", seen)
	}
}

func TestASendErrorOnAnOpenSessionFailsTheCallOnce(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()
	model.mu.Lock()
	model.sendErr = errors.New("socket reset")
	model.mu.Unlock()
	session, tick := tickedSession(t, leg, model)

	tick()

	seen := drainEvents(t, session)
	if n := countOf(seen, EventTypeFailed); n != 1 {
		t.Fatalf("want FAILED once, got %d: %v", n, seen)
	}
	if seen[len(seen)-1] != EventTypeEnded {
		t.Fatalf("the stream did not end with ENDED: %v", seen)
	}
	if !session.isClosed() {
		t.Fatal("the failed session was not closed")
	}
}
