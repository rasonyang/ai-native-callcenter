// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

//
// A goodbye said in reply to a tool result (#630). A tool result backs what the
// reply to it reports (a lookup ran, a message was saved, a transfer is under
// way), never a goodbye: only an ending tool call or an armed ending makes a
// goodbye hung. So the model's goodbye in words in that reply, with no tool
// call of its own, ends the call as any other unhung goodbye does (#453): after
// the grace, at the second one in a row, or at the dead-air backstop, with no
// closing line and no cue. It does so after a result with ok:0 and with ok:1.
//
// The same goodbye with no tool before it is
// TestAnUnhungGoodbyePlayedInFullThenSilenceHangsUpAfterTheGraceWithoutTheClosingLine,
// the control.
//

// toolReplyFlow is farewellFlow with a slot tool whose value must be one of its
// enum, and transfer_to_agent for an ending a tool result arms.
const toolReplyFlow = `{
	"id": "farewell-tool-reply",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {"persona": "You answer the phone.", "closingTarget": "finish",
		"alwaysAllowedTools": ["take_message", "transfer_to_agent", "hangup"]},
	"tools": {
		"classify": {"kind": "slot", "slot": "answer", "description": "Record the decision.",
			"enum": ["agree", "decline"]}
	},
	"nodes": {
		"welcome": {"instruction": "Greet.", "tools": ["classify"]},
		"finish": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true,
			"announce": {"en": "Sorry to have bothered you, goodbye.", "zh": "不好意思打扰了，再见。"}}
	}
}`

// toolResultCase is a tool call whose result asks the model for a turn, and
// the ok flag that result carries.
type toolResultCase struct {
	name, tool, args, ok string
}

var (
	// refusedSlot is the ok:0 of #630: a value outside the enum.
	refusedSlot  = toolResultCase{"refused slot", "classify", `{"value":"later"}`, "0"}
	recordedSlot = toolResultCase{"recorded slot", "classify", `{"value":"decline"}`, "1"}
	savedMessage = toolResultCase{"saved message", flow.ToolTakeMessage, `{"message":"call me back"}`, "1"}
	// armedTransfer is an ok:1 result that arms the call's ending.
	armedTransfer = toolResultCase{"armed transfer", flow.ToolTransferToAgent,
		`{"queue":"support","reason":"AGENT","summary":"asked for a person"}`, "1"}
)

// refusedAndSaved are a result with ok:0 and one with ok:1, each asking for a
// turn.
var refusedAndSaved = []toolResultCase{refusedSlot, savedMessage}

// toolReplyBye is the goodbye the model said in reply to the refused tool on
// the live call of #630, and an English one.
func toolReplyBye(lang string) string {
	if lang == "zh" {
		return "好的，那就不打扰您了，祝您生活愉快！"
	}
	return "Okay, I won't take any more of your time. Have a great day!"
}

// toolReplyRig is farewellRig counting the PLAYBACK_DONEs the session has
// published: an event the test emits after one reaches drive after it.
type toolReplyRig struct {
	*farewellRig
	playbacks *atomic.Int32
}

func newToolReplyRig(t *testing.T, profile provider.Profile, flowJSON, lang, backendURL string) *toolReplyRig {
	t.Helper()
	clock := &graceClock{}
	playbacks := &atomic.Int32{}
	h := newClaimsHarnessSetup(t, profile, flowJSON, lang, backendURL, Config{BargeGuard: -1, NoInput: -1},
		func(h *claimsHarness) {
			h.o.budgetAfter = clock.after
			// Set before any turn: the session reads it only after a turn's
			// playback, which this test's events start.
			h.session.afterPlaybackDone = func() { playbacks.Add(1) }
		})
	return &toolReplyRig{farewellRig: &farewellRig{claimsHarness: h, clock: clock}, playbacks: playbacks}
}

