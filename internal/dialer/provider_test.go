// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
)

// The provider rule's arithmetic, one condition at a time (#604).
func TestTheProviderRuleArithmetic(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	tests := []struct {
		name                 string
		r                    ratemeter.Reading
		sessionRPM, tokenRPM int
		pending, expected    float64
		active               int
		want                 string
		wantUntil            time.Time
	}{
		{name: "nothing spent, no limits", r: ratemeter.Reading{At: t0}},
		{name: "no limits admit any rate", r: ratemeter.Reading{At: t0, Starts: 5000, Tokens: 1e9}},
		// starts60 + pending + E + 1 <= 60
		{name: "the 60th start fits", r: ratemeter.Reading{At: t0, Starts: 50, StartsDropAt: at(time.Second)},
			sessionRPM: 60, pending: 2, expected: 7},
		{name: "the 61st does not", r: ratemeter.Reading{At: t0, Starts: 50, StartsDropAt: at(time.Second)},
			sessionRPM: 60, pending: 2, expected: 7.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "expected answers alone can fill it", r: ratemeter.Reading{At: t0},
			sessionRPM: 60, expected: 59.5, want: obs.DialerBlockedProviderSessions},
		// tokens60 + n x tokens60/active <= TokenRPM, n = pending + E + 1
		{name: "cold start admits on tokens", r: ratemeter.Reading{At: t0}, tokenRPM: 100000, expected: 500},
		// 80000 + 10 x 80000/40 = 100000.
		{name: "the last session's share fits", r: ratemeter.Reading{At: t0, Tokens: 80000, TokensDropAt: at(2 * time.Second)},
			tokenRPM: 100000, expected: 9, active: 40},
		{name: "one more share does not", r: ratemeter.Reading{At: t0, Tokens: 80000, TokensDropAt: at(2 * time.Second)},
			tokenRPM: 100000, expected: 9, pending: 1, active: 40,
			want: obs.DialerBlockedProviderTokens, wantUntil: at(2 * time.Second)},
		{name: "no session in use counts as one", r: ratemeter.Reading{At: t0, Tokens: 40000},
			tokenRPM: 100000, expected: 1, want: obs.DialerBlockedProviderTokens},
		// The backoff holds whatever the limits.
		{name: "a refusal a second ago holds with no limits", r: ratemeter.Reading{At: t0, LastQuotaRefusal: at(-time.Second)},
			want: obs.DialerBlockedProviderBackoff, wantUntil: at(59 * time.Second)},
		{name: "the backoff comes before the rates", r: ratemeter.Reading{At: t0, Starts: 100, LastQuotaRefusal: at(-30 * time.Second)},
			sessionRPM: 60, want: obs.DialerBlockedProviderBackoff, wantUntil: at(30 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, tt.sessionRPM, tt.tokenRPM, tt.pending, tt.expected, tt.active)
			if got != tt.want || !until.Equal(tt.wantUntil) {
				t.Errorf("providerHold = %q until %v, want %q until %v", got, until, tt.want, tt.wantUntil)
			}
		})
	}
}

// No admission for a minute after the latest quota refusal: held at 59.999 s,
// admitted at 60 s.
func TestTheQuotaBackoffEndsAtExactlyOneMinute(t *testing.T) {
	refused := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		after time.Duration
		want  string
	}{
		{0, obs.DialerBlockedProviderBackoff},
		{time.Minute - time.Millisecond, obs.DialerBlockedProviderBackoff},
		{time.Minute, ""},
		{2 * time.Minute, ""},
	} {
		r := ratemeter.Reading{At: refused.Add(tt.after), LastQuotaRefusal: refused}
		if got, _ := providerHold(r, 0, 0, 0, 0, 0); got != tt.want {
			t.Errorf("%v after the refusal: %q, want %q", tt.after, got, tt.want)
		}
	}
}

