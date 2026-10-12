// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// The leg's whole life, and the job's failure, can be dispatched before the
// bgapi reply that names the job. The channel existed, so the CDR is the
// record and no FAILED row may be written.
func TestAChannelThatLivedAndDiedBeforeTheReplyIsNotAPlacementFailure(t *testing.T) {
	t.Parallel()
	s, sw, ledger := jobService(t)
	sw.onSend = func(jobID string) {
		leg := sw.legs[jobID]
		s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg})
		s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelHangup, ChannelID: leg})
		s.HandleSwitchEvent(jobEvent(jobID, "-ERR USER_BUSY\n"))
	}
	callID, err := s.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95012"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := ledger.get(callID); len(got) != 0 {
		t.Fatalf("a call whose channel existed was written as a placement failure: %+v", got)
	}
	eventually(t, "the entry to clear", func() bool { return s.JobCount() == 0 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.legJobs) != 0 {
		t.Errorf("leg index leaked %d entries", len(s.legJobs))
	}
}

// A placement that could not be sent leaves nothing registered.
func TestAnOriginateThatErrorsLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	s, _, _ := jobService(t)
	s.sw = erroringSwitch{s.sw}
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95012"}); err == nil {
		t.Fatal("want the error")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.legJobs) != 0 || len(s.jobs) != 0 {
		t.Errorf("registered placements remain: %d legs, %d jobs", len(s.legJobs), len(s.jobs))
	}
}

type erroringSwitch struct{ Switch }

func (erroringSwitch) Originate(uuid.UUID, string, map[string]string) (string, error) {
	return "", context.DeadlineExceeded
}

// The row a pre-channel failure writes, exactly as failPlacement builds it,
// goes through the real ledger and reads back as the API serves it. And the
// real call's row, if one arrives afterwards, replaces it, while the failure
// never replaces a real row.
func TestAPlacementFailureRoundTripsThroughTheLedger(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.ScratchDSN(t, "placefail")
	st, err := store.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ledger := st.Ledger()

	flowID := uuid.New()
	callID := uuid.Must(uuid.NewV7())
	job := &trackedJob{
		jobID: "job-1", callID: callID, legID: uuid.NewString(),
		placedAt: time.Now().UTC().Add(-2 * time.Second),
		callType: "OUTBOUND", from: "95099", to: "18688886666", did: "95099",
		language: "en", flowID: &flowID, legKind: "TRUNK", legLabel: "18688886666",
	}
	failed := placementFailureCDR(job, "GATEWAY_DOWN", map[string]any{"ticket": "T-1"}, time.Now().UTC())
	if err := ledger.InsertCDR(ctx, failed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := ledger.GetCDR(ctx, callID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != "FAILED" || got.HangupCause != "GATEWAY_DOWN" || !got.AnsweredAt.IsZero() ||
		got.CallType != "OUTBOUND" || got.ToNumber != "18688886666" || len(got.Legs) != 1 {
		t.Errorf("read back wrong: %+v", got)
	}
	// GET /cdrs/{callId} writes this value as JSON.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, has := wire["answeredAt"]; has {
		t.Errorf("answeredAt must be absent for a call never answered: %s", raw)
	}
	if wire["status"] != "FAILED" || wire["hangupCause"] != "GATEWAY_DOWN" {
		t.Errorf("wire form: %s", raw)
	}

	// The coordinator's row for the same call arrives late. Its call ended
	// before the job result did, so by ended_at alone it would lose.
	real := store.CDR{
		CallID: callID, StartedAt: job.placedAt, EndedAt: job.placedAt.Add(time.Second),
		CallType: "OUTBOUND", Status: store.CDRStatusBusy, HangupCause: "USER_BUSY", DID: "95099",
		Legs: []store.Leg{{Kind: "TRUNK", Label: "18688886666"}},
	}
	if err := ledger.InsertCDR(ctx, real); err != nil {
		t.Fatal(err)
	}
	if got, _ = ledger.GetCDR(ctx, callID); got.Status != store.CDRStatusBusy {
		t.Errorf("the real row must replace the placement failure, got %q", got.Status)
	}

	// And a failure arriving after the real row changes nothing.
	if err := ledger.InsertCDR(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if got, _ = ledger.GetCDR(ctx, callID); got.Status != store.CDRStatusBusy {
		t.Errorf("a placement failure replaced a real row: %q", got.Status)
	}
}
