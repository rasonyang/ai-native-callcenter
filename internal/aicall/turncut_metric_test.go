// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"fmt"
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

// #626 step 0: every cut an engine that cuts by itself makes over a bot line is
// counted in aicc_bot_turn_cuts_total once, by what the final transcript of the
// words that cut made of it (WORDS, BLANK, MISSING) and the line it fell on
// (OPENING, LATER, CLOSING), and the call logs its cuts in one "ai turn cuts"
// line. The tests drive real sessions over the fake model. Each gives its
// provider a name of its own, so the global meter's counts for that name are
// the test's alone (the shared reader of turn_unanswered_test.go watches it).

var turnCutProviders atomic.Int64

// turnCutProfile is base under a provider name no other test uses. It installs
// the reader first: the global meter drops what is recorded before it has one.
func turnCutProfile(base provider.Profile) provider.Profile {
	setTurnReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(turnReader)))
	})
	base.Name = fmt.Sprintf("%s-turncut-%d", base.Name, turnCutProviders.Add(1))
	return base
}

// cutKey names a count by its labels.
func cutKey(words, line string) string { return words + "/" + line }

// turnCutCounts is what aicc_bot_turn_cuts_total holds for the provider, by
// cutKey.
func turnCutCounts(t *testing.T, providerName string) map[string]int64 {
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
			if m.Name != "aicc_bot_turn_cuts_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if p, _ := dp.Attributes.Value("provider"); p.AsString() != providerName {
					continue
				}
				words, _ := dp.Attributes.Value("words")
				line, _ := dp.Attributes.Value("line")
				if dp.Value != 0 {
					out[cutKey(words.AsString(), line.AsString())] += dp.Value
				}
			}
		}
	}
	return out
}

// assertTurnCuts fails unless the provider's counts are exactly want.
func assertTurnCuts(t *testing.T, providerName string, want map[string]int64) {
	t.Helper()
	got := turnCutCounts(t, providerName)
	if len(got) != len(want) {
		t.Errorf("counts = %v, want %v", got, want)
		return
	}
	for key, n := range want {
		if got[key] != n {
			t.Errorf("counts = %v, want %v", got, want)
			return
		}
	}
}

// turnCutRig is one session on an engine, its events drained, its log kept.
type turnCutRig struct {
	session *Session
	leg     *fakeLeg
	model   *fakeModel
	logs    *syncBuffer
	wait    *verdictWait
	name    string
	// ended closes when the session's stream has closed: the call's cuts are
	// all counted and its line logged.
	ended chan struct{}
}

// startTurnCutRig starts a session on qwen, a call whose opening is opening
// ("" for none).
func startTurnCutRig(t *testing.T, opening string, configure func(*Config)) *turnCutRig {
	t.Helper()
	return startTurnCutRigOn(t, provider.QwenProfile(), opening, nil, configure)
}

// startTurnCutRigOn is startTurnCutRig on an engine of the test's choice, with
// the fake model wrapped (startBridgeModel).
func startTurnCutRigOn(t *testing.T, base provider.Profile, opening string,
	wrap func(*fakeModel) provider.VoiceSession, configure func(*Config)) *turnCutRig {
	t.Helper()
	profile := turnCutProfile(base)
	r := &turnCutRig{leg: newFakeLeg(media.LawMu), model: newFakeModel(), logs: &syncBuffer{},
		wait: newVerdictWait(), name: profile.Name, ended: make(chan struct{})}
	var voice provider.VoiceSession = r.model
	if wrap != nil {
		voice = wrap(r.model)
	}
	cfg := Config{
		BargeGuard: -1, NoInput: -1,
		Session: provider.SessionConfig{Instructions: "answer the phone",
			Turn: provider.DefaultTurnDetection(), OpeningText: opening},
		Logger: slog.New(slog.NewTextHandler(r.logs, nil)),
	}
	if configure != nil {
		configure(&cfg)
	}
	session, err := New(r.leg, voice, profile, cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	// Both clocks are the test's, and neither runs by itself.
	session.cutWait = r.wait.after
	session.holdAfter = (&holdClock{}).after
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	r.session = session
	t.Cleanup(func() { session.Close(context.Background()) })
	go func() {
		defer close(r.ended)
		for range session.Events() {
		}
	}()
	return r
}

func (r *turnCutRig) send(e provider.Event) { r.model.events <- e }

// botSpeaks starts a turn and returns it once its audio is queued, with 40
// frames of it still to play. The audio reaches an idle leg, so the caller
// starts hearing it then (the barge-in guard's clock).
func (r *turnCutRig) botSpeaks(t *testing.T) (turn int) {
	t.Helper()
	turn = r.session.currentTurn() + 1
	r.send(provider.Event{Type: provider.EventTypeResponseStarted})
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*50)})
	waitLong(t, "the turn's audio to be queued", func() bool {
		_, isSpeaking := r.session.audioProgress()
		return r.session.currentTurn() == turn && isSpeaking
	})
	r.leg.holdFrames(40)
	return turn
}