// awaitPlaybacks waits until the session has published n PLAYBACK_DONEs.
func (r *toolReplyRig) awaitPlaybacks(t *testing.T, n int32) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%d playbacks", n), func() bool { return r.playbacks.Load() >= n })
}

// respond plays one response: its words ("" is none), then its tool calls,
// each a name and its arguments, then its audio when it spoke.
func (r *toolReplyRig) respond(text string, calls ...[2]string) {
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	if text != "" {
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: text, IsFinal: true}
	}
	for _, call := range calls {
		r.model.events <- provider.Event{Type: provider.EventTypeToolCall,
			ToolCallID: "fc_" + call[0], ToolName: call[0], ToolArgs: call[1]}
	}
	if text != "" {
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples)}
	}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// wordlessTurn is a turn that played audio and whose transcript has not landed.
func (r *toolReplyRig) wordlessTurn() {
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// botSays is a final transcript of the bot's, landing on its own.
func (r *toolReplyRig) botSays(text string) {
	r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: text, IsFinal: true}
}

// callTools plays a wordless response making the calls and waits until each
// was answered with the ok flag of its case and the turn's playback has been
// published, so the reply to the results is the next turn.
func (r *toolReplyRig) callTools(t *testing.T, cases ...toolResultCase) {
	t.Helper()
	calls := make([][2]string, 0, len(cases))
	for _, c := range cases {
		calls = append(calls, [2]string{c.tool, c.args})
	}
	r.respond("", calls...)
	r.awaitToolResults(t, len(cases))
	results := r.model.recordedToolResults()
	for _, c := range cases {
		i := slices.IndexFunc(results, func(result toolResult) bool { return result.id == "fc_"+c.tool })
		if i < 0 || !strings.Contains(results[i].output, `"ok":"`+c.ok+`"`) {
			t.Fatalf("%s answered %v, want ok %s", c.tool, results, c.ok)
		}
	}
	r.awaitPlaybacks(t, 1)
}

// awaitGrace waits until drive has started the grace of the bot's own goodbye.
func (r *toolReplyRig) awaitGrace(t *testing.T) {
	t.Helper()
	waitUntil(t, "the grace of a goodbye said in reply to the tool result", func() bool {
		return r.clock.count(ownFarewellGrace) >= 1
	})
}

// eachToolResult runs a scenario after each of the tool results, in each
// language, on each engine eachProfile runs.
func eachToolResult(t *testing.T, cases []toolResultCase,
	run func(t *testing.T, profile provider.Profile, c toolResultCase, lang string)) {
	t.Helper()
	for _, c := range cases {
		for _, lang := range farewellLangs {
			t.Run(c.name+"/"+lang, func(t *testing.T) {
				eachProfile(t, func(t *testing.T, profile provider.Profile) { run(t, profile, c, lang) })
			})
		}
	}
}

// The words these tests rely on are what they say they are.
func TestTheToolReplyLinesAreWhatTheTestsSayTheyAre(t *testing.T) {
	t.Parallel()
	for _, lang := range farewellLangs {
		if got := detectClaims(toolReplyBye(lang)); len(got) != 1 || got[0].kind != store.UnbackedClaimFarewell {
			t.Errorf("%s goodbye: claims = %+v, want one FAREWELL", lang, got)
		}
		if got := detectClaims(lookupReply[lang]); len(got) != 1 || got[0].kind != store.UnbackedClaimLookup {
			t.Errorf("%s lookup reply: claims = %+v, want one LOOKUP", lang, got)
		}
	}
}

