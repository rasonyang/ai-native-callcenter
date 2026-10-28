// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

const billingFlow = `{
  "id": "novanet_billing", "specVersion": "v2", "initialNode": "welcome",
  "global": {"persona": "You are NovaNet Broadband's billing assistant."},
  "nodes": {
    "welcome": {"instruction": "Answer the caller's billing question.", "tools": ["hangup"]}
  }
}`

// strayProblem is how the loader names the key the test writes into the
// stored rows.
const strayProblem = `phase "welcome" has unknown field "anounce"; did you mean "announce"?`

// A row stored with a field the spec does not declare, as rows written before
// the loader refused one are, still answers its number: the call-time load,
// a campaign's load and rebinding a number log one warning naming the field
// and run the flow. Writing it again is refused: publishing the stored draft
// and saving it back both name the field.
//
// Not parallel: it reads the default logger, which the store writes to.
func TestAStoredUnknownFieldWarnsOnReadAndIsRefusedOnWrite(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	flows := st.Flows()

	flowID, err := flows.Create(ctx, "novanet-billing", "Billing", []byte(billingFlow))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := flows.Publish(ctx, flowID, "first"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	did, err := st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: "95362", Language: "en", FlowID: &flowID,
		IsEnabled: true, AllowInbound: true, AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("create did: %v", err)
	}

	// Straight into the tables, as an older version would have stored it.
	const stray = `'{nodes,welcome,anounce}', '"Thank you for calling NovaNet. Goodbye."'`
	if _, err := st.Pool.Exec(ctx, `UPDATE flow_revisions SET spec = jsonb_set(spec, `+stray+`) WHERE flow_id = $1`, flowID); err != nil {
		t.Fatalf("write the revision: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE flows SET draft_spec = jsonb_set(draft_spec, `+stray+`) WHERE id = $1`, flowID); err != nil {
		t.Fatalf("write the draft: %v", err)
	}

	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	warnings := func() []string {
		var lines []string
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, "published flow has a field the spec does not declare") {
				lines = append(lines, line)
			}
		}
		return lines
	}

	// The call-time load: the number answers, and the log names the field.
	spec, err := flows.PublishedSpec(ctx, flowID)
	if err != nil {
		t.Fatalf("a call could not load the published revision: %v", err)
	}
	if _, ok := spec.Node("welcome"); !ok {
		t.Errorf("the loaded flow has no welcome phase: %+v", spec.Nodes)
	}
	lines := warnings()
	if len(lines) != 1 {
		t.Fatalf("warnings after one call-time load = %d, want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"level=WARN", "flowId=" + flowID.String(), "specId=novanet_billing", "anounce", "did you mean"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("warning lacks %q: %s", want, lines[0])
		}
	}

	// A campaign's loads and rebinding the number read the same row the
	// same way.
	campaigns := st.Campaigns()
	one, err := campaigns.DIDFlow(ctx, did.ID)
	if err != nil || one.Spec == nil {
		t.Errorf("campaign DIDFlow = %+v, %v; want the flow", one, err)
	}
	many, err := campaigns.DIDFlows(ctx, []uuid.UUID{did.ID})
	if err != nil || len(many) != 1 || many[0].Spec == nil {
		t.Errorf("campaign DIDFlows = %+v, %v; want the flow", many, err)
	}
	if _, err := st.Catalog().UpdateDID(ctx, did); err != nil {
		t.Errorf("rebinding the number to the flow: %v", err)
	}
	if got := len(warnings()); got != 4 {
		t.Errorf("warnings after four loads = %d, want one each:\n%s", got, logs.String())
	}

	// Writing it again is refused, naming the field.
	refusedWith := func(op string, err error) {
		t.Helper()
		var invalid *flow.ValidationError
		if !errors.As(err, &invalid) {
			t.Fatalf("%s error = %v, want a flow.ValidationError", op, err)
		}
		if !slices.Contains(invalid.Problems, strayProblem) {
			t.Errorf("%s problems = %q, want %q", op, invalid.Problems, strayProblem)
		}
	}
	refusedWith("publish", flows.Publish(ctx, flowID, "again"))
	detail, err := flows.Detail(ctx, flowID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !strings.Contains(string(detail.DraftSpec), "anounce") {
		t.Fatalf("the stored draft lost the key: %s", detail.DraftSpec)
	}
	refusedWith("update draft", flows.UpdateDraft(ctx, flowID, "Billing", detail.DraftSpec))
	_, err = flows.Create(ctx, "novanet-billing-copy", "Billing copy", detail.DraftSpec)
	refusedWith("create", err)

	// The refused publish left the live revision where it was.
	after, err := flows.Detail(ctx, flowID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(after.Revisions) != 1 {
		t.Errorf("revisions = %d, want the one publish that succeeded", len(after.Revisions))
	}
}
