// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The backchannel hold against the real OpenAI Realtime service, through the
// production client (docs/design/11-backchannels.md). The caller's clips are
// synthetic, 8 kHz mu-law (testdata/backchannel_mm.ulaw, about 250 ms of "嗯";
// testdata/backchannel_wait.ulaw, about 640 ms of "等一下"), spoken over the
// bot's long answer. Two sessions, each a real call's worth of money:
//
//   - "嗯" is held and declined: the response keeps playing past the clip, the
//     provider never cancels it, and the client has to be asked for nothing.
//   - "等一下" is words: the client cancels the response (client_cancelled), asks
//     for the caller's turn, and a new response is created and speaks.
//
// What the session does with the transcript, this test does by hand: the
// classifier lives in internal/aicall, so it is a literal list here.
//
//	AICC_LIVE_PROVIDER_TEST=1 OPENAI_API_KEY=... go test -count=1 -run LiveBackchannel -v ./internal/provider/
func TestLiveBackchannelIsHeldAndWordsTakeTheFloor(t *testing.T) {
	profile := liveProfile(OpenAIProfile())
	requireLive(t, profile)

	t.Run("嗯 keeps the response", func(t *testing.T) { runLiveBackchannel(t, "backchannel_mm.ulaw", false) })
	t.Run("等一下 is cancelled and answered", func(t *testing.T) { runLiveBackchannel(t, "backchannel_wait.ulaw", true) })
}

func runLiveBackchannel(t *testing.T, clip string, isWords bool) {
	profile := liveProfile(OpenAIProfile())
	data, err := os.ReadFile("testdata/" + clip)
	if err != nil {
		t.Fatalf("read the clip: %v", err)
	}

	session, err := New(profile, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()

	input, output := profile.FormatsFor(media.LawMu)
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	err = session.Start(ctx, SessionConfig{
		// The opening turn is the monologue, so the run has exactly one response
		// before the clip. A separate request would be owed behind the opening.
		Instructions: "你是电话语音助手，只说中文。接通后立刻讲一个很长的故事，大约四百字，慢慢地讲，一口气说完，不要中途停下来，不要提问。",
		Language:     "zh",
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	frames := make(chan []byte, 512)
	go func() { // a continuous 20 ms stream: silence unless there is a clip frame
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		silence := make([]byte, media.FrameSamples)
		for i := range silence {
			silence[i] = 0xFF
		}
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				frame := silence
				select {
				case f := <-frames:
					frame = f
				default:
				}
				if session.SendAudio(frame) != nil {
					return
				}
			}
		}
	}()

	var (
		audioBytes, audioAtClip int
		isSpoken                bool
		clipAt                  time.Time
		sawSpeech, isCancelled  bool
		isOpen, wasOpenAtWords  bool
		wasAnswered             bool
		finalWords              string
		responses               int
		deadline                = time.After(60 * time.Second)
		verdictTimer            <-chan time.Time
	)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out (audio %d bytes, speech %v, cancelled %v, answered %v, words %q)",
				audioBytes, sawSpeech, isCancelled, wasAnswered, finalWords)
		case <-verdictTimer:
			verdictTimer = nil
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("the session closed")
			}
			switch event.Type {
			case EventTypeResponseStarted:
				responses++
				isOpen = true
			case EventTypeAudioDelta:
				audioBytes += len(event.Audio)
				// Two seconds of the bot's answer in, the caller speaks.
				if !isSpoken && audioBytes >= 2*8000 {
					isSpoken = true
					clipAt = time.Now()
					audioAtClip = audioBytes
					for off := 0; off < len(data); off += media.FrameSamples {
						end := min(off+media.FrameSamples, len(data))
						frame := append([]byte(nil), data[off:end]...)
						for len(frame) < media.FrameSamples {
							frame = append(frame, 0xFF)
						}
						frames <- frame
					}
				}
			case EventTypeSpeechStarted:
				sawSpeech = true
			case EventTypeInputTranscript:
				if !event.IsFinal {
					continue
				}
				finalWords = event.Text
				if isWords {
					// Words: take the floor as the session does. A response
					// that has finished generating has nothing to cancel.
					wasOpenAtWords = isOpen
					if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
						t.Fatalf("interrupt: %v", err)
					}
					if err := session.AnswerCallerTurn(event.ItemID); err != nil {
						t.Fatalf("answer: %v", err)
					}
				} else if err := session.DeclineCallerTurn(event.ItemID); err != nil {
					t.Fatalf("decline: %v", err)
				}
				verdictTimer = time.After(4 * time.Second)
			case EventTypeInterrupted:
				isCancelled = true
				isOpen = false
			case EventTypeResponseDone:
				isOpen = false
				if isWords && responses >= 2 {
					wasAnswered = true
				}
			case EventTypeError:
				t.Fatalf("provider error: %v", event.Err)
			}
		}
		if isSpoken && !isWords && !clipAt.IsZero() && time.Since(clipAt) > 8*time.Second {
			break
		}
		if isWords && wasAnswered {
			break
		}
	}

	if !sawSpeech {
		t.Fatal("the provider never reported the clip as speech; the run proves nothing")
	}
	t.Logf("clip %s: transcript %q, bot audio %d bytes at the clip and %d at the end, responses %d, cancelled %v",
		clip, finalWords, audioAtClip, audioBytes, responses, isCancelled)
	if isWords {
		if responses < 2 {
			t.Errorf("words were not answered: responses %d", responses)
		}
		if wasOpenAtWords && !isCancelled {
			t.Error("words over a response still being generated did not cancel it")
		}
		return
	}
	if isCancelled {
		t.Error("the response was cancelled by an acknowledgement; the switches did not hold")
	}
	if responses != 1 {
		t.Errorf("%d responses, want the one the acknowledgement must not add to", responses)
	}
	if audioBytes <= audioAtClip {
		t.Error("the bot stopped talking after the acknowledgement")
	}
	if strings.TrimSpace(finalWords) == "" {
		t.Log("the clip was not transcribed; the run still shows the response was kept")
	}
}
