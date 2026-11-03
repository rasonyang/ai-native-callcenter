// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// TestLiveResponseCreatedLatency measures how long a real provider takes to
// answer a response.create with response.created once the conversation already
// has a turn in it: the wait responseCreatedTimeout bounds. It costs money, so
// it is skipped unless AICC_LIVE_PROVIDER_TEST is set, and each provider whose
// key is missing is skipped on its own.
//
// Each provider gets one session. The greeting plays out first, then bare
// requests go out one at a time, each after the previous response is done, with
// room tone on the line the whole time as on a real call. The latency is from
// the request leaving this client to the client reporting the response started,
// which is the window the bound applies to. A request the provider never
// answers is logged and left out of the figures; the test fails only if none
// is answered.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run TestLiveResponseCreatedLatency -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 8) is how many requests each provider is sent.
func TestLiveResponseCreatedLatency(t *testing.T) {
	samples := 8
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		samples = v
	}
	for _, arm := range []struct {
		profile      Profile
		instructions string
		language     string
	}{
		{liveProfile(OpenAIProfile()), "You answer the phone for NovaNet. Reply in one short sentence.", "en"},
		{liveProfile(GrokProfile()), "You answer the phone for NovaNet. Reply in one short sentence.", "en"},
		{liveProfile(QwenProfile()), "你是 NovaNet 的电话客服。每次只用一句很短的话回答。", "zh"},
	} {
		t.Run(arm.profile.Name, func(t *testing.T) {
			requireLive(t, arm.profile)
			measureResponseCreated(t, arm.profile, arm.instructions, arm.language, samples)
		})
	}
}

func measureResponseCreated(t *testing.T, profile Profile, instructions, language string, samples int) {
	t.Helper()

	session, err := New(profile, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	// The probe measures the wait itself, so the bound must not cut it short.
	session.responseCreatedDeadline = time.Minute
	defer func() { _ = session.Close(context.Background()) }()

	input, output := profile.FormatsFor(media.LawMu)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, SessionConfig{
		Instructions: instructions,
		Language:     language,
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
	}); err != nil {
		t.Fatalf("%s: start: %v", profile.Name, err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go feedRoomTone(session, media.LawMu, input, stop)

	// awaitTurnEnd reads until the open response ends; it reports false if the
	// session ended or nothing ended within the wait.
	awaitTurnEnd := func() bool {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case <-deadline:
				return false
			case event, ok := <-session.Events():
				if !ok {
					return false
				}
				switch event.Type {
				case EventTypeResponseDone, EventTypeInterrupted:
					return true
				case EventTypeError:
					t.Logf("%s: provider error (fatal=%v): %v", profile.Name, event.IsFatal, event.Err)
					if event.IsFatal {
						return false
					}
				}
			}
		}
	}
	if !awaitTurnEnd() {
		t.Fatalf("%s: the greeting never finished", profile.Name)
	}

	var latencies []time.Duration
	for i := range samples {
		// A beat between turns, as a caller would leave.
		time.Sleep(500 * time.Millisecond)
		sentAt := time.Now()
		if err := session.requestResponse(responseRequest{kind: responseKindOwed}, map[string]any{"type": "response.create"}); err != nil {
			t.Fatalf("%s: request %d: %v", profile.Name, i+1, err)
		}
		var createdIn time.Duration
		deadline := time.After(30 * time.Second)
	created:
		for {
			select {
			case <-deadline:
				t.Logf("%s: sample %d: no response.created within 30s", profile.Name, i+1)
				break created
			case event, ok := <-session.Events():
				if !ok {
					t.Fatalf("%s: the session ended", profile.Name)
				}
				switch event.Type {
				case EventTypeResponseStarted:
					createdIn = time.Since(sentAt)
					break created
				case EventTypeError:
					t.Logf("%s: provider error (fatal=%v): %v", profile.Name, event.IsFatal, event.Err)
					if event.IsFatal {
						t.FailNow()
					}
				}
			}
		}
		if createdIn == 0 {
			continue
		}
		latencies = append(latencies, createdIn)
		t.Logf("%s: sample %d: request -> response.created %v", profile.Name, i+1, createdIn.Round(time.Millisecond))
		if !awaitTurnEnd() {
			t.Fatalf("%s: response %d never finished", profile.Name, i+1)
		}
	}

	if len(latencies) == 0 {
		t.Fatalf("%s: no request was answered", profile.Name)
	}
	slices.Sort(latencies)
	nearestRank := func(p float64) time.Duration {
		rank := int(p*float64(len(latencies)) + 0.999999)
		return latencies[min(max(rank, 1), len(latencies))-1]
	}
	t.Logf("%s (%s): n=%d/%d median=%v p95=%v max=%v (nearest rank)", profile.Name, profile.Model,
		len(latencies), samples,
		nearestRank(0.5).Round(time.Millisecond),
		nearestRank(0.95).Round(time.Millisecond),
		latencies[len(latencies)-1].Round(time.Millisecond))
	// A request never answered is the silent drop the bound exists for, not a
	// latency: it is reported, and kept out of the figures.
	if dropped := samples - len(latencies); dropped > 0 {
		t.Logf("%s: %d of %d requests were never answered within 30s", profile.Name, dropped, samples)
	}
}
