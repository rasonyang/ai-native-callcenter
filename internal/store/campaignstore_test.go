// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

func openCampaigns(t *testing.T) (*Store, *CampaignStore) {
	t.Helper()
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 16)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st, st.Campaigns()
}

// campaignFlow is a loadable flow; withTransfer lets its one phase offer
// transfer_to_agent.
func campaignFlow(slug string, withTransfer bool) []byte {
	tools := `["hangup"]`
	if withTransfer {
		tools = `["hangup", "transfer_to_agent"]`
	}
	return []byte(fmt.Sprintf(`{
	  "id": %q, "specVersion": "v2", "initialNode": "ask",
	  "global": {"persona": "A polite caller."},
	  "nodes": {"ask": {"instruction": "Greet {slots.userData.name}.", "tools": %s}}
	}`, slug, tools))
}

// seedDID creates a flow (published) and a DID answered by it.
func seedDID(t *testing.T, st *Store, number string, withTransfer bool) (didID, flowID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	flowID, err := st.Flows().Create(ctx, "flow-"+number, "Flow "+number, campaignFlow("flow-"+number, withTransfer))
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	if err := st.Flows().Publish(ctx, flowID, "test"); err != nil {
		t.Fatalf("publish flow: %v", err)
	}
	did, err := st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: number, Language: "en", FlowID: &flowID,
		IsEnabled: true, AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("create did: %v", err)
	}
	return did.ID, flowID
}

func newCampaign(didID uuid.UUID, name string) campaign.Campaign {
	return campaign.Campaign{
		ID: uuid.Must(uuid.NewV7()), Name: name, DIDID: didID, TimeZone: "Asia/Shanghai",
		CallingWindow: campaign.CallingWindow{
			Days: []string{"MON", "TUE"}, Ranges: []campaign.TimeRange{{Start: "09:00", End: "17:00"}},
		},
		MaxConcurrent: 5, RingTimeoutSec: 30, RetryPolicy: campaign.DefaultRetryPolicy(),
	}
}

func contactsFor(prefix string, n int) []campaign.NewContact {
	out := make([]campaign.NewContact, n)
	for i := range out {
		num := fmt.Sprintf("+8613%s%07d", prefix, i)
		out[i] = campaign.NewContact{
			ID: uuid.Must(uuid.NewV7()), Number: num, NumberRaw: num,
			Slots: map[string]string{"name": fmt.Sprintf("n%d", i)},
		}
	}
	return out
}

func TestACampaignRoundTripsThroughTheStore(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)

	in := newCampaign(didID, "Round trip")
	in.DefaultCountryCode = "86"
	got, err := cs.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Status != campaign.StatusDraft || got.DIDNumber != "95099" || got.DefaultCountryCode != "86" ||
		got.StartedAt != nil || got.FinishedAt != nil || got.PauseReason != "" || got.FollowUpQueueID != nil {
		t.Errorf("stored = %+v", got)
	}
	if len(got.CallingWindow.Days) != 2 || got.CallingWindow.Ranges[0] != (campaign.TimeRange{Start: "09:00", End: "17:00"}) {
		t.Errorf("calling window = %+v", got.CallingWindow)
	}
	if got.RetryPolicy.MaxAttempts != 3 || got.RetryPolicy.IntervalSec[campaign.OutcomeBusy] != 1800 ||
		got.RetryPolicy.TransientBackoff.CapSec != 14400 || len(got.RetryPolicy.RetryOutcomes) != 6 {
		t.Errorf("retry policy = %+v", got.RetryPolicy)
	}

	if _, err := cs.Get(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("an unknown id: %v", err)
	}

	if _, err := cs.ImportContacts(ctx, in.ID, contactsFor("0", 4)); err != nil {
		t.Fatal(err)
	}
	listed, total, err := cs.List(ctx, campaign.ListFilter{})
	if err != nil || total != 1 || len(listed) != 1 || listed[0].Contacts.Total != 4 || listed[0].Contacts.Active != 4 {
		t.Errorf("list = %+v total %d err %v", listed, total, err)
	}
	if _, total, _ := cs.List(ctx, campaign.ListFilter{Status: campaign.StatusRunning}); total != 0 {
		t.Errorf("a RUNNING filter matched %d DRAFTs", total)
	}

	// A missing DID is a typed refusal, not a storage error.
	bad := newCampaign(uuid.New(), "No such DID")
	if _, err := cs.Create(ctx, bad); !errors.Is(err, campaign.ErrDIDNotFound) {
		t.Errorf("unknown DID: %v", err)
	}
	q := uuid.New()
	bad = newCampaign(didID, "No such queue")
	bad.FollowUpQueueID = &q
	if _, err := cs.Create(ctx, bad); !errors.Is(err, campaign.ErrQueueNotFound) {
		t.Errorf("unknown queue: %v", err)
	}
}