// 2. A goodbye in reply to a tool result, played in full, then 2 s of silence:
// hung up at the grace, the engine at the closing target, no closing line and
// no cue (#630, rows 2 and 3 of its table).
func TestAGoodbyeSaidInReplyToAToolResultHangsUpAfterTheGraceWithoutTheClosingLine(t *testing.T) {
	cases := []toolResultCase{refusedSlot, recordedSlot, savedMessage}
	eachToolResult(t, cases, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
		r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
		r.callTools(t, c)

		r.respond(toolReplyBye(lang))
		r.awaitGrace(t) // the playback has ended
		if r.session.isClosed() {
			t.Fatal("the call was hung up before the grace ran out")
		}
		r.clock.fireNewest(t, ownFarewellGrace)
		r.awaitHungUp(t)

		if got := r.runtime.Engine().NodeID(); got != "finish" {
			t.Errorf("node = %q, want the closing target", got)
		}
		r.assertNeverReopened(t)
		if r.logs.count("rule=GRACE") != 1 {
			t.Errorf("the grace was not logged once:\n%s", r.logs.String())
		}
	})
}

// 3. The same goodbye with dead air in place of the grace: the backstop hangs
// up, and the silent-caller cue is not sent (#630, row 4). Whether the grace is
// still pending, or never started because the caller made a sound before the
// goodbye's transcript landed.
func TestTheDeadAirBackstopHangsUpOnAGoodbyeSaidInReplyToAToolResult(t *testing.T) {
	shapes := []struct {
		name string
		play func(t *testing.T, r *toolReplyRig, lang string)
	}{
		{"grace pending", func(t *testing.T, r *toolReplyRig, lang string) {
			r.respond(toolReplyBye(lang))
			r.awaitPlaybacks(t, 2)
		}},
		{"no grace after a sound", func(t *testing.T, r *toolReplyRig, lang string) {
			r.wordlessTurn()
			r.awaitPlaybacks(t, 2)
			r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
			r.botSays(toolReplyBye(lang))
			r.awaitLog(t, unbackedClaimLog)
		}},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			eachToolResult(t, refusedAndSaved, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
				r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
				r.callTools(t, c)

				shape.play(t, r, lang)
				// The grace never fires here; the session's silence watch does,
				// after everything above has reached drive.
				r.session.emit(Event{Type: EventTypeNoInput})
				waitUntil(t, "the backstop to act", func() bool {
					return r.session.isClosed() || len(r.model.recordedUserText()) > 0
				})

				r.assertNeverReopened(t)
				if !r.session.isClosed() {
					t.Error("the backstop left the call open after the bot's goodbye")
				}
				if r.logs.count("rule=DEAD_AIR") != 1 {
					t.Errorf("the backstop was not logged once:\n%s", r.logs.String())
				}
			})
		})
	}
}

// 4. A goodbye in reply to a tool result that the caller talks over does not
// count: no grace, and the conversation goes on.
func TestAGoodbyeSaidInReplyToAToolResultThatTheCallerTalkedOverDoesNotCount(t *testing.T) {
	interjection := map[string]string{"en": "wait", "zh": "等一下"}
	eachToolResult(t, refusedAndSaved, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
		r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
		r.callTools(t, c)

		r.leg.holdFrames(3)
		r.respond(toolReplyBye(lang))
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		waitUntil(t, "the barge-in", func() bool { return r.leg.clears() > 0 })
		r.leg.holdFrames(0)
		r.callerSays(interjection[lang])

		r.assertOpen(t, "a goodbye in reply to a tool result, cut by the caller")
		if n := r.clock.count(ownFarewellGrace); n != 0 {
			t.Errorf("%d graces started for a goodbye nobody heard to the end", n)
		}
		if r.runtime.Engine().IsTerminal() {
			t.Error("the flow moved on a goodbye the caller talked over")
		}
		r.assertNeverReopened(t)
	})
}

