// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// What the provider sees, byte for byte.
//
// Reading a frame as a decoded map leaves everything the test did not ask about
// free to change — a renamed key, a dropped sibling, a field this service
// refuses the whole session over — and the first thing to notice would be a live
// call. These compare the bytes.
//

// The setup is the one frame that cannot be corrected afterwards. Everything in
// it was measured, and so was the absence of everything that is not.
func TestTheSessionIsConfiguredExactlyAsMeasured(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	startedSession(t, f)

	frames := f.settledFrames(2)
	want := `{"setup":{"model":"models/gemini-3.8-live",` +
		`"generationConfig":{"responseModalities":["AUDIO"],` +
		`"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}},` +
		`"systemInstruction":{"parts":[{"text":"You answer the phone for NovaNet."}]},` +
		`"tools":[{"functionDeclarations":[{"name":"lookup_balance",` +
		`"description":"Read the caller's balance.",` +
		`"parameters":{"properties":{"accountId":{"type":"STRING"}},"type":"OBJECT"},` +
		`"behavior":"BLOCKING"}]}],` +
		`"realtimeInputConfig":{"automaticActivityDetection":{"disabled":false,` +
		`"silenceDurationMs":500},"activityHandling":"START_OF_ACTIVITY_INTERRUPTS"},` +
		`"inputAudioTranscription":{"languageCodes":["en"]},` +
		`"outputAudioTranscription":{}}}`
	if got := string(frames[0]); got != want {
		t.Errorf("the session was configured as\n got: %s\nwant: %s", got, want)
	}

	// The model is this client's, not the deployment's: what this code knows how
	// to hold a conversation with is one model's lifecycle.
	if strings.Contains(string(frames[0]), testProfile("").Model) {
		t.Error("the profile's model reached the wire; this client names its own")
	}
}

// Every field that has been measured closing the socket, and every one this
// client has decided not to send. A setup that grows one of these back is a
// session that ends on the first frame, or a call that behaves differently for
// reasons nobody can see.
func TestTheSetupCarriesNoneOfTheFieldsThatRefuseIt(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	startedSession(t, f)

	setup := string(f.settledFrames(2)[0])
	for _, forbidden := range []string{
		"thinkingConfig",        // closed 1007: not supported for this model
		"proactivity",           // permanently on; false is an error
		"enableAffectiveDialog", // removed from the API
		"sessionResumption",     // this client never resumes a session
		"contextWindowCompression",
		// The native-audio models refuse to be told which language to speak.
		// The key with the "s" is a different field on a different object — a
		// hint for recognising what the CALLER said — and it is sent; this one
		// is the key exactly, which is why it carries its colon.
		`"languageCode":`,
		"activityStart",
		"activityEnd",
		"mediaChunks",
	} {
		if strings.Contains(setup, forbidden) {
			t.Errorf("the setup carries %q, which this client must never send", forbidden)
		}
	}

	// responseModalities belongs inside generationConfig. At the top level the
	// session is refused with close 1007, "Unknown name responseModalities".
	var frame map[string]any
	if err := json.Unmarshal([]byte(setup), &frame); err != nil {
		t.Fatalf("the setup is not JSON: %v", err)
	}
	body, _ := nested(t, frame, "setup").(map[string]any)
	if _, isTopLevel := body["responseModalities"]; isTopLevel {
		t.Error("responseModalities is at the top level of the setup; the service refuses that")
	}
	nested(t, frame, "setup", "generationConfig", "responseModalities")
}

