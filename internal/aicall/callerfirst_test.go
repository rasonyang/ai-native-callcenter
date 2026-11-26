// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// Synthetic callee audio.
//

// squareFrame is 20 ms of a square wave of the given amplitude in the law:
// its RMS is the amplitude.
func squareFrame(law media.Law, amplitude int16) []byte {
	samples := make([]int16, media.FrameSamples)
	for i := range samples {
		samples[i] = amplitude
		if i%2 == 1 {
			samples[i] = -amplitude
		}
	}
	return law.Encode(make([]byte, 0, media.FrameSamples), samples)
}

// Levels, as amplitudes of a square wave (RMS = amplitude).
const (
	speechAmp int16 = 3277 // -20 dBFS
	noiseAmp  int16 = 184  // -45 dBFS
	hissAmp   int16 = 300  // -41 dBFS: just under the absolute floor
	weakAmp   int16 = 600  // -35 dBFS: over the absolute floor
)

// scenario describes a callee's line slot by slot: 'v' speech, 'n' line noise,
// '.' no packet at all.
func scenario(parts ...string) string { return strings.Join(parts, "") }

func slots(kind byte, ms int) string { return strings.Repeat(string(kind), ms/frameDurationMs) }

// run feeds the scenario to a listener and reports the slot (as the time it
// ends, from the answer) at which it decided, and what it decided.
func run(law media.Law, line string) (decidedAt time.Duration, verdict listenVerdict, l *greetingListener) {
	l = newGreetingListener(law, callerFirstWait)
	for i := 0; i < len(line); i++ {
		var frame []byte
		switch line[i] {
		case 'v':
			frame = squareFrame(law, speechAmp)
		case 'n':
			frame = squareFrame(law, noiseAmp)
		case 'h':
			frame = squareFrame(law, hissAmp)
		case 'w':
			frame = squareFrame(law, weakAmp)
		case 'c':
			// A click: one slot of speech-level audio.
			frame = squareFrame(law, speechAmp)
		}
		since := time.Duration(i+1) * uplinkSlot
		if v := l.feed(since, frame); v != listenPending {
			return since, v, l
		}
	}
	return 0, listenPending, l
}

func TestTheListenerDecidesWhereTheCalleesGreetingEnds(t *testing.T) {
	t.Parallel()
	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		for _, tc := range []struct {
			name      string
			line      string
			wantAt    time.Duration
			want      listenVerdict
			wantStart time.Duration // negative: none
			wantEnd   time.Duration
		}{
			{
				name:      "a greeting of 400 ms in the first second ends 300 ms after its last voiced slot",
				line:      scenario(slots('n', 800), slots('v', 400), slots('n', 2000)),
				wantAt:    1500 * time.Millisecond,
				want:      listenGreeting,
				wantStart: 800 * time.Millisecond, wantEnd: 1200 * time.Millisecond,
			},
			{
				name:      "line noise alone waits for the timeout",
				line:      slots('n', 3000),
				wantAt:    callerFirstWait,
				want:      listenTimeout,
				wantStart: -1, wantEnd: -1,
			},
			{
				name:      "no packet at all is silence: the timeout",
				line:      slots('.', 3000),
				wantAt:    callerFirstWait,
				want:      listenTimeout,
				wantStart: -1, wantEnd: -1,
			},
			{
				name:      "two clicks are not speech",
				line:      scenario(slots('n', 500), "c", slots('n', 500), "c", slots('n', 2000)),
				wantAt:    callerFirstWait,
				want:      listenTimeout,
				wantStart: -1, wantEnd: -1,
			},
			{
				name:      "speech that has not begun at the timeout is not waited for",
				line:      scenario(slots('n', 2600), slots('v', 400), slots('n', 1000)),
				wantAt:    callerFirstWait,
				want:      listenTimeout,
				wantStart: -1, wantEnd: -1,
			},
			{
				name:      "a callee still speaking at the timeout is waited for",
				line:      scenario(slots('n', 1800), slots('v', 800), slots('n', 1000)),
				wantAt:    2900 * time.Millisecond,
				want:      listenGreeting,
				wantStart: 1800 * time.Millisecond, wantEnd: 2600 * time.Millisecond,
			},
			{
				name:      "a callee who never stops is waited for until the hard cap",
				line:      scenario(slots('n', 1000), slots('v', 5000)),
				wantAt:    callerFirstHardCap,
				want:      listenHardCap,
				wantStart: 1000 * time.Millisecond, wantEnd: callerFirstHardCap,
			},
			{
				name:      "a gap in the packets inside the greeting is a pause, not its end, under 300 ms",
				line:      scenario(slots('v', 300), slots('.', 280), slots('v', 300), slots('n', 1000)),
				wantAt:    1180 * time.Millisecond,
				want:      listenGreeting,
				wantStart: 0, wantEnd: 880 * time.Millisecond,
			},
			{
				name:      "a pause of 300 ms inside the greeting ends it",
				line:      scenario(slots('v', 300), slots('.', 300), slots('v', 300), slots('n', 1000)),
				wantAt:    600 * time.Millisecond,
				want:      listenGreeting,
				wantStart: 0, wantEnd: 300 * time.Millisecond,
			},
		} {
			t.Run(law.String()+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				at, got, l := run(law, tc.line)
				if got != tc.want || at != tc.wantAt {
					t.Fatalf("decided %v at %v, want %v at %v", got, at, tc.want, tc.wantAt)
				}
				if l.speechStart != tc.wantStart || l.speechEnd != tc.wantEnd {
					t.Errorf("speech from %v to %v, want %v to %v", l.speechStart, l.speechEnd, tc.wantStart, tc.wantEnd)
				}
			})
		}
	}
}

