// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The tool-talk gate (#623): a qwen callee heard the bot say 工具调用 aloud. A
// turn whose words are only a tool's name or those words is dropped unheard on
// an engine whose transcript leads its audio, and counted everywhere.

var talkTools = []string{"lookup_slot", "hangup", "transfer_to_agent"}

//
// The classifier.
//

func newTalkWords(tools ...string) *toolTalkWords {
	g := newToolTalkGate(false, media.PCM16Format(media.RateProviderOut))
	var specs []provider.ToolSpec
	for _, name := range tools {
		specs = append(specs, provider.ToolSpec{Name: name})
	}
	g.setTools(specs)
	w := g.words
	w.reset()
	return &w
}

func TestTheToolTalkClassifierJudgesWholeTokensAsTheyCome(t *testing.T) {
	type verdict int
	const (
		isTalk  verdict = iota // whole tokens and nothing else
		couldBe                // whole tokens and the start of one, or nothing yet
		hasDiverged
	)
	tests := []struct {
		name   string
		pieces []string
		want   verdict
	}{
		{"a tool name", []string{"lookup_slot"}, isTalk},
		{"a tool name, cased and stopped", []string{"Lookup_Slot."}, isTalk},
		{"a tool name said with spaces", []string{"lookup slot"}, isTalk},
		{"a tool name full-width", []string{"ｈａｎｇｕｐ"}, isTalk},
		{"the literal", []string{"工具调用"}, isTalk},
		{"the literal with punctuation", []string{"「工具调用。」"}, isTalk},
		{"the literal in two deltas", []string{"工具", "调用"}, isTalk},
		{"the connective and a name", []string{"调用", " hangup"}, isTalk},
		{"a name split mid-word", []string{"look", "up_s", "lot"}, isTalk},
		{"tool call", []string{"Tool call."}, isTalk},
		{"tool-call, one word", []string{"TOOL-CALL"}, isTalk},
		{"tokens repeated", []string{"hangup hangup"}, isTalk},
		{"the connective alone", []string{"调用"}, isTalk},
		{"tokens in a row", []string{"工具调用：", "transfer_to_agent"}, isTalk},
		{"nothing yet", nil, couldBe},
		{"punctuation only", []string{"。…"}, couldBe},
		{"half the literal", []string{"工具"}, couldBe},
		{"the start of a name", []string{"transfer"}, couldBe},
		{"the start of tool call", []string{"tool"}, couldBe},
		{"a call, then half the literal", []string{"调用 lookup_slot 工具"}, couldBe},
		{"the old direction's tail and the line", []string{"工具调用的规则：", "您好，这里是诺瓦网。"}, hasDiverged},
		{"an ordinary zh turn", []string{"您好"}, hasDiverged},
		{"an ordinary en turn", []string{"Hang on, ", "let me check."}, hasDiverged},
		{"a name inside a longer word", []string{"hangups"}, hasDiverged},
		{"a word that starts like the connective", []string{"调整一下"}, hasDiverged},
		{"a word that starts like a name", []string{"transferred"}, hasDiverged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newTalkWords(talkTools...)
			for _, piece := range tt.pieces {
				w.add(piece)
			}
			var got verdict
			switch {
			case w.isToolTalk():
				got = isTalk
			case w.isDiverged:
				got = hasDiverged
			default:
				got = couldBe
			}
			if got != tt.want {
				t.Errorf("%q: verdict %d, want %d (norm %q)", tt.pieces, got, tt.want, w.norm)
			}
		})
	}
}

// Once the words diverge they stay diverged, whatever follows.
func TestDivergedWordsNeverBecomeToolTalkAgain(t *testing.T) {
	w := newTalkWords(talkTools...)
	w.add("好的")
	w.add("工具调用")
	if w.isToolTalk() || !w.isDiverged {
		t.Errorf("isToolTalk %v, isDiverged %v, want diverged for good", w.isToolTalk(), w.isDiverged)
	}
	w.reset()
	w.add("工具调用")
	if !w.isToolTalk() {
		t.Error("the next turn is not judged afresh")
	}
}

