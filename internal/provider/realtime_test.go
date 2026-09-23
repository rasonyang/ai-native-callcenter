// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

func basicConfig() SessionConfig {
	return SessionConfig{
		Instructions: "You answer the phone for NovaNet.",
		Language:     "en",
		Turn:         DefaultTurnDetection(),
		Tools: []ToolSpec{{
			Name:        "transfer_to_agent",
			Description: "Hand the caller to a person.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"queue":{"type":"string"}}}`),
		}},
		InputFormat:  media.G711Format(media.LawMu),
		OutputFormat: media.G711Format(media.LawMu),
	}
}

//
// Profiles.
//

// Which provider answers is a deployment setting. Nothing about a call — its
// language least of all — may reach into this choice.
func TestProfileIsChosenByNameNotLanguage(t *testing.T) {
	openai, err := ProfileFor(NameOpenAI, Override{})
	if err != nil || openai.Name != NameOpenAI {
		t.Fatalf("ProfileFor(openai) = %q, %v", openai.Name, err)
	}
	qwen, err := ProfileFor(NameQwen, Override{})
	if err != nil || qwen.Name != NameQwen {
		t.Fatalf("ProfileFor(qwen) = %q, %v", qwen.Name, err)
	}
	// Spelling is an operator's input, so it is forgiving about case and space.
	if got, err := ProfileFor("  QWEN ", Override{}); err != nil || got.Name != NameQwen {
		t.Errorf("ProfileFor(\"  QWEN \") = %q, %v", got.Name, err)
	}
	// An unknown name fails at startup rather than on the first call.
	if _, err := ProfileFor("nonesuch", Override{}); err == nil {
		t.Error("an unknown provider name was accepted")
	}
	if _, err := ProfileFor("", Override{}); err == nil {
		t.Error("an empty provider name was accepted")
	}
}

// A deployment that cannot reach the vendor directly — a proxy, a regional
// host, or a gateway that merely speaks the protocol — has to be able to say so.
func TestConnectionDetailsCanBeOverridden(t *testing.T) {
	endpoint, err := ProfileFor(NameOpenAI, Override{Endpoint: "wss://gateway.internal/realtime"})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Endpoint != "wss://gateway.internal/realtime" {
		t.Errorf("endpoint = %q, want the override", endpoint.Endpoint)
	}
	if endpoint.Model != OpenAIProfile().Model {
		t.Errorf("an empty field replaced the model with %q", endpoint.Model)
	}

	model, err := ProfileFor(NameQwen, Override{Model: "qwen-audio-3.0-realtime-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if model.Model != "qwen-audio-3.0-realtime-flash" {
		t.Errorf("model = %q, want the override", model.Model)
	}
	if model.Endpoint != QwenProfile().Endpoint {
		t.Errorf("an empty field replaced the endpoint with %q", model.Endpoint)
	}

	// The model still selects on the connection address, wherever it points.
	if url := model.endpointURL(); !strings.Contains(url, "model=qwen-audio-3.0-realtime-flash") {
		t.Errorf("connection url %q does not carry the overridden model", url)
	}
}

// The whole point of the passthrough path: where the provider takes G.711, the
// call does no conversion at all.
func TestFormatsForFollowTheNegotiatedLaw(t *testing.T) {
	input, output := OpenAIProfile().FormatsFor(media.LawAlaw)
	if input != media.G711Format(media.LawAlaw) || output != media.G711Format(media.LawAlaw) {
		t.Errorf("A-law call got %s in / %s out, want A-law both ways", input, output)
	}

	input, output = QwenProfile().FormatsFor(media.LawMu)
	if input != media.PCM16Format(media.RateProviderIn) {
		t.Errorf("input format = %s, want linear at the provider's input rate", input)
	}
	if output != media.PCM16Format(media.RateProviderOut) {
		t.Errorf("output format = %s, want linear at the provider's output rate", output)
	}
}

//
// Session configuration payloads.
//

func TestSessionUpdateInTheCurrentDialect(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	update := f.awaitMessage("session.update")

	if got := nested(t, update, "session", "instructions"); got != basicConfig().Instructions {
		t.Errorf("instructions = %v", got)
	}
	// G.711 is named as a format in its own right, not as linear audio.
	if got := nested(t, update, "session", "audio", "input", "format", "type"); got != "audio/pcmu" {
		t.Errorf("input format = %v, want audio/pcmu", got)
	}
	if got := nested(t, update, "session", "audio", "output", "format", "type"); got != "audio/pcmu" {
		t.Errorf("output format = %v, want audio/pcmu", got)
	}
	// Companded audio has one rate by definition; stating it would be noise.
	if format, ok := nested(t, update, "session", "audio", "input", "format").(map[string]any); ok {
		if _, present := format["rate"]; present {
			t.Error("a rate was sent alongside a companded format")
		}
	}
	if got := nested(t, update, "session", "audio", "output", "voice"); got != "marin" {
		t.Errorf("voice = %v", got)
	}

	turn := nested(t, update, "session", "audio", "input", "turn_detection").(map[string]any)
	if turn["type"] != "server_vad" || turn["silence_duration_ms"] != float64(500) {
		t.Errorf("turn detection = %v, want server_vad holding 500ms", turn)
	}

	tools := nested(t, update, "session", "tools").([]any)
	if len(tools) != 1 {
		t.Fatalf("sent %d tools", len(tools))
	}
	// The flat shape, which both providers were verified to accept.
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "transfer_to_agent" {
		t.Errorf("tool = %v, want the flat function shape", tool)
	}
	if _, isNested := tool["function"]; isNested {
		t.Error("the tool was sent in the nested shape")
	}
}

func TestSessionUpdateInTheOlderDialect(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	update := f.awaitMessage("session.update")

	if got := nested(t, update, "session", "input_audio_format"); got != "pcm" {
		t.Errorf("input format = %v, want the flat pcm name", got)
	}
	if got := nested(t, update, "session", "voice"); got != "longanqian" {
		t.Errorf("voice = %v", got)
	}
	if _, hasAudioBlock := nested(t, update, "session").(map[string]any)["audio"]; hasAudioBlock {
		t.Error("the newer nested audio block was sent to a provider using the older dialect")
	}
	modalities := nested(t, update, "session", "modalities").([]any)
	if len(modalities) != 2 {
		t.Errorf("modalities = %v", modalities)
	}
}

// Semantic turn taking is forced to a two-second hold on one provider, and any
// value we send is ignored. Sending one anyway would make the configuration
// claim a latency the call will not have.
func TestSemanticTurnDetectionOmitsAHoldTheProviderWouldIgnore(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Turn = TurnDetection{Mode: TurnModeSemantic, SilenceMs: 500}
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	turn := nested(t, f.awaitMessage("session.update"), "session", "turn_detection").(map[string]any)
	if turn["type"] != "smart_turn" {
		t.Errorf("turn type = %v, want this vendor's semantic mode", turn["type"])
	}
	if _, present := turn["silence_duration_ms"]; present {
		t.Error("a silence hold was sent for a mode that overrides it")
	}
}

func TestSemanticTurnDetectionKeepsTheHoldWhereItIsHonoured(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	cfg := basicConfig()
	cfg.Turn = TurnDetection{Mode: TurnModeSemantic, SilenceMs: 400}
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	turn := nested(t, f.awaitMessage("session.update"),
		"session", "audio", "input", "turn_detection").(map[string]any)
	if turn["type"] != "semantic_vad" {
		t.Errorf("turn type = %v", turn["type"])
	}
	if turn["silence_duration_ms"] != float64(400) {
		t.Errorf("silence hold = %v, want it passed through", turn["silence_duration_ms"])
	}
}

//
// Handshake.
//

func TestStartWaitsForConfirmationThenAsksForTheOpeningTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	awaitEvent(t, session, EventTypeSessionReady)
	f.awaitMessage("response.create")

	// The confirmation must precede the request, or the greeting is generated
	// under the provider's defaults rather than ours.
	sent := typesOf(f.messages())
	if len(sent) < 2 || sent[0] != "session.update" {
		t.Errorf("client sent %v, want the configuration first", sent)
	}
}

