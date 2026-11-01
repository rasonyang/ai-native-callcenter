// SPDX-License-Identifier: Apache-2.0

package outbound

// An AI outbound call presents its DID as the caller id, so it may only go
// out from a number the operator opened for outbound (#179). The refusal comes
// before anything exists: no ledger row, no originate, no pending leg, no line
// taken.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

func TestDialAIOnlyGoesOutFromANumberThatAllowsOutbound(t *testing.T) {
	t.Parallel()
	flowID := uuid.Must(uuid.NewV7())
	for _, tc := range []struct {
		name string
		did  catalog.DID
		to   string // the number asked for; defaults to the DID's own
		want error
	}{
		{"inbound only, with a flow",
			catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowInbound: true}, "", ErrDIDNotOutbound},
		{"outbound only, with a flow",
			catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowOutbound: true}, "", nil},
		{"both directions",
			catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowInbound: true, AllowOutbound: true}, "", nil},
		{"both directions, disabled",
			catalog.DID{Number: "95100", FlowID: &flowID, AllowInbound: true, AllowOutbound: true}, "", ErrUnknownDID},
		{"outbound allowed, no flow",
			catalog.DID{Number: "95100", IsEnabled: true, AllowOutbound: true}, "", ErrFlowless},
		{"unknown number",
			catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowOutbound: true}, "95199", ErrUnknownDID},
		{"a leading plus is not the stored number",
			catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowOutbound: true}, "+95100", ErrUnknownDID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sw := &fakeSwitch{}
			s := testService(t, sw, nil)
			s.dids = fakeDIDs{tc.did}
			asked := tc.to
			if asked == "" {
				asked = tc.did.Number
			}
			_, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: asked})
			if !errors.Is(err, tc.want) {
				t.Fatalf("DialAI error = %v, want %v", err, tc.want)
			}
			if tc.want != nil && len(sw.originateList()) != 0 {
				t.Errorf("a refused call (%v) was originated", tc.want)
			}
			if tc.want == nil && len(sw.originateList()) != 1 {
				t.Errorf("originates = %d, want 1", len(sw.originateList()))
			}
		})
	}
}

// Nothing is left behind by a refusal, however often it repeats.
func TestARefusedInboundOnlyDIDLeavesNoStateBehind(t *testing.T) {
	t.Parallel()
	flowID := uuid.Must(uuid.NewV7())
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	s.dids = fakeDIDs{{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowInbound: true}}

	for i := 0; i < 50; i++ {
		id := uuid.Must(uuid.NewV7())
		if _, err := s.DialAI(context.Background(),
			AIDialRequest{CallID: id, To: "13912345678", DIDNumber: "95100"}); !errors.Is(err, ErrDIDNotOutbound) {
			t.Fatalf("request %d: %v, want ErrDIDNotOutbound", i, err)
		}
		if s.lines.Holds(id) {
			t.Fatalf("request %d took a line on the DID", i)
		}
	}
	if n := len(sw.originateList()); n != 0 {
		t.Errorf("originates = %d, want 0", n)
	}
	s.mu.Lock()
	pending, jobs, legJobs := len(s.pending), len(s.jobs), len(s.legJobs)
	s.mu.Unlock()
	if pending != 0 || jobs != 0 || legJobs != 0 {
		t.Errorf("state left behind: pending=%d jobs=%d legJobs=%d", pending, jobs, legJobs)
	}
	if n := s.lines.InUse("95100"); n != 0 {
		t.Errorf("lines in use on the refused DID = %d", n)
	}
}

// The flag is read on every request: a DID opened for outbound dials, and the
// same DID closed again is refused on the next request.
func TestADIDFlippedToInboundOnlyIsRefusedFromTheNextRequest(t *testing.T) {
	t.Parallel()
	flowID := uuid.Must(uuid.NewV7())
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	did := catalog.DID{Number: "95100", FlowID: &flowID, IsEnabled: true, AllowInbound: true, AllowOutbound: true}
	s.dids = fakeDIDs{did}

	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95100"}); err != nil {
		t.Fatalf("while outbound is allowed: %v", err)
	}
	did.AllowOutbound = false
	s.dids = fakeDIDs{did}
	if _, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345679", DIDNumber: "95100"}); !errors.Is(err, ErrDIDNotOutbound) {
		t.Fatalf("after the flip: %v, want ErrDIDNotOutbound", err)
	}
	if n := len(sw.originateList()); n != 1 {
		t.Errorf("originates = %d, want only the one before the flip", n)
	}
}

func (f *fakeSwitch) originateList() []originated {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]originated(nil), f.originates...)
}