// violation runs a statement that must be refused by the named constraint.
func violation(t *testing.T, st *Store, constraint, query string, args ...any) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(), query, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s: %q was accepted (err %v)", constraint, query, err)
		return
	}
	if pgErr.ConstraintName != constraint {
		t.Errorf("%q violated %q (%s), want %q", query, pgErr.ConstraintName, pgErr.Message, constraint)
	}
}

func TestTheSchemaRefusesWhatTheModelForbids(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)
	c, err := cs.Create(ctx, newCampaign(didID, "Constraints"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("1", 2)); err != nil {
		t.Fatal(err)
	}

	const base = `INSERT INTO campaigns (id, name, did_id, status, pause_reason, time_zone, calling_window,
		max_concurrent, ring_timeout_sec, retry_policy, finished_at) VALUES `
	row := func(status, reason, window string, maxConc, ring int, finished string) string {
		return fmt.Sprintf(`('%s', 'x', '%s', '%s', %s, 'UTC', %s, %d, %d, '{}', %s)`,
			uuid.New(), didID, status, reason, window, maxConc, ring, finished)
	}
	ok := `'{}'`
	violation(t, st, "ck_campaigns_status", base+row("BOGUS", "NULL", ok, 1, 30, "NULL"))
	violation(t, st, "ck_campaigns_pause_reason", base+row("DRAFT", "'MANUAL'", ok, 1, 30, "NULL"))
	violation(t, st, "ck_campaigns_pause_reason", base+row("PAUSED", "'WEATHER'", ok, 1, 30, "NULL"))
	violation(t, st, "ck_campaigns_calling_window", base+row("DRAFT", "NULL", `'[]'`, 1, 30, "NULL"))
	violation(t, st, "ck_campaigns_max_concurrent", base+row("DRAFT", "NULL", ok, 0, 30, "NULL"))
	violation(t, st, "ck_campaigns_ring_timeout_sec", base+row("DRAFT", "NULL", ok, 1, 4, "NULL"))
	violation(t, st, "ck_campaigns_finished_at", base+row("CANCELLED", "NULL", ok, 1, 30, "NULL"))
	violation(t, st, "ck_campaigns_finished_at", base+row("RUNNING", "NULL", ok, 1, 30, "now()"))
	violation(t, st, "fk_campaigns_dids",
		fmt.Sprintf(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
			VALUES ('%s', 'x', '%s', 'UTC', '{}', 1, 30, '{}')`, uuid.New(), uuid.New()))
	violation(t, st, "ck_campaigns_name", fmt.Sprintf(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
			VALUES ('%s', '  ', '%s', 'UTC', '{}', 1, 30, '{}')`, uuid.New(), didID))
	violation(t, st, "ck_campaigns_default_country_code", fmt.Sprintf(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy, default_country_code)
			VALUES ('%s', 'x', '%s', 'UTC', '{}', 1, 30, '{}', '+86')`, uuid.New(), didID))

	ins := func(number, cols, vals string) string {
		return fmt.Sprintf(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw%s)
			VALUES ('%s', '%s', '%s', 'raw'%s)`, cols, uuid.New(), c.ID, number, vals)
	}
	violation(t, st, "ck_campaign_contacts_number_e164", ins("13800138000", "", ""))
	violation(t, st, "ck_campaign_contacts_number_e164", ins("+0123456789", "", ""))
	violation(t, st, "ck_campaign_contacts_number_e164", ins("+86138", "", ""))
	violation(t, st, "ck_campaign_contacts_number_e164", ins("+8613800138000123456", "", ""))
	violation(t, st, "ck_campaign_contacts_slots", ins("+8613800138000", ", slots", ", '[]'"))
	violation(t, st, "ck_campaign_contacts_status", ins("+8613800138000", ", status", ", 'GONE'"))
	violation(t, st, "ck_campaign_contacts_attempts", ins("+8613800138000", ", attempts", ", -1"))
	violation(t, st, "ck_campaign_contacts_system_failures", ins("+8613800138000", ", system_failures", ", -1"))
	violation(t, st, "ck_campaign_contacts_last_outcome", ins("+8613800138000", ", last_outcome", ", 'MAYBE'"))
	violation(t, st, "ck_campaign_contacts_lead_intent", ins("+8613800138000", ", lead_intent", ", 'WARM'"))
	violation(t, st, "ck_campaign_contacts_next_attempt_at",
		ins("+8613800138000", ", status, next_attempt_at", ", 'DONE', now()"))
	violation(t, st, "fk_campaign_contacts_campaigns",
		fmt.Sprintf(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw) VALUES ('%s', '%s', '+8613800138000', 'x')`,
			uuid.New(), uuid.New()))

	// Every outcome and every intent the design names is accepted.
	for i, outcome := range []string{"ANSWERED", "BOT_FAILED", "OVERFLOW", "NO_ANSWER", "BUSY", "REJECTED",
		"UNKNOWN", "TRANSIENT", "ORIGINATE_FAILED", "INVALID_NUMBER", "DNC_SKIPPED"} {
		if _, err := st.Pool.Exec(ctx, ins(fmt.Sprintf("+861390000%04d", i), ", last_outcome", fmt.Sprintf(", '%s'", outcome))); err != nil {
			t.Errorf("outcome %s: %v", outcome, err)
		}
	}
	for i, intent := range []string{"HIGH", "MEDIUM", "LOW", "NONE", "DO_NOT_CALL"} {
		if _, err := st.Pool.Exec(ctx, ins(fmt.Sprintf("+861391000%04d", i), ", lead_intent", fmt.Sprintf(", '%s'", intent))); err != nil {
			t.Errorf("intent %s: %v", intent, err)
		}
	}
	for i, status := range []string{"ACTIVE", "DONE", "EXHAUSTED", "INVALID_NUMBER", "DNC"} {
		if _, err := st.Pool.Exec(ctx, ins(fmt.Sprintf("+861392000%04d", i), ", status", fmt.Sprintf(", '%s'", status))); err != nil {
			t.Errorf("contact status %s: %v", status, err)
		}
	}

	// UNIQUE (campaign_id, number_e164); the same number in another campaign is fine.
	violation(t, st, "uq_campaign_contacts_number", ins("+8613100000000", "", "")+"; "+ins("+8613100000000", "", ""))
	other, err := cs.Create(ctx, newCampaign(didID, "Other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw)
		VALUES ('%s', '%s', '+8613100000000', 'x')`, uuid.New(), other.ID)); err != nil {
		t.Errorf("the same number in a second campaign: %v", err)
	}
}