// One provider refuses to speak into an empty conversation, so the greeting
// has to be prompted. Verified live: without this the opening turn is rejected
// with "conversation has no messages or no user message" and the caller is met
// with silence.
func TestOpeningTurnIsPromptedWhereTheProviderNeedsIt(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	if item["role"] != "user" {
		t.Errorf("cue sent with role %v, want user", item["role"])
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "input_text" {
		t.Errorf("cue content type = %v", content["type"])
	}
	if !strings.Contains(content["text"].(string), "问候") {
		t.Errorf("cue is not in the session's language: %v", content["text"])
	}

	// The cue must precede the request, or it does not help. Wait for the
	// request before reading the record: Start returns once both have been
	// sent, but the fake logs them as they arrive, and an assertion landing in
	// that window reads a request still in flight as one never sent (CI went
	// red on a docs-only commit, 2026-09-15).
	f.awaitMessage("response.create")
	sent := typesOf(f.messages())
	cueAt, requestAt := indexOf(sent, "conversation.item.create"), indexOf(sent, "response.create")
	if cueAt < 0 || requestAt < 0 || cueAt > requestAt {
		t.Errorf("client sent %v, want the cue before the request", sent)
	}
}

// Where the provider greets unprompted, injecting a fake user turn would put
// words in the caller's mouth and into the transcript.
func TestNoCueIsSentWhereTheProviderDoesNotNeedOne(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessage("response.create")
	f.refuteMessage("conversation.item.create")
}

func TestGreetingCueCanBeOverridden(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.GreetingCue = "(the caller is calling about an outage)"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	if content["text"] != cfg.GreetingCue {
		t.Errorf("cue = %v, want the configured one", content["text"])
	}
}

//
// Spoken lines: the opening one, and the ones a flow decides on mid-call.
//

// The opening frames are the ones every existing call already sends, and a
// flow that names no opening line must go on sending exactly those: one bare
// request, carrying nothing.
func TestWithNoOpeningLineTheOpeningRequestIsUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile Profile
	}{
		{"GA dialect", OpenAIProfile()},
		{"older dialect", QwenProfile()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, tt.profile)

			cfg := basicConfig()
			cfg.InputFormat, cfg.OutputFormat = tt.profile.FormatsFor(media.LawMu)
			if err := session.Start(t.Context(), cfg); err != nil {
				t.Fatalf("start: %v", err)
			}

			request := f.awaitMessages("response.create", 1)[0]
			if len(request) != 1 {
				t.Errorf("response.create = %v, want the bare request and nothing else", request)
			}
		})
	}
}

// Where the flow owns the opening words, the request carries them and asks for
// them as written. This provider greets unprompted, so nothing is put into the
// conversation: a synthetic user turn would land in the caller's transcript.
func TestAnOpeningLineIsAskedForAsWritten(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	cfg := basicConfig()
	cfg.OpeningText = "Thanks for calling NovaNet, how can I help you today?"
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	request := f.awaitMessages("response.create", 1)[0]
	direction, ok := nested(t, request, "response", "instructions").(string)
	if !ok {
		t.Fatalf("the opening request carries no instructions: %v", request)
	}
	if !strings.Contains(direction, cfg.OpeningText) {
		t.Errorf("the opening request does not carry the line: %q", direction)
	}
	if !strings.Contains(direction, "word for word") {
		t.Errorf("the opening request does not ask for the line as written: %q", direction)
	}
	f.awaitMessages("conversation.item.create", 0)
}