func TestContainedToolTalkIsFoundAsWholeWords(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"工具调用的规则：您好，这里是诺瓦网。", true},
		{"所以说这句话不违反关于工具调用的规则：您好", true},
		{"I'll use lookup_slot for that.", true},
		{"好的，调用 hangup。再见", true},
		{"LOOKUP_SLOT is what I would call.", true},
		{"so saying it breaks no rule about tool calls: hello", true},
		{"a tool-call happened", true},
		{"Hangups are rare here.", false},
		{"Please don't hang up.", false},
		{"Let me call you back with the tool.", false},
		{"a toolcaller", false},
		{"请稍等，我帮您查询一下。", false},
		{"调用资料需要一点时间。", false},
	}
	g := newToolTalkGate(false, media.PCM16Format(media.RateProviderOut))
	var specs []provider.ToolSpec
	for _, name := range talkTools {
		specs = append(specs, provider.ToolSpec{Name: name})
	}
	g.setTools(specs)
	for _, tt := range tests {
		if got := g.containsToolTalk([]byte(tt.text)); got != tt.want {
			t.Errorf("containsToolTalk(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

//
// The session.
//

// toolTalkCounts is what aicc_bot_tool_talk_total holds for one provider
// label, by verdict. Each test runs its sessions under a label of its own, so
// parallel tests do not count into each other.
func toolTalkCounts(t *testing.T, providerName string) map[string]int64 {
	t.Helper()
	setTurnReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(turnReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := turnReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_bot_tool_talk_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if p, _ := dp.Attributes.Value("provider"); p.AsString() != providerName {
					continue
				}
				verdict, _ := dp.Attributes.Value("verdict")
				if dp.Value != 0 {
					out[verdict.AsString()] += dp.Value
				}
			}
		}
	}
	return out
}

// talkProfile is base under a provider label no other test uses, with the
// trait the gate holds on set as asked, and kept by the harness
// (withoutToolTalkHold).
func talkProfile(base provider.Profile, isHolding bool) provider.Profile {
	base.Name = base.Name + "-tooltalk-" + uuid.NewString()[:8]
	base.TranscriptLeadsAudio = isHolding
	toolTalkHeld.Store(base.Name, true)
	return base
}

// toolTalkHeld names the profiles (talkProfile) whose sessions keep
// TranscriptLeadsAudio in the shared harness.
var toolTalkHeld sync.Map

// withoutToolTalkHold is a profile as the tests written before the gate run
// it: without TranscriptLeadsAudio, unless the profile is one of this file's.
// Those tests script a turn's audio before its words, or with none, which
// qwen never sends (its transcript leads its audio, design 02 §5), and
// test what playing audio does: a cut over it, a greeting, a closing line.
// With the hold that audio would wait for words that never come. The gate's
// own tests, here, send what qwen sends.
func withoutToolTalkHold(profile provider.Profile) provider.Profile {
	if _, ok := toolTalkHeld.Load(profile.Name); !ok {
		profile.TranscriptLeadsAudio = false
	}
	return profile
}

// talkRig is one session over the fakes, with the call's tools, every event
// it publishes kept in order.
type talkRig struct {
	session *Session
	leg     *fakeLeg
	model   *fakeModel
	name    string
	logs    *syncBuffer

	mu     sync.Mutex
	events []Event
}

func newTalkRig(t *testing.T, profile provider.Profile, cfg Config, tools ...string) *talkRig {
	t.Helper()
	return newTalkRigWith(t, profile, cfg, nil, tools...)
}

// newTalkRigWith is newTalkRig with a hook that runs on the session before it
// starts.
func newTalkRigWith(t *testing.T, profile provider.Profile, cfg Config, setup func(*Session),
	tools ...string) *talkRig {
	t.Helper()
	r := &talkRig{leg: newFakeLeg(media.LawMu), model: newFakeModel(), name: profile.Name,
		logs: &syncBuffer{}}
	toolTalkCounts(t, r.name) // the meter provider is set before anything counts
	cfg.Session.Instructions = "answer the phone"
	cfg.Session.Turn = provider.DefaultTurnDetection()
	for _, name := range tools {
		cfg.Session.Tools = append(cfg.Session.Tools, provider.ToolSpec{Name: name})
	}
	cfg.Logger = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	session, err := New(r.leg, r.model, profile, cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	freezeToolTalkClock(session)
	if setup != nil {
		setup(session)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	r.session = session
	t.Cleanup(func() { session.Close(context.Background()) })
	go func() {
		for event := range session.Events() {
			r.mu.Lock()
			r.events = append(r.events, event)
			r.mu.Unlock()
		}
	}()
	r.await(t, "READY", func(e Event) bool { return e.Type == EventTypeReady })
	return r
}

// freezeToolTalkClock stops the gate's clock, so a hold ends at its cap only
// when a test moves the clock or holds 2 s of audio, never because a loaded
// machine took that long between two events. Set before the session's first
// event.
func freezeToolTalkClock(s *Session) {
	frozen := time.Now()
	s.talkNow = func() time.Time { return frozen }
}

// awaitTalk waits for cond as waitUntil does, with a deadline a machine as
// loaded as test-stress makes it (CONTRIBUTING.md) does not reach while the
// pumps and the collector are only slow.
func awaitTalk(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// talkConfig switches the guards off, so a test exercises the gate alone.
func talkConfig() Config { return Config{BargeGuard: -1, NoInput: -1} }

func (r *talkRig) send(events ...provider.Event) {
	for _, e := range events {
		r.model.events <- e
	}
}

// find returns the first event match accepts, if one has arrived.
func (r *talkRig) find(match func(Event) bool) (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if match(e) {
			return e, true
		}
	}
	return Event{}, false
}

func (r *talkRig) all(match func(Event) bool) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if match(e) {
			out = append(out, e)
		}
	}
	return out
}

func (r *talkRig) await(t *testing.T, what string, match func(Event) bool) Event {
	t.Helper()
	var found Event
	awaitTalk(t, what, func() bool {
		e, ok := r.find(match)
		found = e
		return ok
	})
	return found
}

func (r *talkRig) turnDone(t *testing.T, turn int) Event {
	t.Helper()
	return r.await(t, "TURN_DONE", func(e Event) bool { return e.Type == EventTypeTurnDone && e.Turn == turn })
}

func (r *talkRig) playbackDone(t *testing.T, turn int) Event {
	t.Helper()
	return r.await(t, "PLAYBACK_DONE", func(e Event) bool {
		return e.Type == EventTypePlaybackDone && e.Turn == turn
	})
}

func (r *talkRig) hasPlaybackDone(turn int) bool {
	_, ok := r.find(func(e Event) bool { return e.Type == EventTypePlaybackDone && e.Turn == turn })
	return ok
}

// talkChunk is 20 ms of the provider's 24 kHz PCM16, distinct for each seed so
// that frames played out of order or twice show.
func talkChunk(seed int) []byte {
	chunk := make([]byte, 960)
	for i := range 480 {
		binary.LittleEndian.PutUint16(chunk[2*i:], uint16(int16((seed*997+i*31)%20000-10000)))
	}
	return chunk
}

// talkTurn is one response as a Realtime provider delivers it: its words
// first (the transcript leads the audio on qwen), then the audio, its final
// transcript, and its end.
func talkTurn(deltas []string, chunks int, seed int) []provider.Event {
	events := []provider.Event{{Type: provider.EventTypeResponseStarted}}
	for _, d := range deltas {
		events = append(events, provider.Event{Type: provider.EventTypeOutputTranscript, Text: d})
	}
	for i := range chunks {
		events = append(events, provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(seed + i)})
	}
	if len(deltas) > 0 {
		events = append(events, provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: strings.Join(deltas, ""), IsFinal: true})
	}
	return append(events, provider.Event{Type: provider.EventTypeResponseDone})
}

