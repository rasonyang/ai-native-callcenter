// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// The agent workspace against a real server: after-call work, the vocabulary
// it files under, and the contact book. These need PostgreSQL for the same
// reason the migration tests do — a UNIQUE, a CHECK and an upsert conflict
// target are only true if the server says so.

// workspaceStore migrates a scratch database and returns stores on it, plus
// the pool, so a test can also speak SQL the stores deliberately do not.
func workspaceStore(t *testing.T) (*LedgerStore, *ContactStore, *pgxpool.Pool) {
	t.Helper()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(context.Background(), db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := queries.New(pool)
	return &LedgerStore{q: q}, &ContactStore{q: q}, pool
}

// A deployment that cannot name a single disposition cannot complete a
// wrap-up, and the disposition is required — so the vocabulary ships with the
// schema rather than waiting for somebody to configure one.
func TestTheWrapUpVocabularyShipsWithTheSchema(t *testing.T) {
	ledger, _, _ := workspaceStore(t)

	items, err := ledger.ListDispositions(context.Background())
	if err != nil {
		t.Fatalf("ListDispositions() error = %v", err)
	}
	var labels []string
	for _, d := range items {
		if d.Code == "" || d.Label == "" {
			t.Errorf("disposition %+v is missing a code or a label", d)
		}
		labels = append(labels, d.Label)
	}
	// The four the directive names, in the order it names them: an agent
	// reaches the word they want without navigating to it.
	want := []string{"Resolved", "Follow-up Required", "No Answer", "Other"}
	if !slices.Equal(labels, want) {
		t.Errorf("vocabulary = %v, want %v", labels, want)
	}
}

// Filing captures the labels, so renaming a category later cannot rewrite what
// an agent said about a call last month.
func TestAWrapUpKeepsTheWordsItWasFiledUnder(t *testing.T) {
	ledger, _, pool := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.New(), uuid.New()

	filed, err := confirm(t, ledger, callID, agentID, "RESOLVED", "replaced the router")
	if err != nil {
		t.Fatalf("FileWrapUp() error = %v", err)
	}
	if filed.DispositionLabel != "Resolved" {
		t.Errorf("filed = %+v, want the vocabulary's label captured with it", filed)
	}

	// The vocabulary is edited; the record is history and does not move.
	if _, err := pool.Exec(ctx,
		`UPDATE dispositions SET label = 'Sorted' WHERE code = 'RESOLVED'`); err != nil {
		t.Fatalf("rename the disposition: %v", err)
	}
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered, PrimaryAgentID: &agentID,
		AgentIDs: []uuid.UUID{agentID},
	}); err != nil {
		t.Fatalf("InsertCDR() error = %v", err)
	}

	cdr, err := ledger.GetCDR(ctx, callID)
	if err != nil {
		t.Fatalf("GetCDR() error = %v", err)
	}
	if cdr.WrapUp == nil {
		t.Fatal("the call carries no wrap-up")
	}
	if cdr.WrapUp.DispositionLabel != filed.DispositionLabel {
		t.Errorf("label = %q, want the one filed (%q): history does not change "+
			"meaning when somebody edits the vocabulary",
			cdr.WrapUp.DispositionLabel, filed.DispositionLabel)
	}
	if cdr.WrapUp.Note != "replaced the router" {
		t.Errorf("note = %q, want what the agent typed", cdr.WrapUp.Note)
	}
}

// The window closes before the call is retired often enough that the ledger
// row does not exist yet when the agent files. The wrap-up must survive that
// order, which is why it is its own row rather than a column on the CDR.
func TestAWrapUpFiledBeforeTheCallEndsStillLandsOnIt(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.New(), uuid.New()

	if _, err := confirm(t, ledger, callID, agentID, "FOLLOW_UP_REQUIRED", ""); err != nil {
		t.Fatalf("FileWrapUp() before the CDR error = %v", err)
	}
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered, PrimaryAgentID: &agentID,
		AgentIDs: []uuid.UUID{agentID},
	}); err != nil {
		t.Fatalf("InsertCDR() error = %v", err)
	}

	cdr, err := ledger.GetCDR(ctx, callID)
	if err != nil {
		t.Fatalf("GetCDR() error = %v", err)
	}
	if cdr.WrapUp == nil || cdr.WrapUp.DispositionCode != "FOLLOW_UP_REQUIRED" {
		t.Errorf("wrapUp = %+v, want the filing that preceded the row", cdr.WrapUp)
	}
}

