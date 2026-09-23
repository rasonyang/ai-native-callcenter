// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The caller's wait per turn — the number the latency budget lives or dies
// on — is measured here and published through obs.RecordTurnLatency, which
// owns the instrument (aicc_turn_latency_ms) with every other metric name.

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
	obs.RecordTurnLatency(providerName, totalMs)
	obs.RecordProviderFirstAudio(providerName, providerMs)
	log.Info("turn latency",
		"totalMs", totalMs, "providerMs", providerMs, "provider", providerName)
}