// framesOf plays a script on a session whose engine does not hold, and
// returns what the caller heard: the reference a gated session must match.
func framesOf(t *testing.T, base provider.Profile, turns int, script []provider.Event) [][]byte {
	t.Helper()
	r := newTalkRig(t, talkProfile(base, false), talkConfig(), talkTools...)
	r.send(script...)
	r.playbackDone(t, turns)
	return r.leg.sentFrames()
}

func sameFrames(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// A turn whose words are only tool talk, however they are cut into deltas, is
// dropped on qwen: nothing reaches the leg, the queue is not flushed, its final
// is emitted muted, its TURN_DONE is an interrupted one with no audio, no
// PLAYBACK_DONE follows, and it is counted DROPPED once.
func TestATurnThatIsOnlyToolTalkIsDroppedUnheardOnQwen(t *testing.T) {
	t.Parallel()
	for _, deltas := range [][]string{
		{"lookup_slot"},
		{"工具调用"},
		{"工具", "调用"},
		{"调用", " hangup"},
		{"look", "up_s", "lot"},
	} {
		t.Run(strings.Join(deltas, "|"), func(t *testing.T) {
			t.Parallel()
			r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
			r.send(talkTurn(deltas, 5, 1)...)
			done := r.turnDone(t, 1)

			if !done.IsInterrupted || done.HasAudio {
				t.Errorf("TURN_DONE interrupted %v, audio %v; want an interrupted turn with no audio",
					done.IsInterrupted, done.HasAudio)
			}
			if n := len(r.leg.sentFrames()); n != 0 {
				t.Errorf("%d frames reached the leg, want none", n)
			}
			if n := r.leg.clears(); n != 0 {
				t.Errorf("the queue was flushed %d times, want never", n)
			}
			finals := r.all(func(e Event) bool { return e.Type == EventTypeBotSaid && e.IsFinal && e.Turn == 1 })
			if len(finals) != 1 || !r.session.isMutedTurn(1) {
				t.Errorf("finals %v, muted %v; want the one final, of a muted turn", finals, r.session.isMutedTurn(1))
			}

			// An ordinary turn after it plays, and only its own audio.
			ordinary := talkTurn([]string{"您好，", "这里是诺瓦网。"}, 4, 50)
			r.send(ordinary...)
			r.playbackDone(t, 2)
			if r.hasPlaybackDone(1) {
				t.Error("the dropped turn reached PLAYBACK_DONE")
			}
			if want := framesOf(t, provider.QwenProfile(), 1, ordinary); !sameFrames(r.leg.sentFrames(), want) {
				t.Errorf("the leg got %d frames, want the next turn's %d alone, in order",
					len(r.leg.sentFrames()), len(want))
			}
			if got := toolTalkCounts(t, r.name); got[obs.ToolTalkDropped] != 1 || len(got) != 1 {
				t.Errorf("counts = %v, want one DROPPED", got)
			}
			if !strings.Contains(r.logs.String(), "dropped a bot turn that was only tool talk") {
				t.Errorf("no WARN for the drop:\n%s", r.logs.String())
			}
		})
	}
}

// The final transcript of a held turn waits for the turn's verdict, so that a
// dropped turn is muted before anyone reads its words: an event the session
// handles after the final but before the response's end reaches the stream
// first, and the final comes with the verdict, right before TURN_DONE.
func TestAHeldTurnsFinalWaitsForItsVerdict(t *testing.T) {
	t.Parallel()
	r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
	script := talkTurn([]string{"工具调用"}, 3, 1)
	r.send(script[:len(script)-1]...) // all but the response's end; the final is last
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, Text: "喂？", IsFinal: true})
	r.await(t, "the caller's words", func(e Event) bool { return e.Type == EventTypeCustomerSaid })
	r.send(script[len(script)-1])
	r.turnDone(t, 1)

	r.mu.Lock()
	defer r.mu.Unlock()
	var order []EventType
	for _, e := range r.events {
		if e.Type == EventTypeCustomerSaid || e.Type == EventTypeTurnDone || (e.Type == EventTypeBotSaid && e.IsFinal) {
			order = append(order, e.Type)
		}
	}
	want := []EventType{EventTypeCustomerSaid, EventTypeBotSaid, EventTypeTurnDone}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("order = %v, want %v: the final went out before the verdict", order, want)
	}
}