// The other provider refuses to answer an empty conversation, and the cue it
// needs is the direction itself: there is no second thing to say, and a cue
// that said something else would be steering the turn two ways at once.
func TestAnOpeningLineIsAlsoTheCueWhereTheConversationMayNotBeEmpty(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.OpeningText = "感谢致电 NovaNet，请问有什么可以帮您？"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	request := f.awaitMessages("response.create", 1)[0]
	item := f.awaitMessages("conversation.item.create", 1)[0]

	direction := nested(t, request, "response", "instructions").(string)
	if !strings.Contains(direction, cfg.OpeningText) {
		t.Errorf("the opening request does not carry the line: %q", direction)
	}
	content := nested(t, item, "item", "content").([]any)[0].(map[string]any)
	if content["text"] != direction {
		t.Errorf("cue = %v, want the same direction the request carries", content["text"])
	}
	if !strings.Contains(direction, "一字不差") {
		t.Errorf("the direction is not in the session's language: %q", direction)
	}

	sent := typesOf(f.messages())
	cueAt, requestAt := indexOf(sent, "conversation.item.create"), indexOf(sent, "response.create")
	if cueAt < 0 || requestAt < 0 || cueAt > requestAt {
		t.Errorf("client sent %v, want the cue before the request", sent)
	}
}

// With the floor free there is nothing to wait for and nothing to stop.
func TestSpeakingWithTheFloorFreeAsksAtOnce(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	if err := session.SpeakText("I am putting you through now.", true); err != nil {
		t.Fatalf("speak: %v", err)
	}

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the request does not carry the line: %q", direction)
	}
	f.awaitMessages("response.cancel", 0)
	// This profile is steered by the override alone, a closing line
	// included; nothing enters the conversation on its behalf.
	if items := f.messagesOfType("conversation.item.create"); len(items) != 0 {
		t.Errorf("the client sent %d conversation items, want none", len(items))
	}
}

// A line pre-empts. The turn in progress is stopped first, and the request for
// the line waits for that turn to end — asking for a second response while one
// is open is refused outright.
func TestSpeakingOverAnOpenResponseStopsItFirstAndWaits(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	// Only the opening request so far: the floor is still taken.
	f.awaitMessages("response.create", 1)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the request does not carry the line: %q", direction)
	}
	f.awaitMessages("response.cancel", 1)
	if items := f.messagesOfType("conversation.item.create"); len(items) != 0 {
		t.Errorf("the client sent %d conversation items, want none", len(items))
	}
}

// qwenConfig is basicConfig as a Chinese call on the qwen profile.
func qwenConfig() SessionConfig {
	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	return cfg
}

// startQwenCall starts a qwen session whose opening turn has finished, and
// returns once the opening cue — the one item this profile always sends — is
// on the wire.
func startQwenCall(t *testing.T) (*fakeProvider, *Realtime) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), qwenConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)
	f.awaitMessages("conversation.item.create", 1)
	return f, session
}

// assertLineIsAlsoInTheConversation checks the last conversation item and the
// last request against each other: the item is a caller message carrying the
// same direction the request carries as its override, and it went first.
func assertLineIsAlsoInTheConversation(t *testing.T, f *fakeProvider,
	items, requests []map[string]any, line string) {
	t.Helper()

	request, item := requests[len(requests)-1], items[len(items)-1]
	direction := nested(t, request, "response", "instructions").(string)
	if !strings.Contains(direction, line) || !strings.Contains(direction, "一字不差") {
		t.Errorf("the request does not ask for the line as written: %q", direction)
	}
	if role := nested(t, item, "item", "role"); role != "user" {
		t.Errorf("item role = %v, want user", role)
	}
	content := nested(t, item, "item", "content").([]any)[0].(map[string]any)
	if content["type"] != "input_text" || content["text"] != direction {
		t.Errorf("item content = %v, want input_text carrying the request's direction", content)
	}

	sent := typesOf(f.messages())
	lastItem, lastRequest := -1, -1
	for i, messageType := range sent {
		switch messageType {
		case "conversation.item.create":
			lastItem = i
		case "response.create":
			lastRequest = i
		}
	}
	if lastItem < 0 || lastRequest < 0 || lastItem > lastRequest {
		t.Errorf("client sent %v, want the line's item before the line's request", sent)
	}
}

// On qwen the override alone lost to the conversation already there — a
// dead-air goodbye came out as a third "are you still there" (W-Q1). A line
// that ends the call is put in the conversation as well, ahead of the request
// for it.
func TestAClosingLineIsAlsoPutInTheConversationWhereTheOverrideAloneLoses(t *testing.T) {
	f, session := startQwenCall(t)

	if err := session.SpeakText("感谢来电，再见。", true); err != nil {
		t.Fatalf("speak: %v", err)
	}

	requests := f.awaitMessages("response.create", 2)
	items := f.messagesOfType("conversation.item.create")
	if len(items) != 2 {
		t.Fatalf("the client sent %d items, want the opening cue and the line's", len(items))
	}
	assertLineIsAlsoInTheConversation(t, f, items, requests, "感谢来电，再见。")
	if cancels := f.messagesOfType("response.cancel"); len(cancels) != 0 {
		t.Errorf("the client sent %d cancels with the floor free", len(cancels))
	}
}

// A line in a phase the conversation goes on from keeps the override alone,
// even on qwen: the item would stay in the history, and what it does to the
// turns after it has not been measured.
func TestALineTheCallGoesOnFromStaysOutOfTheConversation(t *testing.T) {
	f, session := startQwenCall(t)

	if err := session.SpeakText("请稍等，我帮您查一下。", false); err != nil {
		t.Fatalf("speak: %v", err)
	}

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "请稍等，我帮您查一下。") {
		t.Errorf("the request does not carry the line: %q", direction)
	}
	if items := f.messagesOfType("conversation.item.create"); len(items) != 1 {
		t.Errorf("the client sent %d items, want only the opening cue", len(items))
	}
}