// A refusal holds the campaign, which says so (AT_CAPACITY), and the hold ends
// by time alone: with nothing in flight there is no event to wake the dialer,
// so the pass that dials again is the one it asked for at the refusal's minute.
func TestARefusalHoldsAdmissionForAMinuteAndTheDialerWakesForItsEnd(t *testing.T) {
	s := newSim(t, simSpec{capacity: 50, campaigns: 1, seed: 1, maxConcurrent: 5, isGated: true})
	t0 := s.clock.Now()
	s.meter.QuotaRefused() // an inbound call's session, refused at t0
	s.clock.Set(t0.Add(30 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	s.settle()

	c := s.store.camps[0]
	if s.placements != 0 {
		t.Fatalf("%d calls placed within a minute of a quota refusal", s.placements)
	}
	if p, _ := s.d.Progress(c.ID); p.Activity != campaign.ActivityAtCapacity {
		t.Errorf("activity = %v, want AT_CAPACITY", p.Activity)
	}

	s.clock.Set(t0.Add(time.Minute - time.Millisecond))
	if err := s.d.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s.placements != 0 {
		t.Fatalf("%d calls placed before the refusal's minute was over", s.placements)
	}
	// No Wake: only the dialer's own timer, due at the minute, makes it look.
	s.clock.Set(t0.Add(time.Minute))
	if err := s.d.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s.placements != 5 {
		t.Errorf("%d calls placed once the minute was over, want the campaign's 5", s.placements)
	}
}

// simProvider is a speech provider with a per-minute quota, as the vendor of
// #604 documents its own: it refuses a session start once the starts, or the
// tokens, of its own last minute have reached the limit. A refused attempt
// still counts as a connect. A session it accepts reports tokensPerTurn every
// turnEvery until the call ends.
type simProvider struct {
	connectLimit  int
	tokenLimit    int64
	tokensPerTurn int
	turnEvery     time.Duration

	starts           []time.Time
	spent            []simSpend
	accepted, refuse int
	// refusedAfter counts the refusals at or after measureFrom.
	measureFrom  time.Time
	refusedAfter int
	startedAfter int
}

type simSpend struct {
	at time.Time
	n  int64
}

func (p *simProvider) prune(now time.Time) {
	for len(p.starts) > 0 && !now.Before(p.starts[0].Add(time.Minute)) {
		p.starts = p.starts[1:]
	}
	for len(p.spent) > 0 && !now.Before(p.spent[0].at.Add(time.Minute)) {
		p.spent = p.spent[1:]
	}
}

func (p *simProvider) start(now time.Time) bool {
	p.prune(now)
	var tokens int64
	for _, s := range p.spent {
		tokens += s.n
	}
	isRefused := (p.connectLimit > 0 && len(p.starts) >= p.connectLimit) ||
		(p.tokenLimit > 0 && tokens >= p.tokenLimit)
	p.starts = append(p.starts, now)
	isMeasured := !now.Before(p.measureFrom)
	if isMeasured {
		p.startedAfter++
	}
	if isRefused {
		p.refuse++
		if isMeasured {
			p.refusedAfter++
		}
		return false
	}
	p.accepted++
	return true
}

func (p *simProvider) spend(now time.Time, n int) {
	p.prune(now)
	p.spent = append(p.spent, simSpend{at: now, n: int64(n)})
}

// providerRun is one simulated campaign against the provider, gated or not.
type providerRun struct {
	starts, refusals           int
	startsAfter, refusalsAfter int
}

func runAgainstProvider(t *testing.T, isGated bool, prov simProvider, sessionRPM, tokenRPM int, warmUp, d time.Duration) providerRun {
	t.Helper()
	p := prov
	s := newSim(t, simSpec{capacity: 60, inboundReserve: 4, campaigns: 1, seed: 21, maxConcurrent: 1000,
		rates: []func(time.Duration) float64{constRate(0.5)}, provider: &p,
		isGated: isGated, sessionRPM: sessionRPM, tokenRPM: tokenRPM})
	p.measureFrom = s.start.Add(warmUp)
	s.run(d)
	return providerRun{
		starts: p.accepted + p.refuse, refusals: p.refuse,
		startsAfter: p.startedAfter, refusalsAfter: p.refusedAfter,
	}
}

// The deployment of #604, scaled down: more bot capacity than the provider's
// connect rate can feed, a list that answers half its calls, and a provider
// that takes 20 connects a minute. Admission by expected answers alone fills
// the bot, the starts run far above the connect rate and most answered callers
// meet a refusal (each one frees its slot a second later, which admits the
// next). With the provider rule the starts stay at the rate and refusals near
// zero, and the rate is still used.
func TestTheConnectRateGateKeepsRefusalsNearZero(t *testing.T) {
	const minutes, rate = 15, 20
	prov := simProvider{connectLimit: rate}

	open := runAgainstProvider(t, false, prov, 0, 0, 0, minutes*time.Minute)
	gated := runAgainstProvider(t, true, prov, rate, 0, 0, minutes*time.Minute)
	t.Logf("without the gate: %d starts, %d refused", open.starts, open.refusals)
	t.Logf("with the gate:    %d starts, %d refused", gated.starts, gated.refusals)

	if open.refusals < open.starts/5 {
		t.Fatalf("without the gate only %d of %d starts were refused: the scenario proves nothing",
			open.refusals, open.starts)
	}
	// Not zero: the rule counts a ringing leg as p_c of an answer, p_c an
	// upper bound on the campaign's rate, and a run of answers above it can
	// still put one start over the limit.
	if gated.refusals*50 > gated.starts {
		t.Errorf("with the gate %d of %d starts were refused, want at most 2%%", gated.refusals, gated.starts)
	}
	if gated.starts < minutes*rate/2 {
		t.Errorf("with the gate only %d starts in %d minutes: the connect rate of %d a minute is left unused",
			gated.starts, minutes, rate)
	}
}

// The same list against a provider whose binding cap is tokens: 100,000 a
// minute, each session spending 3,600 a minute, so under 28 sessions at once
// where the bot could hold more than 50. Without the gate refusals never stop;
// with it they stop once a minute of usage is known (the token rule estimates
// a session's share from the last minute, so the first minutes of a cold start
// can overshoot, and the refusal backoff is what bounds them).
func TestTheTokenRateGateStopsRefusalsOnceUsageIsKnown(t *testing.T) {
	const minutes = 15
	warmUp := 5 * time.Minute
	prov := simProvider{tokenLimit: 100000, tokensPerTurn: 900, turnEvery: 15 * time.Second}

	open := runAgainstProvider(t, false, prov, 0, 0, warmUp, minutes*time.Minute)
	gated := runAgainstProvider(t, true, prov, 0, 100000, warmUp, minutes*time.Minute)
	t.Logf("without the gate: %d starts, %d refused (%d of %d after the warm-up)",
		open.starts, open.refusals, open.refusalsAfter, open.startsAfter)
	t.Logf("with the gate:    %d starts, %d refused (%d of %d after the warm-up)",
		gated.starts, gated.refusals, gated.refusalsAfter, gated.startsAfter)

	if open.refusalsAfter < open.startsAfter/5 {
		t.Fatalf("without the gate only %d of %d starts were refused after the warm-up: the scenario proves nothing",
			open.refusalsAfter, open.startsAfter)
	}
	if gated.refusalsAfter*50 > gated.startsAfter {
		t.Errorf("with the gate %d of %d starts after the warm-up were refused, want at most 2%%",
			gated.refusalsAfter, gated.startsAfter)
	}
	if gated.startsAfter < 100 {
		t.Errorf("with the gate only %d starts after the warm-up: the token quota is left unused", gated.startsAfter)
	}
}