// Tool talk among other words is played whole, in order, and counted once.
func TestContainedToolTalkIsPlayedAndCounted(t *testing.T) {
	t.Parallel()
	for _, deltas := range [][]string{
		{"工具调用的规则：", "您好，这里是诺瓦网。"},
		{"I will use lookup_slot ", "to check that."},
	} {
		t.Run(deltas[0], func(t *testing.T) {
			t.Parallel()
			r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
			script := talkTurn(deltas, 6, 7)
			r.send(script...)
			r.playbackDone(t, 1)
			if want := framesOf(t, provider.QwenProfile(), 1, script); !sameFrames(r.leg.sentFrames(), want) {
				t.Errorf("the leg got %d frames, want the %d the turn plays without the gate",
					len(r.leg.sentFrames()), len(want))
			}
			if got := toolTalkCounts(t, r.name); got[obs.ToolTalkContained] != 1 || len(got) != 1 {
				t.Errorf("counts = %v, want one CONTAINED", got)
			}
		})
	}
}

// An ordinary turn is not held: every frame plays in order, as without the
// gate, and nothing is counted. So is a turn whose audio comes with no words at
// all: held for them, and played at its end.
func TestAnOrdinaryTurnAndOneWithoutWordsPlayAsWithoutTheGate(t *testing.T) {
	t.Parallel()
	for name, script := range map[string][]provider.Event{
		"ordinary": talkTurn([]string{"您好，", "请问有什么可以帮您？"}, 8, 3),
		"no words": talkTurn(nil, 8, 3),
		"words after the audio": append(talkTurn(nil, 8, 3)[:9],
			provider.Event{Type: provider.EventTypeOutputTranscript, Text: "好的，没问题。", IsFinal: true},
			provider.Event{Type: provider.EventTypeResponseDone}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
			r.send(script...)
			if done := r.turnDone(t, 1); done.IsInterrupted || !done.HasAudio {
				t.Errorf("TURN_DONE interrupted %v, audio %v; want a whole turn with audio",
					done.IsInterrupted, done.HasAudio)
			}
			r.playbackDone(t, 1)
			if want := framesOf(t, provider.QwenProfile(), 1, script); !sameFrames(r.leg.sentFrames(), want) {
				t.Errorf("the leg got %d frames, want the %d the turn plays without the gate",
					len(r.leg.sentFrames()), len(want))
			}
			if got := toolTalkCounts(t, r.name); len(got) != 0 {
				t.Errorf("counts = %v, want none", got)
			}
		})
	}
}

// The hold is bounded: audio held past the cap is released in order before the
// turn ends, and a turn that then turns out to be tool talk is MISSED. Both
// bounds: the audio held (bytes) and the time since the first chunk held.
func TestTheHoldEndsAtItsCapAndAToolTalkTurnPastItIsMissed(t *testing.T) {
	t.Parallel()
	t.Run("audio", func(t *testing.T) {
		t.Parallel()
		r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
		// 2 s of audio is 100 chunks of 20 ms; the 101st is past the cap.
		r.send(provider.Event{Type: provider.EventTypeResponseStarted})
		for i := range 101 {
			r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(i)})
		}
		awaitTalk(t, "the held audio to be released at the cap", func() bool { return len(r.leg.sentFrames()) > 0 })
		r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具调用", IsFinal: true},
			provider.Event{Type: provider.EventTypeResponseDone})
		if done := r.turnDone(t, 1); done.IsInterrupted || !done.HasAudio {
			t.Errorf("TURN_DONE interrupted %v, audio %v; want the turn played", done.IsInterrupted, done.HasAudio)
		}
		if got := toolTalkCounts(t, r.name); got[obs.ToolTalkMissed] != 1 || len(got) != 1 {
			t.Errorf("counts = %v, want one MISSED", got)
		}
		if !strings.Contains(r.logs.String(), "released a turn's held audio at the cap") {
			t.Errorf("no line for the cap:\n%s", r.logs.String())
		}
	})
	t.Run("time", func(t *testing.T) {
		t.Parallel()
		clock := &talkClock{now: time.Now()}
		r := newTalkRigWith(t, talkProfile(provider.QwenProfile(), true), talkConfig(),
			func(s *Session) { s.talkNow = clock.read }, talkTools...)
		r.send(provider.Event{Type: provider.EventTypeResponseStarted},
			provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具"},
			provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(1)})
		awaitTalk(t, "the chunk to be held", func() bool {
			r.session.mu.Lock()
			defer r.session.mu.Unlock()
			return len(r.session.talk.held) == 1
		})
		clock.advance(toolTalkHoldCap)
		r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(2)})
		awaitTalk(t, "the held audio to be released at the cap", func() bool { return len(r.leg.sentFrames()) > 0 })
		r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: "调用"},
			provider.Event{Type: provider.EventTypeResponseDone})
		r.turnDone(t, 1)
		if got := toolTalkCounts(t, r.name); got[obs.ToolTalkMissed] != 1 || len(got) != 1 {
			t.Errorf("counts = %v, want one MISSED", got)
		}
	})
}

