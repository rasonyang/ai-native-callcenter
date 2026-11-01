// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// #268: a call-taking account whose identity has no phone is given one by
// saving the account, through the real route, the real store and a real
// database. Only the switch is a fake, and it records what it was told.

// mirrorRecorder is the agents service, remembering which agents the switch
// was told about.
type mirrorRecorder struct {
	stubAgents
	mu       sync.Mutex
	mirrored []uuid.UUID
}

func (m *mirrorRecorder) MirrorAgent(_ context.Context, agentID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mirrored = append(m.mirrored, agentID)
}

func (m *mirrorRecorder) told() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uuid.UUID(nil), m.mirrored...)
}

func phoneFixture(t *testing.T, extensionRange string) (*fixture, *mirrorRecorder) {
	t.Helper()
	switchFake := &mirrorRecorder{}
	f := newFixtureWithConfig(t, func(c *config.Config) { c.ExtensionRange = extensionRange },
		func(st *store.Store, d *Deps) {
			d.Agents = switchFake
			// The account routes are mounted with the catalogue's.
			d.Catalog = catalog.NewService(st.Catalog(), nil, st.Catalog())
		})
	return f, switchFake
}

// phoneless provisions an account and takes its phone away, the shape of the
// accounts from before a phone came with every account.
func (f *fixture) phoneless(username, role string) store.Account {
	f.t.Helper()
	ctx := context.Background()
	acct, err := f.st.Accounts().Provision(ctx, store.NewAccount{
		Username: username, PasswordHash: "not-a-hash", DisplayName: username,
		Role: "AGENT", CallcenterName: "agent-" + username, SIPPassword: "sip-" + username,
		ExtensionLow: 1900, ExtensionHigh: 1999,
	})
	if err != nil {
		f.t.Fatalf("provision %s: %v", username, err)
	}
	var phoneID uuid.UUID
	if err := f.st.Pool.QueryRow(ctx, `SELECT default_extension_id FROM agents WHERE id = $1`,
		*acct.AgentID).Scan(&phoneID); err != nil {
		f.t.Fatalf("read phone: %v", err)
	}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`UPDATE agents SET default_extension_id = NULL WHERE id = $1`, *acct.AgentID},
		{`DELETE FROM extensions WHERE id = $1`, phoneID},
		{`UPDATE users SET role = '` + role + `' WHERE id = $1`, acct.UserID},
	} {
		if _, err := f.st.Pool.Exec(ctx, q.sql, q.arg); err != nil {
			f.t.Fatalf("make %s phoneless: %v", username, err)
		}
	}
	got, err := f.st.Accounts().Get(ctx, acct.UserID)
	if err != nil {
		f.t.Fatalf("read %s: %v", username, err)
	}
	return got
}

// saveUnchanged is what the screen's "Assign a phone" sends: the account as it
// reads, back again.
func (f *fixture) saveUnchanged(admin string, a store.Account) reply {
	f.t.Helper()
	return f.do(call{method: http.MethodPut, path: "/users/" + a.UserID.String(), cookie: admin,
		body: map[string]any{
			"username": a.Username, "displayName": a.DisplayName,
			"role": a.Role, "status": a.Status, "locale": readsAs(a.Locale),
		}})
}

func userOf(t *testing.T, r reply) struct {
	AgentID         *uuid.UUID `json:"agentId"`
	ExtensionNumber *string    `json:"extensionNumber"`
} {
	t.Helper()
	var out struct {
		AgentID         *uuid.UUID `json:"agentId"`
		ExtensionNumber *string    `json:"extensionNumber"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode user: %v (%.200s)", err, r.body)
	}
	return out
}

func extensionCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM extensions`).Scan(&n); err != nil {
		t.Fatalf("count extensions: %v", err)
	}
	return n
}

