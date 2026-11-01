// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Every claim the platform can record, the column takes and gives back; a word
// outside the vocabulary it refuses. The CHECK and UnbackedClaims are the same
// list written twice, and this is what keeps them so.
func TestTheUnbackedClaimsColumnTakesTheWholeVocabulary(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()

	callID := uuid.Must(uuid.NewV7())
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered, UnbackedClaims: UnbackedClaims,
	}); err != nil {
		t.Fatalf("InsertCDR() error = %v", err)
	}
	cdr, err := ledger.GetCDR(ctx, callID)
	if err != nil {
		t.Fatalf("GetCDR() error = %v", err)
	}
	if !slices.Equal(cdr.UnbackedClaims, UnbackedClaims) {
		t.Errorf("read back %v, want %v", cdr.UnbackedClaims, UnbackedClaims)
	}

	plain := uuid.Must(uuid.NewV7())
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: plain, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered,
	}); err != nil {
		t.Fatalf("InsertCDR() with no claims error = %v", err)
	}
	if cdr, err := ledger.GetCDR(ctx, plain); err != nil || len(cdr.UnbackedClaims) != 0 {
		t.Errorf("a row with no claims read back %v (err %v)", cdr.UnbackedClaims, err)
	}

	if err := ledger.InsertCDR(ctx, CDR{
		CallID: uuid.Must(uuid.NewV7()), StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered, UnbackedClaims: []string{"PROMISE"},
	}); err == nil {
		t.Error("the column took a claim outside the vocabulary")
	}
}
