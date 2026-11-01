// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// A CDR names the campaign whose attempt carries its call, and the webhook
// filter, the listing filter and a fuller revision all agree with that.
func TestACDRNamesItsCampaignAndTheFiltersFollow(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	hooks := e.st.Webhooks()

	a, contactsA := e.campaign(t, "A", "6", 1, nil)
	b, contactsB := e.campaign(t, "B", "7", 1, nil)
	attA := e.begin(t, contactsA[0], ny(t, "2026-06-01 10:00"))
	attB := e.begin(t, contactsB[0], ny(t, "2026-06-01 10:00"))

	hookFor := func(name string, ids ...uuid.UUID) uuid.UUID {
		t.Helper()
		sub, err := hooks.Create(ctx, WebhookSubscriptionWrite{
			Name: name, URL: "https://crm.example.com/" + name, IsEnabled: true,
			Filter: WebhookFilter{CampaignID: ids},
		})
		if err != nil {
			t.Fatal(err)
		}
		return sub.SubscriptionID
	}
	onA, onB, all := hookFor("a", a.ID), hookFor("b", b.ID), hookFor("all")

	start, end := ny(t, "2026-06-01 10:00"), ny(t, "2026-06-01 10:00").Add(30*time.Second)
	if err := e.ledger.InsertCDR(ctx, cdrOf(attA.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, start, end)); err != nil {
		t.Fatal(err)
	}
	// A call that is nobody's campaign's.
	plain := uuid.Must(uuid.NewV7())
	if err := e.ledger.InsertCDR(ctx, cdrOf(plain, "NORMAL_CLEARING", CDRStatusAnswered, true, start, end)); err != nil {
		t.Fatal(err)
	}

	count := func(sub uuid.UUID) int {
		t.Helper()
		d, err := hooks.Deliveries(ctx, sub, 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(d)
	}
	// A's hook: the campaign's call only. B's: nothing. Unfiltered: both.
	if got := count(onA); got != 1 {
		t.Errorf("hook on A holds %d deliveries, want 1", got)
	}
	if got := count(onB); got != 0 {
		t.Errorf("hook on B holds %d deliveries, want 0", got)
	}
	if got := count(all); got != 2 {
		t.Errorf("unfiltered hook holds %d deliveries, want 2", got)
	}

	got, err := e.ledger.GetCDR(ctx, attA.CallID)
	if err != nil || got.CampaignID == nil || *got.CampaignID != a.ID {
		t.Fatalf("CDR of A = %+v, %v; want campaignId %s", got.CampaignID, err, a.ID)
	}
	if got, _ := e.ledger.GetCDR(ctx, plain); got.CampaignID != nil {
		t.Errorf("an ordinary call carries campaign %s", *got.CampaignID)
	}

	// A fuller revision of B's call keeps the campaign.
	if err := e.ledger.InsertCDR(ctx, cdrOf(attB.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, start, end)); err != nil {
		t.Fatal(err)
	}
	if err := e.ledger.InsertCDR(ctx, cdrOf(attB.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, end.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ledger.GetCDR(ctx, attB.CallID); got.CampaignID == nil || *got.CampaignID != b.ID || got.Status != CDRStatusAnswered {
		t.Errorf("revised CDR of B = %+v", got)
	}
	if got := count(onB); got != 1 {
		t.Errorf("hook on B holds %d deliveries after B's call, want 1", got)
	}

	items, total, err := e.ledger.ListCDRs(ctx, CDRFilter{CampaignID: &a.ID})
	if err != nil || total != 1 || len(items) != 1 || items[0].CallID != attA.CallID {
		t.Errorf("listing for A = %d items, total %d, %v", len(items), total, err)
	}
	if _, total, _ := e.ledger.ListCDRs(ctx, CDRFilter{}); total != 3 {
		t.Errorf("unfiltered total = %d, want 3", total)
	}
}

// 00039 adds the column to a ledger that already holds calls: they stay
// campaign-less, and the migration reverses.
func TestMigrationsLeaveExistingCDRsWithoutACampaign(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()
	if err := goose.UpToContext(ctx, db, "migrations", 38); err != nil {
		t.Fatalf("migrating to 38: %v", err)
	}
	callID := uuid.Must(uuid.NewV7())
	if _, err := db.ExecContext(ctx, `INSERT INTO cdrs (call_id, started_at, ended_at, call_type, from_number, to_number, status)
		VALUES ($1, now() - interval '1 minute', now(), 'OUTBOUND', '95001', '+8613000000000', 'ANSWERED')`, callID); err != nil {
		t.Fatalf("seed a CDR: %v", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	var isNull bool
	if err := db.QueryRowContext(ctx, `SELECT campaign_id IS NULL FROM cdrs WHERE call_id = $1`, callID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("existing CDR: null=%v err=%v", isNull, err)
	}
	if err := goose.DownToContext(ctx, db, "migrations", 38); err != nil {
		t.Fatalf("rolling 00039 back: %v", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating up again: %v", err)
	}
}
