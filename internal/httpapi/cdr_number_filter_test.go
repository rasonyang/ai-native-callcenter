// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// ?number= reaches the store on both listings: GET /cdrs for the supervisor's
// ledger and GET /cdrs/mine for the agent's own calls. A call the platform
// placed is stored from the DID to the customer, so the customer's number
// finds it only by matching either end.
func TestTheNumberFilterReachesBothCDRListings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, secret := f.issueKey(f.seedAdmin(), "history:read:all")

	started := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	agentID := f.agent.agentID
	placed := uuid.Must(uuid.NewV7())
	other := uuid.Must(uuid.NewV7())
	for _, cdr := range []store.CDR{
		{CallID: placed, StartedAt: started, EndedAt: started.Add(time.Minute), CallType: "OUTBOUND",
			Status: store.CDRStatusAnswered, FromNumber: "100200", ToNumber: "13800000000", DID: "100200",
			PrimaryAgentID: &agentID, AgentIDs: []uuid.UUID{agentID}},
		{CallID: other, StartedAt: started, EndedAt: started.Add(time.Minute), CallType: "INBOUND",
			Status: store.CDRStatusAnswered, FromNumber: "+861083550341", ToNumber: "95001", DID: "95001",
			PrimaryAgentID: &agentID, AgentIDs: []uuid.UUID{agentID}},
	} {
		if err := f.st.Ledger().InsertCDR(ctx, cdr); err != nil {
			t.Fatalf("InsertCDR: %v", err)
		}
	}

	ids := func(r reply) []string {
		t.Helper()
		if r.status != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200; body %.200s", r.status, r.code, r.body)
		}
		var page struct {
			Items []struct {
				CallID string `json:"callId"`
			} `json:"items"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if page.Total != len(page.Items) {
			t.Errorf("total %d disagrees with %d items", page.Total, len(page.Items))
		}
		var out []string
		for _, it := range page.Items {
			out = append(out, it.CallID)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		call call
	}{
		{"/cdrs", call{method: http.MethodGet, path: "/cdrs?number=13800000000", bearer: secret}},
		{"/cdrs/mine", call{method: http.MethodGet, path: "/cdrs/mine?number=13800000000", cookie: f.agent.cookie}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(f.in(t).do(tc.call))
			if len(got) != 1 || got[0] != placed.String() {
				t.Fatalf("number=13800000000 returned %v, want only %s", got, placed)
			}
		})
	}
	// The caller-only filter is unchanged: the customer's number is not a caller.
	if got := ids(f.do(call{method: http.MethodGet, path: "/cdrs?fromNumber=13800000000", bearer: secret})); len(got) != 0 {
		t.Fatalf("fromNumber=13800000000 returned %v, want none", got)
	}
}
