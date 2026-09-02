// SPDX-License-Identifier: Apache-2.0

package httpapi

// Nine assertions about the unified authorization model, written before the
// implementation and failing on the baseline exactly as predicted.
//
// They run against a **real PostgreSQL and the real routing tree**: sessions,
// scopes and act-as all go through the real middleware, with no mock anywhere
// in the chain. The telephony service is a fake, because the real one wants a
// FreeSWITCH — that line is deliberate, and nothing on the authorization path
// is on the wrong side of it.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

const scopeTestDSNEnv = "AICC_TEST_DATABASE_URL"

// fixture is a running server on a database of its own.
type fixture struct {
	t      *testing.T
	server *httptest.Server
	st     *store.Store
	// agent and other are two agent accounts, so a test can ask "is this call
	// yours?" and mean it.
	agent, other seeded
}

type seeded struct {
	userID  uuid.UUID
	agentID uuid.UUID
	name    string
	cookie  string
}

// newFixture builds a real database, runs the real migrations, and stands up a
// real server.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	admin := os.Getenv(scopeTestDSNEnv)
	if admin == "" {
		t.Skipf("set %s to run the scope tests", scopeTestDSNEnv)
	}
	dsn := scratchDatabase(t, admin)

	ctx := context.Background()
	st, err := store.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.Config{SessionCookie: "aicc_session", SessionTTL: time.Hour}
	srv := New(cfg, Deps{
		Auth:     auth.NewService(st.Queries, cfg.SessionTTL),
		Agents:   stubAgents{},
		AgentDir: dbDirectory{st},
		Ledger:   st.Ledger(),
		Auditor:  st.Ledger(),
		Accounts: st.Accounts(),
		Contacts: st.Contacts(),
		Keys:     APIKeys{APIKeyStore: st.APIKeys()},
		Hub:      events.NewHub(events.NewSequence(dbSeq{st}, "events")),
	})
	f := &fixture{t: t, server: httptest.NewServer(srv.router()), st: st}
	t.Cleanup(f.server.Close)

	f.agent = f.seedAgent("mina", 1000)
	f.other = f.seedAgent("tomas", 1010)
	return f
}

func scratchDatabase(t *testing.T, admin string) string {
	t.Helper()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", scopeTestDSNEnv, err)
	}
	name := fmt.Sprintf("aicc_scopetest_%d", time.Now().UnixNano())
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}

const seedPassword = "correct-horse-battery"

func (f *fixture) seedAgent(username string, poolLow int) seeded {
	f.t.Helper()
	hash, err := auth.HashPassword(seedPassword)
	if err != nil {
		f.t.Fatalf("hash password: %v", err)
	}
	acct, err := f.st.Accounts().Provision(context.Background(), store.NewAccount{
		Username: username, PasswordHash: hash, DisplayName: username,
		Role: string(auth.RoleAgent), CallcenterName: username + "@aicc",
		SIPPassword: "sip-" + username, ExtensionLow: poolLow, ExtensionHigh: poolLow + 9,
	})
	if err != nil {
		f.t.Fatalf("provision %s: %v", username, err)
	}
	if acct.AgentID == nil {
		f.t.Fatalf("%s was provisioned without an agent identity", username)
	}
	return seeded{userID: acct.UserID, agentID: *acct.AgentID, name: username,
		cookie: f.signIn(username)}
}