// 5. Shape C: the goodbye in reply to the tool result is the first of two, and
// the second hangs up as soon as it has played.
func TestASecondGoodbyeAfterOneSaidInReplyToAToolResultHangsUpAtItsPlaybackEnd(t *testing.T) {
	why := map[string]string{"en": "Why haven't you hung up?", "zh": "你怎么还不挂？"}
	eachToolResult(t, refusedAndSaved, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
		r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
		r.callTools(t, c)

		r.respond(toolReplyBye(lang))
		r.awaitGrace(t)
		r.answered(why[lang])
		r.assertOpen(t, "a caller answering the first goodbye")

		r.respond(bye(lang))
		r.awaitHungUp(t) // no clock fired: the playback end is the hangup

		if n := r.clock.count(ownFarewellGrace); n != 1 {
			t.Errorf("%d graces started, want only the first goodbye's", n)
		}
		if got := r.runtime.Engine().NodeID(); got != "finish" {
			t.Errorf("node = %q, want the closing target", got)
		}
		r.assertNeverReopened(t)
		if r.logs.count("rule=TWO_GOODBYES") != 1 {
			t.Errorf("the second goodbye was not logged once:\n%s", r.logs.String())
		}
	})
}

// 6. A goodbye in reply to a tool result whose turn also calls hangup is the
// hangup's: backed, and the call ends on the armed hangup once the reply to it
// has played, as before. None of the rules of #453 acts.
func TestAGoodbyeThatCallsHangupInReplyToAToolResultEndsTheCallOnTheHangup(t *testing.T) {
	eachToolResult(t, refusedAndSaved, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
		r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
		r.callTools(t, c)

		r.respond(toolReplyBye(lang), [2]string{flow.ToolHangup, goodbyeHangup})
		r.awaitToolResults(t, 2)
		if results := r.model.recordedToolResults(); results[1].id != "fc_"+flow.ToolHangup ||
			!flow.IsToolOutputOK(results[1].output) {
			t.Fatalf("results = %v, want the hangup accepted", results)
		}
		r.awaitPlaybacks(t, 2)
		r.assertOpen(t, "the hangup's own turn")

		r.respond(bye(lang)) // the reply to the hangup's result
		r.awaitHungUp(t)

		if r.logs.count("hanging up after the farewell") != 1 {
			t.Errorf("the armed hangup did not end the call:\n%s", r.logs.String())
		}
		if n := r.logs.count("rule="); n != 0 {
			t.Errorf("a rule of #453 acted on a hung goodbye:\n%s", r.logs.String())
		}
		if n := r.clock.count(ownFarewellGrace); n != 0 {
			t.Errorf("%d graces started for a goodbye the hangup backs", n)
		}
		if n := r.logs.count(unbackedClaimLog); n != 0 {
			t.Errorf("%d unbacked claims reported for a goodbye the hangup backs", n)
		}
	})
}

// 7. A goodbye in reply to a tool result over an ending already armed is that
// ending's: a hangup armed in the same response as a refused or an accepted
// tool, or the transfer transfer_to_agent armed. The armed action runs at the
// goodbye's playback, and none of the rules of #453 acts.
func TestAGoodbyeInReplyToAToolResultOverAnArmedEndingIsLeftToThatEnding(t *testing.T) {
	hangup := toolResultCase{"hangup", flow.ToolHangup, goodbyeHangup, "1"}
	cases := []struct {
		name  string
		tools []toolResultCase
		ended func(r *toolReplyRig) bool
	}{
		{"refused slot and hangup", []toolResultCase{refusedSlot, hangup}, func(r *toolReplyRig) bool {
			return r.session.isClosed() && r.logs.count("hanging up after the farewell") == 1
		}},
		{"saved message and hangup", []toolResultCase{savedMessage, hangup}, func(r *toolReplyRig) bool {
			return r.session.isClosed() && r.logs.count("hanging up after the farewell") == 1
		}},
		{"armed transfer", []toolResultCase{armedTransfer}, func(r *toolReplyRig) bool {
			return r.sw.indexOf("transfer chan-9→7001") >= 0
		}},
	}
	for _, tc := range cases {
		for _, lang := range farewellLangs {
			t.Run(tc.name+"/"+lang, func(t *testing.T) {
				eachProfile(t, func(t *testing.T, profile provider.Profile) {
					r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
					r.callTools(t, tc.tools...)

					r.respond(toolReplyBye(lang))
					waitUntil(t, "the armed ending to run", func() bool { return tc.ended(r) })
					settle()

					if n := r.logs.count("rule="); n != 0 {
						t.Errorf("a rule of #453 acted over an armed ending:\n%s", r.logs.String())
					}
					if n := r.clock.count(ownFarewellGrace); n != 0 {
						t.Errorf("%d graces started over an armed ending", n)
					}
					if n := r.logs.count(unbackedClaimLog); n != 0 {
						t.Errorf("%d unbacked claims reported over an armed ending", n)
					}
					r.assertNeverReopened(t)
				})
			})
		}
	}
}

