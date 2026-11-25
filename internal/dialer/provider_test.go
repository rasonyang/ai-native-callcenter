// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
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
		unstarted            float64
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
		// The answered legs whose start is not counted, each once (#640):
		// max(pending + startsWaiting, unstarted).
		{name: "answered legs no slot count sees yet are starts to come", r: ratemeter.Reading{At: t0, Starts: 50, StartsDropAt: at(time.Second)},
			sessionRPM: 60, unstarted: 3, expected: 6.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "a leg in the gate is one start to come, not two", r: ratemeter.Reading{At: t0, Starts: 50, StartsWaiting: 3},
			sessionRPM: 60, unstarted: 3, expected: 6},
		{name: "and one more is over", r: ratemeter.Reading{At: t0, Starts: 50, StartsWaiting: 3, StartsDropAt: at(time.Second)},
			sessionRPM: 60, unstarted: 3, expected: 6.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "reserved, waiting and in the gate, all the dialer's", r: ratemeter.Reading{At: t0, Starts: 50, StartsWaiting: 1},
			sessionRPM: 60, pending: 2, unstarted: 4, expected: 5},
		{name: "legs that are no flight's still count", r: ratemeter.Reading{At: t0, Starts: 50, StartsWaiting: 2, StartsDropAt: at(time.Second)},
			sessionRPM: 60, pending: 3, unstarted: 1, expected: 4.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Second)},
		{name: "on the token rule too", r: ratemeter.Reading{At: t0, Tokens: 80000, StartsWaiting: 1, TokensDropAt: at(2 * time.Second)},
			tokenRPM: 100000, unstarted: 1, expected: 8, active: 40},
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
			want: obs.DialerBlockedProviderBackoff, wantUntil: at(59 * time.Second)},
		{name: "the backoff comes before the rates", r: ratemeter.Reading{At: t0, Starts: 100, LastQuotaRefusalUnknown: at(-30 * time.Second)},
			sessionRPM: 60, want: obs.DialerBlockedProviderBackoff, wantUntil: at(30 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, perMinute(tt.sessionRPM, tt.tokenRPM), tt.pending, tt.unstarted, tt.expected, tt.active)
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
			{0, obs.DialerBlockedProviderBackoff},
			{time.Minute - time.Millisecond, obs.DialerBlockedProviderBackoff},
			{time.Minute, ""},
			{2 * time.Minute, ""},
		} {
			r := ratemeter.Reading{At: refused.Add(tt.after), LastQuotaRefusalUnknown: refused}
			if got, _ := providerHold(r, q, 0, 0, 0, 0); got != tt.want {
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
		unstarted         float64
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
		{name: "a gate waiting for the dialer's own legs counts them once", q: q,
			r: ratemeter.Reading{At: t0, Starts: 20, StartsWaiting: 2}, pending: 1, unstarted: 3, expected: 6},
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
			want: obs.DialerBlockedProviderBackoff, wantUntil: at(11 * time.Second)},
		{name: "with the token limit off, for the session period", q: providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenPeriod: 12 * time.Second},
			r:    ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-500 * time.Millisecond)},
			want: obs.DialerBlockedProviderBackoff, wantUntil: at(500 * time.Millisecond)},
		{name: "a period longer than a minute is its own window", q: providerQuota{sessionLimit: 1000, sessionPeriod: time.Hour},
			r: ratemeter.Reading{At: t0, Starts: 990, StartsDropAt: at(time.Minute)}, expected: 9.5,
			want: obs.DialerBlockedProviderSessions, wantUntil: at(time.Minute)},
		{name: "and its backoff", q: providerQuota{sessionLimit: 1000, sessionPeriod: time.Hour},
			r:    ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: at(-time.Minute)},
			want: obs.DialerBlockedProviderBackoff, wantUntil: at(59 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, tt.q, tt.pending, tt.unstarted, tt.expected, tt.active)
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
		{0, obs.DialerBlockedProviderBackoff},
		{time.Second, obs.DialerBlockedProviderBackoff},
		{12*time.Second - time.Millisecond, obs.DialerBlockedProviderBackoff},
		{12 * time.Second, ""},
	} {
		r := ratemeter.Reading{At: refused.Add(tt.after), LastQuotaRefusalUnknown: refused}
		if got, _ := providerHold(r, q, 0, 0, 0, 0); got != tt.want {
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
			obs.DialerBlockedProviderBackoffRequests, time.Second},
		{"a TOKENS refusal holds for the token period",
			ratemeter.Reading{At: t0, LastQuotaRefusalTokens: t0},
			obs.DialerBlockedProviderBackoffTokens, 12 * time.Second},
		{"an UNKNOWN refusal holds for the longer period",
			ratemeter.Reading{At: t0, LastQuotaRefusalUnknown: t0},
			obs.DialerBlockedProviderBackoff, 12 * time.Second},
		{"a REQUESTS refusal from 900ms ago has ended at 1s, a TOKENS one has not",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-900 * time.Millisecond)},
			obs.DialerBlockedProviderBackoffRequests, 100 * time.Millisecond},
		{"both outstanding: the hold ends when the later one ends",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-time.Second), LastQuotaRefusalTokens: t0.Add(-time.Second)},
			obs.DialerBlockedProviderBackoffTokens, 11 * time.Second},
		{"a REQUESTS refusal during a TOKENS hold does not shorten it",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0, LastQuotaRefusalTokens: t0.Add(-6 * time.Second)},
			obs.DialerBlockedProviderBackoffTokens, 6 * time.Second},
		{"a TOKENS refusal during a REQUESTS hold extends it",
			ratemeter.Reading{At: t0, LastQuotaRefusalTokens: t0, LastQuotaRefusalRequests: t0.Add(-900 * time.Millisecond)},
			obs.DialerBlockedProviderBackoffTokens, 12 * time.Second},
		{"the REQUESTS refusal ended a second ago: the rate rules still apply",
			ratemeter.Reading{At: t0, LastQuotaRefusalRequests: t0.Add(-time.Second), Starts: 3,
				StartsDropAt: t0.Add(9 * time.Second)},
			"", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, until := providerHold(tt.r, q, 0, 0, 0, 0)
			if got != tt.want {
				t.Errorf("providerHold = %q, want %q", got, tt.want)
			}
			if tt.until != 0 && !until.Equal(t0.Add(tt.until)) {
				t.Errorf("until = %v, want %v", until, t0.Add(tt.until))
			}
		})
	}
}