// The flow owns the voice; the profile's own is the fallback, and with neither
// the field goes out at all rather than empty — an empty voice name is not a
// request for the default.
func TestTheFlowsVoiceOverridesTheProfilesAndNeitherIsSentEmpty(t *testing.T) {
	t.Run("the flow names one", func(t *testing.T) {
		f := newFakeGemini(t, acceptSetup)
		session := testSession(t, f)

		cfg := testConfig()
		cfg.Voice = "Puck"
		start(t, session, cfg)

		created := f.awaitMessages("setup", 1)
		voice := nested(t, created[0], "setup", "generationConfig", "speechConfig",
			"voiceConfig", "prebuiltVoiceConfig", "voiceName")
		if voice != "Puck" {
			t.Errorf("the session names voice %v, want the flow's", voice)
		}
	})

	t.Run("neither names one", func(t *testing.T) {
		f := newFakeGemini(t, acceptSetup)
		t.Setenv("GEMINI_API_KEY", "test-key")
		profile := testProfile(f.endpoint())
		profile.Voice = ""
		session, err := newSession(profile, nil)
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		t.Cleanup(func() { _ = session.Close(context.Background()) })
		start(t, session, testConfig())

		setup := string(f.settledFrames(2)[0])
		if strings.Contains(setup, "speechConfig") {
			t.Errorf("the setup named a voice with nothing to name: %s", setup)
		}
	})
}

// The call's language is a hint for recognising what the CALLER says.
//
// Without it the service guesses, and on live calls it guessed wrong: Chinese
// and English callers came back as Spanish, Hindi and Italian, and that reaches
// the transcript, the CDR and the screen a supervisor reads. A language nothing
// here speaks is no hint at all rather than a wrong one.
func TestTheCallersLanguageIsHintedToTheTranscription(t *testing.T) {
	cases := []struct {
		name     string
		language string
		want     string
	}{
		// The table this API publishes lists zh-Hans and zh-Hant rather than a
		// bare zh, and this repository's Chinese is Simplified.
		{"Chinese", "zh", `"inputAudioTranscription":{"languageCodes":["zh-Hans"]}`},
		{"English", "en", `"inputAudioTranscription":{"languageCodes":["en"]}`},
		{"a language nothing here speaks", "ja", `"inputAudioTranscription":{}`},
		{"no language at all", "", `"inputAudioTranscription":{}`},
	}
	for _, kase := range cases {
		t.Run(kase.name, func(t *testing.T) {
			f := newFakeGemini(t, acceptSetup)
			session := testSession(t, f)

			cfg := testConfig()
			cfg.Language = kase.language
			start(t, session, cfg)

			setup := string(f.settledFrames(2)[0])
			if !strings.Contains(setup, kase.want) {
				t.Errorf("the session was configured as\n  got: %s\nwanting: %s",
					setup, kase.want)
			}
			// What the model speaks is still the instructions' business: these
			// models refuse a language of their own.
			if !strings.Contains(setup, `"outputAudioTranscription":{}`) {
				t.Errorf("the output transcription was configured: %s", setup)
			}
		})
	}
}

// A turn hold the flow did not choose is the server's to pick. Sending zero
// would be a hold of no length at all.
func TestTheTurnHoldIsSentOnlyWhenTheFlowChoseOne(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.Turn.SilenceMs = 0
	start(t, session, cfg)

	setup := string(f.settledFrames(2)[0])
	if strings.Contains(setup, "silenceDurationMs") {
		t.Errorf("the setup named a silence hold the flow did not choose: %s", setup)
	}
}

