// SPDX-License-Identifier: Apache-2.0

package mockprovider_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
)

func TestParseVADValidatesTheFile(t *testing.T) {
	cases := []struct {
		name string
		data string
		ok   bool
	}{
		{"a good file", `{"utterances":[{"text":"喂"}]}`, true},
		{"hangover is honoured", `{"utterances":[{"text":"喂"}],"hangoverMs":500}`, true},
		{"empty utterances", `{"utterances":[]}`, false},
		{"no utterances", `{}`, false},
		{"empty text", `{"utterances":[{"text":""}]}`, false},
		{"a huge hangover", `{"utterances":[{"text":"喂"}],"hangoverMs":99999}`, false},
		{"an unknown field", `{"utterances":[{"text":"喂","unknown":1}]}`, false},
		{"negative speak", `{"utterances":[{"text":"喂","speakSec":-1}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vad, err := mockprovider.ParseVAD([]byte(tc.data))
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want an error, got %+v", vad)
			}
		})
	}
}

// readEvents decodes the server's events until close.
func readEvents(t *testing.T, conn *websocket.Conn, events chan<- map[string]any) {
	t.Helper()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var event map[string]any
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		select {
		case events <- event:
		default:
		}
	}
}

func nextEvent(t *testing.T, events <-chan map[string]any, want string,
	deadline time.Duration) map[string]any {
	t.Helper()
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		select {
		case event := <-events:
			if event["type"] == want {
				return event
			}
		case <-time.After(time.Until(until)):
			t.Fatalf("no %s within %s", want, deadline)
		}
	}
	t.Fatalf("no %s within %s", want, deadline)
	return nil
}

// A loud burst on the uplink cancels the turn in progress — the mock settles
// it the way a server's VAD does, with response.done{status:"cancelled"} —
// and the burst's end commits a transcript and earns the scripted reply.
func TestVADModeCancelsTheTurnAndAnswersTheBurst(t *testing.T) {
	vad, err := mockprovider.ParseVAD([]byte(`{
		"hangoverMs": 300,
		"utterances": [{"text": "喂？", "speakSec": 1,
			"toolName": "set_intent", "toolArgs": {"value": "yes"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mockprovider.New(mockprovider.Config{
		VAD:       vad,
		TurnAudio: 4 * time.Second,
		Log:       quietLog(),
	}).Handler())
	t.Cleanup(server.Close)

	conn := uplinkSession(t, server.URL)
	events := make(chan map[string]any, 256)
	go readEvents(t, conn, events)

	// Open a turn: the response the burst will cut.
	send(t, conn, map[string]any{"type": "response.create"})
	nextEvent(t, events, "response.output_item.added", 2*time.Second)
	nextEvent(t, events, "response.output_audio.delta", 2*time.Second)

	// Speak: five loud frames, then quiet.
	for range 5 {
		appendAudio(t, conn, toneFrame(media.LawMu))
		time.Sleep(20 * time.Millisecond)
	}

	started := nextEvent(t, events, "input_audio_buffer.speech_started", 2*time.Second)
	if started["item_id"] == "" {
		t.Fatal("speech_started carries no item_id")
	}
	done := nextEvent(t, events, "response.done", 2*time.Second)
	if status(done) != "cancelled" {
		t.Fatalf("the cut turn ends %q, want cancelled", status(done))
	}
	stopped := nextEvent(t, events, "input_audio_buffer.speech_stopped", 3*time.Second)
	if stopped["item_id"] != started["item_id"] {
		t.Fatalf("speech_stopped item %v != start %v", stopped["item_id"], started["item_id"])
	}
	nextEvent(t, events, "input_audio_buffer.committed", 2*time.Second)
	tr := nextEvent(t, events, "conversation.item.input_audio_transcription.completed", 2*time.Second)
	if tr["transcript"] != "喂？" {
		t.Fatalf("transcript %v, want 喂？", tr["transcript"])
	}

	// The scripted reply: audio, then the function call, then done.
	nextEvent(t, events, "response.created", 2*time.Second)
	call := nextEvent(t, events, "response.function_call_arguments.done", 3*time.Second)
	if call["name"] != "set_intent" {
		t.Fatalf("tool %v, want set_intent", call["name"])
	}
	if call["arguments"] != `{"value": "yes"}` {
		t.Fatalf("args %v", call["arguments"])
	}
	nextEvent(t, events, "response.done", 3*time.Second)

	stats := sessionStats(t, server.URL)
	if len(stats) == 0 || stats[0].VADStarts != 1 || stats[0].VADCancels != 1 {
		t.Fatalf("session record: %+v", stats)
	}
	if len(stats[0].VADTexts) != 1 || stats[0].VADTexts[0] != "喂？" {
		t.Fatalf("transcripts: %+v", stats[0].VADTexts)
	}
}

// A burst while no turn is open still ends in a committed transcript and its
// reply; nothing is cancelled.
func TestVADModeAnswersABurstOutsideATurn(t *testing.T) {
	vad, err := mockprovider.ParseVAD([]byte(`{
		"hangoverMs": 200,
		"utterances": [{"text": "喂，您好", "speakSec": 0.5}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mockprovider.New(mockprovider.Config{
		VAD:       vad,
		TurnAudio: 2 * time.Second,
		TurnEvery: 30 * time.Second,
		Log:       quietLog(),
	}).Handler())
	t.Cleanup(server.Close)

	conn := uplinkSession(t, server.URL)
	events := make(chan map[string]any, 256)
	go readEvents(t, conn, events)

	for range 3 {
		appendAudio(t, conn, toneFrame(media.LawMu))
		time.Sleep(15 * time.Millisecond)
	}
	nextEvent(t, events, "input_audio_buffer.speech_started", 2*time.Second)
	nextEvent(t, events, "conversation.item.input_audio_transcription.completed", 3*time.Second)
	done := nextEvent(t, events, "response.done", 4*time.Second)
	if status(done) != "completed" {
		t.Fatalf("the reply ends %q, want completed", status(done))
	}
	stats := sessionStats(t, server.URL)
	if len(stats) == 0 || stats[0].VADCancels != 0 {
		t.Fatalf("no turn was open to cancel: %+v", stats)
	}
}

// Two bursts inside the reply's own audio settle it cancelled and earn the
// second utterance — the cut-the-replay case's provider half.
func TestVADModeCancelsItsOwnReply(t *testing.T) {
	vad, err := mockprovider.ParseVAD([]byte(`{
		"hangoverMs": 200,
		"utterances": [
			{"text": "喂", "speakSec": 5},
			{"text": "喂？", "speakSec": 0.5}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mockprovider.New(mockprovider.Config{
		VAD:       vad,
		TurnAudio: 30 * time.Second,
		TurnEvery: 30 * time.Second,
		Log:       quietLog(),
	}).Handler())
	t.Cleanup(server.Close)

	conn := uplinkSession(t, server.URL)
	events := make(chan map[string]any, 256)
	go readEvents(t, conn, events)

	burst := func() {
		for range 3 {
			appendAudio(t, conn, toneFrame(media.LawMu))
			time.Sleep(15 * time.Millisecond)
		}
	}
	burst()
	nextEvent(t, events, "conversation.item.input_audio_transcription.completed", 4*time.Second)
	nextEvent(t, events, "response.output_audio.delta", 3*time.Second)

	burst()
	done := nextEvent(t, events, "response.done", 3*time.Second)
	if status(done) != "cancelled" {
		t.Fatalf("the cut reply ends %q, want cancelled", status(done))
	}
	tr := nextEvent(t, events, "conversation.item.input_audio_transcription.completed", 4*time.Second)
	if tr["transcript"] != "喂？" {
		t.Fatalf("second transcript %v, want 喂？", tr["transcript"])
	}
	stats := sessionStats(t, server.URL)
	if len(stats) == 0 || stats[0].VADStarts != 2 || stats[0].VADCancels != 1 {
		t.Fatalf("two bursts, one cancel: %+v", stats)
	}
	if len(stats[0].VADTexts) != 2 || stats[0].VADTexts[0] != "喂" || stats[0].VADTexts[1] != "喂？" {
		t.Fatalf("the queue is consumed in order: %+v", stats[0].VADTexts)
	}
}

func send(t *testing.T, conn *websocket.Conn, event map[string]any) {
	t.Helper()
	data, _ := json.Marshal(event)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func status(done map[string]any) string {
	resp, _ := done["response"].(map[string]any)
	s, _ := resp["status"].(string)
	return s
}