// The refusal hold as the dialer reads it, from the rate meter itself on a
// fake clock (#652). Each case is refusals at offsets from t0, then where the
// hold must end and with which reason: held a millisecond before, admitted at
// the end, and the end is the until the dialer wakes for.
func TestTheMeterAndTheHoldRuleHoldForTheRefusingLimit(t *testing.T) {
	short := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	type refusal struct {
		at    time.Duration
		limit ratemeter.Limit
	}
	for _, tt := range []struct {
		name     string
		q        providerQuota
		refusals []refusal
		end      time.Duration
		reason   string
	}{
		{"REQUESTS holds for the session period", short,
			[]refusal{{0, ratemeter.LimitRequests}}, time.Second, obs.DialerBlockedProviderBackoffRequests},
		{"TOKENS holds for the token period", short,
			[]refusal{{0, ratemeter.LimitTokens}}, 12 * time.Second, obs.DialerBlockedProviderBackoffTokens},
		{"UNKNOWN holds for the longer period", short,
			[]refusal{{0, ratemeter.LimitUnknown}}, 12 * time.Second, obs.DialerBlockedProviderBackoff},
		{"REQUESTS during a TOKENS hold does not shorten it", short,
			[]refusal{{0, ratemeter.LimitTokens}, {6 * time.Second, ratemeter.LimitRequests}},
			12 * time.Second, obs.DialerBlockedProviderBackoffTokens},
		{"TOKENS during a REQUESTS hold extends it", short,
			[]refusal{{0, ratemeter.LimitRequests}, {500 * time.Millisecond, ratemeter.LimitTokens}},
			12500 * time.Millisecond, obs.DialerBlockedProviderBackoffTokens},
		{"REQUESTS and TOKENS at one instant hold for the token period", short,
			[]refusal{{0, ratemeter.LimitRequests}, {0, ratemeter.LimitTokens}},
			12 * time.Second, obs.DialerBlockedProviderBackoffTokens},
		{"a REQUESTS refusal after the TOKENS one ended holds for its own period", short,
			[]refusal{{0, ratemeter.LimitTokens}, {12 * time.Second, ratemeter.LimitRequests}},
			13 * time.Second, obs.DialerBlockedProviderBackoffRequests},
		{"REQUESTS with the session limit off holds for the longer period in force",
			providerQuota{sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second},
			[]refusal{{0, ratemeter.LimitRequests}}, 12 * time.Second, obs.DialerBlockedProviderBackoffRequests},
		{"TOKENS with the token limit off holds for the longer period in force",
			providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenPeriod: 12 * time.Second},
			[]refusal{{0, ratemeter.LimitTokens}}, time.Second, obs.DialerBlockedProviderBackoffTokens},
		// Per-minute limits and no limits: a minute for every kind, as in v0.9.1.
		{"per minute, REQUESTS", perMinute(60, 100000), []refusal{{0, ratemeter.LimitRequests}},
			time.Minute, obs.DialerBlockedProviderBackoffRequests},
		{"per minute, TOKENS", perMinute(60, 100000), []refusal{{0, ratemeter.LimitTokens}},
			time.Minute, obs.DialerBlockedProviderBackoffTokens},
		{"per minute, UNKNOWN", perMinute(60, 100000), []refusal{{0, ratemeter.LimitUnknown}},
			time.Minute, obs.DialerBlockedProviderBackoff},
		{"no limits, REQUESTS", providerQuota{}, []refusal{{0, ratemeter.LimitRequests}},
			time.Minute, obs.DialerBlockedProviderBackoffRequests},
		{"no limits, TOKENS", providerQuota{}, []refusal{{0, ratemeter.LimitTokens}},
			time.Minute, obs.DialerBlockedProviderBackoffTokens},
		{"no limits, UNKNOWN", providerQuota{}, []refusal{{0, ratemeter.LimitUnknown}},
			time.Minute, obs.DialerBlockedProviderBackoff},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
			t0 := clock.Now()
			m := ratemeter.New(clock.Now, ratemeter.Config{SessionPeriod: tt.q.sessionPeriod, TokenPeriod: tt.q.tokenPeriod})
			for _, r := range tt.refusals {
				clock.Set(t0.Add(r.at))
				m.QuotaRefused(r.limit)
			}
			end := t0.Add(tt.end)
			clock.Set(end.Add(-time.Millisecond))
			if got, until := providerHold(m.Read(), tt.q, 0, 0, 0, 0); got != tt.reason || !until.Equal(end) {
				t.Errorf("1 ms before the end: %q until %v, want %q until %v", got, until, tt.reason, end)
			}
			clock.Set(end)
			if got, until := providerHold(m.Read(), tt.q, 0, 0, 0, 0); got != "" || !until.IsZero() {
				t.Errorf("at the end: %q until %v, want admitted", got, until)
			}
		})
	}
}

