// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// ProviderRate is the speech provider's quota as this process has spent it
// (ratemeter.Meter, fed by every bot session, inbound ones included), each
// count over its limit's period. Nil: the provider rule is not applied.
type ProviderRate interface {
	Read() ratemeter.Reading
}

// providerQuota is the provider's quota as the rule applies it: a count over
// a period for session starts and for tokens; a limit of 0 is none.
type providerQuota struct {
	sessionLimit  int
	sessionPeriod time.Duration
	tokenLimit    int
	tokenPeriod   time.Duration
}

func (c Config) providerQuota() providerQuota {
	return providerQuota{
		sessionLimit: c.ProviderSessionLimit, sessionPeriod: c.ProviderSessionPeriod,
		tokenLimit: c.ProviderTokenLimit, tokenPeriod: c.ProviderTokenPeriod,
	}
}

// backoff is how long admission holds after the provider last said its quota
// was spent (#644): the longer of the periods of the limits in force, by when
// whichever window the refusal broke has cleared; a minute (ratemeter.Span)
// when no limit is, the period the vendors document.
func (q providerQuota) backoff() time.Duration {
	var d time.Duration
	if q.sessionLimit > 0 {
		d = max(d, q.sessionPeriod)
	}
	if q.tokenLimit > 0 {
		d = max(d, q.tokenPeriod)
	}
	if d <= 0 {
		return ratemeter.Span
	}
	return d
}

// providerHold applies the provider's quota to one more admission (design 10
// section 6.3, as built for #604 and #644):
//
//	now >= lastQuotaRefusal + backoff
//	starts_W + n <= sessionLimit x W / sessionPeriod               when sessionLimit > 0
//	tokens_T + n x tokens_T / max(1, active) <= tokenLimit         when tokenLimit > 0
//	where n = pending + waiting + E + 1
//
// W is ratemeter.AdmissionWindow of the session period: the period, or ten
// seconds when the period is shorter, with the limit scaled to it, because
// the answers E stands for land over seconds, not at once; the start gate
// (ratemeter.Meter.Pace) keeps every shorter period inside the limit. A
// per-minute limit is judged over its minute, as in v0.9.0. T is the token
// period: tokens are spent all along by the sessions in progress, so their
// period is the vendor's own. starts_W and tokens_T are the meter's. E is the sum over campaigns of p_c x ringing_c, the answers the
// ringing legs are expected to bring, each of which opens a session at the
// answer. pending is the answered legs that hold or wait for a bot slot and
// have not opened their session yet (voice.Usage.Reserved + Waiting), counted
// for the reason they are in the bot rule, and waiting the answered legs
// whose start waits in the gate. active is the bot sessions in use, so
// tokens_T / active is what a session has cost over the token period; with no
// tokens yet the token rule admits, and the session rule is what guards a cold
// burst. backoff is providerQuota.backoff.
//
// The refusal backoff applies whatever the limits are, a limit of 0 included:
// a refusal is the provider's word, not an estimate.
//
// It returns the reason the call is held, empty when it is not, and when the
// condition can next change by time alone (the backoff ending, the oldest
// start or token entry leaving its window); zero when only an event can
// change it.
func providerHold(r ratemeter.Reading, q providerQuota, pending, expected float64, active int) (string, time.Time) {
	if backoff := q.backoff(); !r.LastQuotaRefusal.IsZero() && r.At.Before(r.LastQuotaRefusal.Add(backoff)) {
		return obs.DialerBlockedProviderBackoff, r.LastQuotaRefusal.Add(backoff)
	}
	n := pending + float64(r.StartsWaiting) + expected + 1
	if q.sessionLimit > 0 {
		period := max(q.sessionPeriod, time.Nanosecond)
		limit := float64(q.sessionLimit) * float64(ratemeter.AdmissionWindow(period)) / float64(period)
		if float64(r.Starts)+n > limit+1e-9 {
			return obs.DialerBlockedProviderSessions, r.StartsDropAt
		}
	}
	if q.tokenLimit > 0 && r.Tokens > 0 {
		perSession := float64(r.Tokens) / float64(max(1, active))
		if float64(r.Tokens)+n*perSession > float64(q.tokenLimit)+1e-9 {
			return obs.DialerBlockedProviderTokens, r.TokensDropAt
		}
	}
	return "", time.Time{}
}

// providerLimit is the provider rule as limit applies it: the reason, if the
// provider's quota holds one more call. A hold that time alone can end asks
// for a pass at that moment (rateWake), because no event may arrive to wake
// the dialer: a refusal's backoff can end with nothing in flight.
func (d *Dialer) providerLimit(u voice.Usage, expected float64) string {
	r := d.pr.Read()
	reason, until := providerHold(r, d.cfg.providerQuota(),
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
	q := d.cfg.providerQuota()
	attrs := []any{"starts", r.Starts, "startsWaiting", r.StartsWaiting, "tokens", r.Tokens,
		"sessionLimit", q.sessionLimit, "sessionPeriod", q.sessionPeriod,
		"tokenLimit", q.tokenLimit, "tokenPeriod", q.tokenPeriod}
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