// Every declaration carries BLOCKING. The model's own default is NON_BLOCKING,
// which lets it keep talking while a transfer is being arranged — and every tool
// this application has is a decision the conversation cannot run ahead of.
func TestEveryToolIsDeclaredBlocking(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.Tools = append(cfg.Tools,
		provider.ToolSpec{Name: "hangup", Description: "End the call.",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"isFarewellSpoken":{"type":"boolean"}}}`)},
		provider.ToolSpec{Name: "no_arguments", Description: "Takes nothing."})
	start(t, session, cfg)

	setup := f.awaitMessages("setup", 1)[0]
	declared, ok := nested(t, setup, "setup", "tools").([]any)
	if !ok || len(declared) != 1 {
		t.Fatalf("the tools are %v, want one declaration group", declared)
	}
	functions, ok := nested(t, declared[0], "functionDeclarations").([]any)
	if !ok || len(functions) != 3 {
		t.Fatalf("%d functions were declared, want 3", len(functions))
	}
	for i, function := range functions {
		if got := nested(t, function, "behavior"); got != "BLOCKING" {
			t.Errorf("function %d is declared %v, want BLOCKING", i, got)
		}
	}
	// A tool with no arguments declares none, rather than an empty schema.
	if _, hasParameters := functions[2].(map[string]any)["parameters"]; hasParameters {
		t.Error("a tool with no schema declared one anyway")
	}
}

// Every schema in this repository is written the way JSON Schema spells a type;
// this API's type is a protobuf enum, whose names are matched case-sensitively.
// The rewrite is recursive, and it touches nothing else.
func TestSchemaTypesAreRewrittenForTheEnumAndNothingElseIs(t *testing.T) {
	cases := []struct {
		name string
		from string
		want string
	}{
		{
			name: "a flow's transfer schema, nested objects and an enum of queues",
			from: `{"type":"object","properties":{` +
				`"queue":{"type":"string","description":"Which queue","enum":["support","sales"]},` +
				`"slots":{"type":"object","description":"Everything collected"}},` +
				`"required":["queue"]}`,
			want: `{"properties":{"queue":{"description":"Which queue",` +
				`"enum":["support","sales"],"type":"STRING"},` +
				`"slots":{"description":"Everything collected","type":"OBJECT"}},` +
				`"required":["queue"],"type":"OBJECT"}`,
		},
		{
			name: "arrays carry their item schema",
			from: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`,
			want: `{"properties":{"tags":{"items":{"type":"STRING"},"type":"ARRAY"}},"type":"OBJECT"}`,
		},
		{
			name: "a type that is already the enum's spelling is left alone",
			from: `{"type":"OBJECT","properties":{"n":{"type":"INTEGER"}}}`,
			want: `{"properties":{"n":{"type":"INTEGER"}},"type":"OBJECT"}`,
		},
		{
			name: "a nullable type is the one name this API can express",
			from: `{"type":["string","null"]}`,
			want: `{"type":"STRING"}`,
		},
		{
			name: "a keyword this API may not know is left for it to refuse",
			from: `{"type":"object","additionalProperties":false,"title":"Args"}`,
			want: `{"additionalProperties":false,"title":"Args","type":"OBJECT"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := string(geminiSchema(json.RawMessage(testCase.from)))
			if got != testCase.want {
				t.Errorf("the schema was rewritten as\n got: %s\nwant: %s", got, testCase.want)
			}
		})
	}

	if got := geminiSchema(nil); got != nil {
		t.Errorf("a tool with no schema produced %s, want nothing at all", got)
	}
}

// The credential rides a header. The documented alternative is a query
// parameter, which would put it into every URL this process logs.
func TestTheCredentialRidesTheUpgradeAndNeverTheURL(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	startedSession(t, f)

	if got := f.handshake().Get("x-goog-api-key"); got != "test-key" {
		t.Errorf("x-goog-api-key = %q, want the credential", got)
	}
	if got := f.requestURL(); strings.Contains(got, "test-key") || strings.Contains(got, "key=") {
		t.Errorf("the client dialled %q, which carries the credential", got)
	}
	// This protocol names its own header; an Authorization header would be a
	// different credential scheme entirely.
	if got := f.handshake().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want nothing", got)
	}
}

func TestAMissingCredentialFailsBeforeTheCall(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	if _, err := newSession(testProfile("ws://127.0.0.1:1"), nil); err == nil {
		t.Fatal("a session was built without a credential")
	}
}

//
// Opening the call.
//

// The flow's own line goes out as a line to repeat. Its words are not reported
// from here: this model transcribes itself, and what the caller hears is what
// should be recorded rather than what was asked for.
func TestAnOpeningLineIsAskedForAsALineToRepeat(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)

	cfg := testConfig()
	cfg.OpeningText = "Good morning, NovaNet support."
	start(t, session, cfg)

	frames := f.settledFrames(2)
	want := `{"clientContent":{"turns":[{"role":"user","parts":[{"text":` +
		`"Say exactly this, word for word, and add nothing else:\n` +
		`Good morning, NovaNet support."}]}],"turnComplete":true}}`
	if got := string(frames[1]); got != want {
		t.Errorf("the opening line was asked for as\n got: %s\nwant: %s", got, want)
	}

	// The line is the server's to report, once it says it.
	f.send(modelAudioParts("Good morning, NovaNet support.", "AQID"))
	f.send(generationComplete())
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone,
	)
	if got := events[3]; got.Text != "Good morning, NovaNet support." || !got.IsFinal {
		t.Errorf("the greeting was reported as %q (final=%v), want the words that were said",
			got.Text, got.IsFinal)
	}
	refuteMoreEvents(t, session)
}

// With no line of its own the bot still speaks first: our bot answers the
// telephone. An empty turn is what makes this model greet from its instructions.
func TestWithNoOpeningLineTheModelIsAskedToGreetFromItsInstructions(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	startedSession(t, f)

	frames := f.settledFrames(2)
	want := `{"clientContent":{"turns":[],"turnComplete":true}}`
	if got := string(frames[1]); got != want {
		t.Errorf("the opening turn was asked for as\n got: %s\nwant: %s", got, want)
	}
}

// A setup this service will not have is answered by closing the socket: there is
// no error frame on this protocol, only a code and a sentence.
func TestStartFailsWithTheCloseCodeAndReason(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		reason string
	}{
		{name: "a field the setup may not carry", code: 1007,
			reason: `Invalid JSON payload received. Unknown name "responseModalities" at 'setup': Cannot find field.`},
		{name: "a model that does not exist", code: 1008,
			reason: "models/gemini-3.8-live-does-not-exist is not found for API version v1beta"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			defer noGoroutinesLeft(t)()

			f := newFakeGemini(t, refuseSetup(testCase.code, testCase.reason))
			session := testSession(t, f)

			err := session.Start(t.Context(), testConfig())
			if err == nil {
				t.Fatal("start succeeded although the session was refused")
			}
			if !strings.Contains(err.Error(), testCase.reason[:40]) {
				t.Errorf("start failed with %v, want the provider's own reason", err)
			}
			if got := session.outcome(); got != rejectedOutcome(testCase.code) {
				t.Errorf("the session ended as %q, want %q",
					got, rejectedOutcome(testCase.code))
			}
		})
	}
}

// A setup nobody answers cannot hold a call open, and the bound is this client's
// own because the context it is given may have no deadline in it.
func TestStartFailsWhenTheSetupIsNeverAnswered(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, func(*fakeGemini, map[string]any) {})
	session := testSession(t, f)
	session.setupWait = 100 * time.Millisecond

	started := time.Now()
	err := session.Start(context.Background(), testConfig())
	if err == nil {
		t.Fatal("start succeeded although the setup was never answered")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("start took %v to give up, want the client's own bound", elapsed)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close after a failed start: %v", err)
	}
	drainEvents(t, session)
}

// The caller hung up while the session was still being negotiated. Nothing may
// be left running.
func TestStartFailsWhenTheContextIsCancelledAndLeavesNothingBehind(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, func(*fakeGemini, map[string]any) {})
	session := testSession(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	if err := session.Start(ctx, testConfig()); err == nil {
		t.Fatal("start succeeded although the setup was never answered")
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
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance",
		map[string]any{"accountId": "42"})))
	awaitEvent(t, session, provider.EventTypeToolCall)

	if err := session.SendToolResult("fc_1", `{"balance":"12.50"}`, "Read it out."); err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	frames := f.settledFrames(3)
	want := `{"toolResponse":{"functionResponses":[{"id":"fc_1","name":"lookup_balance",` +
		`"response":{"output":{"balance":"12.50","hint":"Read it out."}}}]}}`
	if got := string(frames[2]); got != want {
		t.Errorf("the tool result was sent as\n got: %s\nwant: %s", got, want)
	}
}

