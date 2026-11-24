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
