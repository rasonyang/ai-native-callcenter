// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The latency budget lives or dies on one number: how long the caller waits
// between finishing their sentence and hearing the answer begin. It is
// measured, not assumed — per turn, from the provider reporting the caller
// stopped to the first frame of the reply being handed to the wire.
//
// The measurement deliberately starts at speech_stopped, after the VAD's
// silence hold: the hold is a configuration constant (500 ms by default), so
// the budget's caller-stop-to-caller-hears p50 ≤ 1.2 s corresponds to
// ≤ ~700 ms on this histogram.
var turnLatency metric.Int64Histogram

func init() {
	var err error
	turnLatency, err = otel.Meter("aicc").Int64Histogram(
		"aicc_turn_latency_ms",
		metric.WithDescription("Per turn: provider speech_stopped to the first "+
			"reply frame handed to the RTP queue, in milliseconds. The VAD "+
			"silence hold happens before this window."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(100, 200, 300, 400, 500, 700, 900,
			1200, 1600, 2000, 3000, 5000),
	)
	if err != nil {
		// The global meter cannot realistically fail; a no-op instrument keeps
		// the audio path alive if it ever does.
		turnLatency = nil
	}
}

// turnTimer follows one turn from the caller falling silent to the reply
// reaching the wire. All fields are guarded by the session's own mutex.
type turnTimer struct {
	// speechStoppedAt is zero when no measurement is in flight. The greeting
	// and cue-prompted turns have no caller speech, so they are not measured.
	speechStoppedAt time.Time
	// firstAudioMs is the provider's share: speech stopped to first delta.
	firstAudioMs int64
}

// onSpeechStopped starts a measurement.
func (t *turnTimer) onSpeechStopped() {
	t.speechStoppedAt = time.Now()
	t.firstAudioMs = 0
}

// onSpeechStarted abandons it: the caller resumed before any reply.
func (t *turnTimer) onSpeechStarted() { t.speechStoppedAt = time.Time{} }

// onFirstAudio records the provider's share of the wait.
func (t *turnTimer) onFirstAudio() {
	if !t.speechStoppedAt.IsZero() && t.firstAudioMs == 0 {
		t.firstAudioMs = time.Since(t.speechStoppedAt).Milliseconds()
	}
}

// onFirstFrame closes the measurement, reporting totals; ok is false when no
// measurement was in flight.
func (t *turnTimer) onFirstFrame() (totalMs, providerMs int64, ok bool) {
	if t.speechStoppedAt.IsZero() {
		return 0, 0, false
	}
	totalMs = time.Since(t.speechStoppedAt).Milliseconds()
	providerMs = t.firstAudioMs
	t.speechStoppedAt = time.Time{}
	return totalMs, providerMs, true
}

// recordTurnLatency publishes one turn's numbers to the histogram and the log.
// The log line is what makes a single phone call analysable from logs/ alone.
func recordTurnLatency(log *slog.Logger, providerName string, totalMs, providerMs int64) {
	if turnLatency != nil {
		turnLatency.Record(context.Background(), totalMs,
			metric.WithAttributes(attribute.String("provider", providerName)))
	}
	obs.RecordProviderFirstAudio(providerName, providerMs)
	log.Info("turn latency",
		"totalMs", totalMs, "providerMs", providerMs, "provider", providerName)
}
