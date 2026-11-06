// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A caller turn the engine drops must not be recorded as a long turn latency
// when the dead-air cue's reply finally closes it (#404). These tests read the
// real instruments through an OTel ManualReader (the global meter provider can
// be set once per test binary, so every test compares counts before and after)
// and the session's own log lines.

var (
	turnReader     = sdkmetric.NewManualReader()
	setTurnReaders sync.Once
)

// turnCounts is what the three instruments hold for one provider.
type turnCounts struct {
	latency, firstAudio, unanswered int64
}

func readTurnCounts(t *testing.T, providerName string) turnCounts {
	t.Helper()
	setTurnReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(turnReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := turnReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out turnCounts
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					if p, _ := dp.Attributes.Value("provider"); p.AsString() != providerName {
						continue
					}
					switch m.Name {
					case "aicc_turn_latency_ms":
						out.latency += int64(dp.Count)
					case "aicc_provider_first_audio_ms":
						out.firstAudio += int64(dp.Count)
					}
				}
			case metricdata.Sum[int64]:
				if m.Name != "aicc_turn_unanswered_total" {
					continue
				}
				for _, dp := range data.DataPoints {
					if p, _ := dp.Attributes.Value("provider"); p.AsString() == providerName {
						out.unanswered += dp.Value
					}
				}
			}
		}
	}
	return out
}

// syncLog is a log sink safe to read while the session writes to it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) lines(contains string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(line, contains) {
			out = append(out, line)
		}
	}
	return out
}

// turnRig is a session on a profile with its log captured and its metrics
// baseline taken.
type turnRig struct {
	session *Session
	leg     *fakeLeg
	model   *fakeModel
	logs    *syncLog
	profile provider.Profile
	before  turnCounts
}

func startTurnRig(t *testing.T, profile provider.Profile, noInput time.Duration) *turnRig {
	t.Helper()
	logs := &syncLog{}
	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()
	cfg := Config{
		BargeGuard: -1,
		NoInput:    noInput,
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		Session: provider.SessionConfig{
			Instructions: "answer the phone",
			Turn:         provider.DefaultTurnDetection(),
		},
	}
	session, err := New(leg, model, profile, cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	awaitBridgeEvent(t, session, EventTypeReady)
	return &turnRig{session: session, leg: leg, model: model, logs: logs,
		profile: profile, before: readTurnCounts(t, profile.Name)}
}

// delta is what the instruments gained since the rig started.
func (r *turnRig) delta(t *testing.T) turnCounts {
	t.Helper()
	now := readTurnCounts(t, r.profile.Name)
	return turnCounts{
		latency:    now.latency - r.before.latency,
		firstAudio: now.firstAudio - r.before.firstAudio,
		unanswered: now.unanswered - r.before.unanswered,
	}
}

func (r *turnRig) send(typ provider.EventType) {
	r.model.events <- provider.Event{Type: typ}
}

// reply is a model response with audio: started, a few frames, done. It returns
// once the session has finished the turn, so everything it recorded is visible.
func (r *turnRig) reply(t *testing.T) {
	t.Helper()
	r.send(provider.EventTypeResponseStarted)
	r.model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*2*5),
	}
	r.send(provider.EventTypeResponseDone)
	awaitBridgeEvent(t, r.session, EventTypeTurnDone)
}

// timerOpen reports whether a turn measurement is in flight.
func (r *turnRig) timerOpen() bool {
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	return !r.session.timer.speechStoppedAt.IsZero()
}

// callerStops is speech_stopped, waited for until the measurement is open.
func (r *turnRig) callerStops(t *testing.T) {
	t.Helper()
	r.send(provider.EventTypeSpeechStopped)
	waitFor(t, "the measurement to open", r.timerOpen)
}

