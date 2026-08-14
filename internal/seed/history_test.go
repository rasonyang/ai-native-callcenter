// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fixedPlan() Plan {
	now := time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC)
	agents := []uuid.UUID{uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		uuid.MustParse("22222222-2222-4222-8222-222222222222")}
	queues := []QueueRef{
		{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), Name: "support-en"},
		{ID: uuid.MustParse("44444444-4444-4444-8444-444444444444"), Name: "support-zh"},
	}
	return planHistory(now, agents, queues, rand.New(rand.NewSource(prngSeed)))
}

// One seed, one story: the generator must be bit-stable so every install
// demos the same numbers and a re-run diffs to nothing.
func TestHistoryIsDeterministic(t *testing.T) {
	a, b := fixedPlan(), fixedPlan()
	if len(a.CDRs) != len(b.CDRs) || len(a.QueueEvents) != len(b.QueueEvents) {
		t.Fatalf("two runs disagree on volume: %d/%d vs %d/%d",
			len(a.CDRs), len(a.QueueEvents), len(b.CDRs), len(b.QueueEvents))
	}
	for i := range a.CDRs {
		if a.CDRs[i].CallID != b.CDRs[i].CallID || !a.CDRs[i].StartedAt.Equal(b.CDRs[i].StartedAt) {
			t.Fatalf("row %d differs between runs", i)
		}
	}
}

// The story must add up: reports computed over the seed have to be
// arithmetically coherent, or the demo teaches the wrong lessons.
func TestHistoryArithmeticHoldsTogether(t *testing.T) {
	plan := fixedPlan()

	if len(plan.CDRs) < 150 {
		t.Fatalf("only %d calls over seven days — too thin to demo reports", len(plan.CDRs))
	}

	var contained, missed, queued int
	seen := map[uuid.UUID]bool{}
	for _, cdr := range plan.CDRs {
		if seen[cdr.CallID] {
			t.Fatalf("call id %s repeats", cdr.CallID)
		}
		seen[cdr.CallID] = true

		total := cdr.BotSec + cdr.QueueWaitSec + cdr.RingSec + cdr.TalkSec
		if cdr.TotalSec != total {
			t.Errorf("call %s: totalSec %d ≠ parts %d", cdr.CallID, cdr.TotalSec, total)
		}
		if !cdr.EndedAt.Equal(cdr.StartedAt.Add(time.Duration(cdr.TotalSec) * time.Second)) {
			t.Errorf("call %s: endedAt does not match the duration", cdr.CallID)
		}
		if cdr.IsContained {
			contained++
			if cdr.Status != "ANSWERED" || cdr.TalkSec != 0 {
				t.Errorf("call %s: contained yet talked to a person", cdr.CallID)
			}
		}
		if cdr.MissedReason != "" {
			missed++
			if cdr.HangupCause != "ORIGINATOR_CANCEL" {
				t.Errorf("call %s: abandoned yet cause %q", cdr.CallID, cdr.HangupCause)
			}
		}
		if cdr.QueueID != nil {
			queued++
		}
	}
	if contained == 0 || missed == 0 || queued == 0 {
		t.Errorf("the mix lost a shape: contained=%d missed=%d queued=%d", contained, missed, queued)
	}

	// Every queue movement belongs to a generated call.
	for _, ev := range plan.QueueEvents {
		if !seen[ev.CallID] {
			t.Errorf("queue event references unknown call %s", ev.CallID)
		}
	}

	// Presence intervals must not run backwards.
	for _, entry := range plan.StateLogs {
		if entry.ExitedAt != nil && entry.ExitedAt.Before(entry.EnteredAt) {
			t.Errorf("state log for %s exits before it enters", entry.AgentID)
		}
	}
}
