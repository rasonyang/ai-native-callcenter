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
//
// A measurement ends one of four ways:
//   - the first reply frame is queued: a latency sample (onFirstFrame);
//   - the caller speaks again or presses a key: abandoned, nothing recorded
//     (onSpeechStarted), because the turn being waited on is no longer the
//     caller's last;
//   - the application steps in while the engine has not answered: the turn
//     went unanswered (onSteppedIn). A dead-air cue, a goodbye or a phase's
//     line says nothing about how fast the engine answers, and the reply that
//     follows it must not be booked as the caller's wait;
//   - the engine answered (a response started after speech_stopped) and the
//     application steps in anyway: nothing changes, the measurement stays
//     open and closes on the first frame. A tool call that makes no sound
//     answers the turn, so a slow tool is latency, never "unanswered".
type turnTimer struct {
	// speechStoppedAt is zero when no measurement is in flight. The greeting
	// and cue-prompted turns have no caller speech, so they are not measured.
	speechStoppedAt time.Time
	// firstAudioMs is the provider's share: speech stopped to first delta.
	firstAudioMs int64
	// isAnswered is true once a model response has started since
	// speech_stopped. It is meaningful only while a measurement is open.
	isAnswered bool
}

// Why the application closed a measurement as unanswered.
const (
	// turnUnansweredNoInput: the dead-air watch fired.
	turnUnansweredNoInput = "NO_INPUT"
	// turnUnansweredCue: a text cue went to the model (SendCue).
	turnUnansweredCue = "CUE"
	// turnUnansweredLine: a line was said as written (Speak, SpeakAside).
	turnUnansweredLine = "LINE"
)

// onSpeechStopped starts a measurement.
func (t *turnTimer) onSpeechStopped() {
	t.speechStoppedAt = time.Now()
	t.firstAudioMs = 0
	t.isAnswered = false
}

// onSpeechStarted abandons it: the caller resumed before any reply.
func (t *turnTimer) onSpeechStarted() {
	t.speechStoppedAt = time.Time{}
	t.isAnswered = false
}

// onResponseStarted notes that the engine has begun a response, audio or not.
func (t *turnTimer) onResponseStarted() {
	if !t.speechStoppedAt.IsZero() {
		t.isAnswered = true
	}
}

// onSteppedIn closes an open, unanswered measurement because the application
// spoke or prompted first; waitMs is speech_stopped to now. ok is false when
// nothing is open or the engine has answered, and then nothing changes.
func (t *turnTimer) onSteppedIn() (waitMs int64, ok bool) {
	if t.speechStoppedAt.IsZero() || t.isAnswered {
		return 0, false
	}
	waitMs = time.Since(t.speechStoppedAt).Milliseconds()
	t.speechStoppedAt = time.Time{}
	return waitMs, true
}

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
	t.isAnswered = false
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

// recordTurnUnanswered publishes a caller turn the engine never answered
// before the application stepped in. It feeds neither latency histogram.
func recordTurnUnanswered(log *slog.Logger, providerName string, waitMs int64, reason string) {
	obs.RecordTurnUnanswered(providerName)
	log.Info("caller turn went unanswered",
		"waitMs", waitMs, "reason", reason, "provider", providerName)
}
