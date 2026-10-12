// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// settleWindow is the window of design 10 section 7.3: 09:00-18:00, Monday to
// Friday, in New York.
func settleWindow(t *testing.T) Schedule {
	t.Helper()
	return sched(t, "America/New_York", []string{"MON", "TUE", "WED", "THU", "FRI"}, "08:00-21:00", "09:00", "18:00")
}

// Section 7.3, reproduced exactly: window 09:00-18:00, maxAttempts 3.
func TestTheWorkedExampleOfSection7_3(t *testing.T) {
	policy := DefaultRetryPolicy()
	s := settleWindow(t)
	mon := func(hhmm string) time.Time { return at(t, "America/New_York", "2026-06-01 "+hhmm) }

	steps := []struct {
		what         string
		outcome      Outcome
		finished     time.Time
		counters     Counters // after this attempt
		wantStatus   ContactStatus
		wantNext     time.Time
		wantNextNone bool
	}{
		{"10:00 rang 30 s, nobody answered", OutcomeNoAnswer, mon("10:00"), Counters{1, 0}, ContactActive, mon("12:00"), false},
		{"12:00 gateway unreachable", OutcomeOriginateFailed, mon("12:00"), Counters{1, 1}, ContactActive, mon("12:05"), false},
		{"12:05 busy", OutcomeBusy, mon("12:05"), Counters{2, 1}, ContactActive, mon("12:35"), false},
		{"12:35 declined", OutcomeRejected, mon("12:35"), Counters{3, 1}, ContactExhausted, time.Time{}, true},
	}
	var counters Counters
	for _, st := range steps {
		counters = counters.Add(st.outcome)
		if counters != st.counters {
			t.Fatalf("%s: counters = %+v, want %+v (the system failure must not use a chance)", st.what, counters, st.counters)
		}
		got := Settle(policy, counters, st.outcome, "", st.finished, s)
		if got.Status != st.wantStatus {
			t.Errorf("%s: status = %s, want %s", st.what, got.Status, st.wantStatus)
		}
		switch {
		case st.wantNextNone && got.NextAttemptAt != nil:
			t.Errorf("%s: next = %v, want none", st.what, got.NextAttemptAt)
		case !st.wantNextNone && (got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(st.wantNext)):
			t.Errorf("%s: next = %v, want %v", st.what, got.NextAttemptAt, st.wantNext)
		}
	}

	// "Had the 12:05 attempt instead been a NO_ANSWER at 17:50, the next time
	// would compute to 19:50, outside the window, and move to Tuesday 09:00."
	got := Settle(policy, Counters{2, 1}, OutcomeNoAnswer, "", mon("17:50"), s)
	want := at(t, "America/New_York", "2026-06-02 09:00")
	if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(want) {
		t.Errorf("17:50 no answer: next = %v, want Tuesday 09:00", got.NextAttemptAt)
	}
}

func TestNoRedialAfterAnAnswer(t *testing.T) {
	policy := DefaultRetryPolicy()
	policy.RetryOutcomes = append(policy.RetryOutcomes, OutcomeAnswered) // not even asked for
	s := settleWindow(t)
	now := at(t, "America/New_York", "2026-06-01 10:00")
	for _, o := range []Outcome{OutcomeAnswered, OutcomeBotFailed, OutcomeOverflow} {
		for _, lead := range []LeadIntent{"", LeadNone, LeadLow, LeadMedium, LeadHigh} {
			// Even with every budget untouched: nothing was used up, and it
			// is still done.
			got := Settle(policy, Counters{}.Add(o), o, lead, now, s)
			if got.Status != ContactDone || got.NextAttemptAt != nil {
				t.Errorf("%s with lead %q: %+v, want DONE and no next attempt", o, lead, got)
			}
		}
	}
	// A DO_NOT_CALL on the call outranks DONE.
	if got := Settle(policy, Counters{1, 0}, OutcomeAnswered, LeadDoNotCall, now, s); got.Status != ContactDoNotCall || got.NextAttemptAt != nil {
		t.Errorf("answered + DO_NOT_CALL = %+v, want DNC", got)
	}
}

