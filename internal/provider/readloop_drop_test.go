// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"testing"
	"time"
)

// #508: when the provider drops the socket the read loop closes the events
// channel, and the response watchdog, which nothing had stopped, could still
// fire OnStall into it: a send on a closed channel, which takes down every
// call in the process. These tests drop the socket a few milliseconds either
// side of the stall deadline.

// dropDeadline is the watchdog's first-audio bound in these tests.
const dropDeadline = 50 * time.Millisecond

// startOpenResponse starts a session whose open response is never going to get
// audio, and returns once the consumer has been told the response started.
func startOpenResponse(t *testing.T, f *fakeProvider) *Realtime {
	t.Helper()
	session, _ := startStallCall(t, f, func(r *Realtime) { r.firstAudioDeadline = dropDeadline })
	f.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-1"}})
	awaitEvent(t, session, EventTypeResponseStarted)
	return session
}

// drainToEnd reads the stream until it closes and returns everything it
// carried, failing if it never closes.
func drainToEnd(t *testing.T, session *Realtime) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				return out
			}
			out = append(out, event)
		case <-deadline:
			t.Fatalf("the event stream never closed (saw %v)", typesOfEvents(out))
		}
	}
}

func typesOfEvents(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// refuteAfterClosed fails if anything follows a CLOSED. A stall that won the
// race may have added its non-fatal error and abandoned done earlier.
func refuteAfterClosed(t *testing.T, events []Event) {
	t.Helper()
	for i, e := range events {
		if e.Type == EventTypeClosed && i != len(events)-1 {
			t.Fatalf("events follow CLOSED: %v", typesOfEvents(events))
		}
	}
}

func TestADroppedSocketNearTheStallDeadlineEndsTheStreamCleanly(t *testing.T) {
	for _, offset := range []time.Duration{-20, -10, 0, 10, 30, 50} {
		offset *= time.Millisecond
		t.Run(fmt.Sprintf("dropped %v from the deadline", offset), func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := startOpenResponse(t, f)

			time.Sleep(dropDeadline + offset)
			f.hangUp()

			events := drainToEnd(t, session)
			if len(events) < 2 {
				t.Fatalf("want at least the fatal error and CLOSED, saw %v", typesOfEvents(events))
			}
			last, before := events[len(events)-1], events[len(events)-2]
			if last.Type != EventTypeClosed {
				t.Fatalf("the stream ended with %s, want CLOSED (saw %v)", last.Type, typesOfEvents(events))
			}
			if before.Type != EventTypeError || !before.IsFatal {
				t.Fatalf("CLOSED is not preceded by the fatal error (saw %v)", typesOfEvents(events))
			}
			refuteAfterClosed(t, events)
			for _, e := range events[:len(events)-2] {
				if e.Type != EventTypeError && e.Type != EventTypeResponseDone {
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
	f := newFakeProvider(t, acceptSession)
	session := startOpenResponse(t, f)

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
	f := newFakeProvider(t, acceptSession)
	session := startOpenResponse(t, f)

	f.hangUp()
	events := drainToEnd(t, session)
	// The deadline passes with nothing left to fire, then Close arrives late.
	time.Sleep(2 * dropDeadline)
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(events) < 2 || events[len(events)-1].Type != EventTypeClosed ||
		!events[len(events)-2].IsFatal {
		t.Fatalf("want the fatal error then CLOSED at the end, saw %v", typesOfEvents(events))
	}
	refuteAfterClosed(t, events)
}
