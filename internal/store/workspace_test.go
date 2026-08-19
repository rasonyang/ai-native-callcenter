// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
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
// wrap-up, so the vocabulary ships with the schema rather than waiting for
// somebody to configure one.
func TestTheWrapUpVocabularyShipsWithTheSchema(t *testing.T) {
	ledger, _, _ := workspaceStore(t)

	categories, err := ledger.ListDispositions(context.Background())
	if err != nil {
		t.Fatalf("ListDispositions() error = %v", err)
	}
	if len(categories) == 0 {
		t.Fatal("no disposition categories — an agent cannot file anything")
	}
	for _, c := range categories {
		if c.Code == "" || c.Label == "" {
			t.Errorf("category %+v is missing a code or a label", c)
		}
		if len(c.Dispositions) == 0 {
			t.Errorf("category %s has no dispositions; empty categories are not offered", c.Code)
		}
		for _, d := range c.Dispositions {
			if d.Code == "" || d.Label == "" {
				t.Errorf("disposition %+v is missing a code or a label", d)
			}
		}
	}
}

// Filing captures the labels, so renaming a category later cannot rewrite what
// an agent said about a call last month.
func TestAWrapUpKeepsTheWordsItWasFiledUnder(t *testing.T) {
	ledger, _, pool := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.New(), uuid.New()

	filed, err := ledger.FileWrapUp(ctx, callID, agentID, "ISSUE_FIXED", "replaced the router")
	if err != nil {
		t.Fatalf("FileWrapUp() error = %v", err)
	}
	if filed.DispositionLabel == "" || filed.CategoryCode == "" || filed.CategoryLabel == "" {
		t.Errorf("filed = %+v, want the vocabulary's labels captured with it", filed)
	}

	// The vocabulary is edited; the record is history and does not move.
	if _, err := pool.Exec(ctx,
		`UPDATE dispositions SET label = 'Fixed it' WHERE code = 'ISSUE_FIXED'`); err != nil {
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

	if _, err := ledger.FileWrapUp(ctx, callID, agentID, "CALLBACK_SCHEDULED", ""); err != nil {
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
	if cdr.WrapUp == nil || cdr.WrapUp.DispositionCode != "CALLBACK_SCHEDULED" {
		t.Errorf("wrapUp = %+v, want the filing that preceded the row", cdr.WrapUp)
	}
}

// Filing twice is one agent changing their mind, not two records.
func TestFilingTwiceReplaces(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID, agentID := uuid.New(), uuid.New()

	if _, err := ledger.FileWrapUp(ctx, callID, agentID, "SPAM_CALL", "first thought"); err != nil {
		t.Fatal(err)
	}
	second, err := ledger.FileWrapUp(ctx, callID, agentID, "WRONG_NUMBER", "on reflection")
	if err != nil {
		t.Fatal(err)
	}
	if second.DispositionCode != "WRONG_NUMBER" || second.Note != "on reflection" {
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

	_, err := ledger.FileWrapUp(context.Background(), uuid.New(), uuid.New(), "NOT_A_CODE", "")
	if !errors.Is(err, ErrUnknownDisposition) {
		t.Errorf("FileWrapUp() error = %v, want ErrUnknownDisposition", err)
	}
}

// Two agents on one call — a transfer — each file their own, and each sees
// their own on their own listing.
func TestEachAgentSeesTheirOwnFilingOnTheirOwnCalls(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	callID := uuid.New()
	first, second := uuid.New(), uuid.New()

	if _, err := ledger.FileWrapUp(ctx, callID, first, "ESCALATED", "handed to L2"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.FileWrapUp(ctx, callID, second, "ISSUE_FIXED", "sorted it"); err != nil {
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
		{"the first agent", first, "ESCALATED"},
		{"the agent who finished it", second, "ISSUE_FIXED"},
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
	if cdr.WrapUp == nil || cdr.WrapUp.DispositionCode != "ISSUE_FIXED" {
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
