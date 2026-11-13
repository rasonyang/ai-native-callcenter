// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Each group of contacts counts once in every dimension it has a value for,
// and an opt-out is a DNC contact with the matching lead or last outcome.
func TestAGroupOfContactsCountsOnceInEachDimension(t *testing.T) {
	s := NewOutcomeSummary(uuid.Must(uuid.NewV7()))
	s.AddContacts(ContactActive, "", "", 40)                               // never tried
	s.AddContacts(ContactActive, OutcomeNoAnswer, "", 30)                  // waiting for a retry
	s.AddContacts(ContactDone, OutcomeAnswered, LeadHigh, 9)               // a HIGH lead
	s.AddContacts(ContactDone, OutcomeAnswered, "", 7)                     // answered, no lead recorded
	s.AddContacts(ContactDoNotCall, OutcomeAnswered, LeadDoNotCall, 4)     // said in conversation
	s.AddContacts(ContactDoNotCall, OutcomeDNCSkipped, "", 6)              // already listed
	s.AddContacts(ContactDoNotCall, OutcomeNoAnswer, "", 2)                // listed by somebody else mid-call
	s.AddContacts(ContactDoNotCall, OutcomeBotFailed, LeadMedium, 1)       // ditto, with a lead
	s.AddContacts(ContactDone, OutcomeAnswered, LeadDoNotCall, 3)          // not DNC: not an opt-out
	s.AddContacts(ContactExhausted, OutcomeDNCSkipped, "", 5)              // not DNC: not an opt-out
	s.AddContacts(ContactInvalidNumber, OutcomeInvalidNumber, LeadNone, 8) // a lead kept from earlier

	wantLast := map[Outcome]int{
		OutcomeNoAnswer: 32, OutcomeAnswered: 23, OutcomeDNCSkipped: 11,
		OutcomeBotFailed: 1, OutcomeInvalidNumber: 8,
	}
	for _, o := range AllOutcomes() {
		if s.ContactsByLastOutcome[o] != wantLast[o] {
			t.Errorf("contacts with last outcome %s = %d, want %d", o, s.ContactsByLastOutcome[o], wantLast[o])
		}
	}
	wantLead := map[LeadIntent]int{LeadHigh: 9, LeadMedium: 1, LeadNone: 8, LeadDoNotCall: 7}
	for _, l := range AllLeadIntents() {
		if s.ContactsByLeadIntent[l] != wantLead[l] {
			t.Errorf("contacts with lead %s = %d, want %d", l, s.ContactsByLeadIntent[l], wantLead[l])
		}
	}
	if s.OptOuts != (OptOutCounts{SaidInConversation: 4, AlreadyListed: 6}) {
		t.Errorf("opt-outs = %+v, want 4 said and 6 already listed", s.OptOuts)
	}
	if _, ok := s.ContactsByLastOutcome[""]; ok {
		t.Error("contacts never tried were counted under an empty outcome")
	}
	if _, ok := s.ContactsByLeadIntent[""]; ok {
		t.Error("contacts with no lead were counted under an empty intent")
	}
}

func TestTheVocabulariesTheSummaryWalks(t *testing.T) {
	if got := AllLeadIntents(); len(got) != 5 {
		t.Errorf("%d lead intents, want 5", len(got))
	}
	for _, l := range AllLeadIntents() {
		if !l.IsValid() {
			t.Errorf("%s is not valid", l)
		}
	}
	if LeadIntent("").IsValid() || LeadIntent("WARM").IsValid() {
		t.Error("an unknown intent is valid")
	}
	if got := AllCallbackStatuses(); len(got) != 4 {
		t.Errorf("%d callback statuses, want 4", len(got))
	}
}

func TestTheSummaryOfAnUnknownCampaignIsNotFound(t *testing.T) {
	f := newFixture(t, Env{})
	if _, err := f.svc.OutcomeSummary(context.Background(), uuid.Must(uuid.NewV7())); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
}

func TestListingContactsRefusesAnUnknownLastOutcome(t *testing.T) {
	f := newFixture(t, Env{})
	_, _, err := f.svc.ListContacts(context.Background(), uuid.Must(uuid.NewV7()), ContactFilter{LastOutcome: "NOPE"})
	var ce *Error
	if !errors.Is(err, ErrValidation) || !errors.As(err, &ce) || ce.Field != "lastOutcome" || ce.Rule != "UNKNOWN_OUTCOME" {
		t.Errorf("unknown last outcome: %v", err)
	}
}
