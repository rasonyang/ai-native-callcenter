// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// A callback names the user who handled it: an agent, an administrator and a
// user who is not an agent alike, and nobody when that user has since been
// deleted (#469). Needs a real server: set AICC_TEST_DATABASE_URL.
func TestACallbackNamesWhoeverHandledIt(t *testing.T) {
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
	ledger := st.Ledger()

	user := func(username, display, role string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO users (id, username, password_hash, display_name, role) VALUES ($1,$2,'x',$3,$4)`,
			id, username, display, role); err != nil {
			t.Fatalf("seed %s: %v", username, err)
		}
		return id
	}
	agent := user("wei", "Wei Chan", "AGENT")
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO agents (id, user_id, callcenter_name) VALUES ($1,$2,'agent-wei')`,
		uuid.Must(uuid.NewV7()), agent); err != nil {
		t.Fatalf("make the user an agent: %v", err)
	}
	admin := user("root", "Root Admin", "ADMIN")
	supervisor := user("lin", "Lin Supervisor", "SUPERVISOR")
	gone := user("ghost", "Ghost", "AGENT")

	want := map[uuid.UUID]string{}
	for _, c := range []struct {
		who  uuid.UUID
		name string
	}{{agent, "Wei Chan"}, {admin, "Root Admin"}, {supervisor, "Lin Supervisor"}, {gone, ""}} {
		cb, err := ledger.InsertCallback(ctx, nil, nil, "18600000000", "m")
		if err != nil {
			t.Fatal(err)
		}
		// The write itself answers with the name, whichever write it is.
		var written Callback
		if c.who == admin {
			written, err = ledger.HandleCallback(ctx, cb.ID, CallbackStatusDismissed, c.who)
		} else {
			written, err = ledger.ClaimCallback(ctx, cb.ID, c.who)
		}
		if err != nil {
			t.Fatal(err)
		}
		if written.HandledByName != c.name && c.who != gone {
			t.Errorf("the write named %q, want %q", written.HandledByName, c.name)
		}
		want[cb.ID] = c.name
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, gone); err != nil {
		t.Fatalf("delete the user: %v", err)
	}

	rows, _, err := ledger.ListCallbacks(ctx, "", nil, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("listed %d rows, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if row.HandledBy == nil {
			t.Errorf("%s lost its handler", row.ID)
		}
		if row.HandledByName != want[row.ID] {
			t.Errorf("%s: handler %v named %q, want %q", row.ID, row.HandledBy, row.HandledByName, want[row.ID])
		}
	}

	// Handing it back leaves nobody to name.
	cb, _ := ledger.InsertCallback(ctx, nil, nil, "18600000001", "m")
	if _, err := ledger.ClaimCallback(ctx, cb.ID, agent); err != nil {
		t.Fatal(err)
	}
	released, err := ledger.ReleaseCallback(ctx, cb.ID, agent)
	if err != nil {
		t.Fatal(err)
	}
	if released.HandledBy != nil || released.HandledByName != "" {
		t.Errorf("a released callback still names %v %q", released.HandledBy, released.HandledByName)
	}
}