// global.callerFirstWaitMs moves the silent-callee wait and nothing else
// (#603): a silent callee gets TIMEOUT at the flow's wait, a greeting that
// began before it is still released at its own end, a callee who starts one
// slot too late is not waited for, the 4000 ms hard cap stands at every wait,
// and the default gives verdicts and slots identical to before on every line.
func TestTheSilentCalleeWaitIsTheFlows(t *testing.T) {
	t.Parallel()
	runAt := func(wait time.Duration, line string) (time.Duration, listenVerdict) {
		l := newGreetingListener(media.LawMu, wait)
		for i := 0; i < len(line); i++ {
			var frame []byte
			switch line[i] {
			case 'v':
				frame = squareFrame(media.LawMu, speechAmp)
			case 'n':
				frame = squareFrame(media.LawMu, noiseAmp)
			}
			since := time.Duration(i+1) * uplinkSlot
			if v := l.feed(since, frame); v != listenPending {
				return since, v
			}
		}
		return 0, listenPending
	}
	for _, tc := range []struct {
		name   string
		wait   time.Duration
		line   string
		wantAt time.Duration
		want   listenVerdict
	}{
		{"a silent callee, wait 500", 500 * time.Millisecond, slots('n', 2000), 500 * time.Millisecond, listenTimeout},
		{"a silent callee, wait 1200", 1200 * time.Millisecond, slots('n', 2000), 1200 * time.Millisecond, listenTimeout},
		{"a silent callee, wait 2000", 2000 * time.Millisecond, slots('n', 3000), callerFirstWait, listenTimeout},
		{"a silent callee, wait 4000", 4000 * time.Millisecond, slots('n', 4500), 4000 * time.Millisecond, listenTimeout},
		{"a greeting that crosses the wait ends at its own end",
			1200 * time.Millisecond,
			scenario(slots('n', 800), slots('v', 600), slots('n', 1000)),
			1700 * time.Millisecond, listenGreeting},
		{"a callee who begins one slot after the wait is not waited for",
			1200 * time.Millisecond,
			scenario(slots('n', 1220), slots('v', 400), slots('n', 500)),
			1200 * time.Millisecond, listenTimeout},
		{"a callee still speaking at the cap is capped at 4000, wait 500",
			500 * time.Millisecond,
			scenario(slots('n', 300), slots('v', 5000)),
			callerFirstHardCap, listenHardCap},
		{"a callee still speaking at the cap, wait 4000",
			4000 * time.Millisecond,
			scenario(slots('n', 500), slots('v', 5000)),
			callerFirstHardCap, listenHardCap},
		{"no packets until 1500 with a wait of 1200",
			1200 * time.Millisecond,
			scenario(slots('.', 1500), slots('n', 500)),
			1200 * time.Millisecond, listenTimeout},
		{"the noisy line at a 1000 ms wait times out, as at 2000",
			1000 * time.Millisecond, slots('n', 3000),
			1000 * time.Millisecond, listenTimeout},
		{"an absent field waits as before: 2000, not earlier",
			0, slots('n', 3000), callerFirstWait, listenTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at, got := runAt(tc.wait, tc.line)
			if got != tc.want || at != tc.wantAt {
				t.Fatalf("decided %v at %v, want %v at %v", got, at, tc.want, tc.wantAt)
			}
		})
	}
}

