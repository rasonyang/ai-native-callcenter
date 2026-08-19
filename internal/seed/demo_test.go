// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The demo dataset is only worth having if somebody can sign in to it and take
// a call. Twice it could not: the agents were never bound to a phone, and the
// queues were never staffed — both invisible to a reading of the seeder, both
// obvious the moment it runs against a server. So it runs against one here.
//
// Set AICC_TEST_DATABASE_URL (the dev stack's is
// postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable) and these run;
// without it they skip, loudly.
const testDSNEnv = "AICC_TEST_DATABASE_URL"

// seededStore migrates a scratch database and seeds it.
func seededStore(t *testing.T) *store.Store {
	t.Helper()
	admin := os.Getenv(testDSNEnv)
	if admin == "" {
		t.Skipf("set %s to run the seed tests (see internal/seed/demo_test.go)", testDSNEnv)
	}

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", testDSNEnv, err)
	}
	name := fmt.Sprintf("aicc_seedtest_%d", time.Now().UnixNano())

	adminDB, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	u.Path = "/" + name
	ctx := context.Background()
	st, err := store.Open(ctx, u.String(), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedDemo(t *testing.T, st *store.Store) {
	t.Helper()
	if err := Demo(context.Background(), st, quietLog()); err != nil {
		t.Fatalf("Demo() error = %v", err)
	}
}

// Every seeded account signs in with the documented password and the role its
// name promises. A demo whose администратор is an agent demos the wrong thing.
func TestEverySeededAccountSignsIn(t *testing.T) {
	st := seededStore(t)
	seedDemo(t, st)
	ctx := context.Background()

	for _, p := range demoPeople {
		t.Run(p.username, func(t *testing.T) {
			user, err := st.Queries.GetUserByUsername(ctx, p.username)
			if err != nil {
				t.Fatalf("no account %q after seeding: %v", p.username, err)
			}
			if string(user.Role) != p.role {
				t.Errorf("role = %s, want %s", user.Role, p.role)
			}
			ok, err := auth.VerifyPassword(demoPassword, user.PasswordHash)
			if err != nil || !ok {
				t.Errorf("the documented password does not open %q (ok=%v err=%v)",
					p.username, ok, err)
			}
		})
	}
}

// The two facts that made a seeded agent unable to answer anything: a phone
// nobody bound them to, and a queue nobody staffed them on. Both are what an
// agent needs before a call can be delivered, and neither is visible from
// reading the seeder.
func TestASeededAgentCanBeReachedByACall(t *testing.T) {
	st := seededStore(t)
	seedDemo(t, st)
	ctx := context.Background()

	for _, p := range demoPeople {
		if p.ext == "" {
			continue
		}
		t.Run(p.username, func(t *testing.T) {
			user, err := st.Queries.GetUserByUsername(ctx, p.username)
			if err != nil {
				t.Fatal(err)
			}
			agent, err := st.Queries.GetAgentByUserID(ctx, user.ID)
			if err != nil {
				t.Fatalf("%q has no agent identity: %v", p.username, err)
			}

			// Sign-in resolves the phone from the binding; without one the
			// agent is refused with "no extension is bound to this agent".
			if agent.DefaultExtensionID == nil {
				t.Fatalf("%q is bound to no extension, so they cannot sign in at all", p.username)
			}
			ext, err := st.Queries.GetExtension(ctx, *agent.DefaultExtensionID)
			if err != nil {
				t.Fatalf("the bound extension does not exist: %v", err)
			}
			if ext.Number != p.ext {
				t.Errorf("bound to extension %s, want %s", ext.Number, p.ext)
			}

			// A tier is what makes the queue offer them a call. Without one
			// they are Available, in no queue, and offered nothing while the
			// caller waits out the timeout.
			queues, err := st.Queries.ListQueuesForAgent(ctx, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, q := range queues {
				names = append(names, q.Name)
			}
			if len(names) == 0 {
				t.Fatalf("%q staffs no queue, so no call is ever offered to them", p.username)
			}
			var staffed bool
			for _, name := range names {
				if name == p.queue {
					staffed = true
				}
			}
			if !staffed {
				t.Errorf("%q staffs %v, want %s", p.username, names, p.queue)
			}
		})
	}
}

// Seeding twice is what an operator actually does — a container restarts, a
// boot repeats — and it must leave one of everything.
func TestSeedingTwiceChangesNothing(t *testing.T) {
	st := seededStore(t)
	seedDemo(t, st)
	before := countRows(t, st)

	seedDemo(t, st)
	after := countRows(t, st)

	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows after one seed, %d after two", table, n, after[table])
		}
	}
}

// A password that drifted — somebody reset it, an old demo had another one —
// is the case the seeder used to walk past, leaving an account nobody could
// open and no way to fix it but SQL. Running the seed is that way.
func TestSeedingResetsAPasswordThatDrifted(t *testing.T) {
	st := seededStore(t)
	seedDemo(t, st)
	ctx := context.Background()

	user, err := st.Queries.GetUserByUsername(ctx, "wei")
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.NewService(st.Queries, time.Hour)
	if err := svc.SetPassword(ctx, user.ID, "somebody-elses-password"); err != nil {
		t.Fatal(err)
	}

	seedDemo(t, st)

	user, err = st.Queries.GetUserByUsername(ctx, "wei")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.VerifyPassword(demoPassword, user.PasswordHash)
	if err != nil || !ok {
		t.Errorf("the seed did not restore the documented password (ok=%v err=%v)", ok, err)
	}
}

// An agent who already has their own phone and their own queue keeps both: the
// seeder fills gaps, it does not rearrange somebody's call centre.
func TestSeedingLeavesAnExistingBindingAndStaffingAlone(t *testing.T) {
	st := seededStore(t)
	ctx := context.Background()

	// A prior installation: wei on extension 1009, staffed on support-zh.
	seedDemo(t, st)
	user, err := st.Queries.GetUserByUsername(ctx, "wei")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.Queries.GetAgentByUserID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var otherExt uuid.UUID
	if err := st.Pool.QueryRow(ctx, `
		INSERT INTO extensions (id, number, kind, password, display_name)
		VALUES ($1, '1009', 'AGENT', 'x', 'Wei elsewhere') RETURNING id`, uuid.New()).
		Scan(&otherExt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE agents SET default_extension_id = $2 WHERE id = $1`, agent.ID, otherExt); err != nil {
		t.Fatal(err)
	}

	seedDemo(t, st)

	agent, err = st.Queries.GetAgentByUserID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agent.DefaultExtensionID == nil || *agent.DefaultExtensionID != otherExt {
		t.Errorf("the seed moved an agent that already had a phone: %v", agent.DefaultExtensionID)
	}
}

// countRows is the shape of the dataset, table by table.
func countRows(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{
		"users", "agents", "extensions", "queues", "queue_agents", "dids", "flows", "cdrs",
	} {
		var n int
		if err := st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}
