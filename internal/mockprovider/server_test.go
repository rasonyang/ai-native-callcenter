// SPDX-License-Identifier: Apache-2.0

package mockprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
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

// An outside test (the end-to-end campaign driver) asks whether the
// application ever opened a session. The handler answers StatsPath with the
// counters as JSON, under the names the driver reads, and still upgrades any
// other path to a session; reading the counters is not a session.
func TestStatsAreServedAsJSONBesideTheSessions(t *testing.T) {
	server := mockprovider.New(mockprovider.Config{TurnEvery: time.Hour, Log: quietLog()})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	stats := func() map[string]any {
		t.Helper()
		resp, err := nethttp.Get(ts.URL + mockprovider.StatsPath)
		if err != nil {
			t.Fatalf("get stats: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != nethttp.StatusOK {
			t.Fatalf("stats status %d", resp.StatusCode)
		}
		got := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		return got
	}
	counts := func(m map[string]any) (total, live float64) {
		total, okTotal := m["total"].(float64)
		live, okLive := m["live"].(float64)
		if !okTotal || !okLive {
			t.Fatalf("stats lack total or live: %v", m)
		}
		return total, live
	}

	if total, live := counts(stats()); total != 0 || live != 0 {
		t.Fatalf("before any session: total %v live %v, want 0 and 0", total, live)
	}
	session, _ := dial(t, ts.URL)
	defer session.Close(t.Context())
	// dial returns once the server's session answered session.update, and
	// the server counts a session before that session starts, so the counters
	// are already there; the poll keeps the test right should that order ever
	// change, and reads both counters at once.
	waitFor(t, func() bool {
		total, live := counts(stats())
		return total == 1 && live == 1
	}, "one session counted over HTTP")
}

// A refusal closes a session during setup with the code and text it was
// given, the sessions taking the refusals in turn, and the application's own
// client reads it as the quota refusal it stands for: the two texts qwen
// closed production sessions with (#604) come back as REQUESTS and TOKENS
// (#652). With RefuseSessions 2 the third session is served.
func TestARefusedSessionIsClosedDuringSetupWithItsCodeAndText(t *testing.T) {
	requests, err := mockprovider.ParseRefusal("1007:Requests rate limit exceeded, please try again later.")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := mockprovider.ParseRefusal("1007:Allocated quota exceeded, please increase your quota limit.")
	if err != nil {
		t.Fatal(err)
	}
	server := mockprovider.New(mockprovider.Config{TurnEvery: time.Hour, Log: quietLog(),
		Refusals: []mockprovider.Refusal{requests, tokens}, RefuseSessions: 2})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	for _, want := range []struct {
		text  string
		limit ratemeter.Limit
	}{{requests.Text, ratemeter.LimitRequests}, {tokens.Text, ratemeter.LimitTokens}} {
		err := start(t, ts.URL)
		var failure *provider.StartFailure
		if !errors.Is(err, provider.ErrQuotaExceeded) || !errors.As(err, &failure) {
			t.Fatalf("start = %v, want a quota refusal", err)
		}
		if failure.Status != "CLOSE_1007" || failure.Detail != want.text || failure.Limit != want.limit {
			t.Errorf("failure = %s %q %s, want CLOSE_1007 %q %s",
				failure.Status, failure.Detail, failure.Limit, want.text, want.limit)
		}
	}
	if err := start(t, ts.URL); err != nil {
		t.Errorf("the third session: %v, want it served", err)
	}
	if stats := server.Stats(); stats.Refused != 2 || stats.Total != 3 {
		t.Errorf("refused %d of %d sessions, want 2 of 3", stats.Refused, stats.Total)
	}
}

func TestARefusalIsCodeColonText(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want mockprovider.Refusal
		ok   bool
	}{
		{"1007:Requests rate limit exceeded, please try again later.",
			mockprovider.Refusal{Code: 1007, Text: "Requests rate limit exceeded, please try again later."}, true},
		{"4008:a: text with colons", mockprovider.Refusal{Code: 4008, Text: "a: text with colons"}, true},
		{"1011:", mockprovider.Refusal{Code: 1011}, true},
		{"1007", mockprovider.Refusal{}, false},
		{"x:quota", mockprovider.Refusal{}, false},
		{"1005:no status", mockprovider.Refusal{}, false},
		{"999:too low", mockprovider.Refusal{}, false},
		{"1007:" + strings.Repeat("x", 124), mockprovider.Refusal{}, false},
	} {
		got, err := mockprovider.ParseRefusal(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseRefusal(%q) = %+v, %v; want %+v, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

// start opens one session with the application's own client, as dial does,
// and returns what Start said.
func start(t *testing.T, url string) error {
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
	defer session.Close(context.Background())
	return session.Start(t.Context(), provider.SessionConfig{Instructions: "say something", Language: "en"})
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
