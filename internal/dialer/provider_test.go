// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"cmp"
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
)

// perMinute is the quota of the RPM settings, and of the profiles' defaults:
// each limit over 60 s.
func perMinute(sessions, tokens int) providerQuota {
	return providerQuota{sessionLimit: sessions, sessionPeriod: time.Minute, tokenLimit: tokens, tokenPeriod: time.Minute}
}

// The provider rule's arithmetic, one condition at a time (#604), with limits
// per minute: what the RPM settings and the profiles' defaults give, the same
// as in v0.9.0.
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
		{name: "a refusal a second ago holds with no limits", r: ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-time.Second)},
			want: obs.DialerBlockedProviderBackoff + "/UNKNOWN", wantUntil: at(59 * time.Second)},
		{name: "the backoff comes before the rates", r: ratemeter.Reading{At: t0, Starts: 100, LastQuotaRefusalUnknown: at(-30 * time.Second)},
			sessionRPM: 60, want: obs.DialerBlockedProviderBackoff + "/UNKNOWN", wantUntil: at(30 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, perMinute(tt.sessionRPM, tt.tokenRPM), tt.pending, tt.expected, tt.active)
			if got != tt.want || !until.Equal(tt.wantUntil) {
				t.Errorf("providerHold = %q until %v, want %q until %v", got, until, tt.want, tt.wantUntil)
			}
		})
	}
}

// No admission for a minute after the latest quota refusal: held at 59.999 s,
// admitted at 60 s. A minute is the backoff when no limit is in force, and
// when the limits are per minute (the RPM settings, the profiles' defaults).
func TestTheQuotaBackoffEndsAtExactlyOneMinute(t *testing.T) {
	refused := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for _, q := range []providerQuota{{}, perMinute(60, 100000), perMinute(60, 0), perMinute(0, 100000)} {
		for _, tt := range []struct {
			after time.Duration
			want  string
		}{
			{0, obs.DialerBlockedProviderBackoff + "/UNKNOWN"},
			{time.Minute - time.Millisecond, obs.DialerBlockedProviderBackoff + "/UNKNOWN"},
			{time.Minute, ""},
			{2 * time.Minute, ""},
		} {
			r := ratemeter.Reading{At: refused.Add(tt.after), LastQuotaRefusalUnknown: refused}
			if got, _ := providerHold(r, q, 0, 0, 0); got != tt.want {
				t.Errorf("%+v, %v after the refusal: %q, want %q", q, tt.after, got, tt.want)
			}
		}
	}
}

