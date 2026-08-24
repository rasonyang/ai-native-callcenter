// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// Deleting a row that is not there is not an error to PostgreSQL, so every
// catalogue delete reported success and the 404 the contract declares was
// unreachable code (C34). An operator who mistyped an id was told the
// extension was gone.
//
// This needs a real server: the whole defect is what `DELETE … WHERE id = $1`
// does when it matches nothing, which no fake reproduces. Set
// AICC_TEST_DATABASE_URL to run it (see migrate_test.go); without it, it skips.
func TestDeletingWhatIsNotThereSaysSoRatherThanReportingSuccess(t *testing.T) {
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
	absent := uuid.New()

	for _, tc := range []struct {
		name string
		del  func() error
	}{
		{"extension", func() error { return cat.DeleteExtension(ctx, absent) }},
		{"queue", func() error { return cat.DeleteQueue(ctx, absent) }},
		{"did", func() error { return cat.DeleteDID(ctx, absent) }},
		{"queue staffing", func() error { return cat.RemoveQueueAgent(ctx, absent, absent) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.del(); !errors.Is(err, catalog.ErrNotFound) {
				t.Errorf("error = %v, want catalog.ErrNotFound — nothing was removed", err)
			}
		})
	}

	t.Run("agent", func(t *testing.T) {
		// The agents package speaks pgx.ErrNoRows where the catalogue speaks
		// ErrNotFound; both reach 404, each through its own handler.
		if err := st.Agents().DeleteAgent(ctx, absent); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("error = %v, want pgx.ErrNoRows — no agent was removed", err)
		}
	})
}