// The wake the dialer asks for is the hold's end (rateWake), for each limit:
// nothing else is in flight, so that wake is what dials again.
func TestTheDialerWakesAtTheEndOfTheRefusingLimitsHold(t *testing.T) {
	for _, tt := range []struct {
		limit ratemeter.Limit
		end   time.Duration
	}{
		{ratemeter.LimitRequests, time.Second},
		{ratemeter.LimitTokens, 12 * time.Second},
		{ratemeter.LimitUnknown, 12 * time.Second},
	} {
		t.Run(string(tt.limit), func(t *testing.T) {
			s := newSim(t, simSpec{capacity: 50, campaigns: 1, seed: 1, maxConcurrent: 5, isGated: true,
				sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second})
			t0 := s.clock.Now()
			s.meter.QuotaRefused(tt.limit)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); s.d.Run(ctx) }()
			defer func() { cancel(); <-done }()
			s.settle()

			// Read on the actor, before any pass of its own: the last pass's.
			var rateWake, nextWake time.Time
			if err := s.d.run(ctx, func(context.Context) { rateWake, nextWake = s.d.rateWake, s.d.nextWake }); err != nil {
				t.Fatal(err)
			}
			end := t0.Add(tt.end)
			if !rateWake.Equal(end) || nextWake.After(end) {
				t.Errorf("rateWake %v, nextWake %v; want the hold's end %v, and no wake after it", rateWake, nextWake, end)
			}
			if s.placements != 0 {
				t.Fatalf("%d calls placed during the hold", s.placements)
			}
			s.clock.Set(end)
			if err := s.d.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if s.placements != 5 {
				t.Errorf("%d calls placed at the hold's end, want the campaign's 5", s.placements)
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
	s.meter.QuotaRefused(ratemeter.LimitUnknown) // an inbound call's session, refused at t0
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
	newSimFor := func(kind ratemeter.Limit) (*sim, time.Time, context.CancelFunc, <-chan struct{}) {
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
		s, t0, cancel, done := newSimFor(ratemeter.LimitRequests)
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
		s, t0, cancel, done := newSimFor(ratemeter.LimitTokens)
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

// holdLog keeps the dialer's provider-hold log lines (noteProviderHold) with
// the time on the fake clock each was written at.
type holdLog struct {
	clock *fakeClock
	mu    sync.Mutex
	lines []holdLine
}

type holdLine struct {
	at    time.Time
	msg   string
	attrs map[string]string
}

func (h *holdLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *holdLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *holdLog) WithGroup(string) slog.Handler            { return h }
func (h *holdLog) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "provider's quota") {
		return nil
	}
	line := holdLine{at: h.clock.Now(), msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.Resolve().String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, line)
	return nil
}

func (h *holdLog) all() []holdLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]holdLine(nil), h.lines...)
}