func TestADIDCampaignsDialFromCannotBeDeletedAndAQueueCanGo(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)

	var queueID uuid.UUID
	if err := st.Pool.QueryRow(ctx, `INSERT INTO queues (id, name, ext_number) VALUES ($1, 'follow-up', '7001') RETURNING id`,
		uuid.Must(uuid.NewV7())).Scan(&queueID); err != nil {
		t.Skipf("cannot insert a queue with this schema: %v", err)
	}
	in := newCampaign(didID, "Restrict")
	in.FollowUpQueueID = &queueID
	c, err := cs.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("2", 3)); err != nil {
		t.Fatal(err)
	}

	err = st.Catalog().DeleteDID(ctx, didID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "fk_campaigns_dids" {
		t.Errorf("deleting the DID of a campaign: %v, want fk_campaigns_dids", err)
	}

	if _, err := st.Pool.Exec(ctx, `DELETE FROM queues WHERE id = $1`, queueID); err != nil {
		t.Fatalf("delete the queue: %v", err)
	}
	got, err := cs.Get(ctx, c.ID)
	if err != nil || got.FollowUpQueueID != nil {
		t.Errorf("after the queue went: %v %v, want no follow-up queue", got.FollowUpQueueID, err)
	}

	if err := cs.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_contacts`).Scan(&n); err != nil || n != 0 {
		t.Errorf("contacts left after the campaign was deleted: %d %v", n, err)
	}
	if err := st.Catalog().DeleteDID(ctx, didID); err != nil {
		t.Errorf("the DID of a deleted campaign: %v", err)
	}
}

func TestTransitionsAreConditionalUpdates(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)

	c, _ := cs.Create(ctx, newCampaign(didID, "Moves"))
	if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("3", 5)); err != nil {
		t.Fatal(err)
	}
	var due int
	count := func() int {
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND next_attempt_at IS NOT NULL`, c.ID).Scan(&due); err != nil {
			t.Fatal(err)
		}
		return due
	}
	if count() != 0 {
		t.Error("contacts imported into a DRAFT are due before the campaign started")
	}

	started, err := cs.Start(ctx, c.ID)
	if err != nil || started.Status != campaign.StatusRunning || started.StartedAt == nil {
		t.Fatalf("start: %+v %v", started, err)
	}
	if count() != 5 {
		t.Errorf("%d contacts due after start, want 5", due)
	}
	if _, err := cs.Start(ctx, c.ID); !errors.Is(err, campaign.ErrInvalidTransition) {
		t.Errorf("starting twice: %v", err)
	}

	// A contact imported into a RUNNING campaign is due at once.
	if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("4", 2)); err != nil {
		t.Fatal(err)
	}
	if count() != 7 {
		t.Errorf("%d contacts due, want 7", due)
	}

	paused, err := cs.Pause(ctx, c.ID)
	if err != nil || paused.Status != campaign.StatusPaused || paused.PauseReason != campaign.PauseManual {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	resumed, err := cs.Resume(ctx, c.ID)
	if err != nil || resumed.Status != campaign.StatusRunning || resumed.PauseReason != "" {
		t.Fatalf("resume: %+v %v", resumed, err)
	}

	// A breaker's pause is resumed the same way.
	if _, err := st.Pool.Exec(ctx, `UPDATE campaigns SET status = 'PAUSED', pause_reason = 'PROVIDER_FAILURE' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := cs.Resume(ctx, c.ID); err != nil || got.Status != campaign.StatusRunning || got.PauseReason != "" {
		t.Fatalf("resume after a breaker: %+v %v", got, err)
	}

	cancelled, err := cs.Cancel(ctx, c.ID)
	if err != nil || cancelled.Status != campaign.StatusCancelled || cancelled.FinishedAt == nil {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	for name, fn := range map[string]func(context.Context, uuid.UUID) (campaign.Campaign, error){
		"start": cs.Start, "pause": cs.Pause, "resume": cs.Resume, "cancel": cs.Cancel,
	} {
		_, err := fn(ctx, c.ID)
		var ce *campaign.Error
		if !errors.As(err, &ce) || !errors.Is(err, campaign.ErrInvalidTransition) || ce.Params["status"] != "CANCELLED" {
			t.Errorf("%s of a CANCELLED campaign: %v", name, err)
		}
	}
	if _, err := cs.Start(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("start of an unknown campaign: %v", err)
	}
}

// Of N callers racing for one transition the database lets exactly one
// through, and the contacts are armed exactly once.
func TestRacingTransitionsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)

	race := func(c campaign.Campaign, n int, move func(context.Context, uuid.UUID) (campaign.Campaign, error)) (wins int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := move(ctx, c.ID)
				if err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				} else if !errors.Is(err, campaign.ErrInvalidTransition) {
					t.Errorf("a loser failed with %v, not a transition error", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		return wins
	}

	for round := range 5 {
		c, _ := cs.Create(ctx, newCampaign(didID, fmt.Sprintf("Race %d", round)))
		if wins := race(c, 12, cs.Start); wins != 1 {
			t.Errorf("round %d: %d of 12 concurrent starts won, want 1", round, wins)
		}
		if wins := race(c, 12, cs.Cancel); wins != 1 {
			t.Errorf("round %d: %d of 12 concurrent cancels won, want 1", round, wins)
		}
	}

	// Start against cancel on a DRAFT: cancel always lands; start either
	// preceded it (and then the campaign had started) or was refused (and the
	// campaign never started).
	for round := range 20 {
		c, _ := cs.Create(ctx, newCampaign(didID, fmt.Sprintf("StartCancel %d", round)))
		if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("5", 3)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var startErr, cancelErr error
		gate := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; _, startErr = cs.Start(ctx, c.ID) }()
		go func() { defer wg.Done(); <-gate; _, cancelErr = cs.Cancel(ctx, c.ID) }()
		close(gate)
		wg.Wait()
		final, _ := cs.Get(ctx, c.ID)
		if cancelErr != nil && startErr != nil {
			t.Fatalf("round %d: neither won: start %v cancel %v", round, startErr, cancelErr)
		}
		switch {
		case final.Status == campaign.StatusRunning && startErr == nil && cancelErr != nil:
			// cancel lost because it ran before start? impossible: cancel from DRAFT is legal.
			t.Fatalf("round %d: cancel was refused from DRAFT: %v", round, cancelErr)
		case final.Status != campaign.StatusCancelled:
			t.Fatalf("round %d: final status %s, want CANCELLED", round, final.Status)
		case startErr != nil && final.StartedAt != nil:
			t.Errorf("round %d: start was refused but the campaign has a startedAt", round)
		case startErr == nil && final.StartedAt == nil:
			t.Errorf("round %d: start succeeded but the campaign has no startedAt", round)
		}
		var due int
		_ = st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND next_attempt_at IS NOT NULL`, c.ID).Scan(&due)
		if startErr != nil && due != 0 {
			t.Errorf("round %d: start was refused but %d contacts were armed", round, due)
		}
	}
}

func TestAnImportIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)
	c, _ := cs.Create(ctx, newCampaign(didID, "Atomic"))

	batch := contactsFor("6", 10)
	batch[7].Number = "not a canonical number" // the one the schema refuses
	if _, err := cs.ImportContacts(ctx, c.ID, batch); err == nil {
		t.Fatal("a batch with a non-canonical number was accepted")
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1`, c.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d contacts survive a failed batch, want 0 (%v)", n, err)
	}

	// First writer wins and nothing is rewritten.
	first := contactsFor("7", 3)
	if _, err := cs.ImportContacts(ctx, c.ID, first); err != nil {
		t.Fatal(err)
	}
	again := contactsFor("7", 3)
	for i := range again {
		again[i].Slots = map[string]string{"name": "changed"}
	}
	stored, err := cs.ImportContacts(ctx, c.ID, again)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range stored {
		if s.IsNew || s.ID != first[i].ID {
			t.Errorf("row %d: %+v, want the first import's contact %s", i, s, first[i].ID)
		}
	}
	items, total, _ := cs.ListContacts(ctx, c.ID, campaign.ContactFilter{})
	if total != 3 || items[0].Slots["name"] != "n0" {
		t.Errorf("after a repeat import: total %d, first slots %v", total, items[0].Slots)
	}

	// A cancelled campaign takes nothing.
	if _, err := cs.Cancel(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ImportContacts(ctx, c.ID, contactsFor("8", 1)); !errors.Is(err, campaign.ErrInvalidTransition) {
		t.Errorf("import into CANCELLED: %v", err)
	}
	if _, err := cs.ImportContacts(ctx, uuid.New(), contactsFor("8", 1)); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("import into an unknown campaign: %v", err)
	}
}

// Two imports of overlapping lists racing: each number lands once, and exactly
// one of the two is told it was the new one.
func TestConcurrentImportsOfOneListHaveOneWriterPerNumber(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, _ := seedDID(t, st, "95099", false)
	c, _ := cs.Create(ctx, newCampaign(didID, "Overlap"))

	a, b := contactsFor("9", 300), contactsFor("9", 300) // same numbers, different contact ids
	var ra, rb []campaign.Stored
	var ea, eb error
	var wg sync.WaitGroup
	gate := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-gate; ra, ea = cs.ImportContacts(ctx, c.ID, a) }()
	go func() { defer wg.Done(); <-gate; rb, eb = cs.ImportContacts(ctx, c.ID, b) }()
	close(gate)
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatalf("imports failed: %v / %v", ea, eb)
	}
	var total int
	_ = st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1`, c.ID).Scan(&total)
	if total != 300 {
		t.Errorf("%d contacts, want 300", total)
	}
	for i := range a {
		if ra[i].IsNew == rb[i].IsNew {
			t.Fatalf("number %d: both new=%v; exactly one import must have written it", i, ra[i].IsNew)
		}
		if ra[i].ID != rb[i].ID {
			t.Fatalf("number %d: the two imports disagree about its contact: %s vs %s", i, ra[i].ID, rb[i].ID)
		}
	}
}