func TestTheListenerLearnsTheNoiseFloorAboveTheAbsoluteOne(t *testing.T) {
	t.Parallel()
	// A hiss at -41 dBFS is under the absolute floor and teaches the listener
	// the line's noise. A sound at -35 dBFS is over the absolute floor but not
	// 9 dB over the hiss: not speech. Speech at -20 dBFS is.
	hiss := func(n int) string { return strings.Repeat("h", n) }
	weak := strings.Repeat("w", 1000/frameDurationMs)
	at, got, l := run(media.LawMu, scenario(hiss(10), weak, hiss(100)))
	if got != listenTimeout || l.speechStart != -1 {
		t.Errorf("a weak sound over a hiss decided %v at %v (speech from %v), want the timeout", got, at, l.speechStart)
	}
	at, got, _ = run(media.LawMu, scenario(hiss(10), slots('v', 400), hiss(100)))
	if want := time.Duration(10+400/frameDurationMs+listenEndSlots) * uplinkSlot; got != listenGreeting || at != want {
		t.Errorf("speech over a hiss decided %v at %v, want the greeting at %v", got, at, want)
	}
}

//
// The session around the listener.
//

// gatedModel is a fakeModel that honours the opening gate as a real client
// does: Start blocks on it.
type gatedModel struct {
	*fakeModel
	entered chan struct{}
	// startErr, when set, is what Start returns once it has been entered.
	startErr error
	// startedAt is the number of audio frames the model had been sent when
	// Start returned.
	audioAtStart atomic.Int64
}

func newGatedModel() *gatedModel {
	return &gatedModel{fakeModel: newFakeModel(), entered: make(chan struct{})}
}

func (m *gatedModel) Start(ctx context.Context, cfg provider.SessionConfig) error {
	_ = m.fakeModel.Start(ctx, cfg)
	close(m.entered)
	if m.startErr != nil {
		return m.startErr
	}
	err := provider.AwaitOpeningGate(ctx, cfg.OpeningGate, nil)
	m.audioAtStart.Store(int64(len(m.receivedAudio())))
	return err
}

// gate is the opening gate the session handed the model.
func (m *gatedModel) gate() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCfg.OpeningGate
}

func (m *gatedModel) isGateOpen() bool {
	g := m.gate()
	if g == nil {
		return false
	}
	select {
	case <-g:
		return true
	default:
		return false
	}
}

// listenRig runs a Session that listens first on a clock the test owns.
type listenRig struct {
	t       *testing.T
	session *Session
	model   *gatedModel
	leg     *fakeLeg
	epoch   time.Time
	ticks   chan time.Time
	offset  atomic.Int64
	// reads counts the pump's reads of the clock: one at its start and one per
	// tick it has taken.
	reads   atomic.Int64
	at      time.Duration
	logs    *lockedBuffer
	started chan error
	cancel  context.CancelFunc
}

