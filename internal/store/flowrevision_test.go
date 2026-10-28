// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// outageSpec is NovaNet's outage line as one publish wrote it: the same flow
// each time, with the credit it announces changed.
func outageSpec(creditDays int, creditZh string) []byte {
	return []byte(`{
	  "id": "novanet_outage", "specVersion": "v2", "initialNode": "greet",
	  "global": {
	    "persona": {"en": "You are NovaNet Broadband's outage line.", "zh": "你是 NovaNet 宽带的故障热线。"},
	    "closingTarget": "farewell", "alwaysAllowedTools": ["hangup"]
	  },
	  "nodes": {
	    "greet": {
	      "instruction": {"en": "Explain the Harbour District outage credit and answer questions about it.", "zh": "说明海港区停网补偿，并回答相关问题。"},
	      "announce": {"en": "Thanks for calling NovaNet. Fibre in Harbour District is back, and ` +
		strconv.Itoa(creditDays) + ` days of service have been credited to your account.", "zh": "感谢致电 NovaNet。海港区光纤已恢复，已为您的账户补偿` + creditZh + `天服务费。"},
	      "transitions": [{"on": "TOOL_RESULT", "tool": "hangup", "target": "farewell"}]
	    },
	    "farewell": {
	      "instruction": {"en": "Say goodbye.", "zh": "道别。"},
	      "announce": {"en": "Thank you for calling NovaNet, goodbye.", "zh": "感谢致电 NovaNet，再见。"},
	      "isTerminal": true
	    }
	  }
	}`)
}

// refusedOutageSpec is a revision an older version stored: a misspelt line and
// a tool the flow never declared, both of which today's loader refuses.
var refusedOutageSpec = []byte(`{
  "id": "novanet_outage", "specVersion": "v2", "initialNode": "greet",
  "global": {"persona": {"en": "You are NovaNet Broadband's outage line.", "zh": "你是 NovaNet 宽带的故障热线。"}},
  "nodes": {
    "greet": {
      "instruction": {"en": "Look up the caller's outage ticket.", "zh": "查询来电者的故障工单。"},
      "anounce": {"en": "Thanks for calling NovaNet.", "zh": "感谢致电 NovaNet。"},
      "tools": ["lookup_outage_ticket", "hangup"]
    }
  }
}`)

// sameJSON reports whether two documents say the same thing: jsonb keeps
// neither the author's key order nor their whitespace, so bytes are not the
// comparison.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

func revisionStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, testdb.ScratchDSN(t, "flowrev"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// storedSpec is the column itself, read around the store.
func storedSpec(t *testing.T, st *Store, revisionID uuid.UUID) []byte {
	t.Helper()
	var raw []byte
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT spec FROM flow_revisions WHERE id = $1`, revisionID).Scan(&raw); err != nil {
		t.Fatalf("read revision %s: %v", revisionID, err)
	}
	return raw
}

// A revision is read with the spec it stored, as stored, including one the
// loader refuses today: that is the revision a reader most needs to see. A
// revision is only ever found through its own flow.
func TestARevisionIsReadAsItWasStored(t *testing.T) {
	ctx := context.Background()
	st := revisionStore(t)
	flows := st.Flows()

	outage, err := flows.Create(ctx, "novanet-outage", "Outage line", outageSpec(3, "三"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := flows.Publish(ctx, outage, "First outage credit"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := flows.UpdateDraft(ctx, outage, "Outage line", outageSpec(5, "五")); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if err := flows.Publish(ctx, outage, ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	refusedID := uuid.Must(uuid.NewV7())
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO flow_revisions (id, flow_id, spec, note) VALUES ($1, $2, $3, $4)`,
		refusedID, outage, refusedOutageSpec, "旧版本留下的配置"); err != nil {
		t.Fatalf("write the old revision: %v", err)
	}
	if _, err := flow.Load(refusedOutageSpec); err == nil {
		t.Fatal("the fixture's old revision loads; it is meant to be one today's loader refuses")
	}

	billing, err := flows.Create(ctx, "novanet-billing", "Billing", miniSpec("You are NovaNet's billing assistant."))
	if err != nil {
		t.Fatalf("create the other flow: %v", err)
	}
	if err := flows.Publish(ctx, billing, "billing"); err != nil {
		t.Fatalf("publish the other flow: %v", err)
	}

	detail, err := flows.Detail(ctx, outage)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Revisions) != 3 {
		t.Fatalf("revisions = %d, want 3", len(detail.Revisions))
	}
	for _, summary := range detail.Revisions {
		got, err := flows.Revision(ctx, outage, summary.ID)
		if err != nil {
			t.Fatalf("revision %s: %v", summary.ID, err)
		}
		if got.FlowRevisionSummary != summary {
			t.Errorf("revision %s = %+v, want what the history lists: %+v", summary.ID, got.FlowRevisionSummary, summary)
		}
		if !sameJSON(t, got.Spec, storedSpec(t, st, summary.ID)) {
			t.Errorf("revision %s spec = %s, want what was stored", summary.ID, got.Spec)
		}
	}
	refused, err := flows.Revision(ctx, outage, refusedID)
	if err != nil {
		t.Fatalf("the refused revision was not served: %v", err)
	}
	if !sameJSON(t, refused.Spec, refusedOutageSpec) || refused.Note != "旧版本留下的配置" || refused.IsPublished {
		t.Errorf("the refused revision = %+v %s, want it as written", refused.FlowRevisionSummary, refused.Spec)
	}

	billingDetail, err := flows.Detail(ctx, billing)
	if err != nil {
		t.Fatalf("detail of the other flow: %v", err)
	}
	for _, tc := range []struct {
		name             string
		flowID, revision uuid.UUID
	}{
		{"a revision that does not exist", outage, uuid.Must(uuid.NewV7())},
		{"another flow's revision", outage, billingDetail.Revisions[0].ID},
		{"a flow that does not exist", uuid.Must(uuid.NewV7()), refusedID},
	} {
		if _, err := flows.Revision(ctx, tc.flowID, tc.revision); !errors.Is(err, ErrFlowRevisionNotFound) {
			t.Errorf("%s: error = %v, want ErrFlowRevisionNotFound", tc.name, err)
		}
	}
}

