// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// What the provider sees, byte for byte.
//
// Reading a frame as a decoded map leaves everything the test did not ask
// about free to change — a renamed key, a dropped sibling, an extra frame
// nobody requested — and the first thing to notice would be a live call. These
// compare the bytes.
//

func TestTheSessionIsCreatedExactlyAsMeasured(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	startedSession(t, f)

	frames := f.settledFrames(1)
	want := `{"type":"session.create","event_id":"evt_1","session":{` +
		`"model":"1.2.6.1",` +
		`"instructions":"You answer the phone for NovaNet.",` +
		`"audio":{"input":{"format":{"type":"pcm","rate":16000}},` +
		`"output":{"format":{"type":"pcm_s16le","rate":24000},"voice":"zh_female_vv_jupiter_bigtts"}},` +
		`"tools":[{"type":"function","name":"lookup_balance","description":"Read the caller's balance.",` +
		`"parameters":{"type":"object","properties":{"accountId":{"type":"string"}}}}]},` +
		`"extension":{"asr":{},"tts":{},"dialog":{}}}`
	if got := string(frames[0]); got != want {
		t.Errorf("the session was created as\n got: %s\nwant: %s", got, want)
	}

	// The model is this client's, not the deployment's: the profile carries a
	// name this protocol has no field for.
	if strings.Contains(string(frames[0]), testProfile("").Model) {
		t.Error("the profile's model reached the wire; this protocol names its own")
	}
}

// The flow owns the voice; the profile's own is the fallback, and it has to be
// the one that reaches the wire when a flow names none.
func TestTheFlowsVoiceOverridesTheProfiles(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.Voice = "zh_male_beijingxiaoye_moon_bigtts"
	start(t, session, cfg)

	created := f.awaitMessages("session.create", 1)
	voice := nested(t, created[0], "session", "audio", "output", "voice")
	if voice != "zh_male_beijingxiaoye_moon_bigtts" {
		t.Errorf("the session names voice %v, want the flow's", voice)
	}
}

// The opening line is committed once the session exists, and never before it:
// a frame that arrives ahead of session.created is answered with an error and
// the socket is dropped.
func TestAnOpeningLineIsCommittedOnceTheSessionExists(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.OpeningText = "Good morning, NovaNet support."
	start(t, session, cfg)

	frames := f.settledFrames(2)
	want := `{"type":"speech_text_buffer.commit","event_id":"evt_2",` +
		`"text":"Good morning, NovaNet support."}`
	if got := string(frames[1]); got != want {
		t.Errorf("the opening line was committed as\n got: %s\nwant: %s", got, want)
	}

	// Its words are the client's to report: the provider sends no text events
	// for a committed line.
	f.send(audioStarted("chat_tts_text"))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeOutputTranscript)
	if got := events[1]; got.Text != "Good morning, NovaNet support." || !got.IsFinal {
		t.Errorf("the opening line was reported as %q (final=%v), want it as written",
			got.Text, got.IsFinal)
	}
	refuteMoreEvents(t, session)
}

// A call with no opening line sends exactly one frame and waits for the caller
// to speak: this engine answers, it does not open.
func TestWithNoOpeningLineNothingFollowsTheSessionCreate(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	startedSession(t, f)

	f.settledFrames(1)
	f.awaitMessages("speech_text_buffer.commit", 0)
}

func TestTheCredentialRidesTheUpgrade(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	startedSession(t, f)

	if got := f.handshake().Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want the credential", got)
	}
	if got := f.handshake().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want nothing: this protocol names its own header", got)
	}
}

func TestAMissingCredentialFailsBeforeTheCall(t *testing.T) {
	t.Setenv("DOUBAO_API_KEY", "")
	if _, err := newSession(testProfile("ws://127.0.0.1:1"), nil); err == nil {
		t.Fatal("a session was built without a credential")
	}
}

// An error instead of the session is the end of it: there is no session to
// retry on, and the server drops the socket in the same millisecond (e8b).
func TestStartFailsOnAnErrorBeforeTheSessionExists(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, func(f *fakeDoubao, message map[string]any) {
		if message["type"] == "session.create" {
			f.send(errorFrame("45000000", "unknown event name"))
		}
	})
	session := testSession(t, f)

	err := session.Start(t.Context(), testConfig())
	if err == nil {
		t.Fatal("start succeeded although the session was refused")
	}
	if !strings.Contains(err.Error(), "45000000") {
		t.Errorf("start failed with %v, want the provider's own code", err)
	}
}

// The caller hung up while the session was still being negotiated. Nothing may
// be left running.
func TestStartFailsWhenTheContextIsCancelledAndLeavesNothingBehind(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, func(*fakeDoubao, map[string]any) {})
	session := testSession(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	if err := session.Start(ctx, testConfig()); err == nil {
		t.Fatal("start succeeded although the session was never confirmed")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close after a failed start: %v", err)
	}
	drainEvents(t, session)
}

