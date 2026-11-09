// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #508: the watchdog's onTurnStalled emits on the events channel, which the
// read loop closes when the socket drops. A stall after that sent on a closed
// channel, and because emit tries a bare send first, every time rather than at
// random.

const dropDeadline = 50 * time.Millisecond

// startOpenTurn starts a session whose open turn is never going to get audio.
func startOpenTurn(t *testing.T, f *fakeDoubao) *Session {
	t.Helper()
	session := testSession(t, f)
	session.firstAudioDeadline = dropDeadline
	session.deltaStallDeadline = dropDeadline
	start(t, session, testConfig())
	f.send(audioStarted("default"))
	expectEvents(t, session, provider.EventTypeResponseStarted)
	return session
}

func drainToEnd(t *testing.T, session *Session) []provider.Event {
	t.Helper()
	var out []provider.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				return out
			}
			out = append(out, event)
		case <-deadline:
			t.Fatalf("the event stream never closed (saw %v)", typesOf(out))
		}
	}
}

func refuteAfterClosed(t *testing.T, events []provider.Event) {
	t.Helper()
	for i, e := range events {
		if e.Type == provider.EventTypeClosed && i != len(events)-1 {
			t.Fatalf("events follow CLOSED: %v", typesOf(events))
		}
	}
}

func TestADroppedSocketNearTheStallDeadlineEndsTheStreamCleanly(t *testing.T) {
	for _, offset := range []time.Duration{-20, -10, 0, 10, 30, 50} {
		offset *= time.Millisecond
		t.Run(fmt.Sprintf("dropped %v from the deadline", offset), func(t *testing.T) {
			f := newFakeDoubao(t, acceptSession)
			session := startOpenTurn(t, f)

			time.Sleep(dropDeadline + offset)
			f.hangUp()

			events := drainToEnd(t, session)
			if len(events) < 2 {
				t.Fatalf("want at least the fatal error and CLOSED, saw %v", typesOf(events))
			}
			last, before := events[len(events)-1], events[len(events)-2]
			if last.Type != provider.EventTypeClosed {
				t.Fatalf("the stream ended with %s, want CLOSED (saw %v)", last.Type, typesOf(events))
			}
			if before.Type != provider.EventTypeError || !before.IsFatal {
				t.Fatalf("CLOSED is not preceded by the fatal error (saw %v)", typesOf(events))
			}
			refuteAfterClosed(t, events)
			for _, e := range events[:len(events)-2] {
				if e.Type != provider.EventTypeError && e.Type != provider.EventTypeResponseDone {
					t.Errorf("unexpected %s before the fatal error", e.Type)
				}
				if e.IsFatal {
					t.Error("a second fatal error")
				}
			}
		})
	}
}

func TestACloseBeforeTheStallDeadlineEndsTheStreamCleanly(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startOpenTurn(t, f)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(2 * dropDeadline)

	events := drainToEnd(t, session)
	refuteAfterClosed(t, events)
	for _, e := range events {
		if e.IsFatal {
			t.Errorf("a closed session reported a fatal error: %q", e.Text)
		}
	}
}

func TestADropThenTheStallDeadlineThenCloseEndsTheStreamCleanly(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startOpenTurn(t, f)

	f.hangUp()
	events := drainToEnd(t, session)
	time.Sleep(2 * dropDeadline)
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(events) < 2 || events[len(events)-1].Type != provider.EventTypeClosed ||
		!events[len(events)-2].IsFatal {
		t.Fatalf("want the fatal error then CLOSED at the end, saw %v", typesOf(events))
	}
	refuteAfterClosed(t, events)
}

func TestTheWatchdogEndsWithTheReadLoopWhenCloseIsNeverCalled(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startOpenTurn(t, f)

	f.hangUp()
	drainToEnd(t, session)

	deadline := time.Now().Add(3 * time.Second)
	for {
		var left []string
		for _, stack := range sessionGoroutines() {
			if strings.Contains(stack, "provider.(*Watchdog)") {
				left = append(left, stack)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watchdog outlived the read loop:\n%s", strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