// The same line asked for over an open response pre-empts exactly as before —
// cancel now, ask when the turn has ended — and nothing enters the conversation
// until the line is actually asked for.
func TestAClosingLineThatWaitedForTheFloorIsAlsoPutInTheConversation(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), qwenConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.SpeakText("感谢来电，再见。", true); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	// Only the opening turn's cue and request so far: the floor is still taken.
	if items, requests := f.messagesOfType("conversation.item.create"),
		f.messagesOfType("response.create"); len(items) != 1 || len(requests) != 1 {
		t.Fatalf("with the floor taken the client sent %d items and %d requests, want 1 and 1",
			len(items), len(requests))
	}

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	requests := f.awaitMessages("response.create", 2)
	items := f.messagesOfType("conversation.item.create")
	if len(items) != 2 {
		t.Fatalf("the client sent %d items, want the opening cue and the line's", len(items))
	}
	assertLineIsAlsoInTheConversation(t, f, items, requests, "感谢来电，再见。")
	if cancels := f.messagesOfType("response.cancel"); len(cancels) != 1 {
		t.Errorf("the client sent %d cancels, want the one that made room", len(cancels))
	}
}

// The floor is claimed before the line's first frame goes out, so a second
// line asked for while the first one's frames are still being written waits
// for the floor instead of asking for a second response the provider would
// refuse ("conversation already has an active response").
func TestASecondLineWhileTheFirstIsBeingAskedForWaits(t *testing.T) {
	f, session := startQwenCall(t)

	// Claimed exactly as SpeakText claims it, and left claimed: the window
	// between the claim and the request going out, held open.
	session.mu.Lock()
	session.claimFloorLocked()
	session.mu.Unlock()

	if err := session.SpeakText("感谢来电，再见。", true); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 1) // the opening turn's only
	session.mu.Lock()
	pending := session.pendingSpeak
	session.mu.Unlock()
	if pending != "感谢来电，再见。" {
		t.Errorf("pendingSpeak = %q, want the line waiting for the floor", pending)
	}
}

// A line that could not be asked for gives the floor back: otherwise every
// later line would wait for a response that was never requested.
func TestALineThatCouldNotBeAskedForReleasesTheFloor(t *testing.T) {
	_, session := startQwenCall(t)
	session.conn.Close()

	if err := session.SpeakText("感谢来电，再见。", true); err == nil {
		t.Fatal("a line asked for on a closed connection reported success")
	}
	session.mu.Lock()
	isRequested := session.isResponseRequested
	session.mu.Unlock()
	if isRequested {
		t.Error("the floor is still claimed for a line that was never asked for")
	}
}

// The gap the flow actually falls into: a tool result asks for a turn on its
// way out, and the phase change that follows asks for a line before the
// provider has created it. There is nothing to cancel yet, and asking again
// would be refused, so the line waits for the turn to exist and stops it then.
func TestALineAskedForBeforeTheProviderAnsweredWaitsForTheTurnToExist(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":1}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 0)
	f.awaitMessages("response.create", 2)

	f.send(map[string]any{"type": "response.created"})
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	requests := f.awaitMessages("response.create", 3)
	direction := nested(t, requests[2], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the request does not carry the line: %q", direction)
	}
}

// Two lines are not a queue. Both describe what should be said next, so the
// later one is the only one still true by the time the floor comes free.
func TestASecondLineReplacesTheFirstRatherThanQueueingBehindIt(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.SpeakText("One moment please.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak again: %v", err)
	}
	// One cancel, not one per line: the turn only has to be stopped once.
	f.awaitMessages("response.cancel", 1)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the request does not carry the later line: %q", direction)
	}
	if strings.Contains(direction, "One moment please.") {
		t.Errorf("the superseded line was spoken as well: %q", direction)
	}
}

// A turn we stopped ourselves is still an interruption — the caller stops
// hearing it, and how much they heard has to reach the provider's history —
// but it is not the caller's. Reporting it as speech would put a barge-in the
// caller never made into the log of the call.
func TestATurnStoppedForALineIsNotBlamedOnTheCaller(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	event := awaitEvent(t, session, EventTypeInterrupted)
	if event.InterruptedBy != InterruptReasonSystem {
		t.Errorf("interruptedBy = %q, want the application's own doing", event.InterruptedBy)
	}

	// And the next cancelled turn is the caller's again: the attribution is
	// spent on the turn it belonged to.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	event = awaitEvent(t, session, EventTypeInterrupted)
	if event.InterruptedBy != InterruptReasonSpeech {
		t.Errorf("interruptedBy = %q, want the caller", event.InterruptedBy)
	}
}

// finishTheOpeningTurn plays the greeting out, so a test about a later line
// starts with the floor free.
func finishTheOpeningTurn(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.awaitMessages("response.create", 1)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func TestStartFailsWhenTheConfigurationIsRejected(t *testing.T) {
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		if message["type"] == "session.update" {
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_value",
				"message": "voice is not available", "param": "session.voice",
			}})
		}
	})
	session := testSession(t, f, OpenAIProfile())

	err := session.Start(t.Context(), basicConfig())
	if err == nil {
		t.Fatal("Start succeeded against a provider that rejected the session")
	}
	if !strings.Contains(err.Error(), "voice is not available") {
		t.Errorf("error = %v, want the provider's reason", err)
	}
}

// A rejected configuration loses everything — persona, tools and all — so it
// is worth one retry without the optional parts before abandoning the call.
func TestRejectedConfigurationIsRetriedOnceWithoutOptionalFields(t *testing.T) {
	attempts := 0
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		if message["type"] != "session.update" {
			return
		}
		attempts++
		if attempts == 1 {
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"code": "invalid_value", "message": "transcription is unsupported",
				"param": "session.update",
			}})
			return
		}
		f.send(map[string]any{"type": "session.updated"})
	})

	profile := OpenAIProfile()
	profile.TranscribeModel = "whisper-1"
	session := testSession(t, f, profile)

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start after retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("the configuration was sent %d times, want exactly one retry", attempts)
	}

	updates := 0
	for _, message := range f.messages() {
		if message["type"] != "session.update" {
			continue
		}
		updates++
		input := nested(t, message, "session", "audio", "input").(map[string]any)
		_, hasTranscription := input["transcription"]
		if updates == 1 && !hasTranscription {
			t.Error("the first attempt already omitted the optional field")
		}
		if updates == 2 {
			if hasTranscription {
				t.Error("the retry repeated the field that was rejected")
			}
			// The business persona must survive the retry; losing it would
			// leave the caller talking to a generic assistant.
			if got := nested(t, message, "session", "instructions"); got != basicConfig().Instructions {
				t.Errorf("the retry lost the instructions: %v", got)
			}
			if _, hasTools := nested(t, message, "session").(map[string]any)["tools"]; !hasTools {
				t.Error("the retry lost the tools")
			}
		}
	}
}