// want checks the log lines and the counters together.
func (r *turnRig) want(t *testing.T, latency, unanswered int, reasons ...string) {
	t.Helper()
	if got := len(r.logs.lines("turn latency")); got != latency {
		t.Errorf("%d `turn latency` lines, want %d", got, latency)
	}
	lines := r.logs.lines("caller turn went unanswered")
	if len(lines) != unanswered {
		t.Errorf("%d `caller turn went unanswered` lines, want %d: %v", len(lines), unanswered, lines)
	}
	for i, reason := range reasons {
		if i < len(lines) && !strings.Contains(lines[i], "reason="+reason) {
			t.Errorf("unanswered line %d = %q, want reason=%s", i, lines[i], reason)
		}
	}
	d := r.delta(t)
	if d.latency != int64(latency) || d.firstAudio != int64(latency) {
		t.Errorf("histograms gained %d latency / %d first-audio samples, want %d each",
			d.latency, d.firstAudio, latency)
	}
	if d.unanswered != int64(unanswered) {
		t.Errorf("aicc_turn_unanswered_total gained %d, want %d", d.unanswered, unanswered)
	}
}

// profiles runs a scenario on an engine that cancels its own response and on
// one that does not.
func eachProfile(t *testing.T, run func(t *testing.T, profile provider.Profile)) {
	t.Helper()
	for _, profile := range []provider.Profile{provider.OpenAIProfile(), provider.QwenProfile()} {
		name := profile.Name
		if profile.CancelsResponseItself {
			name += "/cancels-itself"
		} else {
			name += "/does-not"
		}
		t.Run(name, func(t *testing.T) { run(t, profile) })
	}
}

func TestAReplyAfterSpeechStoppedIsRecordedAsLatency(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		time.Sleep(300 * time.Millisecond)
		rig.reply(t)

		rig.want(t, 1, 0)
		line := rig.logs.lines("turn latency")[0]
		var total int
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "totalMs="); ok {
				total = atoi(v)
			}
		}
		if total < 300 || total > 1500 {
			t.Errorf("totalMs = %d, want about 300", total)
		}
	})
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// The reported bug: no reply, the dead-air watch fires, the cue's reply used to
// close the measurement as an 8-9 s latency.
func TestADroppedTurnIsUnansweredAndTheCuesReplyIsNotItsLatency(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 150*time.Millisecond)
		rig.reply(t) // the greeting; its playback arms the dead-air watch
		awaitBridgeEvent(t, rig.session, EventTypePlaybackDone)
		// The engine reports the caller stopped and then does nothing with it.
		rig.callerStops(t)
		awaitBridgeEvent(t, rig.session, EventTypeNoInput)

		if err := rig.session.SendCue("(the caller has been silent)"); err != nil {
			t.Fatal(err)
		}
		rig.reply(t) // the cue's reply

		rig.want(t, 0, 1, "NO_INPUT")
		line := rig.logs.lines("caller turn went unanswered")[0]
		if !strings.Contains(line, "provider="+profile.Name) || !strings.Contains(line, "waitMs=") {
			t.Errorf("line lacks provider or waitMs: %s", line)
		}
	})
}

func TestACallerWhoSpeaksAgainBeforeAnyReplyRecordsNothing(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		rig.send(provider.EventTypeSpeechStarted)
		waitFor(t, "the measurement to be abandoned", func() bool { return !rig.timerOpen() })
		rig.reply(t)
		rig.want(t, 0, 0)
	})
}