// A result that is not JSON is still an answer, and goes in as the string it is
// rather than as broken JSON the model has to guess at.
func TestAToolResultThatIsNotJSONIsSentAsAString(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
	awaitEvent(t, session, provider.EventTypeToolCall)

	if err := session.SendToolResult("fc_1", "the queue is closed", ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	frames := f.settledFrames(3)
	want := `{"toolResponse":{"functionResponses":[{"id":"fc_1","name":"lookup_balance",` +
		`"response":{"output":"the queue is closed"}}]}}`
	if got := string(frames[2]); got != want {
		t.Errorf("the tool result was sent as\n got: %s\nwant: %s", got, want)
	}
}

// The model is blocked on every call it made, so it goes on only when all of
// them have an answer — and they travel in one frame, in the order it asked.
func TestBatchedToolResultsAreSentOnceTheSetIsComplete(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(toolCallFrame(
		functionCallOf("fc_a", "lookup_order", map[string]any{"order_id": "A1234"}),
		functionCallOf("fc_b", "lookup_customer", map[string]any{"phone": "95001"}),
	))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)
	if got := events[1]; got.ToolCallID != "fc_a" || got.ToolName != "lookup_order" ||
		got.ToolArgs != `{"order_id":"A1234"}` {
		t.Errorf("the first tool call is %+v, want the call as it arrived", got)
	}

	// The second call is answered first; the frame still follows the order the
	// model asked in.
	if err := session.SendToolResult("fc_b", `{"tier":"gold"}`, ""); err != nil {
		t.Fatalf("send the second tool result: %v", err)
	}
	f.awaitMessages("toolResponse", 0)

	if err := session.SendToolResult("fc_a", `{"status":"shipped"}`, ""); err != nil {
		t.Fatalf("send the first tool result: %v", err)
	}

	frames := f.settledFrames(3)
	want := `{"toolResponse":{"functionResponses":[` +
		`{"id":"fc_a","name":"lookup_order","response":{"output":{"status":"shipped"}}},` +
		`{"id":"fc_b","name":"lookup_customer","response":{"output":{"tier":"gold"}}}]}}`
	if got := string(frames[2]); got != want {
		t.Errorf("the batch was answered as\n got: %s\nwant: %s", got, want)
	}
}