// The quota of #644, a count over a period the vendor chooses: 3 session
// starts per 1 s and 60,000 tokens per 12 s.
func TestTheProviderRuleOverTheVendorsPeriods(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	q := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	tests := []struct {
		name              string
		q                 providerQuota
		r                 ratemeter.Reading
		pending, expected float64
		active            int
		want              string
		wantUntil         time.Time
	}{
		// Admission judges starts over ten seconds, 3 per 1 s being 30 per
		// 10 s (ratemeter.AdmissionWindow); the start gate keeps each second.
		{name: "the 30th start of the ten seconds fits", q: q,
			r: ratemeter.Reading{At: t0, Starts: 20, StartsDropAt: at(time.Second)}, pending: 2, expected: 7},
		{name: "the 31st does not, until the oldest start leaves the ten seconds", q: q,
			r: ratemeter.Reading{At: t0, Starts: 20, StartsDropAt: at(time.Second)}, pending: 2, expected: 7.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "a start waiting in the gate is one about to be made", q: q,
			r: ratemeter.Reading{At: t0, Starts: 20, StartsWaiting: 1, StartsDropAt: at(time.Second)}, pending: 2, expected: 7,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "three starts in the last second alone do not hold admission", q: q,
			r: ratemeter.Reading{At: t0, Starts: 3, StartsDropAt: at(9 * time.Second)}, expected: 2},
		// Tokens are judged over their own 12 s: 50,000 + 4 x 50,000/40 = 55,000.
		{name: "the token share over 12 s fits", q: q,
			r: ratemeter.Reading{At: t0, Tokens: 50000, TokensDropAt: at(3 * time.Second)}, expected: 3, active: 40},
		// 50,000 + 9 x 50,000/40 = 61,250.
		{name: "a larger share over 12 s does not, until the oldest tokens leave it", q: q,
			r: ratemeter.Reading{At: t0, Tokens: 50000, TokensDropAt: at(3 * time.Second)}, expected: 8, active: 40,
			want: obs.DialerBlockedProviderTokens, wantUntil: at(3 * time.Second)},
		// The backoff is the longer of the two periods, 12 s.
		{name: "a refusal holds for the longer period", q: q,
			r:    ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-time.Second)},
			want: obs.DialerBlockedProviderBackoff + "/UNKNOWN", wantUntil: at(11 * time.Second)},
		{name: "with the token limit off, for the session period", q: providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenPeriod: 12 * time.Second},
			r:    ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-500 * time.Millisecond)},
			want: obs.DialerBlockedProviderBackoff + "/UNKNOWN", wantUntil: at(500 * time.Millisecond)},
		{name: "a period longer than a minute is its own window", q: providerQuota{sessionLimit: 1000, sessionPeriod: time.Hour},
			r: ratemeter.Reading{At: t0, Starts: 990, StartsDropAt: at(time.Minute)}, expected: 9.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Minute)},
		{name: "and its backoff", q: providerQuota{sessionLimit: 1000, sessionPeriod: time.Hour},
			r:    ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-time.Minute)},
			want: obs.DialerBlockedProviderBackoff + "/UNKNOWN", wantUntil: at(59 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, tt.q, tt.pending, tt.expected, tt.active)
			if got != tt.want || !until.Equal(tt.wantUntil) {
				t.Errorf("providerHold = %q until %v, want %q until %v", got, until, tt.want, tt.wantUntil)
			}
		})
	}
}

// After a refusal at 3 per 1 s and 60,000 per 12 s, admission resumes once the
// longer period has passed: held at 11.999 s, admitted at 12 s.
func TestTheQuotaBackoffEndsAfterTheLongerPeriod(t *testing.T) {
	refused := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	q := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	for _, tt := range []struct {
		after time.Duration
		want  string
	}{
		{0, obs.DialerBlockedProviderBackoff + "/UNKNOWN"},
		{time.Second, obs.DialerBlockedProviderBackoff + "/UNKNOWN"},
		{12*time.Second - time.Millisecond, obs.DialerBlockedProviderBackoff + "/UNKNOWN"},
		{12 * time.Second, ""},
	} {
		r := ratemeter.Reading{At: refused.Add(tt.after), LastQuotaRefusalUnknown: refused}
		if got, _ := providerHold(r, q, 0, 0, 0); got != tt.want {
			t.Errorf("%v after the refusal: %q, want %q", tt.after, got, tt.want)
		}
	}
}