// talkClock is the gate's clock, moved by the test.
type talkClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *talkClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *talkClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Every other engine only detects: the same turn plays untouched, as a whole
// turn with its PLAYBACK_DONE, and is counted MISSED.
func TestOnAnEngineWhoseTranscriptDoesNotLeadToolTalkIsPlayedAndMissed(t *testing.T) {
	t.Parallel()
	script := talkTurn([]string{"lookup_slot"}, 5, 1)
	r := newTalkRig(t, talkProfile(provider.OpenAIProfile(), false), talkConfig(), talkTools...)
	r.send(script...)
	if done := r.turnDone(t, 1); done.IsInterrupted || !done.HasAudio {
		t.Errorf("TURN_DONE interrupted %v, audio %v; want the turn played", done.IsInterrupted, done.HasAudio)
	}
	r.playbackDone(t, 1)
	if want := framesOf(t, provider.OpenAIProfile(), 1, script); !sameFrames(r.leg.sentFrames(), want) {
		t.Errorf("the leg got %d frames, want the %d the turn plays", len(r.leg.sentFrames()), len(want))
	}
	if r.session.isMutedTurn(1) {
		t.Error("the turn was muted")
	}
	if got := toolTalkCounts(t, r.name); got[obs.ToolTalkMissed] != 1 || len(got) != 1 {
		t.Errorf("counts = %v, want one MISSED", got)
	}
}

// A held turn that a keypress cuts, that is muted, or that the provider
// cancels loses what was held: none of it reaches the leg, then or with the
// next turn, and it is DROPPED, as none of it was heard.
func TestAHeldTurnThatIsCutLosesWhatWasHeld(t *testing.T) {
	t.Parallel()
	cuts := map[string]func(r *talkRig){
		"keypress": func(r *talkRig) {
			r.leg.digits <- "1"
			awaitTalk(t, "the keypress to cut the turn", func() bool {
				_, ok := r.find(func(e Event) bool { return e.Type == EventTypeDigit })
				return ok
			})
			r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(90)},
				provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonDTMF})
		},
		"mute": func(r *talkRig) {
			r.session.MuteTurn(1, "test")
			r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(90)},
				provider.Event{Type: provider.EventTypeResponseDone})
		},
		"cancelled": func(r *talkRig) {
			r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech})
		},
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
			r.send(provider.Event{Type: provider.EventTypeResponseStarted},
				provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具调用"},
				provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(1)},
				provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(2)})
			awaitTalk(t, "the turn's audio to be held", func() bool {
				r.session.mu.Lock()
				defer r.session.mu.Unlock()
				return len(r.session.talk.held) == 2
			})
			cut(r)
			r.turnDone(t, 1)

			ordinary := talkTurn([]string{"好的。"}, 3, 50)
			r.send(ordinary...)
			r.playbackDone(t, 2)
			if want := framesOf(t, provider.QwenProfile(), 1, ordinary); !sameFrames(r.leg.sentFrames(), want) {
				t.Errorf("the leg got %d frames, want the next turn's %d alone", len(r.leg.sentFrames()), len(want))
			}
			if !r.session.isMutedTurn(1) {
				t.Error("the cut turn's words were left as heard")
			}
			if got := toolTalkCounts(t, r.name); got[obs.ToolTalkDropped] != 1 || len(got) != 1 {
				t.Errorf("counts = %v, want one DROPPED", got)
			}
		})
	}
}

// A drop is no flush: an earlier turn's tail still queued plays on, and the
// silence after the drop is watched once it has drained (NO_INPUT, with no
// PLAYBACK_DONE for the dropped turn).
func TestADropLeavesAnEarlierTailPlayingAndWatchesTheSilenceAfterIt(t *testing.T) {
	t.Parallel()
	cfg := talkConfig()
	cfg.NoInput = 50 * time.Millisecond
	r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), cfg, talkTools...)
	r.leg.holdFrames(5) // turn 1's tail stays queued until released below
	r.send(talkTurn([]string{"您好。"}, 4, 1)...)
	r.turnDone(t, 1)
	r.send(talkTurn([]string{"工具调用"}, 4, 20)...)
	if done := r.turnDone(t, 2); !done.IsInterrupted {
		t.Fatal("the tool talk was not dropped")
	}
	if n := r.leg.clears(); n != 0 {
		t.Errorf("the queue was flushed %d times, want never", n)
	}
	r.leg.holdFrames(0)
	r.await(t, "NO_INPUT after the drop", func(e Event) bool { return e.Type == EventTypeNoInput })
	if r.hasPlaybackDone(2) {
		t.Error("the dropped turn reached PLAYBACK_DONE")
	}
}