// Restoring is reading a revision and saving its spec as the draft, and the
// usual publish puts it live as a new revision: history is appended to, never
// rewritten. A call that started before the publish keeps the spec it loaded,
// and a revision today's loader refuses is refused as a draft, leaving the
// draft as it was.
func TestARestoredRevisionGoesLiveOnlyThroughThePublishAndACallKeepsItsOwn(t *testing.T) {
	ctx := context.Background()
	st := revisionStore(t)
	flows := st.Flows()

	id, err := flows.Create(ctx, "novanet-outage", "Outage line", outageSpec(3, "三"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := flows.Publish(ctx, id, "A: three days"); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	// Known by the id its publish created, not by its place in the history,
	// which is the database clock's order.
	first, err := flows.Summary(ctx, id)
	if err != nil || first.PublishedRevisionID == nil {
		t.Fatalf("publish A left nothing live: %+v %v", first, err)
	}
	revisionA := *first.PublishedRevisionID
	if err := flows.UpdateDraft(ctx, id, "Outage line", outageSpec(10, "十")); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if err := flows.Publish(ctx, id, "B: ten days"); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	refusedID := uuid.Must(uuid.NewV7())
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO flow_revisions (id, flow_id, spec, note) VALUES ($1, $2, $3, '')`,
		refusedID, id, refusedOutageSpec); err != nil {
		t.Fatalf("write the old revision: %v", err)
	}
	before, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	history := map[uuid.UUID][]byte{}
	listedBefore := map[uuid.UUID]FlowRevisionSummary{}
	for _, rev := range before.Revisions {
		history[rev.ID] = storedSpec(t, st, rev.ID)
		listedBefore[rev.ID] = rev
	}

	// A call answered now runs B, and keeps it.
	inProgress, err := flows.PublishedSpec(ctx, id)
	if err != nil {
		t.Fatalf("a call could not load the published revision: %v", err)
	}
	greeting := func(spec *flow.Spec) string {
		node, ok := spec.Node("greet")
		if !ok || node.Announce.IsEmpty() {
			t.Fatalf("the loaded flow has no greeting line: %+v", spec.Nodes)
		}
		return node.Announce.For("en")
	}
	const saysA, saysB = "3 days of service", "10 days of service"
	if !strings.Contains(greeting(inProgress), saysB) {
		t.Fatalf("the call runs %q, want B", greeting(inProgress))
	}

	// Restore A: the draft becomes A, and nothing a caller hears changes yet.
	a, err := flows.Revision(ctx, id, revisionA)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if err := flows.UpdateDraft(ctx, id, "Outage line", a.Spec); err != nil {
		t.Fatalf("restore A to the draft: %v", err)
	}
	restored, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !sameJSON(t, restored.DraftSpec, history[revisionA]) {
		t.Errorf("draft = %s, want A", restored.DraftSpec)
	}
	if !restored.Flow.HasUnpublishedChanges {
		t.Error("the draft is A and B is live, and the flow reports nothing unpublished")
	}
	if live, err := flows.PublishedSpec(ctx, id); err != nil || !strings.Contains(greeting(live), saysB) {
		t.Errorf("restoring to the draft changed what callers hear: %v", err)
	}

	// The usual publish appends a revision and leaves the others alone.
	if err := flows.Publish(ctx, id, "Restore of "+revisionA.String()[:8]); err != nil {
		t.Fatalf("publish the restored draft: %v", err)
	}
	after, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(after.Revisions) != len(before.Revisions)+1 {
		t.Fatalf("revisions = %d, want one more than %d", len(after.Revisions), len(before.Revisions))
	}
	added := 0
	for _, rev := range after.Revisions {
		was, existed := listedBefore[rev.ID]
		if !existed {
			added++
			if !rev.IsPublished || after.Flow.PublishedRevisionID == nil || *after.Flow.PublishedRevisionID != rev.ID {
				t.Errorf("the publish appended %+v, which is not live", rev)
			}
			if !sameJSON(t, storedSpec(t, st, rev.ID), history[revisionA]) {
				t.Error("the new revision is not A")
			}
			continue
		}
		was.IsPublished = false
		if rev != was {
			t.Errorf("revision %s changed: %+v, was %+v", rev.ID, rev, listedBefore[rev.ID])
		}
		if !slices.Equal(storedSpec(t, st, rev.ID), history[rev.ID]) {
			t.Errorf("revision %s's spec was rewritten", rev.ID)
		}
	}
	if added != 1 {
		t.Errorf("the publish added %d revisions, want 1", added)
	}

	// The call that started on B ends on B; the next call answers with A.
	if !strings.Contains(greeting(inProgress), saysB) {
		t.Errorf("the call in progress now runs %q, want B to its end", greeting(inProgress))
	}
	next, err := flows.PublishedSpec(ctx, id)
	if err != nil {
		t.Fatalf("the next call could not load the published revision: %v", err)
	}
	if !strings.Contains(greeting(next), saysA) {
		t.Errorf("the next call runs %q, want A", greeting(next))
	}

	// The refused revision cannot be restored, and the draft stays A.
	old, err := flows.Revision(ctx, id, refusedID)
	if err != nil {
		t.Fatalf("read the old revision: %v", err)
	}
	err = flows.UpdateDraft(ctx, id, "Outage line", old.Spec)
	var invalid *flow.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("restoring the old revision: error = %v, want a flow.ValidationError", err)
	}
	for _, want := range []string{
		`phase "greet" has unknown field "anounce"; did you mean "announce"?`,
		`phase "greet" allows unknown tool "lookup_outage_ticket"`,
	} {
		if !slices.Contains(invalid.Problems, want) {
			t.Errorf("problems = %q, lack %q", invalid.Problems, want)
		}
	}
	unchanged, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !sameJSON(t, unchanged.DraftSpec, history[revisionA]) {
		t.Errorf("a refused restore changed the draft: %s", unchanged.DraftSpec)
	}
}
