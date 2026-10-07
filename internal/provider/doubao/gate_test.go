// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"testing"
	"time"
)

// With a gate, the session is created at once and the opening line is not
// committed until the gate closes; Start returns after it is committed.
func TestTheOpeningLineWaitsForTheGate(t *testing.T) {
	defer noGoroutinesLeft(t)()
	f := newFakeDoubao(t, acceptSession)
	session := testSession(t, f)

	gate := make(chan struct{})
	cfg := testConfig()
	cfg.OpeningText = "Good morning, NovaNet support."
	cfg.OpeningGate = gate
	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context(), cfg) }()

	f.awaitMessages("session.create", 1)
	time.Sleep(250 * time.Millisecond)
	if got := f.messagesOfType("speech_text_buffer.commit"); len(got) != 0 {
		t.Fatalf("the opening line was committed before the gate closed: %v", got)
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
	f.awaitMessages("speech_text_buffer.commit", 1)
	_ = session.Close(context.Background())
}

func TestACancelledCallEndsTheWaitToGreetPromptly(t *testing.T) {
	defer noGoroutinesLeft(t)()
	f := newFakeDoubao(t, acceptSession)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.OpeningText = "Good morning."
	cfg.OpeningGate = make(chan struct{}) // never closed
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan error, 1)
	go func() { started <- session.Start(ctx, cfg) }()

	f.awaitMessages("session.create", 1)
	cancel()
	select {
	case err := <-started:
		if err == nil {
			t.Error("Start succeeded although the call ended while it waited")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
	if got := f.messagesOfType("speech_text_buffer.commit"); len(got) != 0 {
		t.Errorf("a line was committed after the call ended: %v", got)
	}
	_ = session.Close(context.Background())
}
