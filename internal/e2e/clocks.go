// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The clocks the checks read (issues #504, #506 and #515; deploy/e2e/README.md,
// "Clocks"). The driver, the simulator and the dialer stamp events on the VM's
// wall clock, which steps: on Colima lima-guestagent sets it from the host
// every 10 s. So a duration or an order between two events the simulator
// recorded is read on the simulator's monotonic clock: inside one dialog from
// afterInviteMs, across dialogs from afterStartMs, the timeline every dialog of
// one simulator shares. An order the driver needs between two of its own
// moments is read on the driver's monotonic clock. The wall-clock stamps stay
// in the records and are printed beside the monotonic values, so a step shows
// up as the gap between the two readings.

// simMoment is an event the simulator recorded, on both of its clocks: at is
// its wall-clock stamp and ms its place on the simulator's monotonic timeline
// (milliseconds after the simulator started). Checks compare ms.
type simMoment struct {
	at time.Time
	ms float64
}

// inviteMoment is the dialog's INVITE.
func inviteMoment(d calleesim.DialogRecord) simMoment {
	return simMoment{at: d.InviteAt, ms: d.InviteAfterStartMs}
}

// finalMoment is the dialog's final response; ok is false when it has none.
func finalMoment(d calleesim.DialogRecord) (m simMoment, ok bool) {
	if d.Final == nil {
		return simMoment{}, false
	}
	return simMoment{at: d.Final.SentAt, ms: d.Final.AfterStartMs}, true
}

// dialogEnd is when a dialog stopped holding a line: the BYE or CANCEL that
// ended it, or the error response that refused it. ok is false for one that
// has not ended.
func dialogEnd(d calleesim.DialogRecord) (simMoment, bool) {
	if d.End != nil {
		return simMoment{at: d.End.At, ms: d.End.AfterStartMs}, true
	}
	if d.Final != nil && d.Final.Status != 200 {
		return simMoment{at: d.Final.SentAt, ms: d.Final.AfterStartMs}, true
	}
	return simMoment{}, false
}

// before reports whether m came before o on the simulator's monotonic clock.
func (m simMoment) before(o simMoment) bool { return m.ms < o.ms }

// sinceMs is m's time after from on the simulator's monotonic clock, and
// wallSinceMs the same interval as two wall-clock stamps.
func (m simMoment) sinceMs(from simMoment) float64 { return m.ms - from.ms }

func (m simMoment) wallSinceMs(from simMoment) float64 {
	return float64(m.at.Sub(from.at).Milliseconds())
}

// earliest picks the first moment on the simulator's monotonic clock.
func earliest(first simMoment, rest ...simMoment) simMoment {
	for _, m := range rest {
		if m.before(first) {
			first = m
		}
	}
	return first
}

// holdMsOf is a dialog's time from its 200 OK to its end: on the simulator's
// monotonic clock (ms), and as two wall-clock stamps (wallMs). ok is false
// when the dialog was not answered or has not ended.
func holdMsOf(d calleesim.DialogRecord) (ms, wallMs float64, ok bool) {
	if d.Final == nil || d.Final.Status != 200 || d.End == nil {
		return 0, 0, false
	}
	return d.End.AfterInviteMs - d.Final.AfterInviteMs, float64(d.End.At.Sub(d.Final.SentAt).Milliseconds()), true
}

// durMs is a duration in milliseconds, for a bound compared with a monotonic
// value.
func durMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