func TestSavingACallTakerWithoutAPhoneGivesThemOne(t *testing.T) {
	f, switchFake := phoneFixture(t, "1000-1099")
	admin := f.seedAdmin()

	for _, role := range []string{"AGENT", "SUPERVISOR"} {
		t.Run(role, func(t *testing.T) {
			f := f.in(t)
			before := f.phoneless("amy-"+role, role)
			got := f.saveUnchanged(admin, before)
			if got.status != http.StatusOK {
				t.Fatalf("status %d (%s), want 200: %.200s", got.status, got.code, got.body)
			}
			user := userOf(t, got)
			if user.ExtensionNumber == nil || *user.ExtensionNumber == "" {
				t.Fatalf("no phone in the answer: %.200s", got.body)
			}
			if user.AgentID == nil || *user.AgentID != *before.AgentID {
				t.Errorf("agent id %v → %v", *before.AgentID, user.AgentID)
			}
			told := switchFake.told()
			if len(told) == 0 || told[len(told)-1] != *before.AgentID {
				t.Errorf("the switch was told about %v, want %v last", told, *before.AgentID)
			}
		})
	}
}

func TestSavingAnAccountThatNeedsNoPhoneAllocatesNone(t *testing.T) {
	f, switchFake := phoneFixture(t, "1000-1099")
	admin := f.seedAdmin()
	boss := f.phoneless("boss", "ADMIN")
	before := extensionCount(t, f)

	// An administrator with a leftover identity.
	got := f.saveUnchanged(admin, boss)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (%s): %.200s", got.status, got.code, got.body)
	}
	if user := userOf(t, got); user.ExtensionNumber != nil {
		t.Errorf("an administrator was given phone %s", *user.ExtensionNumber)
	}

	// An agent who has a phone keeps it.
	mina, err := f.st.Accounts().Get(context.Background(), f.agent.userID)
	if err != nil {
		t.Fatalf("read mina: %v", err)
	}
	got = f.saveUnchanged(admin, mina)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (%s): %.200s", got.status, got.code, got.body)
	}
	if user := userOf(t, got); user.ExtensionNumber == nil || *user.ExtensionNumber != mina.ExtensionNumber {
		t.Errorf("phone %s → %v", mina.ExtensionNumber, user.ExtensionNumber)
	}

	if n := extensionCount(t, f); n != before {
		t.Errorf("%d extensions → %d", before, n)
	}
	if told := switchFake.told(); len(told) != 0 {
		t.Errorf("the switch was told about %v, nobody changed", told)
	}
}

// The pool's one number is mina's. The refusal is the pool's own code, and no
// extension is left behind.
func TestAnExhaustedPoolRefusesThePhoneAndWritesNone(t *testing.T) {
	f, switchFake := phoneFixture(t, "1000-1000")
	admin := f.seedAdmin()
	amy := f.phoneless("amy", "AGENT")
	before := extensionCount(t, f)

	got := f.saveUnchanged(admin, amy)
	if got.status != http.StatusConflict || got.code != string(CodeExtensionPoolExhausted) {
		t.Fatalf("status %d code %s, want 409 EXTENSION_POOL_EXHAUSTED: %.200s", got.status, got.code, got.body)
	}
	after, err := f.st.Accounts().Get(context.Background(), amy.UserID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.ExtensionNumber != "" || *after.AgentID != *amy.AgentID {
		t.Errorf("after the refusal: agent=%v phone=%q", after.AgentID, after.ExtensionNumber)
	}
	if n := extensionCount(t, f); n != before {
		t.Errorf("%d extensions → %d", before, n)
	}
	if told := switchFake.told(); len(told) != 0 {
		t.Errorf("the switch was told about %v after a refusal", told)
	}
}

// Two operators saving one phoneless account at once give it one phone.
func TestTwoConcurrentSavesOfAPhonelessAccountGiveItOnePhone(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1099")
	admin := f.seedAdmin()
	amy := f.phoneless("amy", "AGENT")
	before := extensionCount(t, f)

	const savers = 4
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		replies = make([]reply, savers)
	)
	for i := range savers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replies[i] = f.saveUnchanged(admin, amy)
		}()
	}
	close(start)
	wg.Wait()

	var phone string
	for i, r := range replies {
		if r.status != http.StatusOK {
			t.Fatalf("save %d: status %d (%s): %.200s", i, r.status, r.code, r.body)
		}
		user := userOf(t, r)
		if user.ExtensionNumber == nil {
			t.Fatalf("save %d answered no phone", i)
		}
		if phone == "" {
			phone = *user.ExtensionNumber
		} else if *user.ExtensionNumber != phone {
			t.Errorf("saves answered phones %s and %s", phone, *user.ExtensionNumber)
		}
	}
	if n := extensionCount(t, f) - before; n != 1 {
		t.Errorf("%d phones allocated, want exactly 1", n)
	}
}
