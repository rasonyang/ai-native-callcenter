// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// What a provider sees, byte for byte.
//
// The other tests in this package read a frame as a decoded map and ask whether
// the field they care about is right. That leaves everything they do not ask
// about free to change — a renamed key, a dropped field, an extra frame nobody
// requested — and the first thing to notice would be a live call. These tests
// record every frame the client sends, in order, exactly as it arrived, and
// compare the lot to a file. Both dialects are scripted, because the same call
// is two different conversations on the wire.
//
// Run with -update to rewrite the goldens, and read the diff before committing
// it: a changed golden is a changed protocol.
var isWireGoldenUpdate = flag.Bool("update", false,
	"rewrite the golden wire frames under testdata/")

// goldenConfig is what every scripted call starts from. It is spelled out here
// rather than shared with the rest of the package so a golden changes when the
// client's frames change and at no other time.
func goldenConfig() SessionConfig {
	return SessionConfig{
		Instructions: "You answer the phone for NovaNet.",
		// No voice: the profile's own is the one that has to reach the wire.
		Language: "en",
		Turn:     TurnDetection{Mode: TurnModeVAD, SilenceMs: 500, Threshold: 0.5},
		Tools: []ToolSpec{{
			Name:        "lookup_balance",
			Description: "Read the caller's balance.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"accountId":{"type":"string"}}}`),
		}},
		InputFormat:  media.G711Format(media.LawMu),
		OutputFormat: media.G711Format(media.LawMu),
	}
}

// goldenProfiles are the two dialects, each carrying a header of its own so the
// handshake assertion has something profile-shaped to find.
func goldenProfiles() []struct {
	name    string
	profile Profile
} {
	openai := OpenAIProfile()
	openai.Headers = map[string]string{"X-Aicc-Dialect": "ga"}
	qwen := QwenProfile()
	qwen.Headers = map[string]string{"X-Aicc-Dialect": "beta"}
	return []struct {
		name    string
		profile Profile
	}{{"openai", openai}, {"qwen", qwen}}
}

// A whole call, frame by frame: the session is configured, the caller is
// greeted, talked over, answered, handed a tool result and finally spoken to on
// the flow's instructions.
func TestTheFramesOfAWholeCallAreWhatTheyWere(t *testing.T) {
	for _, dialect := range goldenProfiles() {
		name, profile := dialect.name, dialect.profile
		t.Run(name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, profile)

			if err := session.Start(t.Context(), goldenConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			// The opening turn: the configuration, the cue where the dialect
			// needs one, and the request.
			sent := 2
			if profile.NeedsCueForFirstTurn {
				sent = 3
			}
			awaitFrames(t, f, sent)

			// The credential and the profile's headers ride the upgrade.
			handshake := f.handshake()
			if got := handshake.Get("Authorization"); got != "Bearer test-key" {
				t.Errorf("Authorization = %q, want the credential", got)
			}
			if got := handshake.Get("X-Aicc-Dialect"); got != profile.Headers["X-Aicc-Dialect"] {
				t.Errorf("X-Aicc-Dialect = %q, want the profile's own header", got)
			}

			// The bot starts speaking and the caller talks over it.
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.output_item.added",
				"item": map[string]any{"id": "item-1", "type": "message"}})
			f.send(map[string]any{"type": "response.output_audio.delta",
				"delta": base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0x03})})
			awaitEvent(t, session, EventTypeAudioDelta)

			if err := session.SendAudio([]byte{0x00, 0x7f, 0x80, 0xff}); err != nil {
				t.Fatalf("send audio: %v", err)
			}
			sent++
			awaitFrames(t, f, sent)

			// With a response open, whether the cancel goes out is the profile's
			// to say; the truncation is not.
			if err := session.Interrupt(InterruptReasonSpeech, 120); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			sent++
			if !profile.CancelsResponseItself {
				sent++
			}
			awaitFrames(t, f, sent)

			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)

			// Once the turn has ended there is nothing left to cancel, but how
			// much the caller heard is still worth saying.
			if err := session.Interrupt(InterruptReasonDTMF, 200); err != nil {
				t.Fatalf("interrupt after the turn ended: %v", err)
			}
			sent++
			awaitFrames(t, f, sent)

			if err := session.UpdateInstructions("You are now closing the call."); err != nil {
				t.Fatalf("update instructions: %v", err)
			}
			sent++
			awaitFrames(t, f, sent)

			// A keypress the caller did not speak, and the turn it asks for.
			if err := session.SendUserText("2"); err != nil {
				t.Fatalf("send user text: %v", err)
			}
			sent += 2
			awaitFrames(t, f, sent)

			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "call-7", "name": "lookup_balance",
				"arguments": `{"accountId":"42"}`})
			awaitEvent(t, session, EventTypeToolCall)
			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)

			if err := session.SendToolResult("call-7", `{"balance":"12.50"}`,
				"Tell the caller the balance, then offer to close."); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			sent += 2
			awaitFrames(t, f, sent)

			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)

			// The floor is free, so the line the flow chose is asked for outright.
			// A closing line: the one kind a profile may also put in the
			// conversation.
			if err := session.SpeakText("Thanks for calling NovaNet.", true); err != nil {
				t.Fatalf("speak text: %v", err)
			}
			sent++
			if profile.NeedsDirectedLineInConversation {
				// The same direction as a caller message first (W-Q1),
				// because the line ends the call.
				sent++
			}
			awaitFrames(t, f, sent)

			frames := settledFrames(t, f, sent)
			checkWireGolden(t, "call_"+name, frames)

			// Closing says so on the socket, and the session stops taking audio.
			if err := session.Close(t.Context()); err != nil {
				t.Fatalf("close: %v", err)
			}
			closeErr := awaitReadError(t, f)
			if !websocket.IsCloseError(closeErr, websocket.CloseNormalClosure) {
				t.Errorf("the provider saw %v, want a normal close frame", closeErr)
			}
			if err := session.SendAudio([]byte{0x01}); !errors.Is(err, errSessionClosed) {
				t.Errorf("audio after close returned %v, want the closed sentinel", err)
			}
			if got := len(f.rawFrames()); got != len(frames) {
				t.Errorf("%d frames reached the provider after the golden was taken", got-len(frames))
			}
		})
	}
}

