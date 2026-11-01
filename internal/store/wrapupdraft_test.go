// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The draft is written at most once, only while the agent has not confirmed,
// and a suggested code is checked against the enabled vocabulary inside the
// same statement.
func TestAWrapUpDraftIsWrittenOnceAndNeverOverAConfirmedRecord(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	code := "FOLLOW_UP_REQUIRED"

	// No record yet: nothing to write to, and no error.
	if _, wrote, err := ledger.SaveWrapUpDraft(ctx, callID, agentID, &code, "n"); err != nil || wrote {
		t.Fatalf("draft with no record: wrote = %v, err = %v", wrote, err)
	}

	if err := ledger.OpenWrapUp(ctx, callID, agentID); err != nil {
		t.Fatal(err)
	}
	opened, err := ledger.GetWrapUp(ctx, callID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.DraftGeneratedAt != nil || opened.DraftNote != nil || opened.DraftDispositionCode != "" {
		t.Fatalf("an opened record already carries a draft: %+v", opened)
	}

	saved, wrote, err := ledger.SaveWrapUpDraft(ctx, callID, agentID, &code, "Needs a follow-up call.")
	if err != nil || !wrote {
		t.Fatalf("first draft: wrote = %v, err = %v", wrote, err)
	}
	if saved.DraftDispositionCode != "FOLLOW_UP_REQUIRED" || saved.DraftNote == nil || *saved.DraftNote != "Needs a follow-up call." ||
		saved.DraftGeneratedAt == nil {
		t.Errorf("saved = %+v", saved)
	}
	if saved.DispositionCode != opened.DispositionCode || saved.Note != "" || saved.IsConfirmed {
		t.Errorf("the values the agent files changed: %+v", saved)
	}

	// A second job for the same record writes nothing.
	other := "OTHER"
	if _, wrote, err := ledger.SaveWrapUpDraft(ctx, callID, agentID, &other, "second"); err != nil || wrote {
		t.Errorf("second draft: wrote = %v, err = %v", wrote, err)
	}

	// Confirming keeps the draft beside what the agent filed.
	note := "my own words"
	confirmed, err := ledger.ConfirmWrapUp(ctx, callID, agentID, &other, &note)
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed.IsConfirmed || confirmed.Note != note || confirmed.DraftDispositionCode != "FOLLOW_UP_REQUIRED" ||
		confirmed.DraftNote == nil || *confirmed.DraftNote != "Needs a follow-up call." {
		t.Errorf("confirmed = %+v, want the draft kept and the agent's values standing", confirmed)
	}
}

func TestADraftNeverLandsOnAConfirmedRecord(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := ledger.OpenWrapUp(ctx, callID, agentID); err != nil {
		t.Fatal(err)
	}
	code, note := "RESOLVED", "typed by the agent"
	if _, err := ledger.ConfirmWrapUp(ctx, callID, agentID, &code, &note); err != nil {
		t.Fatal(err)
	}
	late := "NO_ANSWER"
	if _, wrote, err := ledger.SaveWrapUpDraft(ctx, callID, agentID, &late, "late draft"); err != nil || wrote {
		t.Fatalf("wrote = %v, err = %v; a draft must not reach a confirmed record", wrote, err)
	}
	got, _ := ledger.GetWrapUp(ctx, callID, agentID)
	if got.DispositionCode != "RESOLVED" || got.Note != note || got.DraftGeneratedAt != nil || got.DraftNote != nil {
		t.Errorf("record = %+v", got)
	}
}

// A code disabled between the prompt and the write is stored as no suggestion,
// and the note is kept.
func TestADraftCodeDisabledSincePromptingIsDropped(t *testing.T) {
	ledger, _, pool := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := ledger.OpenWrapUp(ctx, callID, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE dispositions SET is_enabled = false WHERE code = 'FOLLOW_UP_REQUIRED'`); err != nil {
		t.Fatal(err)
	}
	code := "FOLLOW_UP_REQUIRED"
	saved, wrote, err := ledger.SaveWrapUpDraft(ctx, callID, agentID, &code, "kept note")
	if err != nil || !wrote {
		t.Fatalf("wrote = %v, err = %v", wrote, err)
	}
	if saved.DraftDispositionCode != "" || saved.DraftNote == nil || *saved.DraftNote != "kept note" || saved.DraftGeneratedAt == nil {
		t.Errorf("saved = %+v, want the note kept and no disposition", saved)
	}

	// A draft that suggests none is stored as one with a note and no code.
	call2 := uuid.Must(uuid.NewV7())
	if err := ledger.OpenWrapUp(ctx, call2, agentID); err != nil {
		t.Fatal(err)
	}
	saved, wrote, err = ledger.SaveWrapUpDraft(ctx, call2, agentID, nil, "no code")
	if err != nil || !wrote || saved.DraftDispositionCode != "" || saved.DraftGeneratedAt == nil {
		t.Errorf("no-code draft: %+v wrote = %v err = %v", saved, wrote, err)
	}
}

// The wire shape is the contract's: the draft fields are absent until a draft
// exists, and a draft with no suggested disposition omits that one.
func TestTheDraftFieldsAreAbsentUntilThereIsADraft(t *testing.T) {
	plain, _ := json.Marshal(WrapUp{})
	for _, k := range []string{"draftDispositionCode", "draftNote", "draftGeneratedAt"} {
		if strings.Contains(string(plain), k) {
			t.Errorf("a record with no draft carries %q: %s", k, plain)
		}
	}
	note, at := "n", time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	noCode, _ := json.Marshal(WrapUp{DraftNote: &note, DraftGeneratedAt: &at})
	if !strings.Contains(string(noCode), `"draftNote":"n"`) || !strings.Contains(string(noCode), `"draftGeneratedAt":"2026-10-25T01:02:03Z"`) ||
		strings.Contains(string(noCode), "draftDispositionCode") {
		t.Errorf("draft without a code: %s", noCode)
	}
}
