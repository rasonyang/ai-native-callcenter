// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"context"
	"testing"
	"time"
)

// With a gate, the setup is done at once and the opening turn is not asked for
// until the gate closes; Start returns after it is asked for.
func TestTheOpeningTurnWaitsForTheGate(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)

	gate := make(chan struct{})
	cfg := testConfig()
	cfg.OpeningText = "Good morning, NovaNet support."
	cfg.OpeningGate = gate
	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context(), cfg) }()

	f.awaitFrames(1) // the setup
	time.Sleep(250 * time.Millisecond)
	if got := len(f.messages()); got != 1 {
		t.Fatalf("the client sent %d frames before the gate closed, want only the setup", got)
	}
	select {
	case err := <-started:
		t.Fatalf("Start returned (%v) before the gate closed", err)
	default:
	}

	close(gate)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitFrames(2)
}

func TestACancelledCallEndsTheWaitToGreetPromptly(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.OpeningGate = make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan error, 1)
	go func() { started <- session.Start(ctx, cfg) }()

	f.awaitFrames(1)
	cancel()
	select {
	case err := <-started:
		if err == nil {
			t.Error("Start succeeded although the call ended while it waited")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
	if got := len(f.messages()); got != 1 {
		t.Errorf("the client sent %d frames, want only the setup", got)
	}
	_ = session.Close(context.Background())
}