// signIn goes through the real POST /auth/login for a real session cookie.
func (f *fixture) signIn(username string) string {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": seedPassword})
	res, err := http.Post(f.server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		f.t.Fatalf("login %s: %v", username, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		f.t.Fatalf("login %s: status %d", username, res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == "aicc_session" {
			return c.Value
		}
	}
	f.t.Fatalf("login %s set no session cookie", username)
	return ""
}

// dbSeq is the real source of event sequence numbers — the same SQL main runs.
type dbSeq struct{ st *store.Store }

func (s dbSeq) ReserveSeqBlock(ctx context.Context, name string, size int64) (int64, error) {
	return s.st.Queries.ReserveSeqBlock(ctx, queries.ReserveSeqBlockParams{Name: name, BlockSize: size})
}

// call is the only way these tests issue a request, so which credential is
// being presented is always spelled out.
type call struct {
	method, path string
	body         any
	cookie       string // the page's session
	bearer       string // API Key
	actAs        string // X-AICC-Agent-ID
}

type reply struct {
	status int
	code   string
	body   []byte
	header http.Header
}

func (f *fixture) do(c call) reply {
	f.t.Helper()
	var payload io.Reader
	if c.body != nil {
		raw, _ := json.Marshal(c.body)
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(c.method, f.server.URL+"/api/v1"+c.path, payload)
	if err != nil {
		f.t.Fatalf("build request: %v", err)
	}
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "aicc_session", Value: c.cookie})
		if c.method != http.MethodGet {
			req.Header.Set(csrfHeader, "1")
		}
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.actAs != "" {
		req.Header.Set("X-AICC-Agent-ID", c.actAs)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", c.method, c.path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	return reply{status: res.StatusCode, code: env.Error.Code, body: raw, header: res.Header}
}

// issueKey mints a key through the real POST /api-keys and returns the secret,
// which is shown exactly once.
func (f *fixture) issueKey(admin string, scopes ...string) (id, secret string) {
	f.t.Helper()
	got := f.do(call{method: http.MethodPost, path: "/api-keys", cookie: admin,
		body: map[string]any{"name": "test", "scopes": scopes}})
	if got.status != http.StatusCreated {
		f.t.Fatalf("issue key: status %d body %.200s", got.status, got.body)
	}
	var out struct{ ID, Secret string }
	if err := json.Unmarshal(got.body, &out); err != nil {
		f.t.Fatalf("issue key: %v", err)
	}
	return out.ID, out.Secret
}

func (f *fixture) seedAdmin() string {
	f.t.Helper()
	hash, _ := auth.HashPassword(seedPassword)
	if _, err := f.st.Accounts().Provision(context.Background(), store.NewAccount{
		Username: "root", PasswordHash: hash, DisplayName: "root",
		Role: string(auth.RoleAdmin),
	}); err != nil {
		f.t.Fatalf("provision admin: %v", err)
	}
	return f.signIn("root")
}

// ---------------------------------------------------------------------------
// 1. A key acting for an agent does that agent's work, and the audit row names
// the key.
// ---------------------------------------------------------------------------

func TestAKeyActingForAnAgentDrivesThatAgentsPresence(t *testing.T) {
	f := newFixture(t)
	_, secret := f.issueKey(f.seedAdmin(), "agent:act")

	got := f.do(call{method: http.MethodPost, path: "/agent/ready",
		bearer: secret, actAs: f.agent.agentID.String()})
	// The contract says 200 here, and the contract is what the server owes.
	if got.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200; body %.200s", got.status, got.code, got.body)
	}

	// The subject of the audit row is the key and the agent is its target: the
	// person did not do this.
	var kind, agentID string
	err := f.st.Pool.QueryRow(context.Background(),
		`SELECT subject_kind, coalesce(agent_id::text, '') FROM audit_logs
		 WHERE action = 'POST /api/v1/agent/ready' ORDER BY id DESC LIMIT 1`).Scan(&kind, &agentID)
	if err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if kind != "API_KEY" {
		t.Errorf("subject_kind = %q, want API_KEY — the key acted, not the agent", kind)
	}
	if agentID != f.agent.agentID.String() {
		t.Errorf("agent_id = %q, want %s", agentID, f.agent.agentID)
	}
}

// ---------------------------------------------------------------------------
// 2. Without the act-as header there is nobody for an agent-scoped operation to
// act for.
// ---------------------------------------------------------------------------

func TestAKeyWithNoAgentHeaderCannotActForOne(t *testing.T) {
	f := newFixture(t)
	_, secret := f.issueKey(f.seedAdmin(), "agent:act")

	got := f.do(call{method: http.MethodPost, path: "/agent/ready", bearer: secret})
	if got.code != "AGENT_REQUIRED" {
		t.Errorf("code = %q status %d, want AGENT_REQUIRED; body %.200s", got.code, got.status, got.body)
	}
}

// ---------------------------------------------------------------------------
// 3. A key representing no agent still reads whatever its scopes allow. An
// empty result would pass this test for the wrong reason, so it does not count.
// ---------------------------------------------------------------------------

func TestAKeyWithNoAgentStillReadsWhatItsScopeAllows(t *testing.T) {
	f := newFixture(t)
	f.insertCDR()
	_, secret := f.issueKey(f.seedAdmin(), "history:read:all")

	got := f.do(call{method: http.MethodGet, path: "/cdrs", bearer: secret})
	if got.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200; body %.200s", got.status, got.code, got.body)
	}
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(got.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) < 1 {
		t.Errorf("items = %d, want at least 1 — an empty list would pass this assertion "+
			"for the wrong reason", len(out.Items))
	}
}