// Publishing a flow revision that can transfer is refused while a campaign
// that has not finished dials from a DID it answers, and allowed again once
// the campaign has finished. A DID cannot be rebound to such a flow likewise.
func TestAFlowCannotGainATransferUnderALiveCampaign(t *testing.T) {
	ctx := context.Background()
	st, cs := openCampaigns(t)
	didID, flowID := seedDID(t, st, "95099", false)
	_, transferFlowID := seedDID(t, st, "95100", true)
	flows := st.Flows()

	c, err := cs.Create(ctx, newCampaign(didID, "Guarded"))
	if err != nil {
		t.Fatal(err)
	}

	withTransfer := campaignFlow("flow-95099", true)
	without := campaignFlow("flow-95099", false)

	for _, status := range []string{"DRAFT", "RUNNING", "PAUSED"} {
		if _, err := st.Pool.Exec(ctx,
			`UPDATE campaigns SET status = $2::text, pause_reason = CASE WHEN $2::text = 'PAUSED' THEN 'MANUAL' END WHERE id = $1`,
			c.ID, status); err != nil {
			t.Fatal(err)
		}
		if err := flows.UpdateDraft(ctx, flowID, "Flow", withTransfer); err != nil {
			t.Fatal(err)
		}
		err := flows.Publish(ctx, flowID, "adds transfer")
		var ce *campaign.Error
		if !errors.As(err, &ce) || !errors.Is(err, campaign.ErrFlowHasTransfer) {
			t.Fatalf("%s campaign: publishing a transferring revision gave %v, want ErrFlowHasTransfer", status, err)
		}
		if ce.Params["campaigns"] != int64(1) {
			t.Errorf("params = %v, want the number of campaigns in the way", ce.Params)
		}
		// Nothing was published: the live revision is still the one without.
		spec, err := flows.PublishedSpec(ctx, flowID)
		if err != nil || flow.CanTransfer(spec) {
			t.Fatalf("the refused publish changed the live flow: %v %v", spec, err)
		}

		// A revision that does not transfer is fine at any time.
		if err := flows.UpdateDraft(ctx, flowID, "Flow", without); err != nil {
			t.Fatal(err)
		}
		if err := flows.Publish(ctx, flowID, "no transfer"); err != nil {
			t.Errorf("%s campaign: publishing a revision without transfer: %v", status, err)
		}
	}

	// Rebinding the number to a flow that can transfer.
	did, _ := st.Catalog().ListDIDs(ctx)
	var mine catalog.DID
	for _, d := range did {
		if d.ID == didID {
			mine = d
		}
	}
	mine.FlowID = &transferFlowID
	_, err = st.Catalog().UpdateDID(ctx, mine)
	if !errors.Is(err, campaign.ErrFlowHasTransfer) {
		t.Errorf("rebinding a campaign's DID to a transferring flow: %v", err)
	}
	if got, _ := st.Catalog().ListDIDs(ctx); len(got) > 0 {
		for _, d := range got {
			if d.ID == didID && (d.FlowID == nil || *d.FlowID != flowID) {
				t.Error("the refused rebinding changed the DID")
			}
		}
	}
	// Rebinding it to a flow that cannot transfer is fine.
	_, noTransferFlow := seedDID(t, st, "95101", false)
	mine.FlowID = &noTransferFlow
	if _, err := st.Catalog().UpdateDID(ctx, mine); err != nil {
		t.Errorf("rebinding to a flow without transfer: %v", err)
	}
	mine.FlowID = &flowID

	// Once the campaign has finished the same publish and rebinding go through.
	if _, err := cs.Cancel(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := flows.UpdateDraft(ctx, flowID, "Flow", withTransfer); err != nil {
		t.Fatal(err)
	}
	if err := flows.Publish(ctx, flowID, "after the campaign"); err != nil {
		t.Errorf("publishing a transferring revision after the campaign was cancelled: %v", err)
	}
	mine.FlowID = &transferFlowID
	if _, err := st.Catalog().UpdateDID(ctx, mine); err != nil {
		t.Errorf("rebinding after the campaign was cancelled: %v", err)
	}
}
