// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// Exactly one number is the default outbound one, and claiming it takes it
// from whoever had it. Before this, a second claim hit
// uq_dids_default_outbound and came back as "that identifier is already in
// use" — a conflict report about the number, when nothing was wrong with the
// number.
//
// This needs a real server: the whole behaviour is a partial unique index and
// the transaction that satisfies it. Set AICC_TEST_DATABASE_URL to run it
// (see migrate_test.go); without it, it skips.
func TestClaimingTheDefaultOutboundNumberTakesItFromTheLastOne(t *testing.T) {
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

	cat := st.Catalog()
	outbound := func(number string) catalog.DID {
		return catalog.DID{
			ID: uuid.Must(uuid.NewV7()), Number: number, Language: "en",
			IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true,
		}
	}

	first, err := cat.CreateDID(ctx, outbound("95001"))
	if err != nil {
		t.Fatalf("create the first number: %v", err)
	}
	second, err := cat.CreateDID(ctx, outbound("95002"))
	if err != nil {
		t.Fatalf("create the second number: %v", err)
	}

	assertDefault := func(want uuid.UUID) {
		t.Helper()
		items, err := cat.ListDIDs(ctx)
		if err != nil {
			t.Fatalf("list numbers: %v", err)
		}
		var holders []string
		for _, d := range items {
			if d.IsDefaultOutbound {
				holders = append(holders, d.Number)
			}
			if d.ID == want && !d.IsDefaultOutbound {
				t.Errorf("%s does not hold the flag it just claimed", d.Number)
			}
		}
		if len(holders) != 1 {
			t.Errorf("numbers holding the default flag = %v, want exactly one", holders)
		}
	}

	assertDefault(second.ID)

	// And an update claims it back, which is the same rule on the other write.
	first.IsDefaultOutbound = true
	if _, err := cat.UpdateDID(ctx, first); err != nil {
		t.Fatalf("claim it back: %v", err)
	}
	assertDefault(first.ID)
}
