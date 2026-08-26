// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// liveStore is a migrated database with the webhook tables in it.
func liveStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
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
	return st, ctx
}

func aCDR(callID uuid.UUID, endedAt time.Time, talkSec int) CDR {
	return CDR{
		CallID:    callID,
		StartedAt: endedAt.Add(-time.Duration(talkSec) * time.Second),
		EndedAt:   endedAt,
		CallType:  "OUTBOUND",
		Status:    "ANSWERED",
		DID:       "95001",
		TalkSec:   talkSec,
	}
}

// The enqueue rides in the CDR's own transaction, and this is what proves the
// two are joined at all: writing a call with a subscription in place must leave
// a delivery behind it.
func TestWritingACDRQueuesItForEverySubscriptionThatWantsIt(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()

	wants, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
		Filter: WebhookFilter{CallType: []string{"OUTBOUND"}},
	})
	if err != nil {
		t.Fatalf("create the interested subscription: %v", err)
	}
	// One that wants a different kind of call, and one that is switched off.
	if _, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "inbound-only", URL: "https://bi.example.com/cdr", IsEnabled: true,
		Filter: WebhookFilter{CallType: []string{"INBOUND"}},
	}); err != nil {
		t.Fatalf("create the uninterested subscription: %v", err)
	}
	off, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "paused", URL: "https://staging.example.com/cdr", IsEnabled: false,
	})
	if err != nil {
		t.Fatalf("create the disabled subscription: %v", err)
	}

	callID := uuid.New()
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, time.Now().UTC(), 40)); err != nil {
		t.Fatalf("write the cdr: %v", err)
	}

	got, err := hooks.Deliveries(ctx, wants.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want one for the subscription that asked for OUTBOUND", len(got))
	}
	if got[0].CallID != callID || got[0].Revision != 1 || got[0].Status != DeliveryPending {
		t.Errorf("delivery = %+v, want revision 1 of this call, pending", got[0])
	}

	// A subscription that is switched off is not merely undelivered — nothing
	// is queued for it at all, so turning it back on does not release a
	// backlog of calls it was never meant to see.
	if paused, err := hooks.Deliveries(ctx, off.SubscriptionID, 10); err != nil {
		t.Fatalf("read the disabled subscription's deliveries: %v", err)
	} else if len(paused) != 0 {
		t.Errorf("a disabled subscription accumulated %d deliveries", len(paused))
	}
}

// A CDR that says less than the stored one changes nothing, and must queue
// nothing: the customer already holds the fuller story, and a second delivery
// would replace it with a worse one.
func TestAWriteThatChangesNothingQueuesNothing(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callID := uuid.New()
	ended := time.Now().UTC()
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, ended, 40)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Settle the first delivery, so a second enqueue would be visible as a
	// second row rather than as a replaced payload.
	first, _ := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if len(first) != 1 {
		t.Fatalf("deliveries after the first write = %d, want 1", len(first))
	}
	if err := hooks.MarkDelivered(ctx, first[0].DeliveryID, 200); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	// An earlier ending: the upsert declines, so nothing changed.
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, ended.Add(-time.Minute), 5)); err != nil {
		t.Fatalf("stale write: %v", err)
	}

	after, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("deliveries = %d, want 1 — a write that changed nothing queued a duplicate "+
			"of a CDR the customer already holds", len(after))
	}
}

// The case the revision key exists for. A restart writes the call off short,
// that row is delivered, and the fuller one arrives afterwards: the correction
// goes out as revision 2 rather than being swallowed.
func TestAFullerCDRAfterDeliverySendsACorrection(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callID := uuid.New()
	short := time.Now().UTC().Add(-4 * time.Minute)
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}
	first, _ := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if len(first) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(first))
	}
	if err := hooks.MarkDelivered(ctx, first[0].DeliveryID, 200); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	// The real ending, four minutes later.
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short.Add(4*time.Minute), 270)); err != nil {
		t.Fatalf("fuller write: %v", err)
	}

	after, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("deliveries = %d, want 2 — the customer holds a CDR that is now wrong and "+
			"must be told", len(after))
	}
	// Newest first.
	if after[0].Revision != 2 || after[0].Status != DeliveryPending {
		t.Errorf("correction = %+v, want revision 2 pending", after[0])
	}
	if after[1].Revision != 1 || after[1].Status != DeliveryDelivered {
		t.Errorf("the superseded row = %+v, want revision 1 still recorded as delivered — "+
			"the deliveries screen has to be able to say what was sent", after[1])
	}
}

// The ordinary case, and the one that keeps the wrong story from ever leaving:
// the fuller CDR arrives while the first delivery is still queued, so its
// payload is replaced and the customer hears only the truth.
func TestAFullerCDRBeforeDeliveryReplacesWhatIsQueued(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callID := uuid.New()
	short := time.Now().UTC().Add(-4 * time.Minute)
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short.Add(4*time.Minute), 270)); err != nil {
		t.Fatalf("fuller write: %v", err)
	}

	got, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want 1 — nothing had gone out yet, so there is nothing "+
			"to correct", len(got))
	}
	if got[0].Revision != 1 {
		t.Errorf("revision = %d, want 1", got[0].Revision)
	}
	// And it carries the fuller story, not the one the restart wrote.
	if !containsTalkSec(got[0].Payload, 270) {
		t.Errorf("the queued payload is the short row: %s", got[0].Payload)
	}
}

func containsTalkSec(payload []byte, want int) bool {
	var cdr CDR
	if err := jsonUnmarshal(payload, &cdr); err != nil {
		return false
	}
	return cdr.TalkSec == want
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