// An interruption makes the server discard the calls it was waiting on and ask
// again under new ids (measured). A response carrying the old one is ignored by
// the service, so this client does not send it — and does not report a failure
// the caller never experienced.
func TestACallTheServerWithdrewIsAnsweredWithNothing(t *testing.T) {
	withdrawals := map[string]func(f *fakeGemini){
		"the caller interrupted the turn": func(f *fakeGemini) { f.send(interrupted()) },
		"the server withdrew the call": func(f *fakeGemini) {
			f.send(toolCallCancellationFrame("fc_1"))
		},
	}

	for name, withdraw := range withdrawals {
		t.Run(name, func(t *testing.T) {
			f := newFakeGemini(t, acceptSetup)
			session := startedSession(t, f)

			f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
			awaitEvent(t, session, provider.EventTypeToolCall)

			withdraw(f)
			time.Sleep(100 * time.Millisecond)

			if err := session.SendToolResult("fc_1", `{"balance":"12.50"}`, ""); err != nil {
				t.Errorf("answering a withdrawn call returned %v, want nothing to report", err)
			}
			if err := session.SendToolResult("fc_1", `{"balance":"12.50"}`, ""); err != nil {
				t.Errorf("answering it twice returned %v", err)
			}
			f.awaitMessages("toolResponse", 0)

			// The call the server asks again under is answerable as usual.
			f.send(toolCallFrame(functionCallOf("fc_2", "lookup_balance", nil)))
			awaitEvent(t, session, provider.EventTypeToolCall)
			if err := session.SendToolResult("fc_2", `{"balance":"12.50"}`, ""); err != nil {
				t.Fatalf("answering the fresh call: %v", err)
			}
			answered := f.awaitMessages("toolResponse", 1)
			responses, ok := nested(t, answered[0], "toolResponse", "functionResponses").([]any)
			if !ok || len(responses) != 1 {
				t.Fatalf("the answer carried %v, want one response", responses)
			}
			if got := nested(t, responses[0], "id"); got != "fc_2" {
				t.Errorf("the answer names call %v, want the one the model is waiting on", got)
			}
		})
	}
}

