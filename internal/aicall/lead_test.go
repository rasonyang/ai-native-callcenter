// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

type fakeLeads struct {
	calls  []flow.LeadRequest
	ids    []uuid.UUID
	ok     bool
	failed error
}

func (f *fakeLeads) SetLead(_ context.Context, callID uuid.UUID, intent campaign.LeadIntent, notes string) (bool, error) {
	if f.failed != nil {
		return false, f.failed
	}
	f.ids = append(f.ids, callID)
	f.calls = append(f.calls, flow.LeadRequest{Intent: string(intent), Notes: notes})
	return f.ok, nil
}

func TestRecordLeadWritesTheAttemptOfThisCall(t *testing.T) {
	actions, _, _ := testActions(t, &fakeSwitch{})
	leads := &fakeLeads{ok: true}
	actions.orchestrator.cfg.Leads = leads
	actions.campaignID = "c-1"
	actions.recorder = &callRecorder{callID: uuid.Must(uuid.NewV7())}

	result, err := actions.RecordLead(t.Context(), flow.LeadRequest{Intent: "HIGH", Notes: "wants the offer"})
	if err != nil || !result.IsOK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(leads.calls) != 1 || leads.calls[0].Intent != "HIGH" || leads.ids[0] != actions.recorder.callID {
		t.Errorf("writes = %v for %v", leads.calls, leads.ids)
	}
	if actions.isArmed() {
		t.Error("recording a lead armed an ending")
	}
}

func TestRecordLeadFailureIsAConversationNotAnError(t *testing.T) {
	for name, leads := range map[string]*fakeLeads{
		"store error":      {failed: errors.New("db down")},
		"attempt not live": {ok: false},
	} {
		actions, _, _ := testActions(t, &fakeSwitch{})
		actions.orchestrator.cfg.Leads = leads
		actions.campaignID = "c-1"
		actions.recorder = &callRecorder{callID: uuid.Must(uuid.NewV7())}

		result, err := actions.RecordLead(t.Context(), flow.LeadRequest{Intent: "HIGH"})
		if err != nil || result.IsOK || result.Error == "" || result.Hint == "" {
			t.Errorf("%s: result = %+v, %v, want a refusal with a hint", name, result, err)
		}
	}
}
