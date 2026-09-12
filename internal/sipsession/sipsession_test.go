// SPDX-License-Identifier: Apache-2.0

package sipsession

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the test recomputes the same RFC 2617 A1.
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]store.SIPSession
	getErr  error
	putErr  error
	purged  int64
	upserts int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]store.SIPSession{}}
}

func (f *fakeStore) Get(_ context.Context, agentID uuid.UUID) (store.SIPSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.SIPSession{}, f.getErr
	}
	row, ok := f.rows[agentID]
	if !ok {
		return store.SIPSession{}, store.ErrNoSIPSession
	}
	return row, nil
}

func (f *fakeStore) Upsert(_ context.Context, agentID uuid.UUID, extension, a1Hash string, expiresAt time.Time) (store.SIPSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return store.SIPSession{}, f.putErr
	}
	f.upserts++
	row := store.SIPSession{
		AgentID: agentID, Extension: extension, A1Hash: a1Hash,
		CreatedAt: time.Now(), ExpiresAt: expiresAt,
	}
	f.rows[agentID] = row
	return row, nil
}

func (f *fakeStore) Delete(_ context.Context, agentID uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[agentID]
	if !ok {
		return "", store.ErrNoSIPSession
	}
	delete(f.rows, agentID)
	return row.Extension, nil
}

func (f *fakeStore) PurgeExpired(context.Context) (int64, error) { return f.purged, nil }

type fakeDirectory struct{ extension string }

func (d fakeDirectory) BoundExtensionFor(context.Context, uuid.UUID) string { return d.extension }

// fakeSwitch records the commands it was asked for, in the shape the telephony
// adapter would have sent them.
type fakeSwitch struct {
	mu       sync.Mutex
	commands []string
	up       bool
	err      error
}

func (f *fakeSwitch) FlushRegistration(profile, extension string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, fmt.Sprintf("sofia profile %s flush_inbound_reg %s", profile, extension))
	return f.err
}

func (f *fakeSwitch) IsUp() bool { return f.up }

func (f *fakeSwitch) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// newService wires a Service over fakes and captures everything it logs, so a
// test can assert on what did and did not reach a log line.
func newService(t *testing.T, st Store, dir Directory, sw Switch) (*Service, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := Config{SIPDomain: "aicc.test", WSSURL: "wss://aicc.test:7443/", Profile: "internal"}
	return New(st, dir, sw, cfg, logger, nil), &logged
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestIssueMintsACredentialThePhoneCanRegisterWith(t *testing.T) {
	st := newFakeStore()
	sw := &fakeSwitch{up: true}
	svc, _ := newService(t, st, fakeDirectory{extension: "1001"}, sw)

	agentID := uuid.New()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	issued, err := svc.Issue(context.Background(), agentID, expires)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if issued.Account != "1001" {
		t.Errorf("Account = %q, want the bound extension", issued.Account)
	}
	if issued.SIPDomain != "aicc.test" || issued.WSSURL != "wss://aicc.test:7443/" {
		t.Errorf("the phone was told %q / %q", issued.SIPDomain, issued.WSSURL)
	}
	if !issued.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", issued.ExpiresAt, expires)
	}
	if !hexHash.MatchString(issued.A1Hash) {
		t.Fatalf("A1Hash = %q, want 32 lower-case hex characters", issued.A1Hash)
	}

	// What was stored is exactly what was handed out, and nothing else was.
	row := st.rows[agentID]
	if row.A1Hash != issued.A1Hash {
		t.Errorf("stored hash %q, issued %q", row.A1Hash, issued.A1Hash)
	}
	if row.Extension != "1001" {
		t.Errorf("stored extension %q", row.Extension)
	}

	// Nothing to flush on a first issue: there was no previous registration.
	if got := sw.sent(); len(got) != 0 {
		t.Errorf("flushed %v on a first sign-in", got)
	}
}

// The password is fresh every time, so nothing about the credential is
// derivable from the extension or the agent. Two phones at the same number
// must not be able to answer each other's challenge.
func TestEveryCredentialIsItsOwnSecret(t *testing.T) {
	st := newFakeStore()
	svc, _ := newService(t, st, fakeDirectory{extension: "1001"}, &fakeSwitch{up: true})

	seen := map[string]bool{}
	for range 64 {
		issued, err := svc.Issue(context.Background(), uuid.New(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if seen[issued.A1Hash] {
			t.Fatalf("the same credential was issued twice: %s", issued.A1Hash)
		}
		seen[issued.A1Hash] = true
	}
}

// One session per agent: the second issue replaces the row and ends the
// binding the first was holding, exactly once and at the right number.
func TestASecondIssueReplacesTheFirstAndFlushesIt(t *testing.T) {
	st := newFakeStore()
	sw := &fakeSwitch{up: true}
	dir := &mutableDirectory{extension: "1001"}
	svc, _ := newService(t, st, dir, sw)

	agentID := uuid.New()
	first, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("first Issue: %v", err)
	}

	// The agent's phone is rebound between sign-ins, so the number to flush is
	// the one the old session held, not the one the new one is for.
	dir.extension = "1002"
	second, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("second Issue: %v", err)
	}

	if first.A1Hash == second.A1Hash {
		t.Error("the second session reissued the first credential")
	}
	if st.upserts != 2 || len(st.rows) != 1 {
		t.Errorf("upserts=%d rows=%d, want one row replaced", st.upserts, len(st.rows))
	}
	if st.rows[agentID].A1Hash != second.A1Hash {
		t.Error("the stored row is not the credential that was handed out")
	}

	want := []string{"sofia profile internal flush_inbound_reg 1001"}
	if got := sw.sent(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("flushed %v, want %v", got, want)
	}
}