func newListenRig(t *testing.T, cfg Config, model *gatedModel, leg *fakeLeg) *listenRig {
	t.Helper()
	r := &listenRig{
		t: t, model: model, leg: leg, epoch: time.Unix(1_000_000, 0),
		ticks: make(chan time.Time), logs: &lockedBuffer{}, started: make(chan error, 1),
	}
	leg.answeredAt = r.epoch
	cfg.BargeGuard, cfg.NoInput = -1, -1
	cfg.IsCallerFirst = true
	cfg.Logger = slog.New(slog.NewJSONHandler(r.logs, nil))
	session, err := New(leg, model, provider.OpenAIProfile(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	clockRead := make(chan struct{})
	var once sync.Once
	session.uplinkNow = func() time.Time {
		// The offset is read before the test is told of the read (the first
		// one, or one of a tick): a test that moves the offset on at once must
		// not be what this read sees.
		at := r.epoch.Add(time.Duration(r.offset.Load()))
		r.reads.Add(1)
		once.Do(func() { close(clockRead) })
		return at
	}
	session.uplinkTicks = r.ticks
	r.session = session
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(func() { cancel(); session.Close(context.Background()) })
	go func() { r.started <- session.Start(ctx) }()
	select {
	case <-clockRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the uplink pump never started its clock")
	}
	select {
	case <-model.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was never started")
	}
	return r
}

func (r *listenRig) wake() {
	r.t.Helper()
	select {
	case r.ticks <- r.epoch.Add(r.at):
	case <-time.After(10 * time.Second):
		r.t.Fatal("the uplink pump did not take its tick")
	}
}

// slot advances one slot with the given frame (nil: no packet) and waits for
// the pump to have judged it.
func (r *listenRig) slot(frame []byte) {
	r.t.Helper()
	if frame != nil {
		r.leg.frames <- frame
	}
	r.at += uplinkSlot
	r.offset.Store(int64(r.at))
	// The pump takes a tick only when it has finished the one before, so by
	// the second it has judged every slot the first owed. The second tick has
	// nothing owed, but it reads the clock after it is taken: wait for that
	// read, so the next slot's time is not what it reads.
	base := r.reads.Load()
	r.wake()
	r.wake()
	for deadline := time.Now().Add(10 * time.Second); r.reads.Load() < base+2; {
		if time.Now().After(deadline) {
			r.t.Fatal("the pump did not read the clock for its ticks")
		}
		runtime.Gosched()
	}
}

// play runs a scenario (see scenario) one slot per character.
func (r *listenRig) play(line string) {
	r.t.Helper()
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case 'v':
			r.slot(squareFrame(media.LawMu, speechAmp))
		case 'n':
			r.slot(squareFrame(media.LawMu, noiseAmp))
		default:
			r.slot(nil)
		}
	}
}