// A transcript carries no response id: a turn that began before the last one
// had ended, or after the provider gave the last one up, may hold that one's
// late words. Such a turn is neither held nor judged, so a late 工具调用 cannot
// drop it.
func TestATurnThatMayHoldAnotherTurnsLateWordsIsNotJudged(t *testing.T) {
	t.Parallel()
	ends := map[string][]provider.Event{
		// The next response starts before the cancelled one's end is reported.
		"overlap": {
			{Type: provider.EventTypeResponseStarted},
			{Type: provider.EventTypeResponseStarted},
			{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech},
		},
		"abandoned": {
			{Type: provider.EventTypeResponseStarted},
			{Type: provider.EventTypeResponseDone, IsAbandoned: true},
			{Type: provider.EventTypeResponseStarted},
		},
	}
	for name, before := range ends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTalkRig(t, talkProfile(provider.QwenProfile(), true), talkConfig(), talkTools...)
			r.send(before...)
			// The last turn's late words land in this one, which has audio of
			// its own and no words.
			r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具调用"},
				provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(1)})
			awaitTalk(t, "the turn's audio to play at once", func() bool { return len(r.leg.sentFrames()) > 0 })
			r.send(provider.Event{Type: provider.EventTypeResponseDone})
			r.await(t, "the turn's TURN_DONE", func(e Event) bool {
				return e.Type == EventTypeTurnDone && e.Turn == 2 && e.HasAudio
			})
			if got := toolTalkCounts(t, r.name); len(got) != 0 {
				t.Errorf("counts = %v, want the turn not judged", got)
			}
		})
	}
}

//
// Benchmarks: the gate's per-delta judgement and the hold and release of a
// turn's audio allocate nothing once their buffers have grown.
//

func BenchmarkToolTalkWords(b *testing.B) {
	w := newTalkWords(talkTools...)
	b.ReportAllocs()
	for b.Loop() {
		w.reset()
		w.add("您好，")
		w.add("这里是诺瓦网。")
		w.reset()
		w.add("工具")
		w.add("调用")
		w.add(" lookup_slot")
	}
}

// benchLeg is a leg whose queue takes every frame and keeps none.
type benchLeg struct{ *fakeLeg }

func (benchLeg) Send([]byte) bool { return true }
func (benchLeg) Pending() int     { return 0 }

func BenchmarkToolTalkHoldAndRelease(b *testing.B) {
	profile := provider.QwenProfile()
	s, err := New(benchLeg{newFakeLeg(media.LawMu)}, newFakeModel(), profile,
		Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatal(err)
	}
	s.talk.setTools([]provider.ToolSpec{{Name: "lookup_slot"}, {Name: "hangup"}})
	chunk := talkChunk(1)
	turn := func() {
		s.beginTurn(false, false)
		s.noteBotWords("调", false) // could still be tool talk: held
		for range 5 {
			s.playAudio(chunk)
		}
		s.noteBotWords("整一下。", false) // diverged: the five play, in order
		s.playAudio(chunk)
		// The turn's end without its events (emitting allocates): the next
		// turn judges this one at its start, as one replaced before it ended.
		s.mu.Lock()
		s.isResponding, s.openTurns = false, 0
		s.mu.Unlock()
	}
	turn() // the first frame is measured against the answer, once per call
	if !s.talk.hasHeld || !s.talk.isReleased || s.talk.isUnjudged {
		b.Fatal("the turn's audio was not held and released")
	}
	b.ReportAllocs()
	for b.Loop() {
		turn()
	}
}

//
// Through drive: a dropped turn is a turn cut before anything was heard.
//

// talkWallFlow closes the call after more than one tool-less reply; its note
// tool is the probe that shows drive has handled what came before it.
const talkWallFlow = `{
	"id": "tool-talk-wall",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"closingTarget": "farewell",
		"maxTurnsWithoutTool": 1
	},
	"tools": {"note": {"kind": "slot", "slot": "note", "description": "Record a note."}},
	"nodes": {
		"welcome": {"instruction": "Answer questions.", "tools": ["note"]},
		"farewell": {"instruction": "Say goodbye.", "announce": "Thank you for calling, goodbye.",
			"tools": [], "isTerminal": true}
	}
}`

// A dropped reply is no tool-less reply: after it and one reply the caller
// heard, a wall of one still stands. The control, the same reply played, has
// the wall close the call on the second reply's playback.
func TestADroppedTurnDoesNotAdvanceAWallOfOne(t *testing.T) {
	t.Parallel()
	for name, first := range map[string][]string{
		"dropped": {"工具调用"},
		"played":  {"Sure."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// PLAYBACK_DONEs published so far; a dropped turn publishes none.
			var playbacks atomic.Int32
			h := newClaimsHarnessSetup(t, talkProfile(provider.QwenProfile(), true), talkWallFlow, "en", "",
				talkConfig(), func(h *claimsHarness) {
					freezeToolTalkClock(h.session)
					h.session.afterPlaybackDone = func() { playbacks.Add(1) }
				})
			want := int32(1)
			if name == "played" {
				want = 2
			}
			h.callerSays("no")
			for _, e := range talkTurn(first, 3, 1) {
				h.model.events <- e
			}
			if name == "played" {
				// Heard before the next turn starts, so its playback is not
				// superseded.
				awaitTalk(t, "the first reply to be heard", func() bool { return playbacks.Load() == 1 })
			}
			h.callerSays("no")
			for _, e := range talkTurn([]string{"Sure, ", "anything else?"}, 3, 10) {
				h.model.events <- e
			}
			awaitTalk(t, "the second reply to be heard", func() bool { return playbacks.Load() == want })
			// The probe's call is on the stream after the second reply's
			// PLAYBACK_DONE: once it is answered, drive has handled that.
			h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
			h.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_probe",
				ToolName: "note", ToolArgs: `{"value":"probe"}`}
			h.awaitToolResults(t, 1)

			spoken := h.model.spokenLines()
			switch name {
			case "dropped":
				if len(spoken) != 0 {
					t.Errorf("spoken = %v: the dropped turn counted towards the wall", spoken)
				}
				if !strings.Contains(h.logs.String(), "left a muted turn's words out of the transcript") {
					t.Errorf("the dropped turn's words reached the transcript:\n%s", h.logs.String())
				}
			default:
				if len(spoken) != 1 || spoken[0] != "Thank you for calling, goodbye." {
					t.Errorf("spoken = %v, want the wall to close with the closing line", spoken)
				}
			}
		})
	}
}