// A flow that gives the call its opening line changes the shape of the start:
// the request carries the words, and on a dialect that needs a cue the cue is
// those same words rather than a stage direction.
func TestTheFramesOfAnOpeningLineAreWhatTheyWere(t *testing.T) {
	for _, dialect := range goldenProfiles() {
		name, profile := dialect.name, dialect.profile
		t.Run(name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, profile)

			cfg := goldenConfig()
			cfg.OpeningText = "Good morning, NovaNet support."
			if err := session.Start(t.Context(), cfg); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			sent := 2
			if profile.NeedsCueForFirstTurn {
				sent = 3
			}
			awaitFrames(t, f, sent)
			checkWireGolden(t, "opening_"+name, settledFrames(t, f, sent))
		})
	}
}

//
// The recorder's side of the bargain.
//

// awaitFrames waits until n frames have reached the provider.
func awaitFrames(t *testing.T, f *fakeProvider, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.rawFrames()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d frames arrived, want %d:\n%s",
		len(f.rawFrames()), n, strings.Join(frameStrings(f.rawFrames()), "\n"))
}

// settledFrames holds still long enough for an n+1th frame to turn up, then
// returns the record. Cardinality is half of what a golden pins: a frame that
// is right and sent twice is a different bug from one that is wrong.
func settledFrames(t *testing.T, f *fakeProvider, n int) [][]byte {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	frames := f.rawFrames()
	if len(frames) != n {
		t.Fatalf("the client sent %d frames, want %d:\n%s",
			len(frames), n, strings.Join(frameStrings(frames), "\n"))
	}
	return frames
}

// awaitReadError waits for the provider's read loop to end and says why.
func awaitReadError(t *testing.T, f *fakeProvider) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.readError(); err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the provider's connection never ended")
	return nil
}

func checkWireGolden(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".jsonl")

	if *isWireGoldenUpdate {
		var file bytes.Buffer
		for _, frame := range frames {
			if bytes.ContainsRune(frame, '\n') {
				t.Fatalf("frame %s spans lines, which this format cannot hold", frame)
			}
			file.Write(frame)
			file.WriteByte('\n')
		}
		if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %d frames to %s", len(frames), path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden frames (run with -update to write them): %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(frames) != len(want) {
		t.Fatalf("the client sent %d frames, want %d:\n got: %s\nwant: %s",
			len(frames), len(want),
			strings.Join(frameStrings(frames), "\n      "),
			strings.Join(want, "\n      "))
	}
	for i := range frames {
		if string(frames[i]) != want[i] {
			t.Errorf("frame %d is not what the provider used to see:\n got: %s\nwant: %s",
				i, frames[i], want[i])
		}
	}
}

func frameStrings(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, string(frame))
	}
	return out
}