func (r *listenRig) awaitStart() {
	r.t.Helper()
	select {
	case err := <-r.started:
		if err != nil {
			r.t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		r.t.Fatal("Start did not return")
	}
}

func (r *listenRig) isStartReturned() bool {
	select {
	case err := <-r.started:
		r.started <- err
		return true
	default:
		return false
	}
}

// logRecords is every JSON log line written so far.
func logRecords(t *testing.T, logs *lockedBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

func (r *listenRig) endedWith() string {
	for _, rec := range logRecords(r.t, r.logs) {
		if rec["msg"] == "caller-first wait ended" {
			reason, _ := rec["reason"].(string)
			return reason
		}
	}
	return ""
}

func (r *listenRig) isReleased() bool {
	select {
	case <-r.session.listenEnded:
		return true
	default:
		return false
	}
}

// graceLine is the "caller-first grace ended" log record, nil before it.
func (r *listenRig) graceLine() map[string]any {
	for _, rec := range logRecords(r.t, r.logs) {
		if rec["msg"] == "caller-first grace ended" {
			return rec
		}
	}
	return nil
}

// opening plays a model's opening turn into the session: a response of n
// frames of audio with its words, ended.
func (r *listenRig) opening(n int) {
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for i := 0; i < n; i++ {
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: callerFrame(media.LawMu, byte(i+1))}
	}
	r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: "Hello, this is Ann.", IsFinal: true}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// The opening is requested as soon as the provider is ready: the wait to open
// no longer holds the request, only the downlink.
func TestTheOpeningIsRequestedBeforeTheGreetingEnds(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	r := newListenRig(t, Config{}, newGatedModel(), leg)
	r.awaitStart() // before a single slot has been played
	if r.model.gate() != nil {
		t.Error("the opening request waits on a gate that nothing but the media hold may close")
	}
	r.play(scenario(slots('n', 800), slots('v', 400)))
	if r.isReleased() {
		t.Fatal("the opening was released while the callee was speaking")
	}
	if got := len(r.model.receivedAudio()); got != 0 {
		t.Fatalf("the model was sent %d frames before the opening was released, want none", got)
	}
	if got := r.model.audioAtStart.Load(); got != 0 {
		t.Fatalf("the model had been sent %d frames when it was started", got)
	}
}

// Everything the opening sends toward the leg is held until the wait ends, and
// then flows in order: the frames, then the turn's end and its playback marker.
func TestTheOpeningIsHeldUntilTheGreetingHasEndedAndThenFlowsInOrder(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	r := newListenRig(t, Config{}, newGatedModel(), leg)
	r.awaitStart()
	r.play(scenario(slots('n', 800), slots('v', 400)))
	r.opening(3)
	// Everything the opening sent has reached the session's pump by now: a
	// later event is handled after the ones before it.
	r.play(slots('n', 280))
	time.Sleep(50 * time.Millisecond)
	if got := len(leg.sentFrames()); got != 0 {
		t.Fatalf("%d frames of the opening reached the leg while the callee had the floor, want none", got)
	}
	r.session.mu.Lock()
	queued, responding, turn := r.session.framesQueued, r.session.isResponding, r.session.turnSeq
	r.session.mu.Unlock()
	if queued != 0 || responding || turn != 0 {
		t.Fatalf("the opening's bookkeeping started while held: queued %d responding %v turn %d", queued, responding, turn)
	}

	r.play("n") // 300 ms of quiet after the greeting: the wait ends
	waitFor(t, "the opening to reach the leg", func() bool { return len(leg.sentFrames()) == 3 })
	for i, frame := range leg.sentFrames() {
		if frame[0] != byte(i+1) {
			t.Errorf("frame %d out of order", i)
		}
	}
	events := eventsUntil(t, r.session, EventTypePlaybackDone, nil)
	if got := len(leg.sentFrames()); got != 3 {
		t.Errorf("PLAYBACK_DONE came with %d frames played, want 3", got)
	}
	var sawTurnDone bool
	for _, e := range events {
		if e.Type == EventTypeTurnDone && e.HasAudio {
			sawTurnDone = true
		}
	}
	if !sawTurnDone {
		t.Errorf("the opening's TURN_DONE was lost (events %v)", typesOf(events))
	}
	held := false
	for _, rec := range logRecords(t, r.logs) {
		if rec["msg"] == "the opening was held for the callee" && rec["heldEvents"] == float64(6) {
			held = true
		}
	}
	if !held {
		t.Error("the log does not say how long the opening was held")
	}
	if got := len(r.model.receivedAudio()); got != 0 {
		t.Errorf("the model was sent %d frames while the call listened", got)
	}
}

// After the wait ends the uplink keeps listening for a grace, then sends from
// the next slot with no catch-up.
func TestTheUplinkListensOnForAGraceAfterTheWaitEnds(t *testing.T) {
	setup := func(t *testing.T) *listenRig {
		leg := newFakeLeg(media.LawMu)
		r := newListenRig(t, Config{}, newGatedModel(), leg)
		r.awaitStart()
		r.play(scenario(slots('n', 800), slots('v', 400), slots('n', 300)))
		waitFor(t, "the wait to end", r.isReleased)
		if r.endedWith() != "GREETING" {
			t.Fatalf("the wait did not end with the greeting (%q)", r.endedWith())
		}
		return r
	}
	t.Run("silence: 800 ms", func(t *testing.T) {
		r := setup(t)
		r.play(slots('n', 780))
		if r.graceLine() != nil {
			t.Fatal("the grace ended before 800 ms")
		}
		r.play("n")
		waitFor(t, "the grace to end", func() bool { return r.graceLine() != nil })
		line := r.graceLine()
		if line == nil || line["graceMs"] != float64(800) || line["isExtended"] != false {
			t.Fatalf("grace line = %v, want 800 ms, not extended", line)
		}
		if got := len(r.model.receivedAudio()); got != 0 {
			t.Fatalf("the model was sent %d frames during the grace", got)
		}
		r.slot(squareFrame(media.LawMu, speechAmp))
		r.slot(nil)
		heard := r.model.receivedAudio()
		if len(heard) != 2 || isAllSilence(heard[0], media.SilenceFrame(media.LawMu)) {
			t.Fatalf("after the grace the model got %d frames, want the caller's frame then a fill", len(heard))
		}
		r.session.Close(context.Background())
		r.session.Wait()
		line2 := uplinkLine(t, r.logs)
		if line2["framesSent"] != float64(2) {
			t.Errorf("framesSent = %v, want 2", line2["framesSent"])
		}
	})
	t.Run("voice extends it while it is heard", func(t *testing.T) {
		r := setup(t)
		r.play(scenario(slots('n', 600), slots('v', 200))) // 800 ms in, still speaking
		if r.graceLine() != nil {
			t.Fatal("the grace ended while the callee was speaking")
		}
		r.play(slots('n', 100)) // 100 ms of quiet: 5 slots
		waitFor(t, "the grace to end", func() bool { return r.graceLine() != nil })
		line := r.graceLine()
		if line == nil || line["isExtended"] != true || line["isCapped"] != false {
			t.Fatalf("grace line = %v, want extended and not capped", line)
		}
		if got := len(r.model.receivedAudio()); got != 0 {
			t.Fatalf("the model was sent %d frames during the grace", got)
		}
	})
	t.Run("the cap: 1500 ms", func(t *testing.T) {
		r := setup(t)
		r.play(slots('v', 1480))
		if r.graceLine() != nil {
			t.Fatal("the grace ended before its cap")
		}
		r.play("v")
		waitFor(t, "the grace to end", func() bool { return r.graceLine() != nil })
		line := r.graceLine()
		if line == nil || line["graceMs"] != float64(1500) || line["isCapped"] != true {
			t.Fatalf("grace line = %v, want 1500 ms capped", line)
		}
		if got := len(r.model.receivedAudio()); got != 0 {
			t.Fatalf("the model was sent %d frames during the grace", got)
		}
	})
}

func TestACallThatListensFirstReleasesTheOpeningAtTheTimeoutWhenNothingIsSaid(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	r := newListenRig(t, Config{}, newGatedModel(), leg)
	r.awaitStart()
	r.opening(2)
	r.play(slots('.', 1980))
	if r.isReleased() {
		t.Fatal("the opening was released before the wait was over")
	}
	r.play(".")
	waitFor(t, "the opening to be released", func() bool { return len(leg.sentFrames()) == 2 })
	if got := r.endedWith(); got != "TIMEOUT" {
		t.Errorf("the wait ended with %q, want TIMEOUT", got)
	}
	if got := len(r.model.receivedAudio()); got != 0 {
		t.Errorf("the model was sent %d frames while the call listened", got)
	}
}

// The media hold is the one thing that still holds the request.
func TestTheMediaHoldStillHoldsTheOpeningRequest(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	leg.holdMedia()
	r := newListenRig(t, Config{IsGreetingGated: true}, newGatedModel(), leg)
	r.play(slots('.', 2000))
	if r.isStartReturned() {
		t.Fatal("Start returned with no media")
	}
	if !r.isReleased() {
		t.Fatal("the wait to open did not end on its own")
	}
	leg.startMedia()
	r.awaitStart()
	if !r.model.isGateOpen() {
		t.Fatal("the gate stayed closed after the media started")
	}
}

func TestTheWaitEndsWhenTheCallEndsFirst(t *testing.T) {
	t.Run("the leg stops", func(t *testing.T) {
		leg := newFakeLeg(media.LawMu)
		r := newListenRig(t, Config{}, newGatedModel(), leg)
		r.play(slots('n', 200))
		leg.Stop()
		waitFor(t, "the wait to end", r.isReleased)
		if got := r.endedWith(); got != "LEG_STOPPED" {
			t.Errorf("the wait ended with %q, want LEG_STOPPED", got)
		}
	})
	t.Run("the context is cancelled", func(t *testing.T) {
		leg := newFakeLeg(media.LawMu)
		r := newListenRig(t, Config{}, newGatedModel(), leg)
		r.cancel()
		waitFor(t, "the wait to end", r.isReleased)
	})
	t.Run("the session closes", func(t *testing.T) {
		leg := newFakeLeg(media.LawMu)
		r := newListenRig(t, Config{}, newGatedModel(), leg)
		r.session.Close(context.Background())
		waitFor(t, "the wait to end", r.isReleased)
	})
}

// A call that ends while it holds an opening leaves nothing running and the
// held opening is dropped, not played into a dead line.
func TestAnOpeningHeldWhenTheCallEndsLeavesNothingRunning(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	r := newListenRig(t, Config{}, newGatedModel(), leg)
	r.awaitStart()
	r.opening(5)
	time.Sleep(50 * time.Millisecond)
	r.session.Close(context.Background())
	done := make(chan struct{})
	go func() { r.session.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a goroutine of the session outlived the call")
	}
}

func TestAModelThatCannotStartLeavesNothingReadingTheLeg(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := newGatedModel()
	model.startErr = errors.New("dial refused")
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		IsCallerFirst: true, Logger: slog.New(slog.NewJSONHandler(&lockedBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })

	if err := session.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with a model that cannot start")
	}
	done := make(chan struct{})
	go func() { session.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a goroutine of the session outlived the failed Start")
	}
	// The rescue path owns the leg now: nothing reads its frames.
	leg.frames <- callerFrame(media.LawMu, 7)
	time.Sleep(100 * time.Millisecond)
	if len(leg.frames) != 1 {
		t.Error("the leg's frames were read after Start failed")
	}
	select {
	case <-leg.Stopped():
		t.Error("a failed Start stopped the leg; the rescue transfer goes first")
	default:
	}
}