// The dialer's hold log says which limit refused, the period it holds for and
// when the hold ends (#652): at 3 per 1 s and 60,000 per 12 s, a REQUESTS
// refusal holds for 1 s, a TOKENS refusal half a second later takes the hold
// over for 12 s, and the line that lets calls through again comes at its end.
func TestTheHoldLogNamesTheRefusingLimitAndItsPeriod(t *testing.T) {
	logs := &holdLog{}
	s := newSim(t, simSpec{capacity: 50, campaigns: 1, seed: 1, maxConcurrent: 5, isGated: true,
		sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second,
		log: slog.New(logs)})
	logs.clock = s.clock
	t0 := s.clock.Now()
	s.meter.QuotaRefused(ratemeter.LimitRequests) // before the dialer's first pass

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	s.settle()
	s.clock.Set(t0.Add(500 * time.Millisecond))
	s.meter.QuotaRefused(ratemeter.LimitTokens)
	s.d.Wake()
	s.settle()
	s.clock.Set(t0.Add(12500 * time.Millisecond))
	s.settle()

	lines := logs.all()
	if len(lines) != 3 {
		t.Fatalf("%d hold lines, want 3: %+v", len(lines), lines)
	}
	for i, want := range []struct {
		at                     time.Duration
		reason, limit, backoff string
		until                  time.Duration
	}{
		{0, obs.DialerBlockedProviderBackoffRequests, "REQUESTS", "1s", time.Second},
		{500 * time.Millisecond, obs.DialerBlockedProviderBackoffTokens, "TOKENS", "12s", 12500 * time.Millisecond},
		{12500 * time.Millisecond, "", "", "", 0},
	} {
		got := lines[i]
		if !got.at.Equal(t0.Add(want.at)) || got.attrs["reason"] != want.reason ||
			got.attrs["limit"] != want.limit || got.attrs["backoff"] != want.backoff {
			t.Errorf("line %d = %v %q %v, want at +%v reason %q limit %q backoff %q",
				i, got.at.Sub(t0), got.msg, got.attrs, want.at, want.reason, want.limit, want.backoff)
		}
		if want.until != 0 && got.attrs["until"] != t0.Add(want.until).String() {
			t.Errorf("line %d until = %s, want %v", i, got.attrs["until"], t0.Add(want.until))
		}
	}
	if s.placements != 5 {
		t.Errorf("%d calls placed once the TOKENS hold ended, want the campaign's 5", s.placements)
	}
}