//
// Audio and events.
//

func TestSendAudioWireShape(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	frame := []byte{0xFF, 0x00, 0x7F, 0x80}
	if err := session.SendAudio(frame); err != nil {
		t.Fatalf("send audio: %v", err)
	}

	message := f.awaitMessage("input_audio_buffer.append")
	encoded, ok := message["audio"].(string)
	if !ok {
		t.Fatalf("audio field is %T", message["audio"])
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the hand-built JSON produced invalid base64: %v", err)
	}
	if string(decoded) != string(frame) {
		t.Errorf("audio round-tripped as %v, want %v", decoded, frame)
	}
}

func TestEventMapping(t *testing.T) {
	tests := []struct {
		name  string
		wire  map[string]any
		check func(*testing.T, Event)
		want  EventType
	}{
		{
			name: "caller started speaking",
			wire: map[string]any{"type": "input_audio_buffer.speech_started"},
			want: EventTypeSpeechStarted,
		},
		{
			name: "caller stopped speaking",
			wire: map[string]any{"type": "input_audio_buffer.speech_stopped"},
			want: EventTypeSpeechStopped,
		},
		{
			name: "model audio, current event name",
			wire: map[string]any{"type": "response.output_audio.delta",
				"delta": base64.StdEncoding.EncodeToString([]byte("hello"))},
			want: EventTypeAudioDelta,
			check: func(t *testing.T, e Event) {
				if string(e.Audio) != "hello" {
					t.Errorf("audio = %q", e.Audio)
				}
			},
		},
		{
			// The older name is still what some vendors emit.
			name: "model audio, older event name",
			wire: map[string]any{"type": "response.audio.delta",
				"delta": base64.StdEncoding.EncodeToString([]byte("world"))},
			want: EventTypeAudioDelta,
			check: func(t *testing.T, e Event) {
				if string(e.Audio) != "world" {
					t.Errorf("audio = %q", e.Audio)
				}
			},
		},
		{
			name: "what the caller said",
			wire: map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"transcript": "I need to check my bill",
			},
			want: EventTypeInputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "I need to check my bill" || !e.IsFinal {
					t.Errorf("transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "what the model said",
			wire: map[string]any{"type": "response.output_audio_transcript.done",
				"transcript": "Certainly."},
			want: EventTypeOutputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "Certainly." || !e.IsFinal {
					t.Errorf("transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "partial output transcript",
			wire: map[string]any{"type": "response.output_audio_transcript.delta",
				"delta": "Cert"},
			want: EventTypeOutputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "Cert" || e.IsFinal {
					t.Errorf("partial transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "tool call",
			wire: map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "fc_1", "name": "transfer_to_agent", "arguments": `{"queue":"billing"}`},
			want: EventTypeToolCall,
			check: func(t *testing.T, e Event) {
				if e.ToolCallID != "fc_1" || e.ToolName != "transfer_to_agent" {
					t.Errorf("tool call = %s/%s", e.ToolCallID, e.ToolName)
				}
				if e.ToolArgs != `{"queue":"billing"}` {
					t.Errorf("arguments = %s", e.ToolArgs)
				}
			},
		},
		{
			name: "tool call with no arguments",
			wire: map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "fc_2", "name": "hangup", "arguments": ""},
			want: EventTypeToolCall,
			check: func(t *testing.T, e Event) {
				// An empty string is not valid JSON, and the engine parses it.
				if e.ToolArgs != "{}" {
					t.Errorf("arguments = %q, want an empty object", e.ToolArgs)
				}
			},
		},
		{
			name: "turn finished",
			wire: map[string]any{"type": "response.done", "response": map[string]any{
				"status": "completed",
				"usage":  map[string]any{"input_tokens": 12, "output_tokens": 30, "total_tokens": 42},
			}},
			want: EventTypeResponseDone,
			check: func(t *testing.T, e Event) {
				if e.Status != "completed" {
					t.Errorf("status = %q", e.Status)
				}
				if e.Usage.TotalTokens != 42 {
					t.Errorf("usage = %+v", e.Usage)
				}
			},
		},
		{
			name: "provider error",
			wire: map[string]any{"type": "error", "error": map[string]any{
				"code": "rate_limit_exceeded", "message": "slow down"}},
			want: EventTypeError,
			check: func(t *testing.T, e Event) {
				if e.Text != "slow down" {
					t.Errorf("message = %q", e.Text)
				}
				// Recoverable: the session is still usable.
				if e.IsFatal {
					t.Error("a recoverable error was marked fatal")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, OpenAIProfile())
			if err := session.Start(t.Context(), basicConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			f.send(tt.wire)
			event := awaitEvent(t, session, tt.want)
			if tt.check != nil {
				tt.check(t, event)
			}
		})
	}
}

func TestUndecodableAudioIsReportedRatherThanPlayed(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.sendRaw(`{"type":"response.output_audio.delta","delta":"not!base64!"}`)

	event := awaitEvent(t, session, EventTypeError)
	if event.IsFatal {
		t.Error("one bad frame killed the session")
	}
}

//
// Barge-in.
//

// Where the provider stops on its own, telling it again would be noise; what
// it does need is how much the caller actually heard.
func TestInterruptOnAProviderThatCancelsItself(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_7", "type": "message"}})
	time.Sleep(50 * time.Millisecond)

	if err := session.Interrupt(InterruptReasonSpeech, 640); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	truncate := f.awaitMessage("conversation.item.truncate")
	if truncate["item_id"] != "item_7" {
		t.Errorf("truncated %v, want the response the caller was hearing", truncate["item_id"])
	}
	if truncate["audio_end_ms"] != float64(640) {
		t.Errorf("audio_end_ms = %v, want what was actually played", truncate["audio_end_ms"])
	}
	f.refuteMessage("response.cancel")
}

// A turn that speaks before it calls a tool adds a second output item, and the
// caller can barge in over the sentence that preceded the call. Truncation has
// to name the audio the caller was hearing; naming the function call instead is
// rejected by the provider, and the model then believes a cut-off sentence was
// heard in full.
func TestBargeInTruncatesTheSpokenItemNotTheToolCall(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_7", "type": "message"}})
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_8", "type": "function_call"}})
	time.Sleep(50 * time.Millisecond)

	if err := session.Interrupt(InterruptReasonSpeech, 640); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	truncate := f.awaitMessage("conversation.item.truncate")
	if truncate["item_id"] != "item_7" {
		t.Errorf("truncated %v, want the audio the caller was hearing", truncate["item_id"])
	}
}

// Where it does not, a missed cancel leaves the model talking over the caller.
func TestInterruptOnAProviderThatMustBeTold(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	// A response has to be in progress for there to be one to cancel.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.Interrupt(InterruptReasonDTMF, 0); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	f.awaitMessage("response.cancel")
}

// The provider's response can end in the round trip between Interrupt reading
// it as open and the cancel arriving, and qwen then refuses the cancel. That
// race cannot be closed from this side; the refusal is the benign answer to our
// own request and must not surface as an error the call logs at WARN.
func TestARefusedCancelThatLostTheRaceIsNotAnError(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessage("response.cancel")

	// The response had already finished on the provider's side.
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_value",
		"message": "Conversation has no active response",
	}})
	// A marker that arrives after the refusal: anything the refusal produced
	// would be in front of it.
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})

	select {
	case event := <-session.Events():
		if event.Type != EventTypeSpeechStarted {
			t.Fatalf("got %s (%v), want the refused cancel to pass silently", event.Type, event.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived after the refused cancel")
	}

	// The session carries on as before.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
}

// The same words are a real error when this client sent no cancel, and any
// other error after a cancel is still reported: the match is the refusal of
// our own request and nothing wider.
func TestOnlyTheRefusalOfOurOwnCancelIsSwallowed(t *testing.T) {
	tests := []struct {
		name         string
		isCancelSent bool
		code         string
		message      string
	}{
		{"no cancel was sent", false, "invalid_value", "Conversation has no active response"},
		{"another error after a cancel", true, "invalid_value",
			"Cannot create response while another response is in progress"},
		{"another code after a cancel", true, "rate_limit_exceeded", "no active response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, QwenProfile())

			cfg := basicConfig()
			cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
			if err := session.Start(t.Context(), cfg); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			if tt.isCancelSent {
				f.send(map[string]any{"type": "response.created"})
				awaitEvent(t, session, EventTypeResponseStarted)
				if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
					t.Fatalf("interrupt: %v", err)
				}
				f.awaitMessage("response.cancel")
			}
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "code": tt.code, "message": tt.message,
			}})

			event := awaitEvent(t, session, EventTypeError)
			if event.Text != tt.message {
				t.Errorf("message = %q, want %q", event.Text, tt.message)
			}
			if event.IsFatal {
				t.Error("a recoverable error was marked fatal")
			}
		})
	}
}

