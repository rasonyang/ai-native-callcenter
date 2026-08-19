// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"math/rand"
	"slices"
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
	return planHistory(now, agents, queues, demoDispositions, rand.New(rand.NewSource(prngSeed)))
}

// The vocabulary a real installation has; the plan only ever files codes it
// was handed.
var demoDispositions = []string{"RESOLVED", "FOLLOW_UP_REQUIRED", "OTHER"}

// A demo whose agent screens are empty demos nothing: every call an agent
// handled carries a wrap-up, and the callers who ring most have names.
func TestTheHistoryGivesTheAgentScreensSomethingToShow(t *testing.T) {
	plan := fixedPlan()

	handled := map[uuid.UUID]bool{}
	for _, cdr := range plan.CDRs {
		if cdr.PrimaryAgentID != nil {
			handled[cdr.CallID] = true
		}
	}
	if len(handled) == 0 {
		t.Fatal("no call was handled by an agent")
	}
	if len(plan.WrapUps) != len(handled) {
		t.Errorf("%d wrap-ups for %d agent-handled calls", len(plan.WrapUps), len(handled))
	}
	for _, w := range plan.WrapUps {
		if !handled[w.CallID] {
			t.Errorf("wrap-up on call %s, which no agent handled", w.CallID)
		}
		if !slices.Contains(demoDispositions, w.DispositionCode) {
			t.Errorf("disposition %q is not in the installation's vocabulary", w.DispositionCode)
		}
	}

	if len(plan.Contacts) == 0 {
		t.Fatal("no contacts; the contact book demos empty")
	}
	callers := map[string]bool{}
	for _, cdr := range plan.CDRs {
		if cdr.CallType == "INBOUND" {
			callers[cdr.FromNumber] = true
		}
	}
	seen := map[string]bool{}
	for _, c := range plan.Contacts {
		if !callers[c.PhoneNumber] {
			t.Errorf("contact %s never called; its last-contact line would be empty", c.PhoneNumber)
		}
		if seen[c.PhoneNumber] {
			t.Errorf("two contacts share %s, which the book forbids", c.PhoneNumber)
		}
		seen[c.PhoneNumber] = true
		if c.Name == "" {
			t.Error("a contact with no name is not a contact")
		}
	}
}

// Without a vocabulary there is nothing to file under, and the demo says so
// by filing nothing rather than by inventing a code.
func TestNoVocabularyMeansNoWrapUps(t *testing.T) {
	now := time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC)
	agents := []uuid.UUID{uuid.MustParse("11111111-1111-4111-8111-111111111111")}
	queues := []QueueRef{{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), Name: "support-en"}}

	plan := planHistory(now, agents, queues, nil, rand.New(rand.NewSource(prngSeed)))
	if len(plan.WrapUps) != 0 {
		t.Errorf("%d wrap-ups filed with no vocabulary", len(plan.WrapUps))
	}
	if len(plan.Contacts) == 0 {
		t.Error("the contacts do not depend on the vocabulary and should still be there")
	}
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