// A tool call answers the turn even though it makes no sound, so the wait that
// follows is the tool's and is latency. The dead-air watch is scaled down: 100
// ms stands for 8 s, so 50 ms is a 5 s tool and 400 ms a 12 s one.
func TestASlowToolBeforeTheReplyIsLatencyNeverUnanswered(t *testing.T) {
	for _, tool := range []struct {
		name string
		took time.Duration
		// firesMidTool says the dead-air watch is expected to fire while the
		// tool runs.
		firesMidTool bool
	}{
		{"5s-scaled", 50 * time.Millisecond, false},
		{"12s-scaled", 0, true},
	} {
		t.Run(tool.name, func(t *testing.T) {
			eachProfile(t, func(t *testing.T, profile provider.Profile) {
				rig := startTurnRig(t, profile, 100*time.Millisecond)
				rig.reply(t)
				awaitBridgeEvent(t, rig.session, EventTypePlaybackDone)
				rig.callerStops(t)

				// The model's response: a tool call and no audio.
				rig.send(provider.EventTypeResponseStarted)
				rig.send(provider.EventTypeResponseDone)
				awaitBridgeEvent(t, rig.session, EventTypeTurnDone)

				// The tool runs. The orchestrator's event loop is blocked in
				// answerToolCall meanwhile; the session goes on without it.
				// A tool shorter than the watch ends before it can fire (it
				// counts from the end of the tool-call turn, so a late
				// scheduler cannot make it fire early). A longer one waits
				// for the NO_INPUT itself, so load cannot make it flaky.
				var noInput bool
				if tool.firesMidTool {
					awaitBridgeEvent(t, rig.session, EventTypeNoInput)
					noInput = true
				} else {
					deadline := time.After(tool.took)
				tool:
					for {
						select {
						case event := <-rig.session.Events():
							if event.Type == EventTypeNoInput {
								noInput = true
							}
						case <-deadline:
							break tool
						}
					}
					if noInput {
						t.Error("NO_INPUT emitted during a tool shorter than the dead-air watch")
					}
				}

				if err := rig.session.AnswerTool("call-1", `{"ok":true}`); err != nil {
					t.Fatal(err)
				}
				if noInput {
					// What handleDeadAir does when the loop gets to the stale
					// NO_INPUT after the tool: the cue goes out on top of the
					// tool's answer. It must not turn the turn unanswered.
					if err := rig.session.SendCue("(the caller has been silent)"); err != nil {
						t.Fatal(err)
					}
				}
				rig.reply(t)

				rig.want(t, 1, 0)
			})
		})
	}
}

func TestALineBeforeAnyReplyLeavesTheTurnUnanswered(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.model.answerLinesWithATurn(rig.session)
		rig.callerStops(t)
		if err := rig.session.Speak("One moment, please.", false); err != nil {
			t.Fatal(err)
		}
		rig.reply(t) // the line's own turn
		rig.want(t, 0, 1, "LINE")
	})
}

func TestACueBeforeAnyReplyLeavesTheTurnUnanswered(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		if err := rig.session.SendCue("(goodbye)"); err != nil {
			t.Fatal(err)
		}
		rig.reply(t)
		rig.want(t, 0, 1, "CUE")
	})
}

// A cue or line after the engine has answered (a response started) changes
// nothing: the reply closes the measurement as latency.
func TestACueAfterTheEngineAnsweredStaysLatency(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		rig.send(provider.EventTypeResponseStarted)
		waitFor(t, "the response to start", rig.session.isHoldingTheFloor)
		if err := rig.session.SendCue("(goodbye)"); err != nil {
			t.Fatal(err)
		}
		if err := rig.session.SpeakAside("One moment."); err != nil {
			t.Fatal(err)
		}
		rig.model.events <- provider.Event{
			Type:  provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples*2*5),
		}
		rig.send(provider.EventTypeResponseDone)
		awaitBridgeEvent(t, rig.session, EventTypeTurnDone)
		rig.want(t, 1, 0)
	})
}

// A dropped turn, a barge-in over the cue's reply, then a normal turn: one
// unanswered, one latency.
func TestADroppedTurnThenABargeInThenANormalTurn(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		if err := rig.session.SendCue("(the caller has been silent)"); err != nil {
			t.Fatal(err)
		}
		// The cue's reply is long enough to be talked over.
		rig.send(provider.EventTypeResponseStarted)
		rig.model.events <- provider.Event{
			Type:  provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples*2*20),
		}
		waitFor(t, "the cue's reply to be queued", func() bool { return len(rig.leg.sentFrames()) >= 3 })

		rig.send(provider.EventTypeSpeechStarted)
		awaitBridgeEvent(t, rig.session, EventTypeBargeIn)
		rig.callerStops(t)
		time.Sleep(50 * time.Millisecond)
		rig.reply(t)

		rig.want(t, 1, 1, "CUE")
	})
}

// A keypress is the caller's own input: it abandons the measurement and the
// cue that reports it is not the application stepping in.
func TestAKeypressAfterSpeechStoppedRecordsNothing(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, -1)
		rig.callerStops(t)
		rig.leg.digits <- "5"
		waitFor(t, "the keypress cue", func() bool {
			text := rig.model.recordedUserText()
			return len(text) == 1 && strings.Contains(text[0], "pressed 5")
		})
		rig.reply(t)
		rig.want(t, 0, 0)
	})
}