func TestAnAnswerToACallThatWasNeverMadeWritesNothing(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	if err := session.SendToolResult("fc-nobody-made", `{}`, ""); err != nil {
		t.Errorf("a result for an unknown call returned %v, want nothing to report", err)
	}
	f.awaitMessages("toolResponse", 0)
	refuteMoreEvents(t, session)
}

//
// Instructions that cannot be replaced.
//

// The standing instructions are fixed at setup. A user turn carrying
// replacements was measured being ignored, so this writes nothing at all — and
// keeps the phase's own words for the next thing that is said.
func TestUpdatingTheInstructionsWritesNothingAndTheNextCueCarriesThePhase(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	base := testConfig().Instructions
	if err := session.UpdateInstructions(base + "\n\nNow take a message."); err != nil {
		t.Fatalf("update instructions: %v", err)
	}
	f.settledFrames(2)
	refuteMoreEvents(t, session)

	if err := session.SendUserText("2"); err != nil {
		t.Fatalf("send user text: %v", err)
	}
	frames := f.settledFrames(3)
	want := `{"clientContent":{"turns":[{"role":"user","parts":[{"text":` +
		`"Now take a message.\n\n2"}]}],"turnComplete":true}}`
	if got := string(frames[2]); got != want {
		t.Errorf("the cue was sent as\n got: %s\nwant: %s", got, want)
	}

	// Once said, it is said. The next cue is only the cue.
	if err := session.SendUserText("3"); err != nil {
		t.Fatalf("send the second cue: %v", err)
	}
	frames = f.settledFrames(4)
	want = `{"clientContent":{"turns":[{"role":"user","parts":[{"text":"3"}]}],` +
		`"turnComplete":true}}`
	if got := string(frames[3]); got != want {
		t.Errorf("the second cue was sent as\n got: %s\nwant: %s", got, want)
	}
}

// What a phase added is the difference between the instructions the session was
// started with and the ones it has now. The comparison is by rune: half the
// instructions in this repository are Chinese, and a prefix ending inside a
// character would put a broken one at the front of what the model is told.
func TestThePhaseSegmentIsWhatWasAddedToTheStandingInstructions(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		updated string
		want    string
	}{
		{name: "a phase appended to the persona",
			base: "You answer the phone.", updated: "You answer the phone.\n\nNow close the call.",
			want: "Now close the call."},
		{name: "nothing was added", base: "You answer the phone.",
			updated: "You answer the phone.", want: ""},
		{name: "nothing in common is carried whole",
			base: "You answer the phone.", updated: "Take a message and hang up.",
			want: "Take a message and hang up."},
		{name: "instructions that diverge part way were rewritten, not extended",
			base: "You answer the phone for NovaNet.", updated: "You answer the phone.",
			want: "You answer the phone."},
		{name: "a Chinese phase is not cut inside a character",
			base: "你是 NovaNet 的电话客服。", updated: "你是 NovaNet 的电话客服。\n\n现在请结束通话。",
			want: "现在请结束通话。"},
		{name: "a Chinese update sharing no prefix",
			base: "你是电话客服。", updated: "请记录留言。", want: "请记录留言。"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := phaseSegment(testCase.base, testCase.updated); got != testCase.want {
				t.Errorf("the phase segment is %q, want %q", got, testCase.want)
			}
		})
	}
}

// A line the flow chose is asked for the same way the opening one is, and it
// pre-empts: on this protocol a user turn with turnComplete stops whatever is
// being said, unconditionally.
func TestSpeakTextAsksForTheLineVerbatim(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	if err := session.SpeakText("I am transferring you now.", false); err != nil {
		t.Fatalf("speak text: %v", err)
	}

	frames := f.settledFrames(3)
	want := `{"clientContent":{"turns":[{"role":"user","parts":[{"text":` +
		`"Say exactly this, word for word, and add nothing else:\n` +
		`I am transferring you now."}]}],"turnComplete":true}}`
	if got := string(frames[2]); got != want {
		t.Errorf("the line was asked for as\n got: %s\nwant: %s", got, want)
	}
}
