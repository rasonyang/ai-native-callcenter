// SPDX-License-Identifier: Apache-2.0

package mockprovider

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// What the mock measures of a session's uplink: how many frames the
// application sent that were above silence, and when the first one arrived.
// A test uses it to prove that audio never reached the provider (the callee's
// greeting while the application listens first, #594). The threshold is the
// one the callee simulator uses for its own downlink (G.711 silence decodes to
// zero and a quiet line stays well under it).

// uplinkSilenceRMS is the linear level at or below which a frame is silence.
const uplinkSilenceRMS = 50

// maxSessionStats bounds the per-session list, so a long load run does not
// grow without limit; the oldest are dropped.
const maxSessionStats = 512

// SessionStat is one session's uplink, as served by GET StatsPath?sessions=1.
// Times are on the mock's monotonic clock, counted from the moment the session
// was accepted: they are never to be compared with another process's clock.
type SessionStat struct {
	// Seq numbers the sessions in the order they were accepted, from 1: the
	// n-th session of a server that has accepted n-1 before it.
	Seq int64 `json:"seq"`
	// IsLive is true until the session's socket closes.
	IsLive bool `json:"isLive"`
	// UplinkFrames counts the 20 ms frames of audio received, UplinkLoudFrames
	// those above silence.
	UplinkFrames     int64 `json:"uplinkFrames"`
	UplinkLoudFrames int64 `json:"uplinkLoudFrames"`
	// FirstLoudUplinkAfterMs is when the first frame above silence arrived,
	// FirstUplinkAfterMs the first frame of any kind; absent when there was
	// none.
	FirstLoudUplinkAfterMs *float64 `json:"firstLoudUplinkAfterMs,omitempty"`
	FirstUplinkAfterMs     *float64 `json:"firstUplinkAfterMs,omitempty"`
	// FirstResponseCreateAfterMs is when the application first asked for a
	// response (the opening line).
	FirstResponseCreateAfterMs *float64 `json:"firstResponseCreateAfterMs,omitempty"`
}

// uplinkMeter is a session's uplink counters.
type uplinkMeter struct {
	mu      sync.Mutex
	started time.Time // keeps its monotonic reading
	format  string    // the input format the application declared
	stat    SessionStat
}

func newUplinkMeter(seq int64) *uplinkMeter {
	return &uplinkMeter{started: time.Now(), stat: SessionStat{Seq: seq, IsLive: true}}
}

func (m *uplinkMeter) snapshot() SessionStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.stat
	for _, p := range []**float64{&out.FirstLoudUplinkAfterMs, &out.FirstUplinkAfterMs, &out.FirstResponseCreateAfterMs} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return out
}

func (m *uplinkMeter) sinceMs() float64 {
	return float64(time.Since(m.started)) / float64(time.Millisecond)
}

func (m *uplinkMeter) close() {
	m.mu.Lock()
	m.stat.IsLive = false
	m.mu.Unlock()
}

// noteResponseCreate records the first response.create.
func (m *uplinkMeter) noteResponseCreate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stat.FirstResponseCreateAfterMs == nil {
		v := m.sinceMs()
		m.stat.FirstResponseCreateAfterMs = &v
	}
}

// noteSessionUpdate reads the audio format the application declared for its
// input, from a session.update: "audio/pcmu", "audio/pcma" or "audio/pcm" in
// the GA dialect.
func (m *uplinkMeter) noteSessionUpdate(data []byte) {
	var update struct {
		Session struct {
			Audio struct {
				Input struct {
					Format struct {
						Type string `json:"type"`
					} `json:"format"`
				} `json:"input"`
			} `json:"audio"`
			InputAudioFormat string `json:"input_audio_format"`
		} `json:"session"`
	}
	if json.Unmarshal(data, &update) != nil {
		return
	}
	format := update.Session.Audio.Input.Format.Type
	if format == "" {
		format = update.Session.InputAudioFormat
	}
	if format == "" {
		return
	}
	m.mu.Lock()
	m.format = strings.ToLower(format)
	m.mu.Unlock()
}

// noteAppend measures one input_audio_buffer.append.
func (m *uplinkMeter) noteAppend(data []byte) {
	var event struct {
		Audio string `json:"audio"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(event.Audio)
	if err != nil || len(raw) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.sinceMs()
	for _, level := range frameLevels(m.format, raw) {
		m.stat.UplinkFrames++
		if m.stat.FirstUplinkAfterMs == nil {
			v := at
			m.stat.FirstUplinkAfterMs = &v
		}
		if level > uplinkSilenceRMS {
			m.stat.UplinkLoudFrames++
			if m.stat.FirstLoudUplinkAfterMs == nil {
				v := at
				m.stat.FirstLoudUplinkAfterMs = &v
			}
		}
	}
}

// frameLevels is the RMS level of each 20 ms frame in an append. G.711 (the
// default, and what the application sends on a telephone leg) is cut into
// 160-byte frames; linear PCM16 is measured as one.
func frameLevels(format string, raw []byte) []float64 {
	switch {
	case strings.Contains(format, "pcma"), strings.Contains(format, "alaw"):
		return g711Levels(media.LawAlaw, raw)
	case format == "audio/pcm" || format == "pcm" || strings.Contains(format, "pcm16"):
		samples := media.BytesToPCM16(nil, raw[:len(raw)&^1])
		return []float64{rms(samples)}
	default:
		return g711Levels(media.LawMu, raw)
	}
}

func g711Levels(law media.Law, raw []byte) []float64 {
	var out []float64
	for len(raw) > 0 {
		n := min(len(raw), media.FrameSamples)
		out = append(out, rms(law.Decode(nil, raw[:n])))
		raw = raw[n:]
	}
	return out
}

func rms(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}
