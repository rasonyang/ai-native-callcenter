// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"
	"time"
)

func TestAnAbsentOpeningGateDoesNotWait(t *testing.T) {
	if err := AwaitOpeningGate(t.Context(), nil, nil); err != nil {
		t.Errorf("a nil gate waited or failed: %v", err)
	}
}

func TestTheOpeningGateReleasesWhenClosed(t *testing.T) {
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- AwaitOpeningGate(t.Context(), gate, nil) }()
	select {
	case <-done:
		t.Fatal("the wait ended before the gate closed")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Errorf("a closed gate returned %v", err)
	}
}

func TestTheOpeningGateWaitEndsWithTheContextOrTheSession(t *testing.T) {
	gate := make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := AwaitOpeningGate(ctx, gate, nil); err == nil {
		t.Error("a cancelled context did not end the wait")
	}

	closed := make(chan struct{})
	close(closed)
	if err := AwaitOpeningGate(t.Context(), gate, closed); err == nil {
		t.Error("a closed session did not end the wait")
	}
}

// With a gate, the session is configured at once and the opening turn is not
// asked for until the gate closes; Start returns after it is asked for.
func TestTheOpeningTurnWaitsForTheGate(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	gate := make(chan struct{})
	cfg := basicConfig()
	cfg.OpeningGate = gate
	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context(), cfg) }()

	f.awaitMessage("session.update")
	time.Sleep(200 * time.Millisecond)
	f.refuteMessage("response.create")
	select {
	case err := <-started:
		t.Fatalf("Start returned (%v) before the gate closed", err)
	default:
	}

	close(gate)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessages("response.create", 1)
}

// The cue goes with the request, so a provider that needs one is not handed it
// ahead of the gate either.
func TestTheCueWaitsForTheGateToo(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	gate := make(chan struct{})
	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(1)
	cfg.OpeningGate = gate
	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context(), cfg) }()

	f.awaitMessage("session.update")
	time.Sleep(200 * time.Millisecond)
	f.refuteMessage("conversation.item.create")
	f.refuteMessage("response.create")

	close(gate)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessages("conversation.item.create", 1)
	f.awaitMessages("response.create", 1)
}

func TestACancelledCallEndsTheWaitToGreetPromptly(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	cfg := basicConfig()
	cfg.OpeningGate = make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan error, 1)
	go func() { started <- session.Start(ctx, cfg) }()

	f.awaitMessage("session.update")
	cancel()
	select {
	case err := <-started:
		if err == nil {
			t.Error("Start succeeded although the call ended while it waited")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
	f.refuteMessage("response.create")
}
