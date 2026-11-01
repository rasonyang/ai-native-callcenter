// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// A DID at its outbound line limit refuses a single call and a click-to-dial
// with 409 DID_CHANNELS_EXHAUSTED before anything is sent or recorded, and the
// line comes back when the call ends.
func TestACallFromADIDAtItsLimitIsRefusedWith409AndLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "didlines"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	one := 1
	flowID := uuid.Must(uuid.NewV7())
	sw := &countingSwitch{}
	svc := outbound.New(
		outbound.Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"}, sw,
		didList{
			{Number: "95201", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, MaxOutboundChannels: &one},
			{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true, MaxOutboundChannels: &one},
		},
		nil, st.Ledger().HasCDR, func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	s := &dncStack{t: t, st: st, sw: sw, server: &Server{
		cfg: config.Config{}, outbound: svc, agents: dialerPresence{}, agentDir: dialerDirectory{},
	}}

	first := uuid.Must(uuid.NewV7())
	if status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95201","to":"13800138000","callId":"`+first.String()+`"}`); status != http.StatusCreated {
		t.Fatalf("the first call: %d %s, want 201", status, code)
	}
	second := uuid.Must(uuid.NewV7())
	status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95201","to":"13900139000","callId":"`+second.String()+`"}`)
	if status != http.StatusConflict || code != "DID_CHANNELS_EXHAUSTED" {
		t.Fatalf("the call over the limit: %d %s, want 409 DID_CHANNELS_EXHAUSTED", status, code)
	}
	if s.hasCDR(second) {
		t.Error("a CDR exists for a call that was refused")
	}
	if sw.count() != 1 {
		t.Errorf("%d originates, want only the first call's", sw.count())
	}

	// Click-to-dial goes out from the default outbound number.
	if status, code, _ := s.dial(agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"13800138001"}`); status != http.StatusCreated {
		t.Fatalf("the first click-to-dial: %d %s, want 201", status, code)
	}
	if status, code, _ := s.dial(agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"13800138002"}`); status != http.StatusConflict || code != "DID_CHANNELS_EXHAUSTED" {
		t.Fatalf("the click-to-dial over the limit: %d %s, want 409 DID_CHANNELS_EXHAUSTED", status, code)
	}

	svc.CallEnded(first)
	if status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95201","to":"13900139000"}`); status != http.StatusCreated {
		t.Fatalf("after the line came back: %d %s, want 201", status, code)
	}
}
