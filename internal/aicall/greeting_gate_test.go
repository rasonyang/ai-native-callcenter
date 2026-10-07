// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// startGated starts a session on a leg whose media has not started.
func startGated(t *testing.T, cfg Config) (*Session, *fakeLeg, *fakeModel, *syncBuffer) {
	t.Helper()
	leg := newFakeLeg(media.LawMu)
	leg.holdMedia()
	model := newFakeModel()
	logs := &syncBuffer{}
	cfg.Session = provider.SessionConfig{Instructions: "answer the phone", Turn: provider.DefaultTurnDetection()}
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	cfg.BargeGuard, cfg.NoInput = -1, -1

	session, err := New(leg, model, provider.OpenAIProfile(), cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	return session, leg, model, logs
}

func startedGate(model *fakeModel) <-chan struct{} {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.startCfg.OpeningGate
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func awaitClosed(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatalf("the gate did not open: %s", what)
	}
}

func TestACallWithNoGreetingSettingAsksForTheGreetingImmediately(t *testing.T) {
	_, _, model, _ := startGated(t, Config{})
	if gate := startedGate(model); gate != nil {
		t.Error("the model was started with an opening gate although none was configured")
	}
}

func TestTheGreetingWaitsForTheFirstInboundMedia(t *testing.T) {
	_, leg, model, logs := startGated(t, Config{IsGreetingGated: true})
	gate := startedGate(model)
	if gate == nil {
		t.Fatal("no gate was handed to the model")
	}
	time.Sleep(50 * time.Millisecond)
	if isClosed(gate) {
		t.Fatal("the gate opened before any media arrived")
	}

	leg.startMedia()
	awaitClosed(t, gate, "first media arrived")
	if !strings.Contains(logs.String(), "reason=MEDIA") {
		t.Errorf("the log does not say media opened the gate:\n%s", logs.String())
	}
}

func TestTheGreetingGraceOpensTheGateWithoutMedia(t *testing.T) {
	_, leg, model, logs := startGated(t, Config{IsGreetingGated: true, GreetingMediaWait: 80 * time.Millisecond})
	gate := startedGate(model)
	if isClosed(gate) {
		t.Fatal("the gate was open at once")
	}
	awaitClosed(t, gate, "grace elapsed")
	if !strings.Contains(logs.String(), "reason=GRACE") {
		t.Errorf("the log does not say the grace opened the gate:\n%s", logs.String())
	}

	// Media arriving later is still measured, and does not log a second opening.
	leg.startMedia()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "first media") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logs.String(), "greeting gate opened"); n != 1 {
		t.Errorf("gate opening logged %d times, want once", n)
	}
	if !strings.Contains(logs.String(), "first media") {
		t.Errorf("late media was not measured:\n%s", logs.String())
	}
}

func TestAZeroGraceWaitsForMediaHoweverLong(t *testing.T) {
	_, _, model, _ := startGated(t, Config{IsGreetingGated: true, GreetingMediaWait: 0})
	gate := startedGate(model)
	time.Sleep(150 * time.Millisecond)
	if isClosed(gate) {
		t.Error("a zero grace opened the gate without media")
	}
}

func TestTheGreetingGateOpensWhenTheLegStops(t *testing.T) {
	_, leg, model, logs := startGated(t, Config{IsGreetingGated: true})
	gate := startedGate(model)
	leg.Stop()
	awaitClosed(t, gate, "the leg stopped")
	if !strings.Contains(logs.String(), "reason=LEG_STOPPED") {
		t.Errorf("the log does not say the leg ending opened the gate:\n%s", logs.String())
	}
}

func TestTheGreetingGateOpensWhenTheCallContextEnds(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	leg.holdMedia()
	model := newFakeModel()
	logs := &syncBuffer{}
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		IsGreetingGated: true, BargeGuard: -1, NoInput: -1,
		Session: provider.SessionConfig{Instructions: "x", Turn: provider.DefaultTurnDetection()},
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := session.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	gate := startedGate(model)
	cancel()
	awaitClosed(t, gate, "ctx ended")
	if !strings.Contains(logs.String(), "reason=CTX") {
		t.Errorf("the log does not say the context opened the gate:\n%s", logs.String())
	}
}

func TestMediaThatHasAlreadyStartedOpensTheGateAtOnce(t *testing.T) {
	leg := newFakeLeg(media.LawMu) // media already started
	model := newFakeModel()
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		IsGreetingGated: true, BargeGuard: -1, NoInput: -1,
		Session: provider.SessionConfig{Instructions: "x", Turn: provider.DefaultTurnDetection()},
		Logger:  slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	awaitClosed(t, startedGate(model), "media was already up")
}

func playFirstFrame(t *testing.T, session *Session, model *fakeModel, leg *fakeLeg) {
	t.Helper()
	awaitBridgeEvent(t, session, EventTypeReady)
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples)}
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(leg.sentFrames()) < 1 {
		t.Fatal("the frame never reached the leg")
	}
}

func TestFirstBotAudioBeforeMediaIsRecordedAsSuch(t *testing.T) {
	session, leg, model, logs := startGated(t, Config{})
	playFirstFrame(t, session, model, leg)
	out := logs.String()
	if !strings.Contains(out, "answerToFirstAudioMs=") || !strings.Contains(out, "isBeforeMedia=true") {
		t.Errorf("first audio before media was not recorded as such:\n%s", out)
	}
}

func TestFirstBotAudioAfterMediaIsRecordedAsHeard(t *testing.T) {
	session, leg, model, logs := startGated(t, Config{})
	leg.startMedia()
	playFirstFrame(t, session, model, leg)
	out := logs.String()
	if !strings.Contains(out, "first bot audio") || !strings.Contains(out, "isBeforeMedia=false") {
		t.Errorf("first audio after media was not recorded as heard:\n%s", out)
	}
}

func TestFirstBotAudioIsRecordedOncePerCall(t *testing.T) {
	session, leg, model, logs := startGated(t, Config{})
	leg.startMedia()
	playFirstFrame(t, session, model, leg)
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*3)}
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logs.String(), "answerToFirstAudioMs"); n != 1 {
		t.Errorf("first audio logged %d times, want once", n)
	}
}

// Media can be up long before Start runs: the lookup, the flow load and the
// provider dial sit between the ACK and Start. The measurement is the packet's
// arrival, not the moment the watch looked.
func TestAnswerToFirstMediaIsMeasuredAtThePacketNotAtStart(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	leg.answeredAt = time.Now().Add(-5 * time.Second)
	leg.firstMediaAt = leg.answeredAt.Add(120 * time.Millisecond)
	logs := &syncBuffer{}
	session, err := New(leg, newFakeModel(), provider.OpenAIProfile(), Config{
		BargeGuard: -1, NoInput: -1,
		Session: provider.SessionConfig{Instructions: "x", Turn: provider.DefaultTurnDetection()},
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "first media") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "answerToFirstMediaMs=120") {
		t.Errorf("first media was not measured at the packet's time:\n%s", logs.String())
	}
}