//
// Issue #448: a caller turn the engine dropped carries the caller's words on
// the NO_INPUT event.
//
// The dead-air watch survives a caller's speech only when a response's
// playback ends after it, so these scenarios are the #197 shape: a response
// (a tool call, with no audio) is already in progress when the caller speaks,
// and its end arms the watch. The engine never answers the caller's turn.
//

const rma1003Words = "What is the status of my repair, the order number is RMA1003?"

// responseInProgress starts a response and waits until the session holds the
// floor for it.
func (r *turnRig) responseInProgress(t *testing.T) {
	t.Helper()
	r.send(provider.EventTypeResponseStarted)
	waitFor(t, "the response to start", r.session.isHoldingTheFloor)
}

// callerSpeaks is speech_started then speech_stopped, waited for until the
// measurement is open.
func (r *turnRig) callerSpeaks(t *testing.T) {
	t.Helper()
	r.send(provider.EventTypeSpeechStarted)
	r.callerStops(t)
}

// transcript is the caller's final transcript, waited for until the session
// has passed it on.
func (r *turnRig) transcript(t *testing.T, text string) {
	t.Helper()
	r.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, Text: text, IsFinal: true}
	awaitBridgeEvent(t, r.session, EventTypeCustomerSaid)
}

// deadAir ends the response in progress and waits for the NO_INPUT it arms.
func (r *turnRig) deadAir(t *testing.T) Event {
	t.Helper()
	r.send(provider.EventTypeResponseDone)
	return awaitBridgeEvent(t, r.session, EventTypeNoInput)
}

func TestADroppedTurnReportsTheCallersWordsOnNoInput(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.responseInProgress(t)
		rig.callerSpeaks(t)
		rig.transcript(t, rma1003Words)

		event := rig.deadAir(t)
		if !event.IsCallerUnanswered || event.Text != rma1003Words {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want the caller's words", event.IsCallerUnanswered, event.Text)
		}
		rig.want(t, 0, 1, "NO_INPUT")
	})
}

func TestADroppedTurnWithAnEmptyTranscriptIsUnansweredWithNoWords(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.responseInProgress(t)
		rig.callerSpeaks(t)
		rig.transcript(t, "  ")

		event := rig.deadAir(t)
		if !event.IsCallerUnanswered || event.Text != "" {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want unanswered with no words", event.IsCallerUnanswered, event.Text)
		}
	})
}

func TestDeadAirAfterACallerWhoNeverSpokeIsNotAnUnansweredTurn(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.reply(t)
		event := awaitBridgeEvent(t, rig.session, EventTypeNoInput)
		if event.IsCallerUnanswered || event.Text != "" {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want a plain silence", event.IsCallerUnanswered, event.Text)
		}
		rig.want(t, 0, 0)
	})
}

// OpenAI delivers a turn's input transcript often after the response that
// answers it has started. That transcript is of an answered turn and must not
// be mistaken for the words of a later one the engine drops.
func TestALateTranscriptOfAnAnsweredTurnIsNotTheWordsOfALaterDroppedOne(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.callerSpeaks(t)
		rig.responseInProgress(t) // answers it
		rig.transcript(t, "Please check my order.")

		// A noise turn the engine drops, while that response is still going.
		rig.callerSpeaks(t)
		event := rig.deadAir(t)
		if !event.IsCallerUnanswered || event.Text != "" {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want unanswered with no stale words", event.IsCallerUnanswered, event.Text)
		}
	})
}

// Doubao delivers the final transcript before speech_stopped.
func TestATranscriptBeforeSpeechStoppedIsKept(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.responseInProgress(t)
		rig.send(provider.EventTypeSpeechStarted)
		rig.transcript(t, rma1003Words)
		rig.callerStops(t)

		event := rig.deadAir(t)
		if !event.IsCallerUnanswered || event.Text != rma1003Words {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want the caller's words", event.IsCallerUnanswered, event.Text)
		}
	})
}