func TestTheTerminalOutcomes(t *testing.T) {
	policy := DefaultRetryPolicy()
	s := settleWindow(t)
	now := at(t, "America/New_York", "2026-06-01 10:00")
	if got := Settle(policy, Counters{}, OutcomeInvalidNumber, "", now, s); got.Status != ContactInvalidNumber || got.NextAttemptAt != nil {
		t.Errorf("INVALID_NUMBER = %+v", got)
	}
	if got := Settle(policy, Counters{}, OutcomeDNCSkipped, "", now, s); got.Status != ContactDoNotCall || got.NextAttemptAt != nil {
		t.Errorf("DNC_SKIPPED = %+v", got)
	}
}

func TestTheTwoBudgetsAreSeparate(t *testing.T) {
	policy := DefaultRetryPolicy()
	policy.MaxAttempts = 3
	policy.MaxSystemRetries = 5
	s := settleWindow(t)
	now := at(t, "America/New_York", "2026-06-01 10:00")

	// Four system failures use none of the customer's chances.
	var c Counters
	for i := 1; i <= 4; i++ {
		c = c.Add(OutcomeTransient)
		got := Settle(policy, c, OutcomeTransient, "", now, s)
		if got.Status != ContactActive || got.NextAttemptAt == nil {
			t.Fatalf("system failure %d: %+v, want ACTIVE", i, got)
		}
	}
	if c.Attempts != 0 {
		t.Errorf("attempts = %d after system failures only", c.Attempts)
	}
	// The fifth reaches maxSystemRetries.
	c = c.Add(OutcomeOriginateFailed)
	if got := Settle(policy, c, OutcomeOriginateFailed, "", now, s); got.Status != ContactExhausted || got.NextAttemptAt != nil {
		t.Errorf("fifth system failure = %+v, want EXHAUSTED", got)
	}

	// Two reached attempts, then a system failure: still ACTIVE, and the
	// customer still has a chance left.
	c = Counters{Attempts: 2}.Add(OutcomeTransient)
	if got := Settle(policy, c, OutcomeTransient, "", now, s); got.Status != ContactActive {
		t.Errorf("2 reached + 1 system = %+v, want ACTIVE", got)
	}
	// The third reached attempt is the last, whatever the system count.
	c = Counters{Attempts: 3, SystemFailures: 4}
	if got := Settle(policy, c, OutcomeUnknown, "", now, s); got.Status != ContactExhausted {
		t.Errorf("third reached attempt = %+v, want EXHAUSTED", got)
	}
	// maxSystemRetries 0: the first failure is the last.
	policy.MaxSystemRetries = 0
	if got := Settle(policy, Counters{SystemFailures: 1}, OutcomeTransient, "", now, s); got.Status != ContactExhausted {
		t.Errorf("maxSystemRetries 0 = %+v, want EXHAUSTED", got)
	}
}

func TestTheTransientBackoff(t *testing.T) {
	b := Backoff{BaseSec: 300, Factor: 3, CapSec: 4 * 3600}
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute, 135 * time.Minute, 4 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i, w := range want {
		if got := b.Wait(i + 1); got != w {
			t.Errorf("retry %d waits %s, want %s", i+1, got, w)
		}
	}
	// A huge exponent must neither overflow nor go negative.
	if got := b.Wait(10_000); got != 4*time.Hour {
		t.Errorf("retry 10000 waits %s", got)
	}
	flat := Backoff{BaseSec: 60, Factor: 1, CapSec: 3600}
	if got := flat.Wait(9); got != time.Minute {
		t.Errorf("factor 1 waits %s", got)
	}
}

func TestAnOutcomeThePolicyDoesNotRetryEndsTheContact(t *testing.T) {
	policy := DefaultRetryPolicy()
	policy.RetryOutcomes = []Outcome{OutcomeNoAnswer}
	s := settleWindow(t)
	now := at(t, "America/New_York", "2026-06-01 10:00")
	if got := Settle(policy, Counters{1, 0}, OutcomeBusy, "", now, s); got.Status != ContactExhausted || got.NextAttemptAt != nil {
		t.Errorf("an unretried BUSY = %+v, want EXHAUSTED", got)
	}
	if got := Settle(policy, Counters{1, 0}, OutcomeNoAnswer, "", now, s); got.Status != ContactActive {
		t.Errorf("a retried NO_ANSWER = %+v, want ACTIVE", got)
	}
}