// The model says 工具调用 aloud in the turn that calls hangup, with the closing
// line owed: nothing of that turn reaches the caller, the call does not end on
// it, and it ends after the line's turn, which is heard.
func TestAToolTalkTurnThatArmsTheHangupIsUnheardAndTheLineIsStillSaid(t *testing.T) {
	t.Parallel()
	profile := talkProfile(provider.QwenProfile(), true)
	toolTalkCounts(t, profile.Name) // the meter provider is set before anything counts
	// A cap no loaded machine runs out first: the line's playback ends the call.
	h, sink := startOwedLinePath(t, profile, "en", time.Minute)
	t.Cleanup(h.actions.disarm)
	freezeToolTalkClock(h.session)
	line := owedLines["en"]

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具调用"}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(1)}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(2)}
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: "hangup", ToolArgs: goodbyeHangup}
	awaitTalk(t, "the hangup to be answered", func() bool { return len(h.model.recordedToolResults()) == 1 })
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: talkChunk(3)}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: "工具调用", IsFinal: true}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	awaitTalk(t, "the arming turn to end", func() bool {
		h.session.mu.Lock()
		defer h.session.mu.Unlock()
		return !h.session.isResponding && h.session.talk.isSettled
	})
	if n := len(h.leg.sentFrames()); n != 0 {
		t.Fatalf("%d frames of the tool talk reached the leg", n)
	}
	if h.isCallEnded() {
		t.Fatal("the call ended on the turn that armed the hangup")
	}

	h.sayTurn(line)
	h.awaitCallEnded(t, 2*time.Second)
	if len(h.leg.sentFrames()) == 0 {
		t.Error("the line's turn did not play")
	}
	if n := strings.Count(sink.String(), "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once:\n%s", n, sink.String())
	}
	if strings.Contains(sink.String(), "at the cap") {
		t.Errorf("the call ended on the cap, not on the line:\n%s", sink.String())
	}
	if got := toolTalkCounts(t, profile.Name); got[obs.ToolTalkDropped] != 1 || len(got) != 1 {
		t.Errorf("counts = %v, want one DROPPED", got)
	}
}

// sendTurn plays a whole response on the toolPath's model.
func (h *toolPath) sendTurn(events []provider.Event) {
	for _, e := range events {
		h.model.events <- e
	}
}

// The closing line's own turn is only tool talk: dropped, it is a turn that
// missed the line (#262). The line is asked for once more as a closing line,
// nothing counts as said, no interruption is counted, and the call ends on
// the line's playback, not at the cap.
func TestADroppedClosingLineIsAskedForAgainAndTheCallEndsOnTheLine(t *testing.T) {
	t.Parallel()
	// A cap no loaded machine runs out first: the line's playback ends the call.
	h, sink := startOwedLinePath(t, talkProfile(provider.QwenProfile(), true), "en", time.Minute)
	t.Cleanup(h.actions.disarm)
	freezeToolTalkClock(h.session)
	line := owedLines["en"]
	h.callTool(t, flow.ToolHangup, goodbyeHangup)
	h.endResponse() // the turn that carried the tool call; no audio

	h.sendTurn(talkTurn([]string{"工具调用"}, 3, 1))
	awaitTalk(t, "the line to be asked for again", func() bool { return len(h.model.spokenLines()) == 1 })
	if got, closing := h.model.spokenLines(), h.model.spokenLinesClosing(); got[0] != line || !closing[0] {
		t.Errorf("asked for %q (closing %v), want the closing line, as one", got[0], closing[0])
	}
	if n := len(h.leg.sentFrames()); n != 0 {
		t.Errorf("%d frames of the tool talk reached the leg", n)
	}
	h.actions.mu.Lock()
	isSpoken, isAskedAgain, interruptions := h.actions.isLineSpoken, h.actions.isLineAskedAgain, h.actions.interruptions
	h.actions.mu.Unlock()
	if isSpoken || !isAskedAgain || interruptions != 0 {
		t.Errorf("line spoken %v, asked again %v, interruptions %d; want asked again, not spoken, none",
			isSpoken, isAskedAgain, interruptions)
	}

	// The line's own turn, heard.
	h.sendTurn(talkTurn([]string{line}, 3, 20))
	h.awaitCallEnded(t, 10*time.Second)
	if len(h.leg.sentFrames()) == 0 {
		t.Error("the line's turn did not play")
	}
	logs := sink.String()
	if !strings.Contains(logs, "was dropped as tool talk; the line is asked for once more") {
		t.Errorf("no line for the dropped turn:\n%s", logs)
	}
	if n := strings.Count(logs, "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once", n)
	}
	if strings.Contains(logs, "at the cap") {
		t.Errorf("the call ended at the cap, not on the line:\n%s", logs)
	}
}

