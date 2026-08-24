// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// The extension pool, against a real server. Every part of it is the database's
// behaviour: generate_series over the range, the unique constraint that a
// missing lock would trip, and an advisory lock that only exists between real
// transactions. A fake would agree with whatever the code did.

func poolStore(t *testing.T, maxConns int32) *CatalogStore {
	t.Helper()
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, maxConns)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st.Catalog()
}

func allocate(t *testing.T, cat *CatalogStore, low, high int) catalog.Extension {
	t.Helper()
	e, err := cat.AllocateExtension(context.Background(), catalog.Extension{
		ID: uuid.Must(uuid.NewV7()), Kind: catalog.KindAgent,
		Password: "phone-secret", IsEnabled: true,
	}, low, high)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	return e
}

// The number a departing agent gave back is handed out again. MAX+1 would
// answer 1003 here and leave 1001 dead for the life of the deployment — and
// with a 1000-number pool, a call centre that churns staff runs out of range
// while most of it is free.
//
// Reuse is safe only because no history is keyed by an extension: every
// historical row names the agent's uuid, which is never reused (F12).
func TestTheLowestFreeNumberIsReused_NotTheNextOneUp(t *testing.T) {
	cat := poolStore(t, 4)
	ctx := context.Background()

	first := allocate(t, cat, 1000, 1999)
	second := allocate(t, cat, 1000, 1999)
	third := allocate(t, cat, 1000, 1999)
	if first.Number != "1000" || second.Number != "1001" || third.Number != "1002" {
		t.Fatalf("allocated %s, %s, %s — want 1000, 1001, 1002",
			first.Number, second.Number, third.Number)
	}

	if err := cat.DeleteExtension(ctx, second.ID); err != nil {
		t.Fatalf("delete 1001: %v", err)
	}

	again := allocate(t, cat, 1000, 1999)
	if again.Number != "1001" {
		t.Errorf("allocated %s after 1001 was freed, want 1001 — the pool is "+
			"drifting upwards and the hole will never be filled", again.Number)
	}
}

// A queue's number and the bot endpoint live in the same table and outside the
// pool. The allocator must neither hand them out nor be pushed past them.
func TestNumbersOutsideThePoolAreLeftAlone(t *testing.T) {
	cat := poolStore(t, 4)
	ctx := context.Background()

	// A queue's number: in the same table, outside the pool.
	queueID := uuid.Must(uuid.NewV7())
	if _, err := cat.pool.Exec(ctx, `INSERT INTO queues (id, name, ext_number, display_name)
		VALUES ($1, 'support', '7002', 'Support')`, queueID); err != nil {
		t.Fatalf("seed the queue: %v", err)
	}
	outside := catalog.Extension{
		ID: uuid.Must(uuid.NewV7()), Number: "7002", Kind: catalog.KindQueue,
		QueueID:  &queueID,
		Password: "queue-secret", DisplayName: "Support queue", IsEnabled: true,
	}
	if _, err := cat.CreateExtension(ctx, outside); err != nil {
		t.Fatalf("create the out-of-range extension: %v", err)
	}

	got := allocate(t, cat, 1000, 1999)
	if got.Number != "1000" {
		t.Errorf("allocated %s, want 1000 — a number outside the range changed "+
			"what the pool handed out", got.Number)
	}
}

// Running out is not a collision: nothing the operator asked for was taken,
// the deployment has simply run out of numbers, and the answer is a wider
// range rather than a retry.
func TestAFullPoolSaysItIsFullRatherThanFailingOnTheConstraint(t *testing.T) {
	cat := poolStore(t, 4)

	allocate(t, cat, 1000, 1001)
	allocate(t, cat, 1000, 1001)

	_, err := cat.AllocateExtension(context.Background(), catalog.Extension{
		ID: uuid.Must(uuid.NewV7()), Kind: catalog.KindAgent, Password: "phone-secret",
	}, 1000, 1001)
	if !errors.Is(err, catalog.ErrPoolExhausted) {
		t.Errorf("error = %v, want ErrPoolExhausted", err)
	}
}

// The lock's whole job, and the only test that can see it.
//
// Without it every transaction reads the same lowest free number and all but
// one die on uq_extensions_number — so the assertion is that they all
// *succeed*, not merely that the numbers differ. Eight accounts created at
// once is not a contrived load: it is one import of a team.
func TestParallelAllocationsAllSucceedAndNoneCollide(t *testing.T) {
	const parallel = 8
	cat := poolStore(t, parallel+2)

	var wg sync.WaitGroup
	numbers := make([]string, parallel)
	errs := make([]error, parallel)
	start := make(chan struct{})

	for i := range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e, err := cat.AllocateExtension(context.Background(), catalog.Extension{
				ID: uuid.Must(uuid.NewV7()), Kind: catalog.KindAgent,
				Password: "phone-secret", IsEnabled: true,
			}, 1000, 1999)
			numbers[i], errs[i] = e.Number, err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("allocation %d failed: %v — parallel allocations are "+
				"racing for the same number instead of queueing", i, err)
		}
	}
	if t.Failed() {
		return
	}

	sort.Strings(numbers)
	for i := 1; i < len(numbers); i++ {
		if numbers[i] == numbers[i-1] {
			t.Fatalf("two accounts were given %s: %v", numbers[i], numbers)
		}
	}
	// Contiguous from the bottom of the range: nothing was skipped either.
	if numbers[0] != "1000" || numbers[parallel-1] != "1007" {
		t.Errorf("allocated %v, want 1000..1007", numbers)
	}
}