//
// Tool results.
//

func TestAToolResultIsByteExact(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(map[string]any{
		"type": "response.function_call_arguments.done",
		"items": []map[string]any{
			{"call_id": "call-a", "name": "lookup_balance", "arguments": `{"accountId":"42"}`}}})
	awaitEvent(t, session, provider.EventTypeToolCall)

	if err := session.SendToolResult("call-a", `{"balance":"12.50"}`, "Read it out."); err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	frames := f.settledFrames(2)
	want := `{"type":"conversation.item.create","event_id":"evt_2","items":[` +
		`{"call_id":"call-a","role":"tool","content":[` +
		`{"type":"input_text","text":"{\"balance\":\"12.50\",\"hint\":\"Read it out.\"}"}]}]}`
	if got := string(frames[1]); got != want {
		t.Errorf("the tool result was sent as\n got: %s\nwant: %s", got, want)
	}
}

// The engine goes on only once every call in the set has an answer, so the
// client holds them until the set is complete and sends one message in the
// order the items arrived.
func TestParallelToolResultsAreAggregatedIntoOneMessage(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(map[string]any{
		"type": "response.function_call_arguments.done",
		"items": []map[string]any{
			{"call_id": "call-a", "name": "lookup_balance", "arguments": "{}"},
			{"call_id": "call-b", "name": "lookup_balance", "arguments": "{}"}}})
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)

	// The second call is answered first; the message still follows the items.
	if err := session.SendToolResult("call-b", `{"n":2}`, ""); err != nil {
		t.Fatalf("send the second tool result: %v", err)
	}
	f.awaitMessages("conversation.item.create", 0)

	if err := session.SendToolResult("call-a", `{"n":1}`, ""); err != nil {
		t.Fatalf("send the first tool result: %v", err)
	}

	frames := f.settledFrames(2)
	want := `{"type":"conversation.item.create","event_id":"evt_2","items":[` +
		`{"call_id":"call-a","role":"tool","content":[{"type":"input_text","text":"{\"n\":1}"}]},` +
		`{"call_id":"call-b","role":"tool","content":[{"type":"input_text","text":"{\"n\":2}"}]}]}`
	if got := string(frames[1]); got != want {
		t.Errorf("the aggregated result was sent as\n got: %s\nwant: %s", got, want)
	}
}

func TestAnAnswerToACallThatWasNeverMadeIsRefused(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	if err := session.SendToolResult("call-nobody-made", `{}`, ""); err == nil {
		t.Fatal("a result for an unknown call was accepted")
	}
	f.awaitMessages("conversation.item.create", 0)
}

// Tools are a full overwrite on this API, and the session body is the same one
// the session was created with. Sending the instructions alone would drop
// every tool the flow has.
func TestUpdatingTheInstructionsRewritesTheWholeSession(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	if err := session.UpdateInstructions("You are now closing the call."); err != nil {
		t.Fatalf("update instructions: %v", err)
	}

	frames := f.settledFrames(2)
	want := `{"type":"session.update","event_id":"evt_2","session":{` +
		`"model":"1.2.6.1",` +
		`"instructions":"You are now closing the call.",` +
		`"audio":{"input":{"format":{"type":"pcm","rate":16000}},` +
		`"output":{"format":{"type":"pcm_s16le","rate":24000},"voice":"zh_female_vv_jupiter_bigtts"}},` +
		`"tools":[{"type":"function","name":"lookup_balance","description":"Read the caller's balance.",` +
		`"parameters":{"type":"object","properties":{"accountId":{"type":"string"}}}}]},` +
		`"extension":{"asr":{},"tts":{},"dialog":{}}}`
	if got := string(frames[1]); got != want {
		t.Errorf("the session was updated as\n got: %s\nwant: %s", got, want)
	}
}

// Every upstream event but audio carries an id of our own, and they run in
// order: it is the only way a frame and the answer to it can be matched in a
// provider-side trace.
func TestEventIDsRunInOrder(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	if err := session.SpeakText("One.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if err := session.UpdateInstructions("Two."); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := session.SpeakText("Three.", false); err != nil {
		t.Fatalf("speak again: %v", err)
	}

	frames := f.settledFrames(4)
	for i, want := range []string{"evt_1", "evt_2", "evt_3", "evt_4"} {
		if !strings.Contains(string(frames[i]), `"event_id":"`+want+`"`) {
			t.Errorf("frame %d is %s, want event id %s", i, frames[i], want)
		}
	}
}

// nested walks a decoded JSON object.
func nested(t *testing.T, value any, path ...string) any {
	t.Helper()
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("expected an object at %q, got %T", key, value)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("key %q is missing from %v", key, object)
		}
	}
	return value
}
