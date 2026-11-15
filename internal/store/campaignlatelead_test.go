// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// A lead recorded after the attempt was finished early (#559): a restart's
// reconciliation or a recorded placement failure ended the attempt, then the
// call turned out to be live and the bot talked. The lead is accepted until the
// call has its real CDR, which then runs its side effects once.

// earlyEnds are the ways an attempt is finished before a live call's CDR.
var earlyEnds = map[string]func(t *testing.T, e *attemptEnv, a campaign.Attempt, start time.Time){
	"finished ORIGINATE_FAILED": func(t *testing.T, e *attemptEnv, a campaign.Attempt, start time.Time) {
		t.Helper()
		if _, err := e.attempts.Finish(context.Background(), a.CallID, campaign.Ending{
			Outcome: campaign.OutcomeOriginateFailed, HangupCause: "GATEWAY_DOWN", EndedAt: start.Add(10 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	},
	"finished UNKNOWN by a restart": func(t *testing.T, e *attemptEnv, a campaign.Attempt, start time.Time) {
		t.Helper()
		if _, err := e.attempts.Finish(context.Background(), a.CallID, campaign.Ending{
			Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: start.Add(10 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	},
	"finished by a placement-failure CDR": func(t *testing.T, e *attemptEnv, a campaign.Attempt, start time.Time) {
		t.Helper()
		cdr := cdrOf(a.CallID, "GATEWAY_DOWN", CDRStatusFailed, false, start, start.Add(10*time.Second))
		cdr.Tech = map[string]any{"isPlacementFailure": true}
		if err := e.ledger.InsertCDR(context.Background(), cdr); err != nil {
			t.Fatal(err)
		}
	},
}

func (e *attemptEnv) callbacksOf(t *testing.T, callID uuid.UUID) (n int) {
	t.Helper()
	if err := e.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM callbacks WHERE call_id=$1`, callID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *attemptEnv) dncOf(t *testing.T, number string) []DNCNumber {
	t.Helper()
	listed, _, err := e.st.DNC().List(context.Background(), DNCFilter{Prefix: number})
	if err != nil {
		t.Fatal(err)
	}
	return listed
}

func TestALeadAfterAnEarlyFinishIsAppliedOnceByTheRealCDR(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	start := ny(t, "2026-06-01 10:00")
	c, contacts := e.campaign(t, "Late lead", "7", 6, nil)
	next := 0

	for name, finish := range earlyEnds {
		for _, intent := range []campaign.LeadIntent{campaign.LeadDoNotCall, campaign.LeadHigh} {
			t.Run(name+"/"+string(intent), func(t *testing.T) {
				contact := contacts[next]
				next++
				a := e.begin(t, contact, start)
				finish(t, e, a, start)

				ok, err := e.attempts.SetLead(ctx, a.CallID, intent, "said so on the call")
				if err != nil || !ok {
					t.Fatalf("SetLead = %v, %v; want true", ok, err)
				}
				answered := cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))
				for round := 1; round <= 2; round++ { // the second is the same CDR again
					if err := e.ledger.InsertCDR(ctx, answered); err != nil {
						t.Fatal(err)
					}
					if ct := e.contact(t, c, contact.ID); ct.LeadIntent != intent || ct.LeadNotes != "said so on the call" {
						t.Errorf("round %d: contact = %+v", round, ct)
					}
					switch intent {
					case campaign.LeadDoNotCall:
						listed := e.dncOf(t, contact.Number)
						if len(listed) != 1 || listed[0].Source != DNCSourceConversation ||
							listed[0].CallID == nil || *listed[0].CallID != a.CallID {
							t.Errorf("round %d: dnc = %+v", round, listed)
						}
						if ct := e.contact(t, c, contact.ID); ct.Status != campaign.ContactDoNotCall {
							t.Errorf("round %d: status = %s", round, ct.Status)
						}
					case campaign.LeadHigh:
						if n := e.callbacksOf(t, a.CallID); n != 1 {
							t.Errorf("round %d: %d callbacks, want 1", round, n)
						}
					}
				}
			})
		}
	}
}

func TestALeadAfterTheRealCDRIsRefused(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	start := ny(t, "2026-06-01 10:00")
	c, contacts := e.campaign(t, "Too late lead", "8", 2, nil)

	a := e.begin(t, contacts[0], start)
	if _, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{
		Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: start.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadDoNotCall, "late")
	if err != nil || ok {
		t.Fatalf("SetLead after the real CDR = %v, %v; want false", ok, err)
	}
	got, err := e.attempts.Get(ctx, a.CallID)
	if err != nil || got.LeadIntent != "" {
		t.Errorf("attempt lead = %q, %v", got.LeadIntent, err)
	}
	if ct := e.contact(t, c, contacts[0].ID); ct.LeadIntent != "" || ct.Status == campaign.ContactDoNotCall {
		t.Errorf("contact = %+v", ct)
	}
	if listed := e.dncOf(t, contacts[0].Number); len(listed) != 0 {
		t.Errorf("dnc = %+v", listed)
	}

	if ok, err := e.attempts.SetLead(ctx, uuid.Must(uuid.NewV7()), campaign.LeadHigh, "x"); err != nil || ok {
		t.Errorf("SetLead on an unknown call = %v, %v; want false", ok, err)
	}
}

// SetLead racing the real CDR of an early-finished attempt: the recorder keeps
// re-recording (the model changing its mind, each with its own notes) until
// SetLead first reports false, so the race is straddled for real. The notes of
// the last accepted lead must be the ones the contact ended with, and every
// accepted lead's side effects are there once both are done. Both start behind
// a barrier; the database's lock on the attempt decides the order.
func TestALeadRacingTheRealCDRIsNeverAcceptedAndLost(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	start := ny(t, "2026-06-01 10:00")
	const rounds = 50
	c, contacts := e.campaign(t, "Racing lead", "9", rounds, nil)
	var refusedMidRace int

	for i := 0; i < rounds; i++ {
		a := e.begin(t, contacts[i], start)
		earlyEnds["finished UNKNOWN by a restart"](t, e, a, start)
		answered := cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))

		var (
			wg         sync.WaitGroup
			barrier    = make(chan struct{})
			lastNotes  string
			wasRefused bool
			leadErr    error
			cdrErr     error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-barrier
			for n := 0; n < 1000 && leadErr == nil; n++ {
				notes := fmt.Sprintf("stop %d", n)
				ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadDoNotCall, notes)
				if err != nil {
					leadErr = err
					return
				}
				if !ok {
					wasRefused = true
					return
				}
				lastNotes = notes
			}
		}()
		go func() {
			defer wg.Done()
			<-barrier
			cdrErr = e.ledger.InsertCDR(ctx, answered)
		}()
		close(barrier)
		wg.Wait()
		if leadErr != nil || cdrErr != nil {
			t.Fatalf("round %d: SetLead %v, InsertCDR %v", i, leadErr, cdrErr)
		}
		if wasRefused {
			refusedMidRace++
		}
		// Whatever the order, the call now has its real CDR.
		if ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadDoNotCall, "after"); err != nil || ok {
			t.Fatalf("round %d: SetLead after the CDR = %v, %v; want false", i, ok, err)
		}

		ct := e.contact(t, c, contacts[i].ID)
		listed := e.dncOf(t, contacts[i].Number)
		if lastNotes != "" {
			if ct.LeadIntent != campaign.LeadDoNotCall || ct.LeadNotes != lastNotes ||
				ct.Status != campaign.ContactDoNotCall || len(listed) != 1 {
				t.Fatalf("round %d: lead %q accepted but lost: contact = %+v, dnc = %+v", i, lastNotes, ct, listed)
			}
		} else if ct.LeadIntent != "" || len(listed) != 0 {
			t.Fatalf("round %d: lead refused but applied: contact = %+v, dnc = %+v", i, ct, listed)
		}
	}
	t.Logf("%d of %d rounds saw the recorder refused while the CDR landed", refusedMidRace, rounds)
}