// Filing twice is one agent changing their mind, not two records.
func TestFilingTwiceReplaces(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.New(), uuid.New()

	if _, err := confirm(t, ledger, callID, agentID, "OTHER", "first thought"); err != nil {
		t.Fatal(err)
	}
	second, err := confirm(t, ledger, callID, agentID, "NO_ANSWER", "on reflection")
	if err != nil {
		t.Fatal(err)
	}
	if second.DispositionCode != "NO_ANSWER" || second.Note != "on reflection" {
		t.Errorf("second filing = %+v, want the later word to stand", second)
	}

	rows, err := ledger.q.ListWrapUpsForCalls(ctx, []uuid.UUID{callID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("%d wrap-ups for one agent on one call, want 1", len(rows))
	}
}

// A code the vocabulary does not have is refused, so a disposition report
// cannot be a list of typos.
func TestAnUnknownDispositionIsRefused(t *testing.T) {
	ledger, _, _ := workspaceStore(t)

	_, err := confirm(t, ledger, uuid.New(), uuid.New(), "NOT_A_CODE", "")
	if !errors.Is(err, ErrUnknownDisposition) {
		t.Errorf("ConfirmWrapUp() error = %v, want ErrUnknownDisposition", err)
	}
}

// Two agents on one call — a transfer — each file their own, and each sees
// their own on their own listing.
func TestEachAgentSeesTheirOwnFilingOnTheirOwnCalls(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID := uuid.New()
	first, second := uuid.New(), uuid.New()

	if _, err := confirm(t, ledger, callID, first, "FOLLOW_UP_REQUIRED", "handed to L2"); err != nil {
		t.Fatal(err)
	}
	if _, err := confirm(t, ledger, callID, second, "RESOLVED", "sorted it"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		CallType: "INBOUND", Status: CDRStatusAnswered,
		PrimaryAgentID: &second, AgentIDs: []uuid.UUID{first, second},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		agentID uuid.UUID
		want    string
	}{
		{"the first agent", first, "FOLLOW_UP_REQUIRED"},
		{"the agent who finished it", second, "RESOLVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, _, err := ledger.ListCDRs(ctx, CDRFilter{AgentID: &tc.agentID})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("%d calls, want 1", len(items))
			}
			if items[0].WrapUp == nil || items[0].WrapUp.DispositionCode != tc.want {
				t.Errorf("wrapUp = %+v, want their own filing (%s)", items[0].WrapUp, tc.want)
			}
		})
	}

	// Supervision reads the call, not an agent, and gets the primary agent's.
	cdr, err := ledger.GetCDR(ctx, callID)
	if err != nil {
		t.Fatal(err)
	}
	if cdr.WrapUp == nil || cdr.WrapUp.DispositionCode != "RESOLVED" {
		t.Errorf("wrapUp = %+v, want the primary agent's filing", cdr.WrapUp)
	}
}

//
// Contacts.
//

