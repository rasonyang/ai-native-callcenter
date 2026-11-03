// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/docs"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The contract is a large document: parse and resolve it once per process, not
// once per run of the test (a stress run repeats it dozens of times).
var (
	contractOnce sync.Once
	contractDoc  *openapi3.T
	contractErr  error
)

// cdrDeliveryOperation is webhooks.cdrDelivery's POST from the embedded
// contract, the document the reference is rendered from.
func cdrDeliveryOperation(t *testing.T) *openapi3.Operation {
	t.Helper()
	contractOnce.Do(func() {
		contractDoc, contractErr = openapi3.NewLoader().LoadFromData(docs.Contract)
	})
	if contractErr != nil {
		t.Fatalf("load the contract: %v", contractErr)
	}
	item := contractDoc.Webhooks["cdrDelivery"]
	if item == nil || item.Post == nil {
		t.Fatal("the contract declares no webhooks.cdrDelivery POST")
	}
	return item.Post
}

// The wire gate: what the worker puts on the wire is what the contract says it
// puts there. The request is captured off a real attempt, so the body is the
// worker's own marshalling of a CDR as the ledger stores it, and a field
// renamed, retyped or dropped on either side fails here.
func TestTheDeliveryOnTheWireMatchesTheContract(t *testing.T) {
	t.Parallel()
	op := cdrDeliveryOperation(t)

	var (
		gotBody    []byte
		gotHeaders http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	flowID, queueID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	started := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	cdr := store.CDR{
		CallID: uuid.Must(uuid.NewV7()), StartedAt: started,
		AnsweredAt: started.Add(2 * time.Second), EndedAt: started.Add(95 * time.Second),
		CallType: "INBOUND", Language: "en",
		FromNumber: "+15550100", ToNumber: "95001", DID: "95001",
		FlowID: &flowID, QueueID: &queueID,
		AgentIDs: []uuid.UUID{agentID}, PrimaryAgentID: &agentID,
		RingSec: 2, BotSec: 30, QueueWaitSec: 10, TalkSec: 50, BillSec: 93, TotalSec: 95,
		Status: "ANSWERED", HangupCause: "NORMAL_CLEARING", Disposition: "RESOLVED",
		HasRecording: true,
		UserData:     map[string]any{"botReason": "SESSION_LIMIT"},
		Legs:         []store.Leg{{Kind: "BOT", Label: "bot", DurationSec: 30}},
	}
	payload, err := json.Marshal(cdr)
	if err != nil {
		t.Fatal(err)
	}
	d := pending(MaxAttempts) // the last attempt, to pin the contract's maximum
	d.CallID = cdr.CallID
	d.Revision = 3
	d.Payload = payload

	st := newFakeStore(srv.URL, "their-token", d)
	newWorker(t, st, nil).drain(t.Context())
	if _, ok := st.delivered[d.DeliveryID]; !ok {
		t.Fatalf("not delivered: %s", st.whyNot(d.DeliveryID))
	}

	var body any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	media := op.RequestBody.Value.Content.Get("application/json")
	if media == nil {
		t.Fatal("the contract declares no application/json request body")
	}
	if ct := gotHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, the contract declares application/json", ct)
	}
	if err := media.Schema.Value.VisitJSON(body, openapi3.EnableFormatValidation()); err != nil {
		t.Errorf("the body on the wire does not match webhooks.cdrDelivery: %v\n%s", err, gotBody)
	}

	// Every header the contract declares is on the request and fits its schema.
	for _, ref := range op.Parameters {
		p := ref.Value
		if p.In != "header" {
			continue
		}
		v := gotHeaders.Get(p.Name)
		if v == "" {
			if p.Required {
				t.Errorf("required header %s is missing from the request", p.Name)
			}
			continue
		}
		if err := p.Schema.Value.VisitJSON(v, openapi3.EnableFormatValidation()); err != nil {
			t.Errorf("header %s = %q: %v", p.Name, v, err)
		}
	}
	// The bearer token is a security scheme rather than a parameter, and the
	// subscription token rides in it.
	if gotHeaders.Get("Authorization") != "Bearer their-token" {
		t.Errorf("Authorization = %q, want the subscription's bearer token", gotHeaders.Get("Authorization"))
	}
	if op.Security == nil || len(*op.Security) == 0 {
		t.Error("the contract declares no security for the delivery's bearer token")
	}
}