// A refusal that names a limit holds for that limit's period, not the longer
// of both (#652): at 3 per 1 s and 60,000 per 12 s a REQUESTS refusal ends
// with the session window, a TOKENS or UNKNOWN one with the token window.
// Refusals of both kinds end when the later one ends: REQUESTS arriving
// during a TOKENS hold does not shorten it; TOKENS during REQUESTS extends it.
func TestEachQuotaRefusalHoldsForItsOwnLimitPeriod(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	q := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	for _, tt := range []struct {
		name  string
		r     ratemeter.Reading
		want  string
		until time.Duration
	}{
		{"a REQUESTS refusal holds for the session period",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0},
			obs.DialerBlockedProviderBackoff + "/REQUESTS", time.Second},
		{"a TOKENS refusal holds for the token period",
			ratemeter.Reading{At: t0, LastQuotaRefusalTokens: t0},
			obs.DialerBlockedProviderBackoff + "/TOKENS", 12 * time.Second},
		{"an UNKNOWN refusal holds for the longer period",
			ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: t0},
			obs.DialerBlockedProviderBackoff + "/UNKNOWN", 12 * time.Second},
		{"a REQUESTS refusal from 900ms ago has ended at 1s, a TOKENS one has not",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-900 * time.Millisecond)},
			obs.DialerBlockedProviderBackoff + "/REQUESTS", 100 * time.Millisecond},
		{"both outstanding: the hold ends when the later one ends",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-time.Second), LastQuotaRefusalTokens: t0.Add(-time.Second)},
			obs.DialerBlockedProviderBackoff + "/TOKENS", 11 * time.Second},
		{"a REQUESTS refusal during a TOKENS hold does not shorten it",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0, LastQuotaRefusalTokens: t0.Add(-6 * time.Second)},
			obs.DialerBlockedProviderBackoff + "/TOKENS", 6 * time.Second},
		{"a TOKENS refusal during a REQUESTS hold extends it",
			ratemeter.Reading{At: t0, LastQuotaRefusalTokens: t0, LastQuotaRefusalRequests: t0.Add(-900 * time.Millisecond)},
			obs.DialerBlockedProviderBackoff + "/TOKENS", 12 * time.Second},
		{"the REQUESTS refusal ended a second ago: the rate rules still apply",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-time.Second), Starts: 3,
				StartsDropAt: t0.Add(9 * time.Second)},
			"", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, q, 0, 0, 0)
			if got != tt.want {
				t.Errorf("providerHold = %q, want %q", got, tt.want)
			}
			if tt.until != 0 && !until.Equal(t0.Add(tt.until)) {
				t.Errorf("until = %v, want %v", until, t0.Add(tt.until))
			}
		})
	}
}

// A refusal holds the campaign, which says so (AT_CAPACITY), and the hold ends
// by time alone: with nothing in flight there is no event to wake the dialer,
// so the pass that dials again is the one it asked for at the refusal's minute.
func TestARefusalHoldsAdmissionForAMinuteAndTheDialerWakesForItsEnd(t *testing.T) {
	s := newSim(t, simSpec{capacity: 50, campaigns: 1, seed: 1, maxConcurrent: 5, isGated: true})
	t0 := s.clock.Now()
	s.meter.QuotaRefused("UNKNOWN") // an inbound call's session, refused at t0
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

// The same hold in the dialer's own simulation, on its fake clock (#652): at 3
// per 1 s and 60,000 per 12 s a REQUESTS refusal ends with the session window
// — a call the old minute-long rule would still hold dials a session period
// after it — and a TOKENS refusal keeps it held for the token window.
func TestARefusalEndsWithTheWindowItsLimitCarries(t *testing.T) {
	ctx := context.Background()
	newSimFor := func(kind string) (*sim, time.Time, context.CancelFunc, <-chan struct{}) {
		s := newSim(t, simSpec{capacity: 50, campaigns: 1, seed: 1, maxConcurrent: 5, isGated: true,
			sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second})
		t0 := s.clock.Now()
		s.meter.QuotaRefused(kind) // refused at t0
		s.clock.Set(t0.Add(500 * time.Millisecond))
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); s.d.Run(runCtx) }()
		s.settle()
		if s.placements != 0 {
			t.Fatalf("%d calls placed while a %s refusal held", s.placements, kind)
		}
		return s, t0, cancel, done
	}

	t.Run("a REQUESTS refusal ends with the session window", func(t *testing.T) {
		s, t0, cancel, done := newSimFor("REQUESTS")
		defer func() { cancel(); <-done }()
		s.clock.Set(t0.Add(999 * time.Millisecond))
		if err := s.d.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if s.placements != 0 {
			t.Fatalf("%d calls placed before the refused limit's window was over", s.placements)
		}
		s.clock.Set(t0.Add(time.Second))
		if err := s.d.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if s.placements != 5 {
			t.Errorf("%d calls placed once the session window was over, want the campaign's 5", s.placements)
		}
	})

	t.Run("a TOKENS refusal holds the full token window", func(t *testing.T) {
		s, t0, cancel, done := newSimFor("TOKENS")
		defer func() { cancel(); <-done }()
		s.clock.Set(t0.Add(12*time.Second - time.Millisecond))
		if err := s.d.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if s.placements != 0 {
			t.Fatalf("%d calls placed while a TOKENS refusal held", s.placements)
		}
		s.clock.Set(t0.Add(12 * time.Second))
		if err := s.d.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if s.placements != 5 {
			t.Errorf("%d calls placed once the token window was over, want the campaign's 5", s.placements)
		}
	})
}