// The retry falls where the window says, for contacts in different zones, on
// days the clocks change, and across midnight.
func TestARetryOutsideTheWindowMovesToTheNextWindowStart(t *testing.T) {
	policy := DefaultRetryPolicy()
	days := allDays
	tests := []struct {
		name       string
		zone       string
		window     [2]string
		outer      string
		finishedAt string
		want       string // local, in zone
		outcome    Outcome
	}{
		{"evening spills to the morning", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 17:50", "2026-06-02 09:00", OutcomeNoAnswer},
		{"inside stays put", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 10:00", "2026-06-01 12:00", OutcomeNoAnswer},
		{"exactly at the close", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 16:00", "2026-06-02 09:00", OutcomeNoAnswer},
		{"exactly at the open", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 07:00", "2026-06-01 09:00", OutcomeNoAnswer},
		{"before the window opens", "Asia/Shanghai", [2]string{"09:00", "18:00"}, "", "2026-06-01 04:00", "2026-06-01 09:00", OutcomeBusy},
		{"across midnight: night shift", "Europe/London", [2]string{"20:00", "04:00"}, "20:00-04:00", "2026-06-01 22:00", "2026-06-02 00:00", OutcomeNoAnswer},
		{"across midnight: due after the close", "Europe/London", [2]string{"20:00", "04:00"}, "20:00-04:00", "2026-06-02 02:30", "2026-06-02 20:00", OutcomeNoAnswer},
		{"spring forward Saturday evening", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-03-07 17:00", "2026-03-08 09:00", OutcomeNoAnswer},
		{"fall back Saturday evening", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-10-31 17:30", "2026-11-01 09:00", OutcomeNoAnswer},
		{"london spring forward", "Europe/London", [2]string{"09:00", "18:00"}, "", "2026-03-28 17:30", "2026-03-29 09:00", OutcomeNoAnswer},
		{"london fall back", "Europe/London", [2]string{"09:00", "18:00"}, "", "2026-10-24 17:30", "2026-10-25 09:00", OutcomeNoAnswer},
		{"a system failure is moved too", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 17:58", "2026-06-02 09:00", OutcomeTransient},
		{"a 24 hour wait lands in the window", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-06-01 10:00", "2026-06-02 10:00", OutcomeRejected},
		{"a 24 hour wait over the changeover", "America/New_York", [2]string{"09:00", "18:00"}, "", "2026-03-07 10:00", "2026-03-08 11:00", OutcomeRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sched(t, tt.zone, days, tt.outer, tt.window[0], tt.window[1])
			counters := Counters{}.Add(tt.outcome)
			got := Settle(policy, counters, tt.outcome, "", at(t, tt.zone, tt.finishedAt), s)
			if got.Status != ContactActive || got.NextAttemptAt == nil {
				t.Fatalf("settlement = %+v, want ACTIVE with a next attempt", got)
			}
			// wait is added as an elapsed duration, as the policy says "after
			// 24 h"; only the move into the window is calendar arithmetic.
			want := at(t, tt.zone, tt.want)
			if tt.name == "a 24 hour wait over the changeover" {
				// 10:00 EST + 24 h elapsed = 11:00 EDT.
				want = at(t, tt.zone, tt.finishedAt).Add(24 * time.Hour)
			}
			if !got.NextAttemptAt.Equal(want) {
				t.Errorf("next = %s, want %s", got.NextAttemptAt.In(s.Zone), want.In(s.Zone))
			}
		})
	}
}

func TestContactsInDifferentZonesAreRescheduledIntoTheirOwnWindows(t *testing.T) {
	policy := DefaultRetryPolicy()
	w := CallingWindow{Days: allDays, Ranges: []TimeRange{{"09:00", "18:00"}}}
	finished := utc(t, "2026-06-02 02:00") // 22:00 Mon in New York, 10:00 Tue in Shanghai, 03:00 Tue in London
	zones := map[string]string{
		"America/New_York": "2026-06-02 13:00", // 09:00 EDT Tuesday
		"Asia/Shanghai":    "2026-06-02 04:00", // +2h = 12:00 Tuesday, inside
		"Europe/London":    "2026-06-02 08:00", // 09:00 BST Tuesday
	}
	for zone, wantUTC := range zones {
		s, err := NewSchedule("Asia/Tokyo", zone, w, dayrange.Range{})
		if err != nil {
			t.Fatal(err)
		}
		got := Settle(policy, Counters{1, 0}, OutcomeNoAnswer, "", finished, s)
		if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(utc(t, wantUTC)) {
			t.Errorf("%s: next = %v, want %s", zone, got.NextAttemptAt, wantUTC)
		}
	}
}

// Property: whatever happens, in whatever order, the invariants of the
// contact hold. A seeded run over thousands of histories.
func TestSettlementInvariantsOverRandomHistories(t *testing.T) {
	outcomes := []Outcome{
		OutcomeAnswered, OutcomeBotFailed, OutcomeOverflow, OutcomeNoAnswer, OutcomeBusy, OutcomeRejected,
		OutcomeUnknown, OutcomeTransient, OutcomeOriginateFailed, OutcomeInvalidNumber, OutcomeDNCSkipped,
	}
	rng := rand.New(rand.NewPCG(20261012, 7))
	s := settleWindow(t)
	for h := 0; h < 5000; h++ {
		policy := DefaultRetryPolicy()
		policy.MaxAttempts = 1 + rng.IntN(5)
		policy.MaxSystemRetries = rng.IntN(8)
		if rng.IntN(4) == 0 {
			policy.RetryOutcomes = policy.RetryOutcomes[:1+rng.IntN(len(policy.RetryOutcomes))]
		}
		now := at(t, "America/New_York", "2026-06-01 09:00").Add(time.Duration(rng.IntN(7*24*60)) * time.Minute)
		var c Counters
		status := ContactActive
		for step := 0; status == ContactActive; step++ {
			if step > 100 {
				t.Fatalf("history %d never ended", h)
			}
			o := outcomes[rng.IntN(len(outcomes))]
			var lead LeadIntent
			if rng.IntN(6) == 0 {
				lead = LeadIntent([]string{"HIGH", "DO_NOT_CALL", "LOW"}[rng.IntN(3)])
			}
			c = c.Add(o)
			got := Settle(policy, c, o, lead, now, s)
			status = got.Status

			if (status == ContactActive) != (got.NextAttemptAt != nil) {
				t.Fatalf("history %d: status %s with next %v", h, status, got.NextAttemptAt)
			}
			if got.NextAttemptAt != nil {
				if got.NextAttemptAt.Before(now) {
					t.Fatalf("history %d: next %v is before the call ended %v", h, got.NextAttemptAt, now)
				}
				if !s.IsOpen(*got.NextAttemptAt) {
					t.Fatalf("history %d: next %v is outside the window", h, got.NextAttemptAt)
				}
				now = *got.NextAttemptAt
			}
			if IsAnsweredOutcome(o) && status != ContactDone && status != ContactDoNotCall {
				t.Fatalf("history %d: answered outcome %s left the contact %s", h, o, status)
			}
			if c.Attempts > policy.MaxAttempts && status == ContactActive {
				t.Fatalf("history %d: attempts %d over max %d and still ACTIVE", h, c.Attempts, policy.MaxAttempts)
			}
			if c.SystemFailures > policy.MaxSystemRetries && status == ContactActive {
				t.Fatalf("history %d: system failures %d over max %d and still ACTIVE", h, c.SystemFailures, policy.MaxSystemRetries)
			}
		}
		if c.Attempts > policy.MaxAttempts || c.SystemFailures > max(policy.MaxSystemRetries, 1) {
			t.Fatalf("history %d: ended with %+v against max %d/%d", h, c, policy.MaxAttempts, policy.MaxSystemRetries)
		}
	}
}
