// SPDX-License-Identifier: Apache-2.0

package mockprovider_test

import (
	"encoding/base64"
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
)

func sessionStats(t *testing.T, base string) []mockprovider.SessionStat {
	t.Helper()
	resp, err := nethttp.Get(base + mockprovider.StatsPath + "?sessions=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats mockprovider.Stats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	return stats.Sessions
}

func uplinkSession(t *testing.T, base string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/v1/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func appendAudio(t *testing.T, conn *websocket.Conn, frame []byte) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(frame)})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func toneFrame(law media.Law) []byte {
	samples := make([]int16, media.FrameSamples)
	for i := range samples {
		samples[i] = int16(8000 * float64(i%8-4) / 4)
	}
	return law.Encode(nil, samples)
}

// The mock counts the application's uplink frames that are above silence, per
// session, and says when the first one came: a silent uplink counts frames and
// no loud one, a spoken one counts the loud ones, in either law.
func TestUplinkLoudFramesAreCountedPerSession(t *testing.T) {
	server := mockprovider.New(mockprovider.Config{TurnEvery: time.Hour, Log: quietLog()})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	for _, c := range []struct {
		format string
		law    media.Law
	}{{"audio/pcmu", media.LawMu}, {"audio/pcma", media.LawAlaw}} {
		conn := uplinkSession(t, ts.URL)
		update, _ := json.Marshal(map[string]any{"type": "session.update", "session": map[string]any{
			"audio": map[string]any{"input": map[string]any{"format": map[string]any{"type": c.format}}},
		}})
		if err := conn.WriteMessage(websocket.TextMessage, update); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			appendAudio(t, conn, media.SilenceFrame(c.law))
		}
		for range 3 {
			appendAudio(t, conn, toneFrame(c.law))
		}
		appendAudio(t, conn, media.SilenceFrame(c.law))
	}

	waitFor(t, func() bool {
		s := sessionStats(t, ts.URL)
		return len(s) == 2 && s[0].UplinkFrames == 9 && s[1].UplinkFrames == 9
	}, "both sessions' frames counted")
	for _, s := range sessionStats(t, ts.URL) {
		if s.UplinkLoudFrames != 3 {
			t.Errorf("session %d: loud frames = %d, want 3", s.Seq, s.UplinkLoudFrames)
		}
		if s.FirstLoudUplinkAfterMs == nil || s.FirstUplinkAfterMs == nil || *s.FirstLoudUplinkAfterMs < *s.FirstUplinkAfterMs {
			t.Errorf("session %d: first loud %v, first %v", s.Seq, s.FirstLoudUplinkAfterMs, s.FirstUplinkAfterMs)
		}
	}
	if got := sessionStats(t, ts.URL); got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("sequence numbers = %d, %d, want 1, 2", got[0].Seq, got[1].Seq)
	}
}

func TestASilentUplinkHasNoLoudFrame(t *testing.T) {
	server := mockprovider.New(mockprovider.Config{TurnEvery: time.Hour, Log: quietLog()})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn := uplinkSession(t, ts.URL)
	for range 10 {
		appendAudio(t, conn, media.SilenceFrame(media.LawMu))
	}
	waitFor(t, func() bool {
		s := sessionStats(t, ts.URL)
		return len(s) == 1 && s[0].UplinkFrames == 10
	}, "ten silent frames counted")
	s := sessionStats(t, ts.URL)[0]
	if s.UplinkLoudFrames != 0 || s.FirstLoudUplinkAfterMs != nil {
		t.Errorf("silence counted as loud: %+v", s)
	}
	if !s.IsLive {
		t.Error("the session is not live")
	}
}

// The per-session list is for a test that asks; the plain stats stay small.
func TestSessionStatsAreServedOnlyWhenAskedFor(t *testing.T) {
	server := mockprovider.New(mockprovider.Config{TurnEvery: time.Hour, Log: quietLog()})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn := uplinkSession(t, ts.URL)
	appendAudio(t, conn, media.SilenceFrame(media.LawMu))
	waitFor(t, func() bool { s := sessionStats(t, ts.URL); return len(s) == 1 && s[0].UplinkFrames == 1 }, "a frame")
	resp, err := nethttp.Get(ts.URL + mockprovider.StatsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var plain map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&plain); err != nil {
		t.Fatal(err)
	}
	if _, has := plain["sessions"]; has {
		t.Errorf("the plain stats carry the session list: %v", plain)
	}
}
