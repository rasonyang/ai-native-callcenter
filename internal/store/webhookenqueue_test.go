// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
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

	callID := uuid.Must(uuid.NewV7())
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

	callID := uuid.Must(uuid.NewV7())
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
	if err := hooks.MarkDelivered(ctx, first[0].DeliveryID, 0, 200); err != nil {
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

	callID := uuid.Must(uuid.NewV7())
	short := time.Now().UTC().Add(-4 * time.Minute)
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}
	first, _ := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if len(first) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(first))
	}
	if err := hooks.MarkDelivered(ctx, first[0].DeliveryID, 0, 200); err != nil {
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

	callID := uuid.Must(uuid.NewV7())
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

// A fuller CDR written while the earlier delivery is in flight. The claim
// leaves the row PENDING, so replacing its payload would let the 2xx for the
// old body mark the row delivered and the correction would never be sent
// (#445). The in-flight row keeps its body; the correction is a new delivery.
func TestAFullerCDRWhileTheEarlierDeliveryIsInFlightIsSentAsARevision(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callID := uuid.Must(uuid.NewV7())
	short := time.Now().UTC().Add(-4 * time.Minute)
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}
	claimed, err := hooks.ClaimDue(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d rows, err %v, want the one delivery", len(claimed), err)
	}
	inFlight := claimed[0]

	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short.Add(4*time.Minute), 270)); err != nil {
		t.Fatalf("fuller write: %v", err)
	}
	// The receiver answers 2xx for the body it was sent.
	if err := hooks.MarkDelivered(ctx, inFlight.DeliveryID, inFlight.AttemptCount, 204); err != nil {
		t.Fatalf("mark the in-flight attempt delivered: %v", err)
	}

	got, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("deliveries = %d, want 2 — the correction must not ride on the in-flight row", len(got))
	}
	correction, original := got[0], got[1]
	if correction.Revision != 2 || correction.Status != DeliveryPending ||
		correction.DeliveryID == original.DeliveryID || !containsTalkSec(correction.Payload, 270) {
		t.Errorf("correction = %+v (payload %s), want a new pending delivery, revision 2, the fuller story",
			correction, correction.Payload)
	}
	if original.DeliveryID != inFlight.DeliveryID || original.Revision != 1 ||
		original.Status != DeliveryDelivered || !containsTalkSec(original.Payload, 30) {
		t.Errorf("original = %+v (payload %s), want revision 1 delivered with the body that was sent",
			original, original.Payload)
	}
}

// A delivery waiting to retry may already have reached the receiver without
// being acknowledged. Replacing it would reuse its deliveryId and revision for
// a different body, which a receiver deduping on X-AICC-Webhook-Id discards.
func TestAFullerCDRWhileTheEarlierDeliveryWaitsToRetryIsSentAsARevision(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callID := uuid.Must(uuid.NewV7())
	short := time.Now().UTC().Add(-4 * time.Minute)
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short, 30)); err != nil {
		t.Fatalf("short write: %v", err)
	}
	claimed, err := hooks.ClaimDue(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d rows, err %v, want the one delivery", len(claimed), err)
	}
	if err := hooks.Reschedule(ctx, claimed[0].DeliveryID, claimed[0].AttemptCount,
		time.Now().Add(time.Minute), 503, "HTTP 503"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, short.Add(4*time.Minute), 270)); err != nil {
		t.Fatalf("fuller write: %v", err)
	}

	got, err := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("deliveries = %d, want 2 — a row that was attempted is never replaced in place", len(got))
	}
	if got[0].Revision != 2 || got[0].DeliveryID == claimed[0].DeliveryID || !containsTalkSec(got[0].Payload, 270) {
		t.Errorf("correction = %+v (payload %s), want a new delivery, revision 2, the fuller story",
			got[0], got[0].Payload)
	}
	if got[1].DeliveryID != claimed[0].DeliveryID || got[1].Status != DeliveryPending ||
		got[1].AttemptCount != 1 || !containsTalkSec(got[1].Payload, 30) {
		t.Errorf("earlier = %+v (payload %s), want it untouched and still on its own retry schedule",
			got[1], got[1].Payload)
	}
}

// A settle names the attempt it settles. One for an attempt that is no longer
// the delivery's current one changes nothing and says so.
func TestASettleForASupersededAttemptChangesNothing(t *testing.T) {
	st, ctx := liveStore(t)
	hooks := st.Webhooks()
	sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/cdr", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	callID := uuid.Must(uuid.NewV7())
	if err := st.Ledger().InsertCDR(ctx, aCDR(callID, time.Now().UTC(), 30)); err != nil {
		t.Fatalf("write: %v", err)
	}
	claimed, err := hooks.ClaimDue(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d rows, err %v", len(claimed), err)
	}
	id, attempt := claimed[0].DeliveryID, claimed[0].AttemptCount

	stale := attempt + 1
	if err := hooks.MarkDelivered(ctx, id, stale, 200); !errors.Is(err, ErrWebhookAttemptSuperseded) {
		t.Errorf("MarkDelivered with a stale attempt = %v, want ErrWebhookAttemptSuperseded", err)
	}
	if err := hooks.Reschedule(ctx, id, stale, time.Now(), 500, "x"); !errors.Is(err, ErrWebhookAttemptSuperseded) {
		t.Errorf("Reschedule with a stale attempt = %v, want ErrWebhookAttemptSuperseded", err)
	}
	if err := hooks.MarkFailed(ctx, id, stale, 500, "x"); !errors.Is(err, ErrWebhookAttemptSuperseded) {
		t.Errorf("MarkFailed with a stale attempt = %v, want ErrWebhookAttemptSuperseded", err)
	}
	got, _ := hooks.Deliveries(ctx, sub.SubscriptionID, 10)
	if len(got) != 1 || got[0].Status != DeliveryPending || got[0].LastStatusCode != nil || got[0].LastError != "" {
		t.Fatalf("delivery = %+v, want it untouched", got)
	}

	// The right attempt settles once; a second settle of it is superseded too.
	if err := hooks.MarkDelivered(ctx, id, attempt, 200); err != nil {
		t.Fatalf("settle the claimed attempt: %v", err)
	}
	if err := hooks.MarkFailed(ctx, id, attempt, 500, "late"); !errors.Is(err, ErrWebhookAttemptSuperseded) {
		t.Errorf("settling a delivered row again = %v, want ErrWebhookAttemptSuperseded", err)
	}
}
