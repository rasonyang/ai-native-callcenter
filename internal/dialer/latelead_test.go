// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// A lead the bot records after the attempt was finished early (#559). Two early
// finishes can precede a live call with the same call id, both after a restart
// with a slow switch: the reconciliation that ends an attempt whose leg was not
// yet listed (UNKNOWN), and a placement recorded as failed whose late leg the
// restarted process re-arms. The bot then talks, records its lead and the real
// CDR lands. The rig is the real dialer on the real store; the leg's coming
// back is the bot's SetLead and the CDR the switch writes.
func TestALeadRecordedAfterAnEarlyFinishIsAppliedByTheRealCDR(t *testing.T) {
	ctx := context.Background()
	earlyFinishes := map[string]func(r *rig, call placed){
		// The dialer stops with the call in flight; the leg is not in the
		// switch's listing when it starts again.
		"restart reconciles it UNKNOWN": func(r *rig, call placed) {
			r.stopDialer()
			r.placer.drop(call.Req.CallID)
			r.startDialer()
			r.settle()
		},
		// The placement was never confirmed and was recorded as failed; the
		// process restarts and its record of the late leg is gone.
		"a placement-failure CDR": func(r *rig, call placed) {
			r.stopDialer()
			cdr := cdrFor(call.Req.CallID, call.Req.To, endPlacement, call.At, r.clock.Now())
			if err := r.st.Ledger().InsertCDR(ctx, cdr); err != nil {
				r.t.Fatal(err)
			}
			r.placer.drop(call.Req.CallID)
			r.startDialer()
			r.settle()
		},
	}
	wantOutcome := map[string]campaign.Outcome{
		"restart reconciles it UNKNOWN": campaign.OutcomeUnknown,
		"a placement-failure CDR":       campaign.OutcomeOriginateFailed,
	}

	for name, finish := range earlyFinishes {
		for _, intent := range []campaign.LeadIntent{campaign.LeadDoNotCall, campaign.LeadHigh} {
			t.Run(name+"/"+string(intent), func(t *testing.T) {
				r := newRig(t, opts{})
				c := r.newCampaign("Late leg", func(s *camSpec) { s.max = 1 })
				r.importContacts(c, numbers("61", 1))
				r.start(c)

				live := r.placer.liveCalls()
				if len(live) != 1 {
					t.Fatalf("setup: %d calls in flight, want 1", len(live))
				}
				call := live[0]
				finish(r, call)

				a, err := r.st.Attempts().Get(ctx, call.Req.CallID)
				if err != nil || a.State != campaign.AttemptFinished || a.Outcome != wantOutcome[name] {
					t.Fatalf("attempt = %+v, %v; want FINISHED/%s", a, err, wantOutcome[name])
				}

				// The late leg answers and the bot records what the customer said.
				ok, err := r.st.Attempts().SetLead(ctx, call.Req.CallID, intent, "said so on the call")
				if err != nil || !ok {
					t.Fatalf("SetLead = %v, %v; want true", ok, err)
				}
				cdr := cdrFor(call.Req.CallID, call.Req.To, endAnswered, call.At, r.clock.Now())
				for round := 1; round <= 2; round++ { // the second is the same CDR again
					if err := r.st.Ledger().InsertCDR(ctx, cdr); err != nil {
						t.Fatal(err)
					}
					r.settle()

					// The real CDR revises the early finish: the attempt is the
					// answered call's, and the lead recorded on it is kept (the
					// ledger half of #573; the placement-failure row's
					// replacement is internal/store's).
					a, err := r.st.Attempts().Get(ctx, call.Req.CallID)
					if err != nil || a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeAnswered ||
						a.LeadIntent != intent || a.LeadNotes != "said so on the call" {
						t.Fatalf("round %d: attempt = %+v, %v; want FINISHED/%s with the lead kept", round, a, err, campaign.OutcomeAnswered)
					}

					ct := r.contactOf(c, call.Req.To)
					if ct.LeadIntent != intent || ct.LeadNotes != "said so on the call" || ct.NextAttemptAt != nil {
						t.Errorf("round %d: contact = %+v", round, ct)
					}
					switch intent {
					case campaign.LeadDoNotCall:
						if ct.Status != campaign.ContactDoNotCall {
							t.Errorf("round %d: status = %s, want DO_NOT_CALL", round, ct.Status)
						}
						listed, _, err := r.st.DNC().List(ctx, store.DNCFilter{Prefix: call.Req.To})
						if err != nil || len(listed) != 1 || listed[0].Source != store.DNCSourceConversation ||
							listed[0].CallID == nil || *listed[0].CallID != call.Req.CallID {
							t.Errorf("round %d: dnc = %+v, %v", round, listed, err)
						}
					case campaign.LeadHigh:
						if ct.Status != campaign.ContactDone {
							t.Errorf("round %d: status = %s, want DONE", round, ct.Status)
						}
						if n := callbacksFor(t, r, call.Req.CallID); n != 1 {
							t.Errorf("round %d: %d callbacks, want 1", round, n)
						}
					}
				}

				// Well past any retry interval, the number is never dialled again.
				r.tick(48 * time.Hour)
				r.noViolations()
				if n := r.placer.placedTo(call.Req.To); n != 1 {
					t.Errorf("%s dialled %d times, want once", call.Req.To, n)
				}
			})
		}
	}
}

func callbacksFor(t *testing.T, r *rig, callID uuid.UUID) (n int) {
	t.Helper()
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM callbacks WHERE call_id=$1`, callID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