// Refusals of every limit injected at random into the capacity simulation
// (#652), at 3 per 1 s and 60,000 per 12 s, into a campaign whose only other
// limit is its concurrency (the bot, the rates and the tokens have room), over
// many seeds. Read from the dialer's own trace:
//
//   - no call is placed inside any refusal's period: 1 s after REQUESTS, 12 s
//     after TOKENS and UNKNOWN, the longer one when several are outstanding;
//   - each time the provider rule reads the meter, the meter's latest refusal
//     of each limit is the run's own, the rule holds with the reason of the
//     limit whose period ends last while it lasts, and from its end on it
//     holds for none: admission resumes after the refusing limit's period;
//   - and over the seeds every kind of hold ended and was let through again,
//     refusals of one limit came during holds of the other, both ways, and
//     calls went out after a REQUESTS hold's end that the rule before #652
//     (12 s for every refusal) would still have held.
//
// The periods expected are written out here, not taken from the rule.
func TestAdmissionResumesAfterTheRefusingLimitsPeriodOverManySeeds(t *testing.T) {
	const seeds, minutes = 40, 10
	q := providerQuota{sessionLimit: 3, sessionPeriod: time.Second, tokenLimit: 60000, tokenPeriod: 12 * time.Second}
	period := map[ratemeter.Limit]time.Duration{
		ratemeter.LimitRequests: time.Second, ratemeter.LimitTokens: 12 * time.Second, ratemeter.LimitUnknown: 12 * time.Second,
	}
	reasonOf := map[ratemeter.Limit]string{
		ratemeter.LimitRequests: obs.DialerBlockedProviderBackoffRequests,
		ratemeter.LimitTokens:   obs.DialerBlockedProviderBackoffTokens,
		ratemeter.LimitUnknown:  obs.DialerBlockedProviderBackoff,
	}
	isBackoff := func(reason string) bool {
		return reason == obs.DialerBlockedProviderBackoff || reason == obs.DialerBlockedProviderBackoffRequests ||
			reason == obs.DialerBlockedProviderBackoffTokens
	}
	limits := []ratemeter.Limit{ratemeter.LimitRequests, ratemeter.LimitTokens, ratemeter.LimitUnknown}

	refusals := map[ratemeter.Limit]int{}
	held := map[string]int{}     // reads the rule held for a refusal, by reason
	released := map[string]int{} // holds that ended and were read again, by the reason they had
	overlaps := map[string]int{}
	var placements, earlyPlacements, reads int
	for seed := int64(1); seed <= seeds; seed++ {
		tr := &simTrace{}
		p := simProvider{refuseShare: 0.3, refuseLimits: limits, refuseRng: rand.New(rand.NewSource(seed))}
		s := newSim(t, simSpec{capacity: 400, inboundReserve: 4, campaigns: 1, seed: seed, maxConcurrent: 8,
			rates: []func(time.Duration) float64{constRate(0.5)}, provider: &p, isGated: true,
			sessionLimit: q.sessionLimit, sessionPeriod: q.sessionPeriod, tokenLimit: q.tokenLimit, tokenPeriod: q.tokenPeriod,
			trace: tr})
		s.run(minutes * time.Minute)
		s.checkTerm()

		latest := map[ratemeter.Limit]time.Time{}
		// hold is what the refusals so far give at now: the limit whose
		// period ends last, and that end; zero when none holds.
		hold := func(now time.Time) (ratemeter.Limit, time.Time) {
			var limit ratemeter.Limit
			var until time.Time
			for _, l := range limits {
				if at, ok := latest[l]; ok {
					if end := at.Add(period[l]); now.Before(end) && end.After(until) {
						limit, until = l, end
					}
				}
			}
			return limit, until
		}
		// A read's verdict is the hold line that follows it, before the next
		// read, or the last one logged when none does (the dialer logs a
		// change only).
		reported, lastJudged := "", ""
		type pendingRead struct {
			at    time.Time
			limit ratemeter.Limit
			until time.Time
		}
		var pending *pendingRead
		judge := func() {
			if pending == nil {
				return
			}
			reads++
			if !pending.until.IsZero() {
				if want := reasonOf[pending.limit]; reported != want {
					t.Errorf("seed %d at +%v: the rule read %q, want %q until +%v",
						seed, pending.at.Sub(s.start), reported, want, pending.until.Sub(s.start))
				}
				held[reported]++
			} else {
				if isBackoff(reported) {
					t.Errorf("seed %d at +%v: the rule still held %q after every refusal's period",
						seed, pending.at.Sub(s.start), reported)
				}
				if isBackoff(lastJudged) {
					released[lastJudged]++
				}
			}
			lastJudged = reported
			pending = nil
		}
		for _, e := range tr.all() {
			switch e.kind {
			case traceRefusal:
				refusals[e.limit]++
				if l, until := hold(e.at); !until.IsZero() && l != e.limit && l != ratemeter.LimitUnknown && e.limit != ratemeter.LimitUnknown {
					overlaps[string(e.limit)+" during "+string(l)]++
				}
				if e.at.After(latest[e.limit]) {
					latest[e.limit] = e.at
				}
			case tracePlacement:
				judge()
				placements++
				if l, until := hold(e.at); !until.IsZero() {
					t.Errorf("seed %d: a call placed at +%v inside a %s refusal's period (until +%v)",
						seed, e.at.Sub(s.start), l, until.Sub(s.start))
				}
				if at, ok := latest[ratemeter.LimitRequests]; ok && e.at.Sub(at) < 12*time.Second {
					earlyPlacements++
				}
			case traceRead:
				judge()
				r := e.reading
				for l, got := range map[ratemeter.Limit]time.Time{ratemeter.LimitRequests: r.LastQuotaRefusalRequests,
					ratemeter.LimitTokens: r.LastQuotaRefusalTokens, ratemeter.LimitUnknown: r.LastQuotaRefusalUnknown} {
					if !got.Equal(latest[l]) {
						t.Fatalf("seed %d at +%v: the meter's latest %s refusal is %v, want %v",
							seed, e.at.Sub(s.start), l, got, latest[l])
					}
				}
				limit, until := hold(e.at)
				pending = &pendingRead{at: e.at, limit: limit, until: until}
			case traceHold:
				reported = e.reason
				judge()
			}
		}
		judge()
	}
	t.Logf("%d seeds x %d min: refusals %v; %d placements, %d reads; held %v; released %v; overlaps %v; "+
		"%d placements within 12 s of a REQUESTS refusal",
		seeds, minutes, refusals, placements, reads, held, released, overlaps, earlyPlacements)
	for _, l := range limits {
		if r := reasonOf[l]; refusals[l] == 0 || held[r] == 0 || released[r] == 0 {
			t.Errorf("%s: %d refusals, %d reads held, %d holds released: the run does not show its hold",
				l, refusals[l], held[r], released[r])
		}
	}
	for _, o := range []string{"REQUESTS during TOKENS", "TOKENS during REQUESTS"} {
		if overlaps[o] == 0 {
			t.Errorf("no refusal of %s: the run does not show one limit's refusal in the other's hold", o)
		}
	}
	if earlyPlacements == 0 {
		t.Error("no call placed within 12 s of a REQUESTS refusal: the run does not show its shorter hold")
	}
}

