// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// ProviderRate is the speech provider's quota as this process has spent it
// over the last minute (ratemeter.Meter, fed by every bot session, inbound
// ones included). Nil: the provider rule is not applied.
type ProviderRate interface {
	Read() ratemeter.Reading
}

// quotaBackoff is how long admission holds after the provider last said its
// quota was spent. The vendors' limits are per minute, so a minute is when the
// refusal can have cleared.
const quotaBackoff = ratemeter.Span

// providerHold applies the provider's quota to one more admission (design 10
// section 6.3, as built for #604):
//
//	now >= lastQuotaRefusal + 60 s
//	starts60 + n <= sessionRPM                                    when sessionRPM > 0
//	tokens60 + n x tokens60 / max(1, active) <= tokenRPM           when tokenRPM > 0
//	where n = pending + E + 1
//
// starts60 and tokens60 are the meter's last minute. E is the sum over
// campaigns of p_c x ringing_c, the answers the ringing legs are expected to
// bring, each of which opens a session at the answer. pending is the answered
// legs that hold or wait for a bot slot and have not opened their session yet
// (voice.Usage.Reserved + Waiting), counted for the reason they are in the bot
// rule. active is the bot sessions in use, so tokens60 / active is what a
// session has cost over the last minute; with no tokens yet the token rule
// admits, and the session rule is what guards a cold burst.
//
// The refusal backoff applies whatever the limits are, a limit of 0 included:
// a refusal is the provider's word, not an estimate.
//
// It returns the reason the call is held, empty when it is not, and when the
// condition can next change by time alone (the refusal's minute ending, the
// oldest start or token entry leaving the window); zero when only an event
// can change it.
func providerHold(r ratemeter.Reading, sessionRPM, tokenRPM int, pending, expected float64, active int) (string, time.Time) {
	if !r.LastQuotaRefusal.IsZero() && r.At.Before(r.LastQuotaRefusal.Add(quotaBackoff)) {
		return obs.DialerBlockedProviderBackoff, r.LastQuotaRefusal.Add(quotaBackoff)
	}
	n := pending + expected + 1
	if sessionRPM > 0 && float64(r.Starts)+n > float64(sessionRPM)+1e-9 {
		return obs.DialerBlockedProviderSessions, r.StartsDropAt
	}
	if tokenRPM > 0 && r.Tokens > 0 {
		perSession := float64(r.Tokens) / float64(max(1, active))
		if float64(r.Tokens)+n*perSession > float64(tokenRPM)+1e-9 {
			return obs.DialerBlockedProviderTokens, r.TokensDropAt
		}
	}
	return "", time.Time{}
}

// providerLimit is the provider rule as limit applies it: the reason, if the
// provider's quota holds one more call. A hold that time alone can end asks
// for a pass at that moment (rateWake), because no event may arrive to wake
// the dialer: a refusal's minute can end with nothing in flight.
func (d *Dialer) providerLimit(u voice.Usage, expected float64) string {
	r := d.pr.Read()
	reason, until := providerHold(r, d.cfg.ProviderSessionRPM, d.cfg.ProviderTokenRPM,
		float64(u.Reserved+u.Waiting), expected, u.InUse)
	if reason != "" && !until.IsZero() && (d.rateWake.IsZero() || until.Before(d.rateWake)) {
		d.rateWake = until
	}
	d.noteProviderHold(reason, r)
	return reason
}

// noteProviderHold logs the provider rule starting to hold calls, changing
// reason, and letting them through again: once per change, not per contact,
// so the log says when campaigns were paced by the provider and why.
func (d *Dialer) noteProviderHold(reason string, r ratemeter.Reading) {
	if reason == d.providerHeld {
		return
	}
	attrs := []any{"startsLastMinute", r.Starts, "tokensLastMinute", r.Tokens,
		"sessionRpm", d.cfg.ProviderSessionRPM, "tokenRpm", d.cfg.ProviderTokenRPM}
	if !r.LastQuotaRefusal.IsZero() {
		attrs = append(attrs, "lastQuotaRefusalAt", r.LastQuotaRefusal)
	}
	if reason != "" {
		d.log.Info("dialer: admission held by the provider's quota", append([]any{"reason", reason}, attrs...)...)
	} else {
		d.log.Info("dialer: the provider's quota admits calls again", attrs...)
	}
	d.providerHeld = reason
}
