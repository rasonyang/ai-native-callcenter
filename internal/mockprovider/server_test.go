// SPDX-License-Identifier: Apache-2.0

package mockprovider_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The mock is only worth anything if the application's own client accepts it.
// These tests drive it with that client and nothing else — a mock that speaks
// a dialect of its own would pass every test written against itself and fail
// the moment a load run used it.

func TestTheRealClientCompletesASessionAgainstIt(t *testing.T) {
	session, events := connect(t, mockprovider.Config{
		FirstAudio: 20 * time.Millisecond,
		TurnAudio:  100 * time.Millisecond,
		DeltaAudio: 20 * time.Millisecond,
		TurnEvery:  time.Hour, // the opening turn is enough
	})
	defer session.Close(t.Context())

	var audio int
	var sawResponseDone bool
	deadline := time.After(5 * time.Second)
	for !sawResponseDone {
		select {
		case event := <-events:
			switch event.Type {
			case provider.EventTypeAudioDelta:
				audio += len(event.Audio)
			case provider.EventTypeResponseDone:
				sawResponseDone = true
			case provider.EventTypeError:
				t.Fatalf("provider error: %v", event.Err)
			}
		case <-deadline:
			t.Fatalf("no response completed; %d bytes of audio so far", audio)
		}
	}

	// 100ms of µ-law at 8 kHz is 800 bytes, and the client decodes base64 on
	// the way in, so anything else means the audio path disagrees.
	if want := 100 * media.RateTelephone / 1000; audio != want {
		t.Errorf("received %d bytes of audio, want %d", audio, want)
	}
}

func TestAudioIsUsableTelephoneAudio(t *testing.T) {
	session, events := connect(t, mockprovider.Config{
		FirstAudio: 20 * time.Millisecond,
		TurnAudio:  40 * time.Millisecond,
		DeltaAudio: 20 * time.Millisecond,
		TurnEvery:  time.Hour,
	})
	defer session.Close(t.Context())

	for {
		select {
		case event := <-events:
			if event.Type != provider.EventTypeAudioDelta {
				continue
			}
			if len(event.Audio)%media.FrameSamples != 0 {
				t.Fatalf("a delta of %d bytes is not a whole number of 20ms frames",
					len(event.Audio))
			}
			// A tone, not silence: a load test on silent audio would not
			// exercise anything a real conversation does.
			if allSame(event.Audio) {
				t.Fatal("the delta is a constant byte, so it carries no signal")
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no audio arrived")
		}
	}
}

func TestSessionsAreCounted(t *testing.T) {
	server := mockprovider.New(mockprovider.Config{
		TurnEvery: time.Hour, Log: quietLog(),
	})
	http := httptest.NewServer(server)
	defer http.Close()

	first, _ := dial(t, http.URL)
	second, _ := dial(t, http.URL)
	defer second.Close(t.Context())

	waitFor(t, func() bool { return server.Stats().Live == 2 }, "two live sessions")
	first.Close(t.Context())
	waitFor(t, func() bool { return server.Stats().Live == 1 }, "one live session")

	if stats := server.Stats(); stats.Total != 2 || stats.Peak != 2 {
		t.Errorf("total %d peak %d, want 2 and 2", stats.Total, stats.Peak)
	}
}

func connect(t *testing.T, cfg mockprovider.Config) (*provider.Realtime, <-chan provider.Event) {
	t.Helper()
	cfg.Log = quietLog()
	http := httptest.NewServer(mockprovider.New(cfg))
	t.Cleanup(http.Close)
	return dial(t, http.URL)
}

func dial(t *testing.T, url string) (*provider.Realtime, <-chan provider.Event) {
	t.Helper()

	t.Setenv("OPENAI_API_KEY", "not-a-key")
	profile, err := provider.ProfileFor(provider.NameOpenAI, provider.Override{
		Endpoint: "ws" + strings.TrimPrefix(url, "http") + "/v1/realtime",
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	session, err := provider.New(profile, quietLog())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(t.Context(), provider.SessionConfig{
		Instructions: "say something",
		Language:     "en",
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	return session, session.Events()
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	for range 100 {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func allSame(data []byte) bool {
	for _, b := range data {
		if b != data[0] {
			return false
		}
	}
	return true
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