// simProvider is a speech provider with a quota, as the vendor of #604
// documents its own: it refuses a session start once the starts of its own
// last connectPeriod, or the tokens of its last tokenPeriod, have reached the
// limit (a zero period is a minute). A refused attempt still counts as a
// connect. A session it accepts reports tokensPerTurn every turnEvery until
// the call ends.
type simProvider struct {
	connectLimit  int
	connectPeriod time.Duration
	tokenLimit    int64
	tokenPeriod   time.Duration
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
	connects, tokens := cmp.Or(p.connectPeriod, time.Minute), cmp.Or(p.tokenPeriod, time.Minute)
	for len(p.starts) > 0 && !now.Before(p.starts[0].Add(connects)) {
		p.starts = p.starts[1:]
	}
	for len(p.spent) > 0 && !now.Before(p.spent[0].at.Add(tokens)) {
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
	// The start gate's account (#644): the starts that waited, the longest
	// wait, and the starts made past the bound with the window full.
	waited, pastBound int
	longestWait       time.Duration
}

// providerScenario is what runScenario runs: the list of
// runAgainstProvider's (one campaign answering half its calls) against prov,
// with quota as the deployment's limits, applied by admission when isGated and
// by the start gate when isStartGated.
type providerScenario struct {
	seed                  int64
	prov                  simProvider
	capacity              int
	quota                 providerQuota
	isGated, isStartGated bool
	warmUp, d             time.Duration
}

func runAgainstProvider(t *testing.T, isGated bool, prov simProvider, capacity, sessionRPM, tokenRPM int, warmUp, d time.Duration) providerRun {
	t.Helper()
	return runScenario(t, providerScenario{seed: 21, prov: prov, capacity: capacity,
		quota: perMinute(sessionRPM, tokenRPM), isGated: isGated, warmUp: warmUp, d: d})
}

func runScenario(t *testing.T, sc providerScenario) providerRun {
	t.Helper()
	p := sc.prov
	s := newSim(t, simSpec{capacity: sc.capacity, inboundReserve: 4, campaigns: 1, seed: sc.seed, maxConcurrent: 1000,
		rates: []func(time.Duration) float64{constRate(0.5)}, provider: &p,
		isGated: sc.isGated, isStartGated: sc.isStartGated,
		sessionLimit: sc.quota.sessionLimit, sessionPeriod: sc.quota.sessionPeriod,
		tokenLimit: sc.quota.tokenLimit, tokenPeriod: sc.quota.tokenPeriod})
	p.measureFrom = s.start.Add(sc.warmUp)
	s.run(sc.d)
	run := providerRun{
		starts: p.accepted + p.refuse, refusals: p.refuse,
		startsAfter: p.startedAfter, refusalsAfter: p.refusedAfter,
	}
	for _, w := range s.startWaits {
		if w.Waited > 0 {
			run.waited++
		}
		if w.IsPastBound {
			run.pastBound++
		}
		run.longestWait = max(run.longestWait, w.Waited)
	}
	return run
}

// The deployment of #604 at the quota the qwen and doubao profiles document:
// a provider that takes 60 connects a minute, a list that answers half its
// calls, and more bot capacity than that rate can feed (160 slots, where 60
// sessions a minute of 60-130 s calls hold about 95). Admission by expected
// answers alone fills the bot, the starts run far above the connect rate and
// most answered callers meet a refusal (each one frees its slot a second
// later, which admits the next). With the provider rule the starts stay at the
// rate and refusals near zero, and the rate is still used.
//
// Not zero: the rule counts a ringing leg as p_c of an answer, p_c an upper
// bound on the campaign's answer rate, and a burst of fresh legs that answer
// above it can still put a start over the limit, and the rest of the burst
// behind it (a refused start counts as a connect too). The bursts are few, so
// the share they leave is chance unless the run holds many starts: scaled down
// to 20 connects a minute for 15 minutes, 9 seeds in 100 went over 2%; at the
// documented rate for half an hour, none of 300 did (#604).
func TestTheConnectRateGateKeepsRefusalsNearZero(t *testing.T) {
	const rate, capacity, minutes = 60, 160, 30
	prov := simProvider{connectLimit: rate}

	// Without the gate the minute's 60 connects are spent within the first
	// one, and every start after that is refused: a refused start counts as a
	// connect, so the window never empties. Two minutes show it.
	open := runAgainstProvider(t, false, prov, capacity, 0, 0, 0, 2*time.Minute)
	gated := runAgainstProvider(t, true, prov, capacity, rate, 0, 0, minutes*time.Minute)
	t.Logf("without the gate: %d starts, %d refused", open.starts, open.refusals)
	t.Logf("with the gate:    %d starts, %d refused", gated.starts, gated.refusals)

	if open.refusals < open.starts/5 {
		t.Fatalf("without the gate only %d of %d starts were refused: the scenario proves nothing",
			open.refusals, open.starts)
	}
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

	open := runAgainstProvider(t, false, prov, 60, 0, 0, warmUp, minutes*time.Minute)
	gated := runAgainstProvider(t, true, prov, 60, 0, 100000, warmUp, minutes*time.Minute)
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

// The quota of #644 over the vendor's own periods: 3 session starts per 1 s
// and 60,000 tokens per 12 s, with bot capacity enough that the session
// window is what binds (sessions spend 600 tokens a minute, so the tokens
// stay near half their limit). Admission judges the starts over ten seconds
// (ratemeter.AdmissionWindow), but their sessions open when callees answer,
// and four answers can land in one second: without the start gate a share of
// starts is refused, and each refusal holds admission for the backoff. With
// the gate a start waits for the window instead, and refusals stay within the
// bound of the per-minute scenario above.
//
// The rate admission reaches here, about 58 starts a minute, is a third of
// the quota: it counts every ringing leg's expected answer, in this list half
// of them, against the ten seconds, although a leg nobody answers rings for
// thirty. The assertion only keeps it from falling further.
func TestTheStartGateKeepsRefusalsNearZeroOverShortPeriods(t *testing.T) {
	const capacity, minutes = 400, 10
	q := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	prov := simProvider{connectLimit: 3, connectPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second,
		tokensPerTurn: 150, turnEvery: 15 * time.Second}

	open := runScenario(t, providerScenario{seed: 21, prov: prov, capacity: capacity, quota: q, isGated: true,
		d: minutes * time.Minute})
	gated := runScenario(t, providerScenario{seed: 21, prov: prov, capacity: capacity, quota: q, isGated: true,
		isStartGated: true, d: minutes * time.Minute})
	t.Logf("admission only:      %d starts, %d refused", open.starts, open.refusals)
	t.Logf("with the start gate: %d starts, %d refused; %d waited (longest %s), %d past the bound",
		gated.starts, gated.refusals, gated.waited, gated.longestWait, gated.pastBound)

	if open.refusals*50 <= open.starts {
		t.Fatalf("without the start gate only %d of %d starts were refused: the scenario proves nothing",
			open.refusals, open.starts)
	}
	if gated.refusals*50 > gated.starts {
		t.Errorf("with the start gate %d of %d starts were refused, want at most 2%%", gated.refusals, gated.starts)
	}
	if gated.starts < minutes*45 {
		t.Errorf("with the start gate only %d starts in %d minutes: the rate of 3 a second is left unused",
			gated.starts, minutes)
	}
	if gated.longestWait > ratemeter.StartWaitBound {
		t.Errorf("a start waited %s, past the bound of %s", gated.longestWait, ratemeter.StartWaitBound)
	}
}
