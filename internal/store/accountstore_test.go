// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// Provisioning against a real server. The whole design is one transaction, and
// a transaction is the one thing a fake cannot have: rolling back is what
// makes a rejected username cost nothing.

func accountStore(t *testing.T) *AccountStore {
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
	return st.Accounts()
}

func newAccount(username, role string) NewAccount {
	return NewAccount{
		Username: username, PasswordHash: "not-a-real-hash", DisplayName: username,
		Role: role, CallcenterName: "agent-" + username, SIPPassword: "phone-secret",
		ExtensionLow: 1000, ExtensionHigh: 1999,
	}
}

// An account that takes calls arrives complete or not at all. Half of it — an
// agent with no phone — is a state nothing in the product can act on and
// nothing distinguishes from a phone deliberately unbound.
func TestProvisioningACallTakingAccountWritesAllThreeRows(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	for _, role := range []string{"AGENT", "SUPERVISOR"} {
		got, err := acc.Provision(ctx, newAccount("wei-"+role, role))
		if err != nil {
			t.Fatalf("provision %s: %v", role, err)
		}
		if got.AgentID == nil {
			t.Errorf("%s got no agent identity — a supervisor who cannot pick up "+
				"a call is not supervising a call centre", role)
		}
		if got.ExtensionNumber == "" {
			t.Errorf("%s got no phone", role)
		}
	}

	// Both came out of the pool, in order, from the bottom.
	first, _ := acc.Get(ctx, mustFind(t, acc, "wei-AGENT"))
	second, _ := acc.Get(ctx, mustFind(t, acc, "wei-SUPERVISOR"))
	if first.ExtensionNumber != "1000" || second.ExtensionNumber != "1001" {
		t.Errorf("phones %s and %s, want 1000 and 1001",
			first.ExtensionNumber, second.ExtensionNumber)
	}
}

