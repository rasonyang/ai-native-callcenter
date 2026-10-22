// SPDX-License-Identifier: Apache-2.0

package httpapi

// A deployment whose AICC_OUTBOUND_ENDPOINT is empty cannot place an AI
// outbound call at all, so POST /calls answers 409 OUTBOUND_NOT_CONFIGURED
// instead of 503 STORAGE_DOWN: the code names the cause, where a storage
// failure would send the operator to a database that is not at fault (#293).
// Real PostgreSQL for the ledger, the real dialer, and a switch that counts
// what it is told: the refusal comes before any CDR or originate exists.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

func TestAnAIOutboundCallIsRefusedWhenTheDeploymentHasNoEndpoint(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "noendpoint"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	flowID := uuid.New()
	sw := &countingSwitch{}
	// No EndpointFormat: exactly the deployment the issue is about.
	svc := outbound.New(
		outbound.Config{DefaultCountryCode: "1"},
		sw,
		didList{{Number: "95011", Language: "en", FlowID: &flowID,
			IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true}},
		nil,
		st.Ledger().HasCDR,
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	s := &dncStack{t: t, st: st, sw: sw, server: &Server{
		cfg: config.Config{DefaultCountryCode: "1"}, outbound: svc,
		agents: dialerPresence{}, agentDir: dialerDirectory{},
	}}

	// Both kinds of credential reach the operation and get the same answer.
	session := AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "ada",
		scopes: grantedScopes(auth.RoleAdmin),
	}
	for _, c := range []struct {
		name string
		ac   AuthContext
	}{{"session cookie", session}, {"API key", keySubject()}} {
		id := uuid.New()
		status, code, _ := s.dial(c.ac, fmt.Sprintf(
			`{"kind":"AI_OUTBOUND","did":"95011","to":"13912345678","callId":"%s"}`, id))
		if status != http.StatusConflict || code != "OUTBOUND_NOT_CONFIGURED" {
			t.Errorf("%s: %d %q, want 409 OUTBOUND_NOT_CONFIGURED", c.name, status, code)
		}
		if sw.count() != 0 || s.hasCDR(id) {
			t.Errorf("%s: a refused call originated or wrote a CDR", c.name)
		}
	}
}