// A response that started before the caller spoke (#197: a tool-result
// response still in progress) and ends without the engine answering them keeps
// the words, however late its end arrives after the transcript.
func TestWordsSaidOverAResponseThatEndsLaterAreKept(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.responseInProgress(t)
		rig.callerSpeaks(t)
		rig.transcript(t, rma1003Words)
		// The response the caller spoke over ends; nothing new starts.
		event := rig.deadAir(t)
		if event.Text != rma1003Words || !event.IsCallerUnanswered {
			t.Errorf("NO_INPUT = unanswered %v, text %q; want the caller's words", event.IsCallerUnanswered, event.Text)
		}
	})
}

// A caller who speaks in two VAD segments may get the first one's transcript
// during the second.
func TestTheFirstSegmentsTranscriptSurvivesTheSecondSegmentStarting(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		rig := startTurnRig(t, profile, 100*time.Millisecond)
		rig.responseInProgress(t)
		rig.callerSpeaks(t)
		rig.send(provider.EventTypeSpeechStarted) // the second segment
		rig.transcript(t, "What is the status of my repair,")
		rig.callerStops(t)

		event := rig.deadAir(t)
		if event.Text != "What is the status of my repair," {
			t.Errorf("text = %q, want the first segment's words", event.Text)
		}
	})
}

// A response that starts after the words answers them: they are not carried to
// a later silence.
func TestWordsAreForgottenOnceAResponseStarts(t *testing.T) {
	var timer turnTimer
	timer.onSpeechStarted()
	timer.onSpeechStopped()
	timer.onCallerWords(rma1003Words)
	timer.onResponseStarted()
	if timer.words != "" {
		t.Errorf("words = %q after a response started, want none", timer.words)
	}
	timer.onCallerWords("late transcript of the answered turn")
	if timer.words != "" {
		t.Errorf("a transcript after the response started was kept: %q", timer.words)
	}
}

//
// Issue #404 item 3, settled by #448: what a dropped turn does to
// global.closingTarget. The NO_INPUT count (flow.Engine.OnNoInput) resets on a
// non-empty final transcript (OnCallerSpoke), a keypress or a tool result.
// The dead-air watch reports NO_INPUT whenever no turn followed; the session
// says whether the caller's turn went unanswered and with which words
// (Event.IsCallerUnanswered), and handleDeadAir does not tell the flow a
// caller with words was silent.
//

// dropThreeTurns drives three dropped caller turns through the orchestrator's
// dead-air handler, saying callerWords before each one the way the engine
// transcribed it ("" is a transcript with no words). It returns the cue texts
// the model received and whether the flow reached its closing target.
func dropThreeTurns(t *testing.T, callerWords string) (cues, lines []string, closed bool) {
	t.Helper()
	o, session, model, engine, runtime, actions, log := deadAirHarness(t, farewellBackstopFlow)
	model.answerLinesWithATurn(session)
	for range 3 {
		runtime.OnCallerSpoke(callerWords)
		o.handleDeadAir(session, runtime, actions, false, nil, callerWords, log)
	}
	return model.recordedUserText(), model.spokenLines(), engine.IsTerminal()
}

func TestThreeDroppedTurnsWithNoTranscriptEndTheCallAsNoInput(t *testing.T) {
	cues, lines, closed := dropThreeTurns(t, "")

	if !closed {
		t.Fatal("three dropped turns with no transcript must reach the closing target")
	}
	// The first two dead airs send the re-engagement cue; the third moves the
	// flow to the closing target and its announce is the line, with no cue.
	if len(cues) != 2 || !strings.Contains(cues[0], "still there") || cues[0] != cues[1] {
		t.Errorf("cues = %q, want the same re-engagement cue twice", cues)
	}
	if len(lines) != 1 || lines[0] != "Sorry to have bothered you, goodbye." {
		t.Errorf("lines = %q, want the closing announce once", lines)
	}
}

func TestThreeDroppedTurnsWithAFinalTranscriptNeverEndTheCallAndAreEachAnswered(t *testing.T) {
	cues, lines, closed := dropThreeTurns(t, "what is the status of my repair?")

	if closed {
		t.Fatal("a caller with words was told to be silent three times and the call was closed")
	}
	if len(cues) != 3 || len(lines) != 0 {
		t.Fatalf("cues = %q, lines = %q, want three cues and no line", cues, lines)
	}
	for _, cue := range cues {
		if cue != answerCueEN {
			t.Errorf("cue = %q, want the answer cue", cue)
		}
	}
}