// The line asked for again is dropped as well: nothing is asked for a third
// time, and the cap that the ask restarted ends the call, so a line dropped
// every time cannot loop.
func TestASecondDroppedClosingLineEndsAtTheCap(t *testing.T) {
	t.Parallel()
	h, sink := startOwedLinePath(t, talkProfile(provider.QwenProfile(), true), "en", time.Minute)
	t.Cleanup(h.actions.disarm)
	freezeToolTalkClock(h.session)
	h.callTool(t, flow.ToolHangup, goodbyeHangup)
	h.endResponse() // the turn that carried the tool call; no audio
	// The cap the line's ask restarts: long enough for the next turn to be
	// judged first on a loaded machine.
	h.actions.mu.Lock()
	h.actions.graceCap = 5 * time.Second
	h.actions.mu.Unlock()

	h.sendTurn(talkTurn([]string{"工具调用"}, 3, 1))
	awaitTalk(t, "the line to be asked for again", func() bool { return len(h.model.spokenLines()) == 1 })
	h.actions.mu.Lock()
	capDue := h.actions.capDue
	h.actions.mu.Unlock()

	// The line's turn, said as tool talk again.
	again := talkTurn([]string{"工具调用"}, 3, 10)
	again[0].IsSpokenLine = true
	h.sendTurn(again)
	awaitTalk(t, "the second drop to be judged", func() bool {
		return strings.Contains(sink.String(), "asked for again was dropped as tool talk too")
	})
	if got := h.model.spokenLines(); len(got) != 1 {
		t.Errorf("lines asked for = %v, want the one ask", got)
	}
	h.actions.mu.Lock()
	isSpoken, interruptions, due := h.actions.isLineSpoken, h.actions.interruptions, h.actions.capDue
	h.actions.mu.Unlock()
	if isSpoken || interruptions != 0 || !due.Equal(capDue) {
		t.Errorf("line spoken %v, interruptions %d, cap moved by %v; want none of them",
			isSpoken, interruptions, due.Sub(capDue))
	}

	h.awaitCallEnded(t, 15*time.Second)
	logs := sink.String()
	if !strings.Contains(logs, "running an armed action at the cap; no closing line was generated") {
		t.Errorf("the call did not end at the cap:\n%s", logs)
	}
	if n := strings.Count(logs, "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once", n)
	}
	if n := len(h.leg.sentFrames()); n != 0 {
		t.Errorf("%d frames of the tool talk reached the leg", n)
	}
}

// The opening said again after a greeting cut it, dropped as tool talk: the
// replay's budget and the owed opening were spent when it was asked for, so the
// opening is said once more in its place, and only once.
func TestADroppedOpeningReplayIsSaidAgainOnce(t *testing.T) {
	t.Parallel()
	clock := &holdClock{}
	wait := newVerdictWait()
	h := newClaimsHarnessModel(t, talkProfile(provider.QwenProfile(), true), quietFlow, "en", "",
		quietConfig(), nil, func(c *claimsHarness) {
			c.session.cutWait = wait.after
			c.session.holdAfter = clock.after
			c.session.cfg.OnCutAcknowledged = c.actions.onCutAcknowledged
			c.session.cfg.Session.OpeningText = openingLine
			freezeToolTalkClock(c.session)
		})

	// The opening, its words first as qwen sends them, cut by a greeting.
	h.leg.holdFrames(40)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: openingLine}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*50)}
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
	awaitTalk(t, "the cut to be recorded", func() bool {
		h.session.bc.mu.Lock()
		defer h.session.bc.mu.Unlock()
		return h.session.bc.cut != nil
	})
	h.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	h.leg.holdFrames(0)
	h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u1", Text: "hello", IsFinal: true}
	awaitTalk(t, "the replay to wait for the reply", func() bool { return clock.pending() == 1 })
	clock.advance(replayWaitCap)
	awaitTalk(t, "the opening to be asked for again", func() bool { return len(h.model.spokenLines()) == 1 })
	heard := len(h.leg.sentFrames())

	// The replay's turn says only tool talk: the opening is said once more.
	// That turn says only tool talk too: it is dropped, and not said again.
	for i := range 2 {
		h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
		for _, e := range talkTurn([]string{"工具调用"}, 3, 10*i)[1:] {
			h.model.events <- e
		}
		awaitTalk(t, "the dropped replay to be settled", func() bool {
			h.session.mu.Lock()
			defer h.session.mu.Unlock()
			return h.session.talk.isSettled && h.session.mutedTurn == 2+i
		})
	}
	if n := len(h.leg.sentFrames()); n != heard {
		t.Errorf("%d frames of the dropped replays reached the leg", n-heard)
	}
	// The model's pump handles events in order: once a later turn is settled,
	// the second drop's endResponse has returned.
	for _, e := range talkTurn([]string{"Sorry, ", "go ahead."}, 2, 50) {
		h.model.events <- e
	}
	awaitTalk(t, "a later turn to be settled", func() bool {
		h.session.mu.Lock()
		defer h.session.mu.Unlock()
		return h.session.turnSeq == 4 && h.session.talk.isSettled
	})
	if got := h.model.spokenLines(); len(got) != 2 || got[0] != openingLine || got[1] != openingLine {
		t.Errorf("spoken = %v, want the opening asked for again, then once more for the dropped replay", got)
	}
}