// Cancelling and trimming answer different questions, and the second outlives
// the first. The caller goes on hearing an utterance for seconds after the
// provider finished making it, so speech over that tail has to trim the
// history — while there is no longer any response to cancel, and asking to
// cancel one is an error on the vendors that take the cancel at all.
func TestInterruptAfterTheResponseEndedTrimsWithoutCancelling(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_3", "type": "message"}})
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)

	if err := session.Interrupt(InterruptReasonSpeech, 1200); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	truncate := f.awaitMessage("conversation.item.truncate")
	if truncate["item_id"] != "item_3" || truncate["audio_end_ms"] != float64(1200) {
		t.Errorf("truncate = %v, want item_3 trimmed at what was heard", truncate)
	}
	f.refuteMessage("response.cancel")
}

// The interruption surfaces where the provider confirms it, not from the call
// that requested it. Emitting from Interrupt would deadlock: it is normally
// called from the goroutine draining this very stream.
func TestACancelledTurnIsReportedAsAnInterruption(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	event := awaitEvent(t, session, EventTypeInterrupted)
	if event.Status != "cancelled" {
		t.Errorf("status = %q", event.Status)
	}
}

// Interrupting must not block, including when called from the event consumer,
// which is where a barge-in is actually detected.
func TestInterruptDoesNotBlockTheEventConsumer(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	// Drain from one goroutine and interrupt from inside that same loop, which
	// is exactly how the bridge is wired.
	done := make(chan error, 1)
	go func() {
		for event := range session.Events() {
			if event.Type == EventTypeSpeechStarted {
				done <- session.Interrupt(InterruptReasonSpeech, 200)
				return
			}
		}
		done <- errors.New("the stream ended before speech was detected")
	}()

	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupt from the consumer goroutine: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupting from the event consumer deadlocked")
	}
}

//
// Tool results and steering.
//

func TestToolResultCarriesTheHintAndAsksForTheNextTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	err := session.SendToolResult("fc_1", `{"ok":1,"balance":"42.10"}`,
		"Tell the caller the balance, then ask if they want to pay now.")
	if err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	if item["call_id"] != "fc_1" {
		t.Errorf("call_id = %v", item["call_id"])
	}

	var output map[string]any
	if err := json.Unmarshal([]byte(item["output"].(string)), &output); err != nil {
		t.Fatalf("the tool output is not valid JSON: %v", err)
	}
	if output["balance"] != "42.10" {
		t.Errorf("the result lost its own fields: %v", output)
	}
	if !strings.Contains(output["hint"].(string), "ask if they want to pay") {
		t.Errorf("the hint did not reach the model: %v", output["hint"])
	}

	// The model does not speak again until asked.
	f.awaitMessage("response.create")
}

//
// Tool results answered while the response that called the tool is still
// open (qwen-findings W-Q4).
//

