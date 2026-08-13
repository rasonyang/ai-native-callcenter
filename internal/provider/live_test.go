// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// These run against the real vendors and cost money, so they are skipped
// unless AICC_LIVE_PROVIDER_TEST is set. They exist because one thing cannot
// be checked any other way: at least one vendor never echoes the audio format
// it was given, so the only proof that a format was accepted is audio coming
// back in it.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/ -run Live -v

func requireLive(t *testing.T, profile Profile) {
	t.Helper()
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	if os.Getenv(profile.APIKeyEnv) == "" {
		t.Skipf("%s is not set", profile.APIKeyEnv)
	}
}

// liveResult is what a probe observed.
type liveResult struct {
	firstAudioAfter time.Duration
	deltas          int
	audioBytes      int
	modelSaid       string
	heard           string
	sessionReadyIn  time.Duration
}

// probe opens a real session, feeds it room tone, and listens for one turn.
func probe(t *testing.T, profile Profile, instructions string, listenFor time.Duration) liveResult {
	t.Helper()

	law := media.LawMu
	input, output := profile.FormatsFor(law)
	t.Logf("%s (%s): input %s, output %s", profile.Name, profile.Model, input, output)

	session, err := New(profile, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()

	cfg := SessionConfig{
		Instructions: instructions,
		Turn:         DefaultTurnDetection(),
		Tools: []ToolSpec{{
			Name:        "transfer_to_agent",
			Description: "Hand the caller to a human agent.",
			Parameters: json.RawMessage(`{"type":"object","properties":` +
				`{"queue":{"type":"string"},"reason":{"type":"string"}},"required":["queue"]}`),
		}},
		InputFormat:  input,
		OutputFormat: output,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	started := time.Now()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	result := liveResult{sessionReadyIn: time.Since(started)}

	// A real call carries a continuous stream even while the caller listens.
	stop := make(chan struct{})
	defer close(stop)
	go feedRoomTone(session, law, input, stop)

	deadline := time.After(listenFor)
	for {
		select {
		case <-deadline:
			return result

		case event, ok := <-session.Events():
			if !ok {
				return result
			}
			switch event.Type {
			case EventTypeAudioDelta:
				if result.deltas == 0 {
					result.firstAudioAfter = time.Since(started)
				}
				result.deltas++
				result.audioBytes += len(event.Audio)
			case EventTypeOutputTranscript:
				if event.IsFinal {
					result.modelSaid += event.Text
				}
			case EventTypeInputTranscript:
				if event.IsFinal {
					result.heard += event.Text
				}
			case EventTypeResponseDone:
				t.Logf("turn finished: status=%q usage=%+v", event.Status, event.Usage)
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", event.IsFatal, event.Err)
				if event.IsFatal {
					return result
				}
			}
		}
	}
}

func feedRoomTone(session *Realtime, law media.Law, to media.AudioFormat, stop <-chan struct{}) {
	converter, err := media.NewConverter(media.G711Format(law), to)
	if err != nil {
		return
	}

	ticker := time.NewTicker(FrameInterval)
	defer ticker.Stop()

	samples := make([]int16, media.FrameSamples)
	frame := make([]byte, 0, media.FrameSamples)
	converted := make([]byte, 0, media.FrameSamples*8)
	phase := 0

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		// Very quiet noise rather than digital silence: some detectors read a
		// perfectly flat signal as a dead line.
		for i := range samples {
			samples[i] = int16(30 * math.Sin(float64(phase+i)/7.0))
		}
		phase += media.FrameSamples

		frame = law.Encode(frame, samples)
		converted = converter.Convert(converted, frame)
		if err := session.SendAudio(converted); err != nil {
			return
		}
	}
}

// checkAudioArrived asserts the vendor honoured the format it was given.
func checkAudioArrived(t *testing.T, result liveResult, output media.AudioFormat) {
	t.Helper()

	t.Logf("session ready in %v, first audio after %v, %d deltas, %d bytes",
		result.sessionReadyIn.Round(time.Millisecond),
		result.firstAudioAfter.Round(time.Millisecond),
		result.deltas, result.audioBytes)
	t.Logf("model said: %q", result.modelSaid)

	if result.audioBytes == 0 {
		t.Fatalf("no audio came back, so %s was not honoured", output)
	}

	bytesPerSecond := output.RateHz
	if output.Encoding == media.EncodingPCM16 {
		bytesPerSecond *= 2
	}
	duration := float64(result.audioBytes) / float64(bytesPerSecond)
	t.Logf("that is %.2fs of audio interpreted as %s", duration, output)

	// A one-sentence greeting is a second or two. An order-of-magnitude miss
	// means the audio is not in the format we asked for — half the expected
	// duration would mean twice the sample width, and so on.
	if duration < 0.4 || duration > 20 {
		t.Errorf("%.2fs of audio for a one-sentence greeting: the bytes are "+
			"probably not %s", duration, output)
	}
}

// TestLiveOpenAIPassthrough proves the English leg needs no conversion at all:
// G.711 goes up and comes back down untouched.
func TestLiveOpenAIPassthrough(t *testing.T) {
	profile := OpenAIProfile()
	requireLive(t, profile)

	_, output := profile.FormatsFor(media.LawMu)
	if output.Encoding != media.EncodingMuLaw {
		t.Fatalf("this provider was expected to take G.711, got %s", output)
	}

	result := probe(t, profile,
		"You answer the phone for NovaNet. Greet the caller in one short "+
			"sentence and ask how you can help.", 12*time.Second)

	checkAudioArrived(t, result, output)
	if result.modelSaid == "" {
		t.Error("the model produced no transcript, so the turn may not have completed")
	}
}

// TestLiveQwenLinear closes the open question from the verification spike: the
// vendor never echoes audio formats, so only real audio proves the documented
// 16 kHz in / 24 kHz out is what it actually speaks.
func TestLiveQwenLinear(t *testing.T) {
	profile := QwenProfile()
	requireLive(t, profile)

	input, output := profile.FormatsFor(media.LawMu)
	if input.RateHz != media.RateProviderIn || output.RateHz != media.RateProviderOut {
		t.Fatalf("unexpected formats: %s in, %s out", input, output)
	}

	result := probe(t, profile,
		"你是 NovaNet 的电话客服。用一句话问候来电者，并询问需要什么帮助。",
		12*time.Second)

	checkAudioArrived(t, result, output)
	if result.modelSaid == "" {
		t.Error("the model produced no transcript, so the turn may not have completed")
	}
}