// simProvider is a speech provider with a quota, as the vendor of #604
// documents its own: it refuses a session start once the starts of its own
// last connectPeriod, or the tokens of its last tokenPeriod, have reached the
// limit (a zero period is a minute), naming no limit (UNKNOWN). A refused
// attempt still counts as a connect. A session it accepts reports
// tokensPerTurn every turnEvery until the call ends.
type simProvider struct {
	connectLimit  int
	connectPeriod time.Duration
	tokenLimit    int64
	tokenPeriod   time.Duration
	tokensPerTurn int
	turnEvery     time.Duration
	// refuseShare is the share of the starts its quota admits that it
	// refuses all the same, each naming a limit drawn evenly from
	// refuseLimits by refuseRng: refusals of chosen limits, injected (#652).
	refuseShare  float64
	refuseLimits []ratemeter.Limit
	refuseRng    *rand.Rand

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

// start is one session start: it returns the limit a refusal named, empty
// when the session is accepted.
func (p *simProvider) start(now time.Time) ratemeter.Limit {
	p.prune(now)
	var tokens int64
	for _, s := range p.spent {
		tokens += s.n
	}
	var limit ratemeter.Limit
	switch {
	case (p.connectLimit > 0 && len(p.starts) >= p.connectLimit) || (p.tokenLimit > 0 && tokens >= p.tokenLimit):
		limit = ratemeter.LimitUnknown
	case p.refuseShare > 0 && p.refuseRng.Float64() < p.refuseShare:
		limit = p.refuseLimits[p.refuseRng.Intn(len(p.refuseLimits))]
	}
	p.starts = append(p.starts, now)
	isMeasured := !now.Before(p.measureFrom)
	if isMeasured {
		p.startedAfter++
	}
	if limit != "" {
		p.refuse++
		if isMeasured {
			p.refusedAfter++
		}
		return limit
	}
	p.accepted++
	return ""
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
	// startWindow is how long an answered leg stands between the answer and
	// its counted session start (#640); hangupInWindow is the share of
	// answered calls whose callee hangs up inside it; isWindowJittered draws each
	// call's span in [startWindow/2, 3·startWindow/2) instead of fixing it.
	startWindow      time.Duration
	hangupInWindow   float64
	isWindowJittered bool
	warmUp, d        time.Duration
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
		tokenLimit: sc.quota.tokenLimit, tokenPeriod: sc.quota.tokenPeriod,
		startWindow: sc.startWindow, hangupInWindow: sc.hangupInWindow, isWindowJittered: sc.isWindowJittered})
	p.measureFrom = s.start.Add(sc.warmUp)
	s.run(sc.d)
	s.checkTerm()
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