// openATurnThatCallsATool plays the greeting out and opens the next turn, in
// which the model calls a tool and keeps going: the shape qwen was seen to
// produce, a function call followed by more speech in the same response.
func openATurnThatCallsATool(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	finishTheOpeningTurn(t, f, session)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_1", "name": "transfer_to_agent", "arguments": `{}`})
	awaitEvent(t, session, EventTypeToolCall)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_after_call", "type": "message"}})
	f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	awaitEvent(t, session, EventTypeAudioDelta)
}

// functionCallOutputs is the tool results the client put into the
// conversation, in the order it sent them.
func functionCallOutputs(f *fakeProvider) []map[string]any {
	var out []map[string]any
	for _, message := range f.messagesOfType("conversation.item.create") {
		if item, ok := message["item"].(map[string]any); ok && item["type"] == "function_call_output" {
			out = append(out, item)
		}
	}
	return out
}

// The live defect: the result is sent while the model is still talking in the
// response that called the tool, and a request for the next turn in that
// moment is refused. The result goes in at once; the request waits for the
// response to end, and then goes out after the result it answers.
func TestAToolResultSentWhileTheResponseIsOpenAsksForItsTurnWhenItEnds(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, "Say goodbye."); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	// The result goes in at once; only the opening request has been made,
	// because the floor is still taken.
	f.awaitMessages("conversation.item.create", 1)
	if got := len(functionCallOutputs(f)); got != 1 {
		t.Fatalf("the result was not put into the conversation at once: %d outputs", got)
	}
	f.awaitMessages("response.create", 1)
	f.awaitMessages("response.cancel", 0)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	done := awaitEvent(t, session, EventTypeResponseDone)
	if done.Status != "completed" {
		t.Errorf("status = %q", done.Status)
	}

	f.awaitMessages("response.create", 2)
	types := typesOf(f.messages())
	lastRequest := len(types) - 1 - indexOf(reversed(types), "response.create")
	lastItem := len(types) - 1 - indexOf(reversed(types), "conversation.item.create")
	if lastRequest < lastItem {
		t.Errorf("the turn was asked for ahead of the result it answers: %v", types)
	}
}

// With the floor free nothing waits: the result and the request go out together,
// as they always did.
func TestAToolResultWithTheFloorFreeAsksAtOnce(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	if got := len(functionCallOutputs(f)); got != 1 {
		t.Errorf("function_call_output items = %d, want 1", got)
	}
}

// Parallel tool calls in one response are answered in one turn: every result
// goes in, and the floor is asked for once.
func TestSeveralToolResultsForOneResponseAskForOneTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send first tool result: %v", err)
	}
	if err := session.SendToolResult("fc_2", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send second tool result: %v", err)
	}
	f.awaitMessages("response.create", 1)
	if got := len(functionCallOutputs(f)); got != 2 {
		t.Fatalf("function_call_output items = %d, want 2", got)
	}

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	f.awaitMessages("response.create", 2)

	// The released turn plays out, and nothing more is asked for: the
	// results were owed one turn between them, not one each.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.awaitMessages("response.create", 2)
}

// The caller barges in on the response that called the tool. The provider
// answers the caller with a response of its own, and that response reads the
// result that is already in the conversation; a request of ours on top of it
// would be refused. So nothing is asked for — neither when the cancelled
// response ends nor when the provider's own begins.
func TestACallerWhoBargesInIsAnsweredByTheProviderAloneAfterTheDone(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)
	// The caller is still talking: nothing is asked for.
	f.awaitMessages("response.create", 1)

	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)

	// The provider's reply was the turn the result was owed.
	f.awaitMessages("response.create", 1)
	f.awaitMessages("response.cancel", 1)
}

// The same, with the provider's own response created before the one that
// called the tool is reported done: that response is the owed turn, and the
// done that follows releases nothing.
func TestAResponseTheProviderStartsBeforeTheDoneDischargesTheOwedTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)

	f.awaitMessages("response.create", 1)
}

// A line asked for while a tool result's turn waits takes that turn: the line
// is said with the result in view, and the floor is asked for once, for the
// line.
func TestALineAskedForWhileAToolTurnWaitsIsTheOneTurnAskedFor(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	f.awaitMessages("response.create", 1)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	event := awaitEvent(t, session, EventTypeInterrupted)
	if event.InterruptedBy != InterruptReasonSystem {
		t.Errorf("interruptedBy = %q, want the application's own doing", event.InterruptedBy)
	}

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the one request is not the line: %q", direction)
	}
}