// ---------------------------------------------------------------------------
// 4. A session cookie may never act for someone else, whatever the role.
// ---------------------------------------------------------------------------

func TestASessionMayNeverActForAnotherAgent(t *testing.T) {
	f := newFixture(t)
	for _, who := range []struct {
		name, cookie string
	}{
		{"an agent", f.agent.cookie},
		{"an administrator", f.seedAdmin()},
	} {
		t.Run(who.name, func(t *testing.T) {
			// /auth/me is the least interesting endpoint there is, which is the
			// point: the rule is not about the endpoint, so pick one that is
			// always mounted and cannot answer 404 over the real assertion.
			got := f.do(call{method: http.MethodGet, path: "/auth/me",
				cookie: who.cookie, actAs: f.other.agentID.String()})
			if got.code != "AGENT_IMPERSONATION_NOT_ALLOWED" {
				t.Errorf("code = %q status %d, want AGENT_IMPERSONATION_NOT_ALLOWED; body %.200s",
					got.code, got.status, got.body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Revocation is terminal: the key stops working at once, and stops leaving a
// "last used" trace behind it.
// ---------------------------------------------------------------------------

func TestARevokedKeyStopsAuthenticatingAndStopsBeingTouched(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	id, secret := f.issueKey(admin, "history:read:all")

	// Use it once so last_used_at has something in it.
	if got := f.do(call{method: http.MethodGet, path: "/cdrs", bearer: secret}); got.status != http.StatusOK {
		t.Fatalf("the key should work before revocation: %d %s", got.status, got.code)
	}
	before := f.lastUsedAt(id)

	if got := f.do(call{method: http.MethodPost, path: "/api-keys/" + id + "/revoke",
		cookie: admin}); got.status != http.StatusOK {
		t.Fatalf("revoke: status %d body %.200s", got.status, got.body)
	}

	got := f.do(call{method: http.MethodGet, path: "/cdrs", bearer: secret})
	if got.status != http.StatusUnauthorized {
		t.Errorf("status = %d (%s), want 401 — REVOKED is terminal", got.status, got.code)
	}
	if after := f.lastUsedAt(id); after != before {
		t.Errorf("last_used_at moved from %q to %q; a refused request is not a use", before, after)
	}
}

func (f *fixture) lastUsedAt(id string) string {
	f.t.Helper()
	var v string
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT coalesce(last_used_at::text, '') FROM api_keys WHERE id = $1`, id).Scan(&v); err != nil {
		f.t.Fatalf("read last_used_at: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------
// 6. An agent cannot listen to somebody else's call recording.
// ---------------------------------------------------------------------------

func TestAnAgentCannotHearSomebodyElsesCall(t *testing.T) {
	f := newFixture(t)
	recordingID := f.insertRecordingFor(f.other.agentID)

	got := f.do(call{method: http.MethodGet, path: "/recordings/" + recordingID + "/audio",
		cookie: f.agent.cookie})
	if got.status != http.StatusForbidden {
		t.Errorf("status = %d (%s), want 403 — the call was not theirs", got.status, got.code)
	}
}

// ---------------------------------------------------------------------------
// 7. A key without the scope the operation declares.
// ---------------------------------------------------------------------------

func TestAKeyMissingTheScopeIsToldWhichWayItFailed(t *testing.T) {
	f := newFixture(t)
	_, secret := f.issueKey(f.seedAdmin(), "agent:read") // no history:read:all

	got := f.do(call{method: http.MethodGet, path: "/cdrs", bearer: secret})
	if got.code != "INSUFFICIENT_SCOPE" {
		t.Errorf("code = %q status %d, want INSUFFICIENT_SCOPE — not FORBIDDEN, which says "+
			"nothing about what to fix; body %.200s", got.code, got.status, got.body)
	}
}

// ---------------------------------------------------------------------------
// 8. The secret never reaches the database in clear, and never comes back out
// of a read endpoint.
// ---------------------------------------------------------------------------

func TestTheSecretIsReturnedOnceAndStoredNever(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	id, secret := f.issueKey(admin, "history:read:all")

	// No column holds it in clear.
	rows, err := f.st.Pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'api_keys'`)
	if err != nil {
		t.Fatalf("read the columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, c)
	}
	if len(columns) == 0 {
		t.Fatal("api_keys has no columns; the table is missing")
	}
	for _, c := range columns {
		var hit int
		if err := f.st.Pool.QueryRow(context.Background(),
			fmt.Sprintf(`SELECT count(*) FROM api_keys WHERE %s::text = $1`, c), secret).Scan(&hit); err != nil {
			continue // a column that will not compare cannot be holding it
		}
		if hit > 0 {
			t.Errorf("column %q holds the secret in clear", c)
		}
	}

	// The hash is a fixed-length SHA-256.
	var hashLen int
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT length(key_hash) FROM api_keys WHERE id = $1`, id).Scan(&hashLen); err != nil {
		t.Fatalf("read key_hash: %v", err)
	}
	if hashLen != 32 {
		t.Errorf("key_hash is %d bytes, want 32 (SHA-256)", hashLen)
	}

	// The read endpoint returns neither the secret nor the hash.
	for _, path := range []string{"/api-keys", "/api-keys/" + id} {
		got := f.do(call{method: http.MethodGet, path: path, cookie: admin})
		body := string(got.body)
		if strings.Contains(body, secret) {
			t.Errorf("GET %s returned the secret", path)
		}
		for _, word := range []string{"\"secret\"", "keyHash", "key_hash"} {
			if strings.Contains(body, word) {
				t.Errorf("GET %s carries %s", path, word)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 9. The event stream is open to a machine — everything live about this product
// arrives on that one stream.
// ---------------------------------------------------------------------------

func TestAKeyCanSubscribeToTheEventStream(t *testing.T) {
	f := newFixture(t)
	_, secret := f.issueKey(f.seedAdmin(), "calls:read:own")

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-AICC-Agent-ID", f.agent.agentID.String())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body %.200s", res.StatusCode, raw)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// ---------------------------------------------------------------------------
// Two direct writes, for seeding. Going around the API is deliberate: what is
// under test is the authorization of the read endpoints, not the write ones.
// ---------------------------------------------------------------------------

func (f *fixture) insertCDR() {
	f.t.Helper()
	_, err := f.st.Pool.Exec(context.Background(),
		`INSERT INTO cdrs (call_id, call_type, from_number, to_number,
		                   started_at, ended_at, status, primary_agent_id, agent_ids)
		 VALUES ($1, 'INBOUND', '18600000000', '95001', now() - interval '1 minute',
		         now(), 'ANSWERED', $2, ARRAY[$2]::uuid[])`,
		uuid.Must(uuid.NewV7()), f.agent.agentID)
	if err != nil {
		f.t.Fatalf("seed a CDR: %v", err)
	}
}

func (f *fixture) insertRecordingFor(agentID uuid.UUID) string {
	f.t.Helper()
	callID, recordingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := f.st.Pool.Exec(context.Background(),
		`INSERT INTO cdrs (call_id, call_type, from_number, to_number,
		                   started_at, ended_at, status, primary_agent_id, agent_ids)
		 VALUES ($1, 'INBOUND', '18600000001', '95001', now() - interval '1 minute',
		         now(), 'ANSWERED', $2, ARRAY[$2]::uuid[])`, callID, agentID); err != nil {
		f.t.Fatalf("seed a CDR: %v", err)
	}
	if _, err := f.st.Pool.Exec(context.Background(),
		`INSERT INTO recordings (id, call_id, backend, object_key, format)
		 VALUES ($1, $2, 'FS', 'test/one.wav', 'WAV')`, recordingID, callID); err != nil {
		f.t.Fatalf("seed a recording: %v", err)
	}
	return recordingID.String()
}

// dbDirectory resolves agents the way cmd/aicc does: out of the database.
type dbDirectory struct{ st *store.Store }

func (d dbDirectory) AgentIDForUser(r *http.Request, userID uuid.UUID) (uuid.UUID, error) {
	agent, err := d.st.Queries.GetAgentByUserID(r.Context(), userID)
	if err != nil {
		return uuid.Nil, err
	}
	return agent.ID, nil
}

func (d dbDirectory) QueuesForAgent(r *http.Request, agentID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.st.Queries.ListQueuesForAgent(r.Context(), agentID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids, nil
}

func (d dbDirectory) UserIDForAgent(r *http.Request, agentID uuid.UUID) (uuid.UUID, error) {
	agent, err := d.st.Queries.GetAgent(r.Context(), agentID)
	if err != nil {
		return uuid.Nil, err
	}
	return agent.UserID, nil
}

// ---------------------------------------------------------------------------
// Revocation is terminal, and terminal means nothing about the key changes after.
// ---------------------------------------------------------------------------

// The contract says "A revoked key cannot be edited"; the server edited them
// anyway. One PATCH rewrote a revoked key's capability from calls:* to
// users:write + keys:manage and answered 200.
//
// That is not cosmetic. An audit row says this key did something, and if the
// capability it records can be rewritten afterwards, rows from months ago
// describe a key that never existed — in the one table that must not be
// forgeable.
func TestARevokedKeyCannotBeEdited(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	id, _ := f.issueKey(admin, "history:read:all")

	if got := f.do(call{method: http.MethodPost, path: "/api-keys/" + id + "/revoke",
		cookie: admin}); got.status != http.StatusOK {
		t.Fatalf("revoke: status %d body %.200s", got.status, got.body)
	}

	got := f.do(call{method: http.MethodPatch, path: "/api-keys/" + id, cookie: admin,
		body: map[string]any{"name": "edited-after-revoke", "scopes": []string{"keys:manage"}}})
	if got.status != http.StatusConflict {
		t.Errorf("status = %d (%s), want 409 — the contract says a revoked key "+
			"cannot be edited; body %.200s", got.status, got.code, got.body)
	}

	// And nothing actually changed. Asserting the status code is not enough: an
	// implementation that writes first and errors afterwards would pass that.
	after := f.do(call{method: http.MethodGet, path: "/api-keys/" + id, cookie: admin})
	var key struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(after.body, &key); err != nil {
		t.Fatalf("read the key back: %v", err)
	}
	if key.Name == "edited-after-revoke" {
		t.Error("the name was rewritten anyway")
	}
	if slices.Contains(key.Scopes, "keys:manage") {
		t.Errorf("scopes = %v — a revoked key gained the ability to issue further keys, "+
			"and every audit row naming it now describes a key that never existed", key.Scopes)
	}
}