// cutBySpeech has the caller talk over the turn in the order of a provider
// whose speech start comes first: the session's barge-in cuts, and the
// provider's cancellation that follows finds nothing left to cut.
func (r *turnCutRig) cutBySpeech(t *testing.T, turn int, item string) {
	t.Helper()
	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item})
	r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech})
	r.awaitCut(t, turn, item)
}

// cutByProvider is the other order (qwen): the provider cancels the turn on
// its own and then reports the speech, which names the utterance.
func (r *turnCutRig) cutByProvider(t *testing.T, turn int, item string) {
	t.Helper()
	r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech})
	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item})
	r.awaitCut(t, turn, item)
}

// awaitCut waits until the open cut is the turn's and knows its utterance.
func (r *turnCutRig) awaitCut(t *testing.T, turn int, item string) {
	t.Helper()
	waitLong(t, "the cut to be recorded", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		cut := r.session.bc.cut
		return cut != nil && cut.event.Turn == turn && cut.itemID == item
	})
	r.leg.holdFrames(0)
}

func (r *turnCutRig) final(item, text string) {
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text, IsFinal: true})
}

// counted is how many cuts the call has counted so far.
func (r *turnCutRig) counted() int {
	r.session.bc.mu.Lock()
	defer r.session.bc.mu.Unlock()
	c := r.session.bc.cuts
	return c.words + c.blank + c.missing
}

func (r *turnCutRig) awaitCounted(t *testing.T, n int) {
	t.Helper()
	waitLong(t, fmt.Sprintf("%d cuts to be counted", n), func() bool { return r.counted() >= n })
}

// end closes the call and waits until its cuts are all counted.
func (r *turnCutRig) end(t *testing.T) {
	t.Helper()
	r.session.Close(context.Background())
	select {
	case <-r.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the session's stream did not close")
	}
}