// An administrator is a user but not an agent. Giving them an ACD identity
// would put them in the roster, in the wallboard and in mod_callcenter, as
// somebody a queue could try to offer a call to.
func TestAnAdministratorGetsAnAccountAndNothingElse(t *testing.T) {
	acc := accountStore(t)
	got, err := acc.Provision(context.Background(), newAccount("boss", "ADMIN"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got.AgentID != nil || got.ExtensionNumber != "" {
		t.Errorf("administrator got agent=%v phone=%q — they configure the "+
			"platform, they do not staff it", got.AgentID, got.ExtensionNumber)
	}
}

// The payoff of doing it in one transaction: a username somebody already has
// costs nothing. Without the rollback the extension is allocated before the
// user insert fails, and every mistyped form burns a number out of a pool of
// a thousand — invisibly, because a burnt number looks exactly like a used one.
func TestARejectedUsernameBurnsNoNumber(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	first, err := acc.Provision(ctx, newAccount("wei", "AGENT"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if first.ExtensionNumber != "1000" {
		t.Fatalf("first phone = %s, want 1000", first.ExtensionNumber)
	}

	if _, err := acc.Provision(ctx, newAccount("wei", "AGENT")); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("second provision error = %v, want ErrUsernameTaken", err)
	}

	next, err := acc.Provision(ctx, newAccount("ben", "AGENT"))
	if err != nil {
		t.Fatalf("provision after the rejection: %v", err)
	}
	if next.ExtensionNumber != "1001" {
		t.Errorf("phone after a rejected username = %s, want 1001 — the failed "+
			"attempt kept a number nobody has", next.ExtensionNumber)
	}
}

// Demotion must not take the identity away with the role: agent_state_logs and
// wrap_ups cascade off the agents row, so deleting it destroys the record of
// what the person did. "Change this person's role" does not ask for that.
func TestDemotingAnAgentKeepsTheirIdentityAndTheirHistory(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	before, err := acc.Provision(ctx, newAccount("wei", "AGENT"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// A second administrator, so the guard is not what refuses the change.
	if _, err := acc.Provision(ctx, newAccount("boss", "ADMIN")); err != nil {
		t.Fatalf("provision admin: %v", err)
	}

	after, err := acc.Update(ctx, before.UserID, Changes{
		Username: "wei", DisplayName: "Wei", Role: "ADMIN", Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if after.AgentID == nil || *after.AgentID != *before.AgentID {
		t.Errorf("the agent identity went away on demotion (%v → %v) — with it "+
			"go the state history and every wrap-up they filed",
			before.AgentID, after.AgentID)
	}
	if after.ExtensionNumber != before.ExtensionNumber {
		t.Errorf("phone %s → %s", before.ExtensionNumber, after.ExtensionNumber)
	}
}

// Promotion is the direction that provisions: an administrator who becomes a
// supervisor has an account and has never had a phone.
func TestPromotingAnAdministratorGivesThemAPhone(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	if _, err := acc.Provision(ctx, newAccount("keeper", "ADMIN")); err != nil {
		t.Fatalf("provision the administrator who stays: %v", err)
	}
	boss, err := acc.Provision(ctx, newAccount("boss", "ADMIN"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	promoted, err := acc.EnsureAgentIdentity(ctx, boss.UserID, newAccount("boss", "SUPERVISOR"))
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if promoted.AgentID == nil || promoted.ExtensionNumber != "1000" {
		t.Errorf("promoted account: agent=%v phone=%q, want an identity and 1000",
			promoted.AgentID, promoted.ExtensionNumber)
	}

	// Asking twice is not two phones.
	again, err := acc.EnsureAgentIdentity(ctx, boss.UserID, newAccount("boss", "SUPERVISOR"))
	if err != nil {
		t.Fatalf("ensure identity again: %v", err)
	}
	if again.ExtensionNumber != promoted.ExtensionNumber {
		t.Errorf("a second promotion allocated %s on top of %s",
			again.ExtensionNumber, promoted.ExtensionNumber)
	}
}

// The last way in stays open. Without this the product is one PUT away from
// having nobody who can administer it, and no screen that could undo it.
func TestTheLastAdministratorCannotBeDemotedOrSuspended(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	only, err := acc.Provision(ctx, newAccount("boss", "ADMIN"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	for _, tc := range []struct {
		name string
		to   Changes
	}{
		{"demoted", Changes{Username: "boss", DisplayName: "Boss", Role: "AGENT", Status: "ACTIVE"}},
		{"suspended", Changes{Username: "boss", DisplayName: "Boss", Role: "ADMIN", Status: "SUSPENDED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := acc.Update(ctx, only.UserID, tc.to); !errors.Is(err, ErrLastAdmin) {
				t.Errorf("error = %v, want ErrLastAdmin", err)
			}
		})
	}

	// With a second administrator in place, the same change is allowed.
	if _, err := acc.Provision(ctx, newAccount("deputy", "ADMIN")); err != nil {
		t.Fatalf("provision the second administrator: %v", err)
	}
	if _, err := acc.Update(ctx, only.UserID, Changes{
		Username: "boss", DisplayName: "Boss", Role: "AGENT", Status: "ACTIVE",
	}); err != nil {
		t.Errorf("demotion refused with a second administrator present: %v", err)
	}
}

func mustFind(t *testing.T, acc *AccountStore, username string) uuid.UUID {
	t.Helper()
	rows, err := acc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.Username == username {
			return r.UserID
		}
	}
	t.Fatalf("no account named %s", username)
	return uuid.Nil
}

// The retention query, against a real server: the age comparison is
// PostgreSQL's make_interval and the exclusion is the partial condition every
// other recording read already uses.
func TestExpiredRecordingsAreOldEnoughAndNotAlreadySwept(t *testing.T) {
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

	callID := uuid.Must(uuid.NewV7())
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status)
		VALUES ($1, now() - interval '100 days', now() - interval '100 days', 'INBOUND', 'ANSWERED')`,
		callID); err != nil {
		t.Fatalf("seed the call: %v", err)
	}
	seed := func(ageDays int, sweptAlready bool) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		deleted := "NULL"
		if sweptAlready {
			deleted = "now()"
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO recordings (id, call_id, backend, object_key, created_at, deleted_at)
			VALUES ($1, $2, 'FS', $3, now() - make_interval(days => $4), `+deleted+`)`,
			id, callID, id.String()+".wav", ageDays); err != nil {
			t.Fatalf("seed a recording: %v", err)
		}
		return id
	}
	old := seed(40, false)
	seed(40, true)  // already swept
	seed(10, false) // too young

	due, err := st.Ledger().ExpiredRecordings(ctx, 30, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(due) != 1 || due[0].ID != old {
		t.Fatalf("due = %+v, want only the unswept 40-day-old one", due)
	}
	if due[0].Key == "" {
		t.Error("the object key is empty; the sweep would have nothing to delete")
	}

	if err := st.Ledger().MarkRecordingDeleted(ctx, old); err != nil {
		t.Fatalf("mark: %v", err)
	}
	again, err := st.Ledger().ExpiredRecordings(ctx, 30, 100)
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("still due after being marked: %+v — every sweep would delete it again", again)
	}
}