// lookupReply reports a lookup the turn before ran: a lookup claim's words.
var lookupReply = map[string]string{
	"zh": "好的，我帮您查询一下，您的维修单正在维修中。",
	"en": "Let me check that for you. Your repair is in progress.",
}

// 8. A lookup claim in reply to the lookup's own result is still backed by it:
// not reported, not re-asked at its playback, and the dead air after it gets
// the ordinary silent-caller cue, not the claim cue.
func TestALookupClaimInReplyToTheLookupsResultIsStillBacked(t *testing.T) {
	lookup := toolResultCase{"lookup", "repair_status", zhRepairCall, "1"}
	for _, lang := range farewellLangs {
		t.Run(lang, func(t *testing.T) {
			eachProfile(t, func(t *testing.T, profile provider.Profile) {
				r := newToolReplyRig(t, profile, lookupCueFlow, lang, repairBackend(t))
				r.callTools(t, lookup)

				r.respond(lookupReply[lang])
				r.awaitPlaybacks(t, 2)
				settle() // where a re-ask would go out
				if got := r.claimCueTexts(); len(got) != 0 {
					t.Errorf("a backed lookup was re-asked: %q", got)
				}
				if n := r.logs.count(unbackedClaimLog); n != 0 {
					t.Errorf("%d unbacked claims reported for a lookup that ran:\n%s", n, r.logs.String())
				}

				r.session.emit(Event{Type: EventTypeNoInput})
				texts := r.awaitUserTexts(t, 1)
				silent := map[string]string{"en": silentCueMarker, "zh": "来电者一直没有说话"}[lang]
				if len(r.claimCueTexts()) != 0 || !strings.Contains(texts[0], silent) {
					t.Errorf("dead-air cue = %q, want the ordinary silent-caller cue", texts[0])
				}
				if r.session.isClosed() {
					t.Error("the call was hung up on a lookup reply")
				}
			})
		})
	}
}

// 9. A goodbye in reply to a tool result whose transcript lands after its turn
// is done, after its playback or before it ends, still counts.
func TestAGoodbyeInReplyToAToolResultWhoseTranscriptLandsAfterTurnDoneStillCounts(t *testing.T) {
	timings := []struct {
		name string
		play func(t *testing.T, r *toolReplyRig, lang string)
	}{
		{"after the playback", func(t *testing.T, r *toolReplyRig, lang string) {
			r.wordlessTurn()
			r.awaitPlaybacks(t, 2) // done and played, no words yet
			r.botSays(toolReplyBye(lang))
		}},
		{"before the playback ends", func(t *testing.T, r *toolReplyRig, lang string) {
			r.leg.holdFrames(3)
			r.wordlessTurn()
			r.botSays(toolReplyBye(lang))
			r.awaitLog(t, unbackedClaimLog)
			r.leg.holdFrames(0)
		}},
	}
	for _, timing := range timings {
		t.Run(timing.name, func(t *testing.T) {
			eachToolResult(t, refusedAndSaved, func(t *testing.T, profile provider.Profile, c toolResultCase, lang string) {
				r := newToolReplyRig(t, profile, toolReplyFlow, lang, "")
				r.callTools(t, c)

				timing.play(t, r, lang)
				r.awaitGrace(t)
				r.clock.fireNewest(t, ownFarewellGrace)
				r.awaitHungUp(t)
				r.assertNeverReopened(t)
			})
		})
	}
}