func TestAContactIsFoundByItsNumberAndKeepsItUnique(t *testing.T) {
	_, contacts, _ := workspaceStore(t)
	ctx := context.Background()
	name := "Zhang Wei"

	created, err := contacts.Create(ctx, ContactWrite{
		PhoneNumber: "+8613800138000", Name: &name,
		Tags: &[]string{"VIP", ""},
	}, nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if len(created.Tags) != 1 || created.Tags[0] != "VIP" {
		t.Errorf("tags = %v, want the blank one dropped", created.Tags)
	}

	// The number is how a caller is identified, so two records for one number
	// would make the cockpit's lookup a coin toss.
	if _, err := contacts.Create(ctx, ContactWrite{PhoneNumber: "+8613800138000"}, nil); !errors.Is(err, ErrContactExists) {
		t.Errorf("second Create() error = %v, want ErrContactExists", err)
	}

	found, total, err := contacts.List(ctx, ContactFilter{PhoneNumber: "+8613800138000"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(found) != 1 || found[0].ID != created.ID {
		t.Fatalf("lookup by number found %d of %d, want the one contact", len(found), total)
	}
}

func TestSearchingContactsMatchesNumberNameAndCompany(t *testing.T) {
	_, contacts, _ := workspaceStore(t)
	ctx := context.Background()

	name, company := "Emily Carter", "NovaNet"
	if _, err := contacts.Create(ctx, ContactWrite{
		PhoneNumber: "+14155238001", Name: &name, Company: &company,
	}, nil); err != nil {
		t.Fatal(err)
	}
	other := "Li Na"
	if _, err := contacts.Create(ctx, ContactWrite{PhoneNumber: "+8613700990011", Name: &other}, nil); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"emily", "novanet", "4155"} {
		items, _, err := contacts.List(ctx, ContactFilter{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Name != name {
			t.Errorf("search %q found %d, want just Emily", q, len(items))
		}
	}
}

// An update leaves alone what the caller did not send: a screen editing the
// note must not blank the company.
func TestUpdatingAContactLeavesUnsentFieldsAlone(t *testing.T) {
	_, contacts, _ := workspaceStore(t)
	ctx := context.Background()

	name, company := "Sun Qiang", "Acme"
	created, err := contacts.Create(ctx, ContactWrite{
		PhoneNumber: "+85251234222", Name: &name, Company: &company,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	note := "prefers callbacks after 16:00"
	updated, err := contacts.Update(ctx, created.ID,
		ContactWrite{PhoneNumber: created.PhoneNumber, Notes: &note}, nil)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Company != company || updated.Name != name {
		t.Errorf("update blanked what it was not given: %+v", updated)
	}
	if updated.Notes != note {
		t.Errorf("notes = %q, want the new one", updated.Notes)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) && !updated.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("updatedAt went backwards: %v then %v", created.UpdatedAt, updated.UpdatedAt)
	}
}

// The contact card's "last contact" line comes from the ledger, on either
// side of the call: an outbound call to this number counts as much as one
// from it.
func TestAContactsLastCallComesFromTheLedger(t *testing.T) {
	ledger, contacts, _ := workspaceStore(t)
	ctx := context.Background()

	created, err := contacts.Create(ctx, ContactWrite{PhoneNumber: "+8615012340199"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := contacts.List(ctx, ContactFilter{PhoneNumber: created.PhoneNumber})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].LastCallAt != nil {
		t.Errorf("lastCallAt = %v before any call, want none", items[0].LastCallAt)
	}

	older := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	newer := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: uuid.New(), StartedAt: older, EndedAt: older.Add(time.Minute),
		CallType: "INBOUND", Status: CDRStatusAnswered, FromNumber: created.PhoneNumber,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: uuid.New(), StartedAt: newer, EndedAt: newer.Add(time.Minute),
		CallType: "OUTBOUND", Status: CDRStatusAnswered, ToNumber: created.PhoneNumber,
	}); err != nil {
		t.Fatal(err)
	}

	items, _, err = contacts.List(ctx, ContactFilter{PhoneNumber: created.PhoneNumber})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].LastCallAt == nil {
		t.Fatal("lastCallAt is empty although the number appears on two calls")
	}
	if !items[0].LastCallAt.Equal(newer) {
		t.Errorf("lastCallAt = %v, want the most recent call (%v), either side of it",
			items[0].LastCallAt, newer)
	}
}

func TestDeletingAContactSaysWhenThereWasNoneToDelete(t *testing.T) {
	_, contacts, _ := workspaceStore(t)
	ctx := context.Background()

	created, err := contacts.Create(ctx, ContactWrite{PhoneNumber: "+14085550166"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := contacts.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := contacts.Delete(ctx, created.ID); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("second Delete() error = %v, want ErrContactNotFound", err)
	}
}

func TestAContactWithoutANumberIsRefused(t *testing.T) {
	_, contacts, _ := workspaceStore(t)

	_, err := contacts.Create(context.Background(), ContactWrite{PhoneNumber: "  "}, nil)
	if !errors.Is(err, ErrContactInvalid) {
		t.Errorf("Create() error = %v, want ErrContactInvalid", err)
	}
}

//
// The agent's own day.
//

// Every number on the Today card is a division by something that can be zero:
// a day with no calls, a shift that has not started, a wrap-up nobody filed.
func TestTheAgentsDayDividesSafely(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		calls, talk, wrapUp, wrapUps, signedIn int
		opened, confirmed                      int
		wantHandle, wantWrapUp, wantOccupancy  int
		wantConfirmedPct                       int
	}{
		{name: "an ordinary morning", calls: 10, talk: 2400, wrapUp: 600, wrapUps: 10, signedIn: 4000,
			opened: 10, confirmed: 9,
			wantHandle: 300, wantWrapUp: 60, wantOccupancy: 75, wantConfirmedPct: 90},
		{name: "signed in, nothing yet", signedIn: 1800},
		{name: "handled calls, wrapped none", calls: 4, talk: 400, signedIn: 1000,
			opened: 4, wantHandle: 100, wantOccupancy: 40},
		{name: "not signed in at all"},
		{name: "busier than the window can explain", calls: 1, talk: 900, wrapUp: 300, wrapUps: 1, signedIn: 600,
			opened: 1, confirmed: 1,
			wantHandle: 1200, wantWrapUp: 300, wantOccupancy: 100, wantConfirmedPct: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agentDay(tc.calls, tc.talk, tc.wrapUp, tc.wrapUps, tc.signedIn,
				tc.opened, tc.confirmed)
			if got.ConfirmedPct != tc.wantConfirmedPct {
				t.Errorf("confirmedPct = %d, want %d", got.ConfirmedPct, tc.wantConfirmedPct)
			}
			if got.AvgHandleSec != tc.wantHandle {
				t.Errorf("avgHandleSec = %d, want %d", got.AvgHandleSec, tc.wantHandle)
			}
			if got.AvgWrapUpSec != tc.wantWrapUp {
				t.Errorf("avgWrapUpSec = %d, want %d", got.AvgWrapUpSec, tc.wantWrapUp)
			}
			if got.OccupancyPct != tc.wantOccupancy {
				t.Errorf("occupancyPct = %d, want %d — an occupancy above 100 reads "+
					"as a bug rather than as a window edge", got.OccupancyPct, tc.wantOccupancy)
			}
			if got.CallsHandled != tc.calls || got.TalkSec != tc.talk ||
				got.WrapUpSec != tc.wrapUp || got.SignedInSec != tc.signedIn {
				t.Errorf("the totals were not carried through: %+v", got)
			}
		})
	}
}

// The day is read from two places at once — the ledger for calls, the presence
// history for time — and both have to be clipped to the window. This is the
// query doing that against a real server.
func TestTheAgentsDayReadsTheLedgerAndThePresenceHistory(t *testing.T) {
	ledger, _, pool := workspaceStore(t)
	ctx := context.Background()
	// The presence history is foreign-keyed to a real agent, so the day needs
	// one to belong to.
	me, somebodyElse := insertAgent(t, pool, "wei"), uuid.New()

	// The window is a fixed hour, so nothing depends on when the test runs.
	from := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	// Two calls in the window, one before it, one somebody else's.
	for _, c := range []struct {
		at      time.Time
		agent   uuid.UUID
		talkSec int
	}{
		{from.Add(5 * time.Minute), me, 300},
		{from.Add(20 * time.Minute), me, 180},
		{from.Add(-2 * time.Hour), me, 999},
		{from.Add(30 * time.Minute), somebodyElse, 999},
	} {
		agent := c.agent
		if err := ledger.InsertCDR(ctx, CDR{
			CallID: uuid.New(), StartedAt: c.at, EndedAt: c.at.Add(time.Minute),
			CallType: "INBOUND", Status: CDRStatusAnswered,
			PrimaryAgentID: &agent, AgentIDs: []uuid.UUID{agent}, TalkSec: c.talkSec,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A shift that started before the window and is still open, with two
	// wrap-ups inside it — one of them still running.
	for _, i := range []struct {
		state, reason string
		entered       time.Time
		exited        *time.Time
	}{
		{"READY", "", from.Add(-30 * time.Minute), timePtr(from.Add(10 * time.Minute))},
		{"NOT_READY", "AFTER_CALL_WORK", from.Add(10 * time.Minute), timePtr(from.Add(12 * time.Minute))},
		{"READY", "", from.Add(12 * time.Minute), timePtr(from.Add(25 * time.Minute))},
		{"NOT_READY", "AFTER_CALL_WORK", from.Add(25 * time.Minute), nil},
	} {
		var reason *string
		if i.reason != "" {
			r := i.reason
			reason = &r
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_state_logs (agent_id, state, reason, entered_at, exited_at)
			VALUES ($1, $2, $3, $4, $5)`, me, i.state, reason, i.entered, i.exited); err != nil {
			t.Fatal(err)
		}
	}

	day, err := ledger.ReportAgentDay(ctx, me, from, to)
	if err != nil {
		t.Fatalf("ReportAgentDay() error = %v", err)
	}

	if day.CallsHandled != 2 {
		t.Errorf("callsHandled = %d, want 2 — the earlier call and somebody else's "+
			"are not this agent's day", day.CallsHandled)
	}
	if day.TalkSec != 480 {
		t.Errorf("talkSec = %d, want 480", day.TalkSec)
	}
	// 2 minutes closed + 35 minutes still open at the window's end.
	if day.WrapUpSec != 2220 {
		t.Errorf("wrapUpSec = %d, want 2220: an open wrap-up counts up to the "+
			"window's end, not to zero", day.WrapUpSec)
	}
	if day.AvgWrapUpSec != 1110 {
		t.Errorf("avgWrapUpSec = %d, want 1110 across the two wrap-ups", day.AvgWrapUpSec)
	}
	// The shift began half an hour before the window; only the hour inside it
	// counts.
	if day.SignedInSec != 3600 {
		t.Errorf("signedInSec = %d, want 3600 — the window, not the shift", day.SignedInSec)
	}
	if day.OccupancyPct != 75 {
		t.Errorf("occupancyPct = %d, want 75", day.OccupancyPct)
	}
}

// A signed-out agent's day is empty rather than an error or a division by
// zero: the card renders zeroes, which is the truth.
func TestAnAgentWhoDidNothingHasAnEmptyDay(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	from := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	day, err := ledger.ReportAgentDay(context.Background(), uuid.New(), from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("ReportAgentDay() error = %v", err)
	}
	if day != (AgentDay{}) {
		t.Errorf("day = %+v, want every number zero", day)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// confirm is an agent pressing Done with both fields filled in, which is what
// most of these tests are about.
func confirm(t *testing.T, ledger *LedgerStore, callID, agentID uuid.UUID,
	dispositionCode, note string) (WrapUp, error) {
	t.Helper()
	return ledger.ConfirmWrapUp(context.Background(), callID, agentID, &dispositionCode, &note)
}

// insertAgent creates the account and agent identity a presence history hangs
// off, which is the only reason these tests need one.
func insertAgent(t *testing.T, pool *pgxpool.Pool, username string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	userID, agentID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, password_hash, display_name, role)
		VALUES ($1, $2::text, 'x', $2::text, 'AGENT')`, userID, username); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, user_id, callcenter_name) VALUES ($1, $2, $3)`,
		agentID, userID, "agent-"+username); err != nil {
		t.Fatal(err)
	}
	return agentID
}