func TestOnlyACallPlacedToACustomerListensFirst(t *testing.T) {
	spec := &flow.Spec{}
	spec.Global.IsCallerFirst = true
	for _, tc := range []struct {
		name  string
		spec  *flow.Spec
		facts *callFacts
		want  bool
	}{
		{"placed to a customer", spec, &callFacts{callType: callTypeOutbound, isPlacedToCustomer: true}, true},
		{"inbound", spec, &callFacts{callType: callTypeInbound}, false},
		{"internal", spec, &callFacts{callType: callTypeInternal}, false},
		{"an agent's call to the bot's DID", spec, &callFacts{callType: callTypeOutbound}, false},
		{"the flow does not ask", &flow.Spec{}, &callFacts{callType: callTypeOutbound, isPlacedToCustomer: true}, false},
	} {
		if got := listensFirst(tc.spec, tc.facts); got != tc.want {
			t.Errorf("%s: listensFirst = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTheFlowsWaitReachesTheSessionOnAPlacedToCustomerCall(t *testing.T) {
	wait := 1200
	spec := &flow.Spec{}
	spec.Global.IsCallerFirst = true
	spec.Global.CallerFirstWaitMs = &wait
	for _, tc := range []struct {
		name        string
		spec        *flow.Spec
		callerFirst bool
		want        time.Duration
	}{
		{"a placed-to-customer call gets the flow's wait", spec, true, 1200 * time.Millisecond},
		{"a call that does not listen first gets none", spec, false, 0},
		{"a flow without the field gets the default", &flow.Spec{Global: flow.Global{IsCallerFirst: true}}, true, 0},
	} {
		if got := callerFirstWaitOf(tc.spec, tc.callerFirst); got != tc.want {
			t.Errorf("%s: callerFirstWaitOf = %v, want %v", tc.name, got, tc.want)
		}
	}
	// And the session keeps it for the listener its Start builds.
	session, err := New(newFakeLeg(media.LawMu), nopModel{}, provider.OpenAIProfile(), Config{
		IsCallerFirst:   true,
		CallerFirstWait: callerFirstWaitOf(spec, true),
		Logger:          slog.New(slog.NewJSONHandler(&lockedBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.cfg.CallerFirstWait != 1200*time.Millisecond {
		t.Fatalf("session's CallerFirstWait = %v, want 1200ms", session.cfg.CallerFirstWait)
	}
}

func TestACallThatDoesNotListenFirstBehavesAsBefore(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := newGatedModel()
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		Logger: slog.New(slog.NewJSONHandler(&lockedBuffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.gate() != nil {
		t.Error("a call with no gate and no listening was handed an opening gate")
	}
	if session.isListening.Load() {
		t.Error("an inbound call was left listening")
	}
}

//
// Benchmarks: the per-slot paths stay at 0 allocs/op.
//

func BenchmarkGreetingListener(b *testing.B) {
	law := media.LawMu
	speech, noise := squareFrame(law, speechAmp), squareFrame(law, noiseAmp)
	l := newGreetingListener(law, callerFirstWait)
	since := time.Duration(0)
	b.ReportAllocs()
	var i int
	for b.Loop() {
		since += uplinkSlot
		frame := noise
		if i%50 < 10 {
			frame = speech
		}
		if i%50 == 25 {
			frame = nil
		}
		i++
		if l.feed(since, frame) != listenPending {
			*l = *newGreetingListener(law, callerFirstWait)
			since = 0
		}
	}
}

func BenchmarkUplinkClockListening(b *testing.B) {
	newListening := func(b *testing.B) (*Session, *uplinkClock, time.Time) {
		session, err := New(newFakeLeg(media.LawMu), nopModel{}, provider.OpenAIProfile(), Config{IsCallerFirst: true})
		if err != nil {
			b.Fatal(err)
		}
		session.isListening.Store(true)
		session.listener = newGreetingListener(media.LawMu, callerFirstWait)
		epoch := time.Unix(1_000_000, 0)
		// The answer is far in the future, so the listener never decides.
		session.listenAnsweredAt = epoch.Add(24 * time.Hour)
		return session, newUplinkClock(epoch), epoch
	}
	// A real frame through the listener, without the pool's own boxing that
	// every consumer of the pool shares.
	b.Run("real frame", func(b *testing.B) {
		session, clock, _ := newListening(b)
		frame := squareFrame(media.LawMu, speechAmp)
		b.ReportAllocs()
		for b.Loop() {
			// The same slot every time: the clock is not allowed to run to
			// the end of the wait.
			clock.slotsDone = 1
			session.listenToSlot(clock, 1, frame)
		}
	})
	// A real frame in the grace after the wait: judged for voice, never ending.
	b.Run("grace", func(b *testing.B) {
		session, clock, _ := newListening(b)
		session.isListenerDone = true
		session.listenAnsweredAt = clock.start
		frame := squareFrame(media.LawMu, speechAmp)
		b.ReportAllocs()
		for b.Loop() {
			clock.slotsDone = 1
			session.listenToSlot(clock, 1, frame)
		}
	})
	// A slot with no packet, through the pump's own slot loop.
	b.Run("slot with no packet", func(b *testing.B) {
		session, clock, epoch := newListening(b)
		converted := make([]byte, 0, media.FrameSamples*8)
		now := epoch.Add(uplinkSlot)
		b.ReportAllocs()
		for b.Loop() {
			clock.slotsDone = 0
			converted, _ = session.sendDueSlots(clock, nil, now, converted)
		}
	})
}
