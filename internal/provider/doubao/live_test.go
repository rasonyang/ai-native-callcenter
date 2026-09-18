// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// This runs against the real provider and costs money, so it is skipped unless
// AICC_LIVE_PROVIDER_TEST is set. It exists because of one thing no fake can
// answer: the output format this client asks for is undocumented, and the only
// proof that pcm_s16le still means signed 16-bit at 24 kHz is audio coming
// back that reads as speech when it is interpreted that way.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/doubao/ -run Live -count=1 -v
func TestLiveDoubaoSpeaksTheOpeningLine(t *testing.T) {
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	if os.Getenv("DOUBAO_API_KEY") == "" {
		t.Skip("DOUBAO_API_KEY is not set")
	}

	profile := provider.Profile{
		Name:         "doubao",
		Endpoint:     "wss://openspeech.bytedance.com/api/v3/duplex/realtime/dialogue",
		APIKeyEnv:    "DOUBAO_API_KEY",
		Voice:        "zh_female_vv_jupiter_bigtts",
		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
	}

	session, err := newSession(profile, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	cfg := provider.SessionConfig{
		Instructions: "你是 NovaNet 的电话客服。请用中文，简短自然地回答。",
		Language:     "zh",
		Turn:         provider.DefaultTurnDetection(),
		OpeningText:  "您好，这里是 NovaNet 客服中心，请问有什么可以帮您？",
		InputFormat:  media.PCM16Format(media.RateProviderIn),
		OutputFormat: media.PCM16Format(media.RateProviderOut),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("X-Tt-Logid: %s", session.logID())

	// A real call carries a continuous stream even while the caller listens.
	stop := make(chan struct{})
	defer close(stop)
	go feedRoomTone(session, stop)

	var audio []byte
	var said string
	listening := time.After(12 * time.Second)
collect:
	for {
		select {
		case <-listening:
			break collect
		case event, ok := <-session.Events():
			if !ok {
				break collect
			}
			switch event.Type {
			case provider.EventTypeAudioDelta:
				audio = append(audio, event.Audio...)
			case provider.EventTypeOutputTranscript:
				if event.IsFinal {
					said += event.Text
				}
			case provider.EventTypeError:
				t.Logf("provider error (fatal=%v): %v", event.IsFatal, event.Err)
				if event.IsFatal {
					break collect
				}
			}
		}
	}

	t.Logf("the model said %q in %d bytes (%.2fs at 24 kHz, 16-bit)",
		said, len(audio), float64(len(audio))/(2*float64(media.RateProviderOut)))
	checkTheAudioIsSignedSixteenBit(t, audio)

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
	t.Logf("stats: %+v", session.Stats())
}

// checkTheAudioIsSignedSixteenBit is the undocumented dependency, measured.
//
// The same sentence in this provider's documented "pcm" is 32-bit float in
// [-1,1]; read as int16 that is a signal where an eighth of the samples are
// past ±20000 and it sounds like noise on the line. Speech does not do that.
func checkTheAudioIsSignedSixteenBit(t *testing.T, audio []byte) {
	t.Helper()

	if len(audio) == 0 {
		t.Fatal("no audio came back, so the opening line was never spoken")
	}
	if len(audio)%2 != 0 {
		t.Fatalf("%d bytes of audio is not a whole number of 16-bit samples", len(audio))
	}

	var loud, nonZero int
	var energy float64
	samples := len(audio) / 2
	for i := range samples {
		sample := float64(int16(binary.LittleEndian.Uint16(audio[2*i:])))
		if sample != 0 {
			nonZero++
		}
		if math.Abs(sample) > 20000 {
			loud++
		}
		energy += sample * sample
	}
	rms := math.Sqrt(energy / float64(samples))
	loudShare := float64(loud) / float64(samples)
	t.Logf("%d samples, rms %.0f, %.1f%% past ±20000", samples, rms, 100*loudShare)

	if nonZero*4 < samples {
		t.Errorf("%d of %d samples are silent: this is not speech", samples-nonZero, samples)
	}
	if rms < 200 || rms > 20000 {
		t.Errorf("rms %.0f is not a plausible level for speech read as 16-bit", rms)
	}
	if loudShare > 0.05 {
		t.Errorf("%.1f%% of samples are past ±20000: these bytes are probably not int16",
			100*loudShare)
	}
}

// feedRoomTone keeps the uplink alive with very quiet noise rather than
// digital silence: a perfectly flat signal reads as a dead line.
func feedRoomTone(session *Session, stop <-chan struct{}) {
	ticker := time.NewTicker(provider.FrameInterval)
	defer ticker.Stop()

	const samplesPerFrame = 320 // 20 ms at 16 kHz
	frame := make([]byte, 2*samplesPerFrame)
	phase := 0

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		for i := range samplesPerFrame {
			sample := int16(30 * math.Sin(float64(phase+i)/7.0))
			binary.LittleEndian.PutUint16(frame[2*i:], uint16(sample))
		}
		phase += samplesPerFrame
		if err := session.SendAudio(frame); err != nil {
			return
		}
	}
}
