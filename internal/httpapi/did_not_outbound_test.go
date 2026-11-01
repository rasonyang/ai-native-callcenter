// SPDX-License-Identifier: Apache-2.0

package httpapi

// POST /calls with AI_OUTBOUND presents the DID as the caller id, so a DID that
// does not allow outbound is refused 422 DID_NOT_OUTBOUND (#179). Real
// PostgreSQL for the ledger, the real dialer, and a switch that counts what it
// is told: the refusal has to come before any CDR or originate exists.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// editableDIDs is the catalog as the dialer reads it, with numbers an operator
// can change between two requests.
type editableDIDs struct {
	mu   sync.Mutex
	dids []catalog.DID
}

func (e *editableDIDs) DIDs(context.Context) ([]catalog.DID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]catalog.DID(nil), e.dids...), nil
}

func (e *editableDIDs) set(number string, fn func(*catalog.DID)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.dids {
		if e.dids[i].Number == number {
			fn(&e.dids[i])
		}
	}
}

func TestAnAIOutboundCallFromAnInboundOnlyDIDIsRefused(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "didout"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	flowID := uuid.Must(uuid.NewV7())
	dids := &editableDIDs{dids: []catalog.DID{
		{Number: "95301", Language: "en", FlowID: &flowID, IsEnabled: true, AllowInbound: true},
		{Number: "95302", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true},
		{Number: "95303", Language: "en", FlowID: &flowID, IsEnabled: true, AllowInbound: true, AllowOutbound: true},
		{Number: "95304", Language: "en", FlowID: &flowID, AllowInbound: true, AllowOutbound: true},
		{Number: "95305", Language: "en", IsEnabled: true, AllowOutbound: true},
		{Number: "95306", Language: "en", FlowID: &flowID, IsEnabled: true, AllowInbound: true, AllowOutbound: true},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}}
	sw := &countingSwitch{}
	svc := outbound.New(
		outbound.Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s"},
		sw, dids, nil,
		st.Ledger().HasCDR,
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	s := &dncStack{t: t, st: st, sw: sw, server: &Server{
		cfg: config.Config{}, outbound: svc,
		agents: dialerPresence{}, agentDir: dialerDirectory{},
	}}

	// Both kinds of credential reach the operation and get the same answer.
	session := AuthContext{
		Kind: SubjectUser, SubjectID: uuid.Must(uuid.NewV7()), SubjectName: "ada",
		scopes: grantedScopes(auth.RoleAdmin),
	}
	callers := []struct {
		name string
		ac   AuthContext
	}{{"session cookie", session}, {"API key", keySubject()}}

	dial := func(ac AuthContext, did string) (int, string, uuid.UUID) {
		id := uuid.Must(uuid.NewV7())
		status, code, _ := s.dial(ac, fmt.Sprintf(
			`{"kind":"AI_OUTBOUND","did":%q,"to":"13912345678","callId":"%s"}`, did, id))
		return status, code, id
	}

	for _, c := range callers {
		for _, tc := range []struct {
			name, did string
			status    int
			code      string
		}{
			{"inbound only", "95301", http.StatusUnprocessableEntity, "DID_NOT_OUTBOUND"},
			{"outbound only", "95302", http.StatusCreated, ""},
			{"both directions", "95303", http.StatusCreated, ""},
			{"both directions, disabled", "95304", http.StatusBadRequest, "VALIDATION_FAILED"},
			{"outbound allowed, no flow", "95305", http.StatusConflict, "CONFLICT"},
			{"unknown", "95399", http.StatusBadRequest, "VALIDATION_FAILED"},
			{"leading plus", "+95302", http.StatusBadRequest, "VALIDATION_FAILED"},
		} {
			before := sw.count()
			status, code, id := dial(c.ac, tc.did)
			if status != tc.status || code != tc.code {
				t.Errorf("%s, %s: %d %q, want %d %q", c.name, tc.name, status, code, tc.status, tc.code)
			}
			refused := tc.status >= 400
			if refused && (sw.count() != before || s.hasCDR(id)) {
				t.Errorf("%s, %s: a refused call originated or wrote a CDR", c.name, tc.name)
			}
		}
	}

	// A DID opened for outbound, then closed: the next request is refused.
	if status, code, _ := dial(keySubject(), "95306"); status != http.StatusCreated {
		t.Fatalf("before the flip: %d %s", status, code)
	}
	dids.set("95306", func(d *catalog.DID) { d.AllowOutbound = false })
	before := sw.count()
	status, code, id := dial(keySubject(), "95306")
	if status != http.StatusUnprocessableEntity || code != "DID_NOT_OUTBOUND" {
		t.Errorf("after the flip: %d %s, want 422 DID_NOT_OUTBOUND", status, code)
	}
	if sw.count() != before || s.hasCDR(id) {
		t.Error("the flipped DID's refused call originated or wrote a CDR")
	}

	// Repetition accumulates nothing: no originate, no ledger row, and no line
	// taken (a leaked line would turn into DID_CHANNELS_EXHAUSTED on a DID
	// with a limit, which the unit test in internal/outbound checks directly).
	before = sw.count()
	for i := 0; i < 50; i++ {
		c := callers[i%2]
		status, code, id := dial(c.ac, "95301")
		if status != http.StatusUnprocessableEntity || code != "DID_NOT_OUTBOUND" {
			t.Fatalf("repeat %d: %d %s", i, status, code)
		}
		if s.hasCDR(id) {
			t.Fatalf("repeat %d wrote a CDR", i)
		}
	}
	if sw.count() != before {
		t.Errorf("50 refused requests originated %d calls", sw.count()-before)
	}
}
