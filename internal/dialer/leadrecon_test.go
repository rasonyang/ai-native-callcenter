// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// A call that recorded a lead and then left no trace (the app died and the
// customer hung up) is reconciled as UNKNOWN, but the lead proves the customer
// talked to the bot: its contact is never dialled again, and ends DONE or DNC
// as the lead says (#264). The calls that recorded nothing are retried as
// before. The rig is the real dialer on the real store, so the rule under test
// is the one campaign.Settle applies in the finish routine, not a fake's.
func TestAnAttemptThatRecordedALeadLeftNoTraceIsNotDialledAgain(t *testing.T) {
	for _, path := range []string{"restart", "backstop"} {
		t.Run(path, func(t *testing.T) {
			r := newRig(t, opts{})
			c := r.newCampaign("Lead then crash", func(s *camSpec) { s.max = 40 })
			r.importContacts(c, numbers("51", 200))
			r.start(c)

			first := r.placer.liveCalls()
			if len(first) != 40 {
				t.Fatalf("setup: %d calls in flight, want 40", len(first))
			}
			intents := []campaign.LeadIntent{
				campaign.LeadDoNotCall, campaign.LeadNone, campaign.LeadHigh, campaign.LeadMedium, campaign.LeadLow,
			}
			wantLead := map[string]campaign.LeadIntent{} // number -> recorded intent
			for i, call := range first[:20] {
				intent := intents[i%len(intents)]
				ok, err := r.st.Attempts().SetLead(context.Background(), call.Req.CallID, intent, "notes "+string(intent))
				if err != nil || !ok {
					t.Fatalf("SetLead %s: %v %v", intent, ok, err)
				}
				wantLead[call.Req.To] = intent
			}
			noTrace := map[string]bool{}
			for _, call := range first[20:] {
				noTrace[call.Req.To] = true
			}

			// The crash: no channel, no CDR, for all 40.
			if path == "restart" {
				r.stopDialer()
			}
			for _, call := range first {
				r.placer.drop(call.Req.CallID)
			}
			switch path {
			case "restart":
				r.startDialer()
				r.settle()
			default:
				// Events lost: past ring timeout plus the margin the backstop
				// reconciles each.
				r.tick(2 * time.Minute)
			}

			for _, call := range first {
				a, err := r.st.Attempts().Get(context.Background(), call.Req.CallID)
				if err != nil || a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeUnknown {
					t.Fatalf("%s: attempt = %+v, %v; want FINISHED/UNKNOWN", call.Req.To, a, err)
				}
			}
			for number, intent := range wantLead {
				ct := r.contactOf(c, number)
				want := campaign.ContactDone
				if intent == campaign.LeadDoNotCall {
					want = campaign.ContactDoNotCall
				}
				if ct.Status != want || ct.NextAttemptAt != nil || ct.LeadIntent != intent {
					t.Errorf("%s (%s): contact = %+v, want %s with no next attempt", number, intent, ct, want)
				}
			}
			for number := range noTrace {
				ct := r.contactOf(c, number)
				if ct.Status != campaign.ContactActive || ct.NextAttemptAt == nil ||
					ct.NextAttemptAt.Before(r.clock.Now().Add(23*time.Hour)) {
					t.Errorf("%s: contact = %+v, want ACTIVE and a retry a day away", number, ct)
				}
			}

			// Run it out, past the 24 hour retry interval and beyond.
			for step := 0; step < 400; step++ {
				r.endAll(func(string) string { return endAnswered })
				if len(r.placer.liveCalls()) == 0 {
					next := r.nextDue(c)
					if next.IsZero() {
						break
					}
					r.at(next)
				}
			}
			r.noViolations()

			for _, ct := range r.contacts(c) {
				placed := r.placer.placedTo(ct.Number)
				switch {
				case wantLead[ct.Number] != "":
					if placed != 1 {
						t.Errorf("%s recorded %s and was dialled %d times, want once", ct.Number, wantLead[ct.Number], placed)
					}
				case noTrace[ct.Number]:
					if placed != 2 || ct.Status != campaign.ContactDone {
						t.Errorf("%s left no trace: dialled %d times, status %s; want a retry (2) and DONE", ct.Number, placed, ct.Status)
					}
				default:
					if placed != 1 || ct.Status != campaign.ContactDone {
						t.Errorf("%s: dialled %d times, status %s; want once and DONE", ct.Number, placed, ct.Status)
					}
				}
			}
			var attempts int
			for _, a := range r.attempts(c) {
				if a.State != campaign.AttemptFinished {
					t.Errorf("attempt %s is %s at the end", a.ID, a.State)
				}
				attempts++
			}
			if want := 200 + 20; attempts != want {
				t.Errorf("%d attempts, want %d (200 contacts and the 20 retries)", attempts, want)
			}
		})
	}
}