// cutLines are the call's "ai turn cuts" log lines.
func (r *turnCutRig) cutLines() []string {
	var lines []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, `msg="ai turn cuts"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertCutLine fails unless the call logged one "ai turn cuts" line, and it
// carries every key=value in want.
func (r *turnCutRig) assertCutLine(t *testing.T, want ...string) {
	t.Helper()
	lines := r.cutLines()
	if len(lines) != 1 {
		t.Fatalf("logged %d \"ai turn cuts\" lines, want 1:\n%s", len(lines), r.logs.String())
	}
	for _, kv := range want {
		if !strings.Contains(lines[0]+" ", " "+kv+" ") {
			t.Errorf("the line %q lacks %s", lines[0], kv)
		}
	}
}

// probeCut makes one more cut, by the provider, on a turn of its own, judged
// by words, and waits until it is counted: events are handled in order, so
// everything sent before it has been handled.
func (r *turnCutRig) probeCut(t *testing.T) {
	t.Helper()
	before := r.counted()
	turn := r.botSpeaks(t)
	r.cutByProvider(t, turn, "probe")
	r.final("probe", "Sorry, could you say that again?")
	r.awaitCounted(t, before+1)
}

// A cut on a later line is counted once, by what its final said, or MISSING
// when the call ends before any final judged it. A second final for the same
// utterance does not count it again.
func TestACutIsCountedOnceByWhatItsFinalSaid(t *testing.T) {
	for _, tt := range []struct {
		name  string
		final func(r *turnCutRig)
		words string
		log   string
	}{
		{"words", func(r *turnCutRig) {
			r.final("u1", "No thanks, not today.")
			r.final("u1", "No thanks, not today.")
		}, obs.TurnCutWords, "cutsWords=1"},
		{"blank", func(r *turnCutRig) {
			r.final("u1", "")
			r.final("u1", "")
		}, obs.TurnCutWordsBlank, "cutsBlank=1"},
		{"none before the call ends", func(*turnCutRig) {}, obs.TurnCutWordsMissing, "cutsMissing=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := startTurnCutRig(t, "", nil)
			turn := r.botSpeaks(t)
			r.cutBySpeech(t, turn, "u1")
			tt.final(r)
			if tt.words != obs.TurnCutWordsMissing {
				r.awaitCounted(t, 1)
			}
			r.end(t)
			assertTurnCuts(t, r.name, map[string]int64{cutKey(tt.words, obs.TurnCutLineLater): 1})
			r.assertCutLine(t, tt.log, "cutsLater=1", "cutsOpening=0", "cutsClosing=0")
		})
	}
}

// A cut another cut replaces before any final judged it is MISSING at once;
// its utterance's late final judges nothing. A cut already counted is not
// counted again when it is replaced.
func TestACutAnotherReplacesIsCountedOnce(t *testing.T) {
	t.Run("before its final", func(t *testing.T) {
		r := startTurnCutRig(t, "", nil)
		r.cutBySpeech(t, r.botSpeaks(t), "u1")
		r.cutBySpeech(t, r.botSpeaks(t), "u2")
		r.awaitCounted(t, 1)
		assertTurnCuts(t, r.name, map[string]int64{cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineLater): 1})

		r.final("u1", "I said I am busy.") // the replaced cut's own final, late
		r.final("u2", "Please call me next week.")
		r.awaitCounted(t, 2)
		r.end(t)
		assertTurnCuts(t, r.name, map[string]int64{
			cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineLater): 1,
			cutKey(obs.TurnCutWords, obs.TurnCutLineLater):        1,
		})
		r.assertCutLine(t, "cutsWords=1", "cutsMissing=1", "cutsLater=2")
	})
	t.Run("after its final", func(t *testing.T) {
		r := startTurnCutRig(t, "", nil)
		r.cutBySpeech(t, r.botSpeaks(t), "u1")
		r.final("u1", "Please call me next week.")
		r.awaitCounted(t, 1)
		r.cutBySpeech(t, r.botSpeaks(t), "u2")
		r.end(t)
		assertTurnCuts(t, r.name, map[string]int64{
			cutKey(obs.TurnCutWords, obs.TurnCutLineLater):        1,
			cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineLater): 1,
		})
		r.assertCutLine(t, "cutsWords=1", "cutsMissing=1", "cutsLater=2")
	})
}

// A cut of the opening is OPENING, and so is a cut of any line while the
// opening is still owed after one (#610's notion of a cut over the opening):
// here the model's reply after a blank final.
func TestACutOfTheOpeningIsCountedOnTheOpening(t *testing.T) {
	r := startTurnCutRig(t, openingLine, nil)
	r.cutByProvider(t, r.botSpeaks(t), "u1") // turn 1 is the opening
	r.final("u1", "")
	r.awaitCounted(t, 1)
	r.cutBySpeech(t, r.botSpeaks(t), "u2") // the reply, while the opening is owed
	r.final("u2", "Who is this, please?")
	r.awaitCounted(t, 2)
	r.end(t)
	assertTurnCuts(t, r.name, map[string]int64{
		cutKey(obs.TurnCutWordsBlank, obs.TurnCutLineOpening): 1,
		cutKey(obs.TurnCutWords, obs.TurnCutLineOpening):      1,
	})
	r.assertCutLine(t, "cutsWords=1", "cutsBlank=1", "cutsMissing=0", "cutsOpening=2", "cutsLater=0")
}

// The call's line adds up its cuts, by words and by line, and names the
// campaign that placed it.
func TestTheCallLogsItsCutsInOneLine(t *testing.T) {
	campaign := uuid.Must(uuid.NewV7()).String()
	r := startTurnCutRig(t, "", func(c *Config) { c.CampaignID = campaign })
	r.cutBySpeech(t, r.botSpeaks(t), "u1")
	r.final("u1", "Not interested.")
	r.cutByProvider(t, r.botSpeaks(t), "u2")
	r.final("u2", "")
	r.cutBySpeech(t, r.botSpeaks(t), "u3")
	r.awaitCounted(t, 2)
	r.end(t)
	r.assertCutLine(t, "cutsWords=1", "cutsBlank=1", "cutsMissing=1",
		"cutsOpening=0", "cutsLater=3", "cutsClosing=0", "campaignId="+campaign)
}

// Whatever ends a cut first, its final, the call closing (twice, at once), or
// on the opening a tool call's wait for the verdict, it is counted once.
func TestACutIsCountedOnceWhateverEndsItFirst(t *testing.T) {
	for _, tt := range []struct {
		name, opening, line string
	}{
		{"a later line", "", obs.TurnCutLineLater},
		{"the opening", openingLine, obs.TurnCutLineOpening},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for range 8 {
				r := startTurnCutRig(t, tt.opening, nil)
				r.cutByProvider(t, r.botSpeaks(t), "u1")
				r.session.bc.mu.Lock()
				cut := r.session.bc.cut
				r.session.bc.mu.Unlock()
				// Sent before the close, so the pump may take it or stop first.
				r.final("u1", "Wait, what is this about?")
				var wg sync.WaitGroup
				for _, f := range []func(){
					func() { r.session.cutVerdictOf(cut) },
					r.wait.expire,
					func() { r.session.Close(context.Background()) },
					func() { r.session.Close(context.Background()) },
				} {
					wg.Go(f)
				}
				wg.Wait()
				r.end(t)
				got := turnCutCounts(t, r.name)
				total := int64(0)
				for key, n := range got {
					total += n
					if !strings.HasSuffix(key, "/"+tt.line) {
						t.Errorf("counted %s, want only %s", key, tt.line)
					}
				}
				if total != 1 {
					t.Fatalf("counted %v, want one cut", got)
				}
				if lines := r.cutLines(); len(lines) != 1 {
					t.Fatalf("logged %d \"ai turn cuts\" lines, want 1", len(lines))
				}
			}
		})
	}
}

// Only a cut the engine made is counted: not a keypress, not a turn the
// application stopped to make room for a line, not speech inside the barge-in
// guard or with nothing playing. The probe cut after each proves the events
// before it were handled, and is the one thing counted.
func TestOnlyACutTheEngineMadeIsCounted(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*Config)
		act       func(t *testing.T, r *turnCutRig)
	}{
		{"a keypress", nil, func(t *testing.T, r *turnCutRig) {
			r.botSpeaks(t)
			r.leg.digits <- "5"
			waitLong(t, "the keypress to reach the model", func() bool { return len(r.model.recordedUserText()) == 1 })
			// The provider confirms the cancel the keypress asked for.
			r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonDTMF})
			r.leg.holdFrames(0)
		}},
		{"a line that pre-empts", nil, func(t *testing.T, r *turnCutRig) {
			r.botSpeaks(t)
			r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSystem})
			waitLong(t, "the turn to be flushed", func() bool { return r.leg.clears() >= 1 })
			r.leg.holdFrames(0)
		}},
		{"speech inside the barge-in guard", func(c *Config) { c.BargeGuard = time.Hour }, func(t *testing.T, r *turnCutRig) {
			r.botSpeaks(t)
			r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"})
			waitLong(t, "the speech to be ignored", func() bool {
				return strings.Contains(r.logs.String(), "ignored speech detected inside the barge-in guard")
			})
			r.final("u1", "Hello?")
			r.leg.holdFrames(0)
			r.send(provider.Event{Type: provider.EventTypeResponseDone})
		}},
		{"speech with nothing playing", nil, func(t *testing.T, r *turnCutRig) {
			r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"})
			r.final("u1", "Hello?")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := startTurnCutRig(t, "", tt.configure)
			tt.act(t, r)
			r.probeCut(t)
			r.end(t)
			assertTurnCuts(t, r.name, map[string]int64{cutKey(obs.TurnCutWords, obs.TurnCutLineLater): 1})
			r.assertCutLine(t, "cutsWords=1", "cutsLater=1", "cutsMissing=0")
		})
	}
}

// On openai the client holds the caller's speech until the words are known: a
// declined acknowledgement cuts nothing, and words that take the floor are no
// cut the engine made. Nothing is counted and no line is logged.
func TestNothingIsCountedOnAnEngineThatHoldsSpeech(t *testing.T) {
	var model *holdModel
	r := startTurnCutRigOn(t, provider.OpenAIProfile(), "", func(m *fakeModel) provider.VoiceSession {
		model = &holdModel{fakeModel: m}
		return model
	}, nil)
	r.botSpeaks(t)
	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"})
	r.final("u1", "mm-hmm")
	waitLong(t, "the acknowledgement to be declined", func() bool { return len(model.declines()) == 1 })
	if got := r.leg.clears(); got != 0 {
		t.Fatalf("a declined acknowledgement flushed the playback %d times", got)
	}

	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"})
	r.final("u2", "What does the plan cost?")
	waitLong(t, "the words to take the floor", func() bool { return r.leg.clears() >= 1 })
	r.end(t)
	assertTurnCuts(t, r.name, map[string]int64{})
	if lines := r.cutLines(); len(lines) != 0 {
		t.Errorf("logged %v, want no \"ai turn cuts\" line", lines)
	}
}

// newTurnCutDamageRig is a damageRig whose provider has a name of its own, on
// a call whose opening is opening ("" for none).
func newTurnCutDamageRig(t *testing.T, opening string) (*damageRig, string) {
	t.Helper()
	wait := newVerdictWait()
	profile := turnCutProfile(provider.QwenProfile())
	h := newClaimsHarnessModel(t, profile, quietFlow, "en", "", quietConfig(), nil, func(c *claimsHarness) {
		c.session.cutWait = wait.after
		c.session.holdAfter = (&holdClock{}).after
		c.session.cfg.OnCutAcknowledged = c.actions.onCutAcknowledged
		c.session.cfg.Session.OpeningText = opening
	})
	return &damageRig{quietHarness: &quietHarness{claimsHarness: h}, wait: wait}, profile.Name
}

// endCall closes the call and waits until drive has returned, which is after
// the session counted its cuts.
func (r *damageRig) endCall(t *testing.T) {
	t.Helper()
	r.session.Close(context.Background())
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("drive did not return")
	}
}

// A moving tool waits for the verdict on the cut its response answers, and the
// wait runs out. On a later line that settles nothing: the final that arrives
// afterwards still judges the cut and counts it. On the opening the wait
// settles it as words unknown (#610): MISSING, and the late final changes
// nothing.
func TestALateFinalAfterTheToolsWaitCountsTheCutOnce(t *testing.T) {
	// The tool's move is to a phase that ends nothing, so the call goes on.
	const maybe = `{"value":"maybe"}`
	t.Run("a later line", func(t *testing.T) {
		r, name := newTurnCutDamageRig(t, "")
		r.botSpeaksUntilCut(t, "u1")
		r.started()
		r.toolCall("fc_1", "classify", maybe)
		waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
		r.wait.expire()
		r.ended()
		r.awaitResult(t)
		if n := r.countedCuts(); n != 0 {
			t.Fatalf("%d cuts counted when the wait ran out, want none: the cut is still open to its final", n)
		}

		r.callerFinal("u1", "Maybe, let me think about it.")
		waitLong(t, "the late final to count the cut", func() bool { return r.countedCuts() == 1 })
		r.endCall(t)
		assertTurnCuts(t, name, map[string]int64{cutKey(obs.TurnCutWords, obs.TurnCutLineLater): 1})
	})
	t.Run("the opening", func(t *testing.T) {
		r, name := newTurnCutDamageRig(t, openingLine)
		r.botSpeaksUntilCut(t, "u1") // turn 1 is the opening
		r.started()
		r.toolCall("fc_1", "classify", maybe)
		waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
		r.wait.expire()
		r.ended()
		r.awaitResult(t)
		assertTurnCuts(t, name, map[string]int64{cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineOpening): 1})

		r.callerFinal("u1", "Maybe, let me think about it.")
		// The call stays in its phase (the move was refused): a later
		// response's call answered shows the final before it was handled.
		r.started()
		r.toolCall("fc_probe", "note", `{"value":"probe"}`)
		r.ended()
		waitFor(t, "the probe to be answered", func() bool { return len(r.model.recordedToolResults()) == 2 })
		r.endCall(t)
		assertTurnCuts(t, name, map[string]int64{cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineOpening): 1})
	})
}

// countedCuts is how many cuts the call has counted so far.
func (r *damageRig) countedCuts() int {
	r.session.bc.mu.Lock()
	defer r.session.bc.mu.Unlock()
	c := r.session.bc.cuts
	return c.words + c.blank + c.missing
}

// Where a cut fell is fixed when it is made, by the armed ending's own notion
// of its closing line: a turn after the one it was armed in. A cut of a later
// line before the ending was armed stays LATER when the call ends armed; a cut
// of the turn that armed it is no cut of the closing line; a cut of the line
// itself is CLOSING.
func TestACutIsCountedOnTheLineItFellOn(t *testing.T) {
	t.Run("a later line, then the ending armed", func(t *testing.T) {
		profile := turnCutProfile(provider.QwenProfile())
		h := startToolPathFull(t, profile, "en", owedLineFlow, "", map[string]any{"name": "Ann"}, &syncBuffer{})
		h.leg.holdFrames(40)
		h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*50)}
		h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
		waitLong(t, "the cut of turn 1", func() bool {
			h.session.bc.mu.Lock()
			defer h.session.bc.mu.Unlock()
			return h.session.bc.cut != nil && h.session.bc.cut.event.Turn == 1
		})
		h.leg.holdFrames(0)
		// The model's reply, then the hangup in a response of its own: no
		// moving tool answers the cut, so nothing waits for its verdict and
		// it is still open when the ending is armed and the call ends.
		h.playTurn()
		waitLong(t, "the reply to begin", func() bool { return h.session.currentTurn() == 2 })
		h.callTool(t, flow.ToolHangup, goodbyeHangup)
		if !h.actions.isArmed() {
			t.Fatal("the hangup did not arm the ending")
		}
		h.session.Close(context.Background())
		h.awaitDone(t)
		assertTurnCuts(t, profile.Name, map[string]int64{cutKey(obs.TurnCutWordsMissing, obs.TurnCutLineLater): 1})
	})
	t.Run("the turn the ending was armed in", func(t *testing.T) {
		profile := turnCutProfile(provider.QwenProfile())
		h := startToolPathFull(t, profile, "en", claimsFlow, "", nil, &syncBuffer{})
		h.actions.mu.Lock()
		h.actions.graceCap = time.Minute
		h.actions.mu.Unlock()
		h.callTool(t, flow.ToolHangup, goodbyeHangup)
		if !h.actions.isArmed() {
			t.Fatal("the hangup did not arm the ending")
		}
		// The same response goes on to speak, and the caller talks over it.
		h.leg.holdFrames(40)
		h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*50)}
		waitLong(t, "the arming turn's audio to be queued", func() bool {
			_, isSpeaking := h.session.audioProgress()
			return isSpeaking
		})
		h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
		h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u1",
			Text: "Wait, one more question.", IsFinal: true}
		waitLong(t, "the cut to be counted", func() bool { return len(turnCutCounts(t, profile.Name)) == 1 })
		h.session.Close(context.Background())
		h.awaitDone(t)
		assertTurnCuts(t, profile.Name, map[string]int64{cutKey(obs.TurnCutWords, obs.TurnCutLineLater): 1})
	})
	t.Run("the closing line", func(t *testing.T) {
		profile := turnCutProfile(provider.QwenProfile())
		r := startCutRigOn(t, profile, time.Minute)
		r.lineTurn(t, shortLineFrames, 100, cutLine)
		r.interrupt(t, 100, 1) // 2 s of the line left: an interruption
		if r.isCallEnded() {
			t.Fatal("the cut ended the call")
		}
		r.callerFinal("Wait, when will they call?")
		waitLong(t, "the cut to be counted", func() bool { return len(turnCutCounts(t, profile.Name)) == 1 })
		r.session.Close(context.Background())
		r.awaitDone(t)
		assertTurnCuts(t, profile.Name, map[string]int64{cutKey(obs.TurnCutWords, obs.TurnCutLineClosing): 1})
	})
}

// awaitDone waits until drive has returned, which is after the session
// counted its cuts.
func (h *toolPath) awaitDone(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("drive did not return")
	}
}
