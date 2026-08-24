// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// The smallest spec the loader accepts: a persona, one phase, and a phase to
// start in.
func miniSpec(persona string) []byte {
	return []byte(`{
	  "id": "probe",
	  "specVersion": "v2",
	  "initialNode": "welcome",
	  "global": {"persona": "` + persona + `"},
	  "nodes": {"welcome": {"instruction": "Greet the caller."}}
	}`)
}

// What an author is editing and what a caller hears are two different things,
// and the gap between them is the one fact the flows screen exists to show.
//
// It needs a real server: has_unpublished_changes is decided by PostgreSQL's
// jsonb comparison, which is semantic — reindenting a draft or reordering its
// keys is not a change, and a byte comparison in Go would call it one and
// invite an operator to republish an identical spec.
func TestAFlowKnowsWhetherItsDraftIsWhatCallersHear(t *testing.T) {
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

	id, err := flows.Create(ctx, "probe", "Probe", miniSpec("A helpful assistant."))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Created is not live. A number pointing here reaches no bot yet, and the
	// screen must not imply otherwise.
	before, err := flows.Summary(ctx, id)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if before.PublishedRevisionID != nil || before.PublishedAt != nil {
		t.Errorf("a new flow reports itself published: revision=%v at=%v",
			before.PublishedRevisionID, before.PublishedAt)
	}
	if !before.HasUnpublishedChanges {
		t.Error("a flow with nothing published reports no unpublished changes — " +
			"there is a draft nobody can hear, which is exactly the gap this flags")
	}

	if err := flows.Publish(ctx, id, "first"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	live, err := flows.Summary(ctx, id)
	if err != nil {
		t.Fatalf("summary after publish: %v", err)
	}
	if live.PublishedRevisionID == nil || live.PublishedAt == nil {
		t.Fatalf("published flow has no revision: %+v", live)
	}
	if live.HasUnpublishedChanges {
		t.Error("the draft was just published and still reports changes")
	}

	// Whitespace and key order are not changes. jsonb says so; a byte
	// comparison would not.
	reformatted := []byte(`{"specVersion":"v2","nodes":{"welcome":{"instruction":"Greet the caller."}},` +
		`"global":{"persona":"A helpful assistant."},"initialNode":"welcome","id":"probe"}`)
	if err := flows.UpdateDraft(ctx, id, "Probe", reformatted); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	same, err := flows.Summary(ctx, id)
	if err != nil {
		t.Fatalf("summary after reformat: %v", err)
	}
	if same.HasUnpublishedChanges {
		t.Error("reindenting a draft counted as a change — an operator would be " +
			"invited to republish a spec identical to the one already live")
	}

	// A real edit does count.
	if err := flows.UpdateDraft(ctx, id, "Probe", miniSpec("A brisk assistant.")); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	edited, err := flows.Summary(ctx, id)
	if err != nil {
		t.Fatalf("summary after edit: %v", err)
	}
	if !edited.HasUnpublishedChanges {
		t.Error("the persona changed and the flow reports nothing unpublished")
	}
	if edited.PublishedRevisionID == nil || *edited.PublishedRevisionID != *live.PublishedRevisionID {
		t.Error("editing the draft moved the published revision — a live number " +
			"changed what it says because somebody saved")
	}

	// Two publishes, and the history says which one is live.
	if err := flows.Publish(ctx, id, "second"); err != nil {
		t.Fatalf("republish: %v", err)
	}
	detail, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2", len(detail.Revisions))
	}
	if detail.Revisions[0].Note != "second" {
		t.Errorf("revisions[0].note = %q, want the newest first", detail.Revisions[0].Note)
	}
	if !detail.Revisions[0].IsPublished || detail.Revisions[1].IsPublished {
		t.Error("the wrong revision is marked live")
	}
}

// A draft that cannot publish is a spec nobody can use, and storing it only
// moves the failure to whoever presses Publish. The update path used to skip
// this check while create enforced it, so an invalid file could be written
// over a working flow through the CLI's update branch.
func TestAnUnpublishableDraftIsRefusedRatherThanStored(t *testing.T) {
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

	id, err := flows.Create(ctx, "probe", "Probe", miniSpec("A helpful assistant."))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	broken := []byte(`{"id":"probe","specVersion":"v2","initialNode":"missing",
	  "global":{"persona":""},"nodes":{"welcome":{"instruction":"Hi."}}}`)
	err = flows.UpdateDraft(ctx, id, "Probe", broken)
	var invalid *flow.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a flow.ValidationError", err)
	}
	// Every problem, not the first: an author fixing a spec should see the
	// whole report rather than discover it one save at a time.
	if len(invalid.Problems) < 2 {
		t.Errorf("problems = %v, want both the empty persona and the missing phase",
			invalid.Problems)
	}

	detail, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if _, err := flow.Load(detail.DraftSpec); err != nil {
		t.Errorf("the refused draft was stored anyway: %v", err)
	}
}

// Asking for a flow that is not there is not the same as asking for one with
// nothing published, and the API turns them into 404 and an empty page.
func TestAskingForAFlowThatIsNotThereSaysSo(t *testing.T) {
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
	absent := uuid.New()

	if _, err := flows.Summary(ctx, absent); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("summary error = %v, want ErrFlowNotFound", err)
	}
	if _, err := flows.Detail(ctx, absent); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("detail error = %v, want ErrFlowNotFound", err)
	}
	if err := flows.Publish(ctx, absent, ""); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("publish error = %v, want ErrFlowNotFound", err)
	}
	if err := flows.UpdateDraft(ctx, absent, "Probe", miniSpec("A helpful assistant.")); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("update error = %v, want ErrFlowNotFound", err)
	}
}
