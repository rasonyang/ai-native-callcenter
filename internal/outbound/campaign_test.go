// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// A campaign attempt mints its call and leg ids before the originate; the
// originate must pin the leg id, carry the campaign on the leg, and hand back
// the job the switch named.
func TestACampaignAttemptPinsItsLegAndCarriesTheCampaignOnTheLegAndTheBridge(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	callID, legID, campaignID, attemptID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	var heard []uuid.UUID
	var mu sync.Mutex
	s.SetAnswerObserver(func(id uuid.UUID) { mu.Lock(); heard = append(heard, id); mu.Unlock() })

	placed, err := s.PlaceAI(context.Background(), AIDialRequest{
		CallID: callID, LegID: legID, To: "13912345678", DIDNumber: "95012",
		CampaignID: campaignID, AttemptID: attemptID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if placed.CallID != callID || placed.JobID != "job-1" {
		t.Errorf("placement = %+v, want the call id and the job the switch returned", placed)
	}
	first := sw.lastOriginate()
	if first.partyID != legID {
		t.Errorf("originated leg %s, want the attempt's %s", first.partyID, legID)
	}
	if first.vars["aicc_campaign_id"] != campaignID.String() || first.vars["aicc_attempt_id"] != attemptID.String() ||
		first.vars["aicc_call_id"] != callID.String() {
		t.Errorf("leg vars = %v", first.vars)
	}

	answer(s, legID.String())
	waitBridges(t, sw, 1)
	bridge := sw.lastBridge()
	if bridge.vars["sip_h_X-AICC-Campaign-ID"] != campaignID.String() ||
		bridge.vars["sip_h_X-AICC-Attempt-ID"] != attemptID.String() {
		t.Errorf("bridge vars = %v, want the campaign and the attempt on the INVITE", bridge.vars)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 || heard[0] != callID {
		t.Errorf("answer observer heard %v, want the call once", heard)
	}
}

// A single call carries no campaign.
func TestASingleCallCarriesNoCampaignHeaders(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"}); err != nil {
		t.Fatal(err)
	}
	first := sw.lastOriginate()
	answer(s, first.partyID.String())
	waitBridges(t, sw, 1)
	for k := range sw.lastBridge().vars {
		if strings.Contains(k, "Campaign") || strings.Contains(k, "Attempt") {
			t.Errorf("a single call carries %s", k)
		}
	}
	if _, ok := first.vars["aicc_campaign_id"]; ok {
		t.Error("a single call's leg is marked as a campaign's")
	}
}

// Recovery re-arms a ringing campaign leg and the bridge still says whose it is.
func TestARecoveredCampaignLegKeepsItsCampaignOnTheBridge(t *testing.T) {
	rig := newRecoveryRig(t)
	callID, id, campaignID, attemptID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	vars := aiVars(callID, "95099", "en", "18688886666")
	vars[telephony.VarCampaignID] = campaignID.String()
	vars[telephony.VarAttemptID] = attemptID.String()
	rig.sw.add(t0, chanSpec{id: id, callState: "RINGING", age: 10 * time.Second, vars: vars})
	rep := rig.reconcile(t)
	if rep.Rearmed != 1 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	answer(rig.svc, id.String())
	cmd := waitForTransfer(t, rig.sw, id.String())
	for _, want := range []string{
		"sip_h_X-AICC-Campaign-ID=" + campaignID.String(),
		"sip_h_X-AICC-Attempt-ID=" + attemptID.String(),
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("bridge command lacks %q:\n%s", want, cmd)
		}
	}
}

func TestReconciledIsClosedOnlyOnceTheGateOpens(t *testing.T) {
	t.Parallel()
	s := testService(t, &fakeSwitch{}, nil)
	select {
	case <-s.Reconciled():
	default:
		t.Error("a service that was never gated should report itself reconciled")
	}
	s.HoldUntilReconciled()
	gate := s.Reconciled()
	select {
	case <-gate:
		t.Fatal("reconciled while the gate is closed")
	default:
	}
	s.MarkReconciled()
	select {
	case <-gate:
	default:
		t.Error("the gate opened and the channel stayed open")
	}
}