type mutableDirectory struct{ extension string }

func (d *mutableDirectory) BoundExtensionFor(context.Context, uuid.UUID) string { return d.extension }

func TestIssueRefusesAnAgentWithNoPhone(t *testing.T) {
	svc, _ := newService(t, newFakeStore(), fakeDirectory{extension: ""}, &fakeSwitch{up: true})

	_, err := svc.Issue(context.Background(), uuid.New(), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrNoExtensionBound) {
		t.Fatalf("err = %v, want ErrNoExtensionBound", err)
	}
}

// A switch that cannot be reached must not stop somebody signing in. The stale
// registration it leaves behind expires on its own; an agent who cannot get a
// phone at all does not.
func TestASwitchThatIsDownStillLetsASessionBeIssued(t *testing.T) {
	st := newFakeStore()
	sw := &fakeSwitch{up: false}
	svc, logged := newService(t, st, fakeDirectory{extension: "1001"}, sw)

	agentID := uuid.New()
	if _, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if _, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second Issue: %v", err)
	}

	if got := sw.sent(); len(got) != 0 {
		t.Errorf("commanded a switch that is down: %v", got)
	}
	if !strings.Contains(logged.String(), "the switch is not reachable") {
		t.Error("a registration that could not be flushed was not reported")
	}
}

func TestRevokeEndsTheSessionAndTheRegistration(t *testing.T) {
	st := newFakeStore()
	sw := &fakeSwitch{up: true}
	svc, _ := newService(t, st, fakeDirectory{extension: "1001"}, sw)

	agentID := uuid.New()
	if _, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := svc.Revoke(context.Background(), agentID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(st.rows) != 0 {
		t.Error("the row survived the revocation")
	}
	want := "sofia profile internal flush_inbound_reg 1001"
	if got := sw.sent(); len(got) != 1 || got[0] != want {
		t.Errorf("flushed %v, want [%s]", got, want)
	}

	// Idempotent: revoking again asks for a state that already holds.
	if err := svc.Revoke(context.Background(), agentID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	if got := sw.sent(); len(got) != 1 {
		t.Errorf("a second revocation flushed again: %v", got)
	}
}

// The password is a local variable and the hash is a credential. Neither
// belongs in a log line, and this is the assertion that keeps it that way.
func TestNeitherThePasswordNorTheHashIsEverLogged(t *testing.T) {
	st := newFakeStore()
	sw := &fakeSwitch{up: true}
	svc, logged := newService(t, st, fakeDirectory{extension: "1001"}, sw)

	agentID := uuid.New()
	issued, err := svc.Issue(context.Background(), agentID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := svc.Revoke(context.Background(), agentID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	out := logged.String()
	if out == "" {
		t.Fatal("nothing was logged at all, so this proves nothing")
	}
	if strings.Contains(out, issued.A1Hash) {
		t.Error("the a1-hash reached a log line")
	}
	// No 32-hex run of any kind: a hash logged under another name is the same
	// leak, and a uuid does not match this shape (it carries dashes).
	if m := regexp.MustCompile(`\b[0-9a-f]{32}\b`).FindString(out); m != "" {
		t.Errorf("a credential-shaped value reached a log line: %q", m)
	}
	// And nothing base64-ish long enough to be the password.
	if m := regexp.MustCompile(`\b[A-Za-z0-9_-]{32}\b`).FindString(out); m != "" {
		t.Errorf("a secret-shaped value reached a log line: %q", m)
	}
}

// Every hash that is stored is a well-formed A1. The table's CHECK enforces
// the same thing in PostgreSQL; this catches it before a migration has to.
func TestEveryStoredHashIsAWellFormedA1(t *testing.T) {
	st := newFakeStore()
	svc, _ := newService(t, st, fakeDirectory{extension: "1001"}, &fakeSwitch{up: true})

	for range 32 {
		if _, err := svc.Issue(context.Background(), uuid.New(), time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	for id, row := range st.rows {
		if !hexHash.MatchString(row.A1Hash) {
			t.Fatalf("agent %s holds %q, which is not md5 hex", id, row.A1Hash)
		}
	}
}

// The recipe itself, pinned: md5(account:realm:password) lower-case hex, which
// is what a registrar computes on its side. Getting the separator or the order
// wrong produces a hash of exactly the right shape that authenticates nothing.
func TestA1FollowsRFC2617(t *testing.T) {
	sum := md5.Sum([]byte("1001:aicc.test:swordfish")) //nolint:gosec // RFC 2617 A1.
	if got, want := a1Hash("1001", "aicc.test", "swordfish"), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("a1Hash = %q, want %q", got, want)
	}
}
