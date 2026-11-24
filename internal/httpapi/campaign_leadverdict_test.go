// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// The verdict vocabulary the store writes and the contract's
// CampaignLeadVerdict enum stay in step, both ways.
func TestEveryLeadVerdictIsOneTheContractNames(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, v := range campaign.AllLeadVerdicts() {
		known[string(v)] = true
		if !api.CampaignLeadVerdict(v).Valid() {
			t.Errorf("the store can record %s, which the contract's CampaignLeadVerdict does not name", v)
		}
	}
	for _, promised := range []api.CampaignLeadVerdict{
		api.CampaignLeadVerdictPENDING, api.CampaignLeadVerdictCONFIRMED, api.CampaignLeadVerdictUNDECIDED,
		api.CampaignLeadVerdictREFUSED, api.CampaignLeadVerdictMACHINE, api.CampaignLeadVerdictNOEVIDENCE,
		api.CampaignLeadVerdictNOTJUDGED,
	} {
		if !known[string(promised)] {
			t.Errorf("the contract names %s but nothing can record it", promised)
		}
	}
}

// An attempt's judgement reaches the API as the contract says: the three
// fields only on a judged attempt, the reason only when there is one.
func TestAnAttemptCarriesItsVerdictOnlyWhenJudged(t *testing.T) {
	t.Parallel()
	base := campaign.Attempt{ID: uuid.Must(uuid.NewV7()), State: campaign.AttemptFinished,
		Outcome: campaign.OutcomeAnswered, LeadIntent: campaign.LeadHigh}
	if out := campaignAttemptOut(base); out.LeadVerdict != nil || out.DeclaredLeadIntent != nil || out.LeadVerdictReason != nil {
		t.Errorf("an unjudged attempt carries %v %v %v", out.LeadVerdict, out.DeclaredLeadIntent, out.LeadVerdictReason)
	}
	judged := base
	judged.LeadIntent, judged.DeclaredLeadIntent = campaign.LeadMedium, campaign.LeadHigh
	judged.LeadVerdict, judged.LeadVerdictReason = campaign.VerdictUndecided, "The callee asked a question and did not decide."
	out := campaignAttemptOut(judged)
	if out.LeadIntent == nil || *out.LeadIntent != api.CampaignLeadIntentMEDIUM ||
		out.DeclaredLeadIntent == nil || *out.DeclaredLeadIntent != api.CampaignLeadIntentHIGH ||
		out.LeadVerdict == nil || *out.LeadVerdict != api.CampaignLeadVerdictUNDECIDED ||
		out.LeadVerdictReason == nil || *out.LeadVerdictReason != judged.LeadVerdictReason {
		t.Errorf("judged attempt = %+v", out)
	}
	pending := base
	pending.DeclaredLeadIntent, pending.LeadVerdict = campaign.LeadHigh, campaign.VerdictPending
	if out := campaignAttemptOut(pending); out.LeadVerdict == nil || *out.LeadVerdict != api.CampaignLeadVerdictPENDING ||
		out.LeadVerdictReason != nil {
		t.Errorf("pending attempt = %+v", out)
	}
}

// The outcome vocabulary the store writes and the contract's CampaignOutcome
// enum stay in step, in the same order (the outcome summary lists them in the
// enum's order), and every outcome the default policy retries is one the
// contract lets a policy name.
func TestEveryOutcomeIsOneTheContractNamesInItsOrder(t *testing.T) {
	t.Parallel()
	contract := []api.CampaignOutcome{
		api.CampaignOutcomeANSWERED, api.CampaignOutcomeBOTFAILED, api.CampaignOutcomeOVERFLOW,
		api.CampaignOutcomeNOANSWER, api.CampaignOutcomeBUSY, api.CampaignOutcomeREJECTED,
		api.CampaignOutcomeUNKNOWN, api.CampaignOutcomeTRANSIENT, api.CampaignOutcomeLINEREJECTED,
		api.CampaignOutcomeORIGINATEFAILED, api.CampaignOutcomeINVALIDNUMBER, api.CampaignOutcomeDNCSKIPPED,
	}
	all := campaign.AllOutcomes()
	if len(all) != len(contract) {
		t.Fatalf("the store has %d outcomes and the contract %d", len(all), len(contract))
	}
	for i, o := range all {
		if api.CampaignOutcome(o) != contract[i] || !contract[i].Valid() {
			t.Errorf("outcome %d is %s here and %s in the contract", i, o, contract[i])
		}
	}
	for _, o := range campaign.DefaultRetryPolicy().RetryOutcomes {
		if !api.CampaignRetryOutcome(o).Valid() {
			t.Errorf("the default policy retries %s, which CampaignRetryOutcome does not name", o)
		}
	}
	if !api.CampaignRetryOutcomeLINEREJECTED.Valid() {
		t.Error("CampaignRetryOutcome does not name LINE_REJECTED")
	}
}

// An attempt carries the far end's SIP status only when the switch reported
// one.
func TestAnAttemptCarriesItsSIPStatusOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()
	refused := campaign.Attempt{ID: uuid.Must(uuid.NewV7()), State: campaign.AttemptFinished,
		Outcome: campaign.OutcomeLineRejected, HangupCause: "CALL_REJECTED", SIPStatus: 402}
	out := campaignAttemptOut(refused)
	if out.Outcome == nil || *out.Outcome != api.CampaignOutcomeLINEREJECTED || out.SIPStatus == nil || *out.SIPStatus != 402 {
		t.Errorf("a line refusal = %+v", out)
	}
	none := refused
	none.Outcome, none.SIPStatus = campaign.OutcomeRejected, 0
	if out := campaignAttemptOut(none); out.SIPStatus != nil {
		t.Errorf("an attempt with no status carries %d", *out.SIPStatus)
	}
}