// The window of #640 between an answer and its counted session start — the
// reservation, the INVITE, +OK, the provider session: production's dialer
// hears of the answer first, and for that span the leg was in none of the
// counts either rule read. With the unstarted term covering it, the
// connect-rate gate keeps refusals within the windowless bound at every
// window a deployment measures, callees hanging up inside it included; the
// rate is still used. Over these runs it passes without the term as well, so
// it guards the window from making things worse; the test below is the one
// the term is needed for.
func TestTheConnectRateGateHoldsAcrossTheSessionStartWindow(t *testing.T) {
	const rate, capacity, minutes = 60, 160, 8
	for _, window := range []time.Duration{0, 50 * time.Millisecond, 200 * time.Millisecond, time.Second} {
		hang := 0.0
		if window > 0 {
			hang = 0.15
		}
		for _, jit := range []bool{false, true} {
			if window == 0 && jit {
				continue
			}
			for _, seed := range []int64{21, 42, 77} {
				run := runScenario(t, providerScenario{seed: seed, prov: simProvider{connectLimit: rate},
					capacity: capacity, quota: perMinute(rate, 0), isGated: true,
					startWindow: window, hangupInWindow: hang, isWindowJittered: jit, d: minutes * time.Minute})
				t.Logf("window %4s jitter=%v seed %d: %d starts, %d refused", window, jit, seed, run.starts, run.refusals)
				if run.refusals*50 > run.starts {
					t.Errorf("window %s jitter=%v seed %d: %d of %d starts were refused, want at most 2%%",
						window, jit, seed, run.refusals, run.starts)
				}
				if run.starts < minutes*rate/2 {
					t.Errorf("window %s jitter=%v seed %d: only %d starts in %d minutes: the connect rate of %d a minute is left unused",
						window, jit, seed, run.starts, minutes, rate)
				}
			}
		}
	}
}

