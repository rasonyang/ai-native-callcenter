// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// The reverse of the binding: which agent a phone belongs to. The answer
// follows the binding as it stands, so a rebind moves it, and a phone nobody
// is bound to has no answer rather than an error.
func TestTheAgentBoundToAPhoneFollowsTheBinding(t *testing.T) {
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

	ext1, ext2, ext3 := uuid.New(), uuid.New(), uuid.New()
	user, agent := uuid.New(), uuid.New()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO extensions (id, number, password) VALUES ($1, '7001', 'x')`, []any{ext1}},
		{`INSERT INTO extensions (id, number, password) VALUES ($1, '7002', 'x')`, []any{ext2}},
		{`INSERT INTO extensions (id, number, password) VALUES ($1, '7003', 'x')`, []any{ext3}},
		{`INSERT INTO users (id, username, password_hash, display_name, role)
		  VALUES ($1, 'bound-probe', 'x', 'Probe', 'AGENT')`, []any{user}},
		{`INSERT INTO agents (id, user_id, callcenter_name, default_extension_id)
		  VALUES ($1, $2, 'bound-probe', $3)`, []any{agent, user, ext1}},
	} {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	store := st.Agents()
	for number, want := range map[string]bool{"7001": true, "7002": false, "no-such": false} {
		id, ok, err := store.AgentBoundTo(ctx, number)
		if err != nil {
			t.Fatalf("%s: %v", number, err)
		}
		if ok != want || (ok && id != agent) {
			t.Errorf("%s: (%v, %v), want bound=%v to the agent", number, id, ok, want)
		}
	}

	if _, err := db.ExecContext(ctx, `UPDATE agents SET default_extension_id = $2 WHERE id = $1`, agent, ext2); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.AgentBoundTo(ctx, "7001"); ok {
		t.Error("7001 still answers after the agent was rebound away from it")
	}
	if id, ok, _ := store.AgentBoundTo(ctx, "7002"); !ok || id != agent {
		t.Errorf("7002 = (%v, %v), want the rebound agent", id, ok)
	}

	// The profile carries the binding by identity as well as by number.
	prof, err := store.AgentProfile(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if prof.ExtensionID == nil || *prof.ExtensionID != ext2 || prof.ExtensionNumber != "7002" {
		t.Errorf("profile binding = %v/%q, want %v/7002", prof.ExtensionID, prof.ExtensionNumber, ext2)
	}
}