// While the turn is held for the provider's reply to a caller who is still
// speaking, the floor is spoken for: a line waits for that reply to exist,
// stops it, and is asked for when it ends — once.
func TestALineAskedForWhileTheToolTurnIsHeldWaitsForTheProvidersReply(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`, ""); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)

	if err := session.SpeakText("I am putting you through now.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 1)

	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("response.cancel", 2)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)

	requests := f.awaitMessages("response.create", 2)
	direction := nested(t, requests[1], "response", "instructions").(string)
	if !strings.Contains(direction, "I am putting you through now.") {
		t.Errorf("the one request is not the line: %q", direction)
	}
}

func reversed(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[len(values)-1-i] = v
	}
	return out
}

func TestMergeHint(t *testing.T) {
	tests := []struct {
		name   string
		output string
		hint   string
		want   string
	}{
		{"no hint leaves the result alone", `{"ok":1}`, "", `{"ok":1}`},
		{"hint joins a JSON result", `{"ok":1}`, "say hello", `{"hint":"say hello","ok":1}`},
		{"a plain result gets wrapped", `not json`, "say hello",
			`{"hint":"say hello","result":"not json"}`},
		{"a JSON array gets wrapped", `[1,2]`, "say hello",
			`{"hint":"say hello","result":"[1,2]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeHint(tt.output, tt.hint); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// Re-sending the whole configuration mid-call would re-assert turn detection,
// which at least one provider rejects once audio is flowing.
func TestUpdateInstructionsSendsOnlyTheInstructions(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessage("session.update")

	if err := session.UpdateInstructions("You are now confirming the appointment."); err != nil {
		t.Fatalf("update instructions: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		messages := f.messages()
		for _, message := range messages {
			if message["type"] != "session.update" {
				continue
			}
			session := message["session"].(map[string]any)
			if session["instructions"] != "You are now confirming the appointment." {
				continue
			}
			if _, hasAudio := session["audio"]; hasAudio {
				t.Error("the instruction update re-asserted the audio configuration")
			}
			if _, hasTools := session["tools"]; hasTools {
				t.Error("the instruction update re-sent the tools")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the instruction update never arrived")
}

//
// Failure handling.
//

// There is no reconnect: the provider holds conversation state that cannot be
// rebuilt, so the call has to go somewhere a person can take it.
func TestALostConnectionIsFatalAndClosesTheStream(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.hangUp()

	event := awaitEvent(t, session, EventTypeError)
	if !event.IsFatal {
		t.Error("a lost connection was not reported as fatal")
	}
	awaitEvent(t, session, EventTypeClosed)

	select {
	case _, ok := <-session.Events():
		if ok {
			t.Error("events continued after the session closed")
		}
	case <-time.After(time.Second):
		t.Error("the event stream was never closed")
	}
}

func TestSendingAfterCloseFails(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	_ = session.Close(t.Context())
	// Close is idempotent; the call teardown path may reach it twice.
	_ = session.Close(t.Context())

	if err := session.SendAudio([]byte{1, 2}); err == nil {
		t.Error("audio was accepted after the session closed")
	}
}

// A provider that accepts a turn and then goes quiet would otherwise leave the
// flow waiting for a completion that never comes, and the caller in silence.
func TestAnAbandonedTurnIsClosedOut(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	session.firstAudioDeadline = 150 * time.Millisecond
	session.deltaStallDeadline = 150 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	event := awaitEvent(t, session, EventTypeResponseDone)
	if event.Status != StatusStalled {
		t.Errorf("status = %q, want the turn closed out as stalled", event.Status)
	}
}

func TestATurnThatCompletesDoesNotTripTheWatchdog(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	session.firstAudioDeadline = 200 * time.Millisecond
	session.deltaStallDeadline = 200 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	f.send(map[string]any{"type": "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString([]byte("hi"))})
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})

	event := awaitEvent(t, session, EventTypeResponseDone)
	if event.Status != "completed" {
		t.Fatalf("status = %q", event.Status)
	}

	// Well past both deadlines, nothing further should be invented.
	time.Sleep(400 * time.Millisecond)
	select {
	case event := <-session.Events():
		t.Errorf("the watchdog fired on a completed turn: %+v", event)
	default:
	}
}

func TestMissingCredentialFailsBeforeAnyCall(t *testing.T) {
	profile := OpenAIProfile()
	t.Setenv(profile.APIKeyEnv, "")

	if _, err := New(profile, nil); err == nil {
		t.Error("a session was built with no credential")
	}
}

// A bot names its own voice; the profile's is only the fallback. Both dialects
// carry it, because the deployment that runs either one has bots of its own.
func TestTheSessionsVoiceOverridesTheProfiles(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile Profile
		path    []string
	}{
		{"GA dialect", OpenAIProfile(), []string{"session", "audio", "output", "voice"}},
		{"older dialect", QwenProfile(), []string{"session", "voice"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := basicConfig()
			cfg.Voice = "cherry"
			client := &Realtime{profile: tt.profile}

			update := client.buildSessionUpdate(cfg, false)
			if got := nested(t, update, tt.path...); got != "cherry" {
				t.Errorf("voice = %v, want the bot's own", got)
			}

			// Naming none leaves the provider's default in place.
			cfg.Voice = ""
			update = client.buildSessionUpdate(cfg, false)
			if got := nested(t, update, tt.path...); got != tt.profile.Voice {
				t.Errorf("voice = %v, want the profile's %q", got, tt.profile.Voice)
			}
		})
	}
}

// A turn arrives in a burst — fifty deltas back to back — and the watchdog's
// progress signals are deliberately droppable so the read loop never blocks on
// them. The completion signal rides in that same burst and can be dropped with
// the rest, which under sustained load had about 1% of perfectly healthy turns
// reported as abandoned mid-sentence.
func TestACompletedResponseIsNotAbandonedWhenItsSignalIsLost(t *testing.T) {
	fake := newFakeProvider(t, acceptSession)
	session := testSession(t, fake, OpenAIProfile())
	session.deltaStallDeadline = 100 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	fake.send(map[string]any{"type": "response.created"})
	fake.send(map[string]any{"type": "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(make([]byte, media.FrameSamples))})
	awaitEvent(t, session, EventTypeAudioDelta)

	// Fill the signal channel so the completion's own signal is dropped, which
	// is exactly what a burst does to it.
	for range cap(session.watch) {
		session.watch <- watchAudioArrived
	}
	fake.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-session.Events():
			switch {
			case event.Type == EventTypeResponseDone && event.Status == StatusStalled:
				t.Fatal("a response that completed cleanly was reported as abandoned")
			case event.Type == EventTypeResponseDone:
				// The real completion arrived. Give the timer its chance to
				// fire behind it before declaring the test won.
				time.Sleep(3 * session.deltaStallDeadline)
				select {
				case late := <-session.Events():
					if late.Status == StatusStalled {
						t.Fatal("the turn was abandoned after it had already completed")
					}
				default:
				}
				return
			}
		case <-deadline:
			t.Fatal("the response never completed")
		}
	}
}

// The watchdog still has to fire when the provider really does stop.
func TestARealStallIsStillCaught(t *testing.T) {
	fake := newFakeProvider(t, acceptSession)
	session := testSession(t, fake, OpenAIProfile())
	session.deltaStallDeadline = 100 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	fake.send(map[string]any{"type": "response.created"})
	fake.send(map[string]any{"type": "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(make([]byte, media.FrameSamples))})
	// ...and then nothing.

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-session.Events():
			if event.Type == EventTypeResponseDone && event.Status == StatusStalled {
				return
			}
		case <-deadline:
			t.Fatal("a genuinely stalled turn was never closed out")
		}
	}
}