// The window of #640 where the rules are exact: every callee answers, so the
// answer-rate bound stays 1 and each ringing leg is one session to come. A leg
// in the window that no count saw would let admission place one call more
// than the bot or the provider can take, at every answer. With the term, at
// every window, fixed or jittered, with callees hanging up inside it, the bot
// never reaches into the inbound reserve, nothing overflows and no start is
// refused, as with no window, and both are still used. Without the term the
// same runs took the bot into its reserve and had starts refused at every
// window.
func TestWithEveryCalleeAnsweringTheWindowAdmitsNoCallTheRulesCannotHold(t *testing.T) {
	const capacity, reserve, rate, minutes = 60, 6, 20, 10
	for _, window := range []time.Duration{0, 50 * time.Millisecond, 200 * time.Millisecond, time.Second} {
		hang := 0.0
		if window > 0 {
			hang = 0.15
		}
		for _, jit := range []bool{false, true} {
			if window == 0 && jit {
				continue
			}
			name := fmt.Sprintf("window %s jitter=%v", window, jit)
			bot := newSim(t, simSpec{capacity: capacity, inboundReserve: reserve, campaigns: 1, seed: 1, maxConcurrent: 1000,
				rates:       []func(time.Duration) float64{constRate(1)},
				startWindow: window, hangupInWindow: hang, isWindowJittered: jit})
			bot.run(2 * minutes * time.Minute)
			bot.checkTerm()
			overflows := bot.store.count(bot.store.camps[0].ID, campaign.OutcomeOverflow)
			t.Logf("%s, bot rule: %d calls, peak %d sessions, %d overflowed", name, bot.placements, bot.peakInUse, overflows)
			if overflows != 0 || bot.peakInUse > capacity-reserve {
				t.Errorf("%s: %d calls overflowed and the bot peaked at %d sessions, want none and at most %d",
					name, overflows, bot.peakInUse, capacity-reserve)
			}
			if bot.placements < 500 {
				t.Errorf("%s: only %d calls placed in %d minutes: the bot is left unused", name, bot.placements, 2*minutes)
			}

			p := simProvider{connectLimit: rate}
			prov := newSim(t, simSpec{capacity: 400, inboundReserve: 4, campaigns: 1, seed: 1, maxConcurrent: 1000,
				rates: []func(time.Duration) float64{constRate(1)}, provider: &p,
				isGated: true, sessionLimit: rate, sessionPeriod: time.Minute,
				startWindow: window, hangupInWindow: hang, isWindowJittered: jit})
			prov.run(minutes * time.Minute)
			prov.checkTerm()
			starts := p.accepted + p.refuse
			t.Logf("%s, provider rule: %d starts, %d refused", name, starts, p.refuse)
			if p.refuse != 0 {
				t.Errorf("%s: %d of %d starts were refused, want none", name, p.refuse, starts)
			}
			if starts*5 < minutes*rate*4 {
				t.Errorf("%s: only %d starts in %d minutes: the connect rate of %d a minute is left unused",
					name, starts, minutes, rate)
			}
		}
	}
}
