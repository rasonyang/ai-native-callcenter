// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00042 gives a contact its latest finished attempt and that attempt's call,
// backfilled on a database that already holds campaigns: a contact never
// dialled gets neither, a retried contact gets its later attempt (by
// started_at, not by insertion order), and an attempt still in flight is not
// a finished one. The Down drops both cleanly with values in place.
func TestMigrationBackfillsAContactsLastCall(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 41); err != nil {
		t.Fatalf("migrating to 41 failed: %v", err)
	}

	didID, campaignID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	never, once, twice, inFlight := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95301', 'en', false, true)`, didID)
	exec(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'old', $2, 'UTC', '{}', 1, 30, '{}')`, campaignID, didID)
	for i, c := range []uuid.UUID{never, once, twice, inFlight} {
		exec(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw)
		      VALUES ($1, $2, $3, $3)`, c, campaignID, "+861380013800"+string(rune('0'+i)))
	}

	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	type attempt struct {
		id, contact, call uuid.UUID
		started           time.Time
		isFinished        bool
		lead              *string
	}
	high, low := "HIGH", "LOW"
	onceAtt := attempt{uuid.Must(uuid.NewV7()), once, uuid.Must(uuid.NewV7()), t0, true, &high}
	// Inserted later-first, so a backfill that ordered by insertion would pick
	// the wrong one.
	twiceSecond := attempt{uuid.Must(uuid.NewV7()), twice, uuid.Must(uuid.NewV7()), t0.Add(26 * time.Hour), true, &low}
	twiceFirst := attempt{uuid.Must(uuid.NewV7()), twice, uuid.Must(uuid.NewV7()), t0.Add(time.Hour), true, &high}
	flightFirst := attempt{uuid.Must(uuid.NewV7()), inFlight, uuid.Must(uuid.NewV7()), t0, true, nil}
	flightNow := attempt{uuid.Must(uuid.NewV7()), inFlight, uuid.Must(uuid.NewV7()), t0.Add(48 * time.Hour), false, nil}
	for _, a := range []attempt{onceAtt, twiceSecond, twiceFirst, flightFirst, flightNow} {
		var number string
		if err := db.QueryRowContext(ctx, `SELECT number_e164 FROM campaign_contacts WHERE id = $1`, a.contact).Scan(&number); err != nil {
			t.Fatal(err)
		}
		if a.isFinished {
			exec(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
			          state, outcome, is_touched, lead_intent, started_at, finished_at)
			      VALUES ($1, $2, $3, $4, $5, $6, 'FINISHED', 'ANSWERED', true, $7, $8, $8)`,
				a.id, campaignID, a.contact, number, a.call, uuid.Must(uuid.NewV7()), a.lead, a.started)
		} else {
			exec(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id, started_at)
			      VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				a.id, campaignID, a.contact, number, a.call, uuid.Must(uuid.NewV7()), a.started)
		}
	}

	if err := goose.UpToContext(ctx, db, "migrations", 42); err != nil {
		t.Fatalf("migrating a database that already holds attempts failed: %v", err)
	}

	read := func(contact uuid.UUID) (attemptID, callID *uuid.UUID) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT last_attempt_id, last_call_id FROM campaign_contacts WHERE id = $1`, contact).
			Scan(&attemptID, &callID); err != nil {
			t.Fatal(err)
		}
		return attemptID, callID
	}
	cases := []struct {
		name    string
		contact uuid.UUID
		want    *attempt
	}{
		{"a contact never dialled", never, nil},
		{"a contact dialled once", once, &onceAtt},
		{"a contact dialled twice", twice, &twiceSecond},
		{"a contact with an attempt in flight", inFlight, &flightFirst},
	}
	for _, c := range cases {
		gotAttempt, gotCall := read(c.contact)
		if c.want == nil {
			if gotAttempt != nil || gotCall != nil {
				t.Errorf("%s: got (%v, %v), want neither", c.name, gotAttempt, gotCall)
			}
			continue
		}
		if gotAttempt == nil || gotCall == nil || *gotAttempt != c.want.id || *gotCall != c.want.call {
			t.Errorf("%s: got (%v, %v), want attempt %s with call %s",
				c.name, gotAttempt, gotCall, c.want.id, c.want.call)
		}
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE campaign_contacts SET last_call_id = NULL WHERE id = $1`, once); err == nil {
		t.Error("the database accepted a last attempt without its call")
	}

	if err := goose.DownToContext(ctx, db, "migrations", 41); err != nil {
		t.Fatalf("rolling 00042 back with values in place failed: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 42); err != nil {
		t.Fatalf("migrating forward again failed: %v", err)
	}
	if gotAttempt, _ := read(twice); gotAttempt == nil || *gotAttempt != twiceSecond.id {
		t.Errorf("after re-applying, the retried contact names %v, want %s", gotAttempt, twiceSecond.id)
	}
}
