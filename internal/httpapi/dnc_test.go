// SPDX-License-Identifier: Apache-2.0

package httpapi

// The do-not-call list at the HTTP boundary, against a real PostgreSQL (the
// numbers are compared as stored, canonical E.164, so the SQL is part of what
// is under test) and the real outbound service with a switch that counts what
// it is told. The switch is the line that matters for "no call was placed": a
// refusal that still sent an originate would be a refusal in name only.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// stubDNC is the list for the tests that only need the routes mounted.
type stubDNC struct{}

func (stubDNC) Add(context.Context, string, string, *uuid.UUID, *uuid.UUID) (store.DNCNumber, bool, error) {
	return store.DNCNumber{}, true, nil
}
func (stubDNC) Remove(context.Context, string) error { return nil }
func (stubDNC) List(context.Context, store.DNCFilter) ([]store.DNCNumber, int64, error) {
	return nil, 0, nil
}

// countingSwitch is the switch as the dialer sees it: it records every
// originate and accepts everything.
type countingSwitch struct {
	mu         sync.Mutex
	originates int
}

func (s *countingSwitch) Originate(uuid.UUID, string, map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.originates++
	return "job", nil
}
func (*countingSwitch) BridgeToEndpoint(string, uuid.UUID, string, map[string]string) error {
	return nil
}
func (*countingSwitch) TransferToExtension(string, string, string) error { return nil }
func (*countingSwitch) Hangup(string, string) error                      { return nil }
func (*countingSwitch) SetVariable(string, string, string) error         { return nil }
func (*countingSwitch) Endpoint(ext string) string                       { return "user/" + ext + "@test" }

func (s *countingSwitch) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.originates
}

// dncStack is the whole chain a dial passes through, on a database of its own:
// the list's endpoints, the real dialer in front of a counting switch, and the
// real ledger the dialer asks whether a call already exists.
type dncStack struct {
	t      *testing.T
	st     *store.Store
	sw     *countingSwitch
	server *Server
}

func newDNCStack(t *testing.T, countryCode string) *dncStack {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "dnc"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	flowID := uuid.New()
	sw := &countingSwitch{}
	svc := outbound.New(
		outbound.Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", DefaultCountryCode: countryCode},
		sw,
		didList{
			{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowInbound: true},
			{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
		},
		nil,
		st.Ledger().HasCDR,
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).WithDNC(st.DNC())
	return &dncStack{
		t: t, st: st, sw: sw,
		server: &Server{
			cfg:      config.Config{DefaultCountryCode: countryCode},
			outbound: svc, dnc: st.DNC(),
			agents: dialerPresence{}, agentDir: dialerDirectory{},
		},
	}
}

type didList []catalog.DID

func (d didList) DIDs(context.Context) ([]catalog.DID, error) { return d, nil }

// add posts a number the way a client would and returns the status and body.
func (s *dncStack) add(ac AuthContext, number string) (int, []byte) {
	s.t.Helper()
	raw, _ := json.Marshal(map[string]string{"number": number})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/dnc-numbers", strings.NewReader(string(raw)))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.server.CreateDNCNumber(w, r)
	return w.Code, w.Body.Bytes()
}

func (s *dncStack) remove(number string) int {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/dnc-numbers/"+url.PathEscape(number), nil)
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{}))
	w := httptest.NewRecorder()
	s.server.DeleteDNCNumber(w, r, number)
	return w.Code
}

// dial places a call of the given kind and returns the status and error code.
func (s *dncStack) dial(ac AuthContext, body string) (int, string, uuid.UUID) {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.server.CreateCall(w, r)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		CallID uuid.UUID `json:"callId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Error.Code, env.CallID
}

func (s *dncStack) hasCDR(id uuid.UUID) bool {
	s.t.Helper()
	ok, err := s.st.Ledger().HasCDR(context.Background(), id)
	if err != nil {
		s.t.Fatalf("HasCDR: %v", err)
	}
	return ok
}

// A list entry typed in one format has to stop a dial written in another, on
// every path that places a call, and it has to stop it before anything exists:
// no originate, no ledger row.
func TestADNCNumberStopsEveryCallToItInWhateverFormat(t *testing.T) {
	s := newDNCStack(t, "86")
	person := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), ActorUserID: uuid.New()}

	if code, body := s.add(person, "+86 138-0013-8000"); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, body := s.add(person, "0086 139 1234 5678"); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}

	for _, tc := range []struct {
		name string
		ac   AuthContext
		body string
	}{
		{"AI, national form", keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"13800138000","callId":"%s"}`},
		{"AI, plus form", keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"+8613800138000","callId":"%s"}`},
		{"AI, double-zero form of a plus entry", keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"008613912345678","callId":"%s"}`},
		{"click-to-dial by an agent, national", agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"13800138000","callId":"%s"}`},
		{"click-to-dial by an agent, plus form of a 00 entry", agentSubject(), `{"to":"+8613912345678","callId":"%s"}`},
		{"click-to-dial by a system naming the phone", keySubject(), `{"kind":"AGENT_OUTBOUND","extensionNumber":"1009","to":"+8613800138000","callId":"%s"}`},
	} {
		id := uuid.New()
		status, code, _ := s.dial(tc.ac, fmt.Sprintf(tc.body, id))
		if status != http.StatusUnprocessableEntity || code != "NUMBER_ON_DNC" {
			t.Errorf("%s: %d %s, want 422 NUMBER_ON_DNC", tc.name, status, code)
		}
		if s.hasCDR(id) {
			t.Errorf("%s: a CDR exists for a call that was refused", tc.name)
		}
	}
	if n := s.sw.count(); n != 0 {
		t.Errorf("the switch was told to originate %d times for numbers on the list", n)
	}

	// A number nobody listed still dials, so the refusals above are the list's
	// doing and not a dial path that is simply broken.
	if status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"13700137000"}`); status != http.StatusCreated {
		t.Fatalf("an unlisted number: %d %s, want 201", status, code)
	}
	if s.sw.count() != 1 {
		t.Errorf("originates = %d, want the one unlisted call", s.sw.count())
	}
}

// Taking a number off the list makes it dialable at once.
func TestARemovedNumberDialsAgain(t *testing.T) {
	s := newDNCStack(t, "86")
	person := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), ActorUserID: uuid.New()}
	s.add(person, "13800138000")

	body := `{"kind":"AI_OUTBOUND","did":"95012","to":"+8613800138000"}`
	if status, code, _ := s.dial(keySubject(), body); status != http.StatusUnprocessableEntity || code != "NUMBER_ON_DNC" {
		t.Fatalf("while listed: %d %s", status, code)
	}
	if got := s.remove("+8613800138000"); got != http.StatusNoContent {
		t.Fatalf("remove: %d", got)
	}
	if got := s.remove("+8613800138000"); got != http.StatusNotFound {
		t.Errorf("removing it twice: %d, want 404", got)
	}
	if got := s.remove("13800138000"); got != http.StatusUnprocessableEntity {
		t.Errorf("removing by a non-canonical path: %d, want 422", got)
	}
	status, code, id := s.dial(keySubject(), body)
	if status != http.StatusCreated {
		t.Fatalf("after removal: %d %s, want 201", status, code)
	}
	if s.sw.count() != 1 || id == uuid.Nil {
		t.Errorf("originates = %d, id = %s, want the call to have gone out", s.sw.count(), id)
	}
}

// The gap the deployment setting exists for, written down as behaviour: with
// no default country code a national number cannot be completed, so it is not
// matched and the call goes through. A number with a plus is checked anyway.
func TestWithoutACountryCodeOnlyTheCompleteFormIsChecked(t *testing.T) {
	s := newDNCStack(t, "")
	person := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), ActorUserID: uuid.New()}
	if code, body := s.add(person, "+8613800138000"); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, body := s.add(person, "13800138000"); code != http.StatusUnprocessableEntity {
		t.Errorf("a national entry with no country code to complete it: %d %s, want 422", code, body)
	}

	if status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"+8613800138000"}`); status != http.StatusUnprocessableEntity || code != "NUMBER_ON_DNC" {
		t.Errorf("plus form: %d %s, want 422 NUMBER_ON_DNC", status, code)
	}
	if status, code, _ := s.dial(keySubject(), `{"kind":"AI_OUTBOUND","did":"95012","to":"13800138000"}`); status != http.StatusCreated {
		t.Errorf("national form with no country code configured: %d %s, want the documented pass-through", status, code)
	}
}

// A list built from somebody's spreadsheet: thousands of numbers in whatever
// shape the sheet had them, repeated in other shapes, some not numbers at all.
// Added one by one, as an integration would, with the count checked against an
// independent tally.
func TestAMessyListLoadedOneByOne(t *testing.T) {
	s := newDNCStack(t, "86")
	person := AuthContext{Kind: SubjectUser, SubjectID: uuid.New(), ActorUserID: uuid.New()}
	rng := rand.New(rand.NewSource(20261012))

	render := func(national string) string {
		d := func(i int) string { return national[i:] }
		switch rng.Intn(7) {
		case 0:
			return national
		case 1:
			return "+86" + national
		case 2:
			return "0086" + national
		case 3:
			return "+86 " + national[:3] + " " + national[3:7] + " " + national[7:]
		case 4:
			return "+86-" + national[:3] + "-" + national[3:7] + "-" + national[7:]
		case 5:
			return "(+86) " + national[:3] + "." + d(3)
		default:
			return "0" + national
		}
	}

	// 750 distinct mobile numbers; the other 250 rows repeat earlier ones.
	distinct := map[string]bool{}
	var invalidCount int
	invalid := []string{"", "   ", "abc", "+12", "+0123456789", "12-34", "+86 138 0013 80 00 00 00 00 00", "++8613800138000", "138 0013 80a0"}
	for i := 0; i < 1000; i++ {
		var row string
		var want string // canonical, or "" when the row is garbage
		switch {
		case i%9 == 8:
			row = invalid[rng.Intn(len(invalid))]
		case i >= 750:
			// Repeat something already loaded, written another way.
			var pick string
			n := rng.Intn(len(distinct))
			for c := range distinct {
				if n == 0 {
					pick = c
					break
				}
				n--
			}
			row, want = render(strings.TrimPrefix(pick, "+86")), pick
		default:
			national := fmt.Sprintf("1%02d%08d", 30+rng.Intn(60), rng.Intn(100000000))
			row, want = render(national), "+86"+national
		}

		code, body := s.add(person, row)
		switch {
		case want == "":
			invalidCount++
			var env struct {
				Error struct{ Params map[string]string }
			}
			_ = json.Unmarshal(body, &env)
			rule := env.Error.Params["rule"]
			if code != http.StatusUnprocessableEntity || (rule != "NUMBER_EMPTY" && rule != "NUMBER_INVALID") {
				t.Fatalf("row %d %q: %d %s, want 422 with a reason", i, row, code, body)
			}
		case distinct[want]:
			if code != http.StatusOK {
				t.Fatalf("row %d %q again: %d %s, want 200 (already listed)", i, row, code, body)
			}
		default:
			if code != http.StatusCreated {
				t.Fatalf("row %d %q: %d %s, want 201", i, row, code, body)
			}
			distinct[want] = true
		}
	}
	if invalidCount == 0 || len(distinct) < 500 {
		t.Fatalf("the generator is not exercising what it should: %d invalid rows, %d distinct", invalidCount, len(distinct))
	}

	// The list holds exactly the distinct numbers, and pages through whole.
	seen := map[string]bool{}
	for offset := 0; ; offset += 200 {
		items, total, err := s.st.DNC().List(context.Background(), store.DNCFilter{Limit: 200, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		if int(total) != len(distinct) {
			t.Fatalf("total = %d, want %d", total, len(distinct))
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if seen[it.Number] {
				t.Fatalf("%s listed twice across pages", it.Number)
			}
			seen[it.Number] = true
		}
	}
	if len(seen) != len(distinct) {
		t.Errorf("paged %d numbers, want %d", len(seen), len(distinct))
	}

	// A prefix narrows to what starts with it, against an independent count.
	wantPrefix := 0
	for n := range distinct {
		if strings.HasPrefix(n, "+86138") {
			wantPrefix++
		}
	}
	_, total, err := s.st.DNC().List(context.Background(), store.DNCFilter{Prefix: "+86138", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if int(total) != wantPrefix {
		t.Errorf("prefix +86138: total = %d, want %d", total, wantPrefix)
	}

	// And every one of them is refused when dialled, in the form a person
	// would dial it.
	checked := 0
	for n := range distinct {
		if checked++; checked > 40 {
			break
		}
		body := fmt.Sprintf(`{"kind":"AI_OUTBOUND","did":"95012","to":%q}`, strings.TrimPrefix(n, "+86"))
		if status, code, _ := s.dial(keySubject(), body); status != http.StatusUnprocessableEntity || code != "NUMBER_ON_DNC" {
			t.Fatalf("dialling listed %s: %d %s", n, status, code)
		}
	}
	if s.sw.count() != 0 {
		t.Errorf("%d originates for listed numbers", s.sw.count())
	}
}

// Who may do what, through the real router and real credentials. The list is
// a compliance control: an agent neither reads nor edits it.
func TestOnlyScopedCredentialsReachTheDNCList(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	_, readKey := f.issueKey(admin, "dnc:read")
	_, writeKey := f.issueKey(admin, "dnc:write")

	add := func(c call) reply { c.method, c.path = http.MethodPost, "/dnc-numbers"; return f.do(c) }

	if got := add(call{cookie: f.agent.cookie, body: map[string]string{"number": "+8613800138000"}}); got.status != http.StatusForbidden || got.code != "INSUFFICIENT_SCOPE" {
		t.Errorf("an agent adding: %d %s, want 403 INSUFFICIENT_SCOPE", got.status, got.code)
	}
	if got := f.do(call{method: http.MethodGet, path: "/dnc-numbers", cookie: f.agent.cookie}); got.status != http.StatusForbidden {
		t.Errorf("an agent reading: %d, want 403", got.status)
	}
	if got := add(call{bearer: readKey, body: map[string]string{"number": "+8613800138000"}}); got.status != http.StatusForbidden {
		t.Errorf("a read-only key adding: %d, want 403", got.status)
	}

	created := add(call{cookie: admin, body: map[string]string{"number": "+8613800138000"}})
	if created.status != http.StatusCreated {
		t.Fatalf("an administrator adding: %d %s", created.status, created.body)
	}
	var entry api.DNCNumber
	if err := json.Unmarshal(created.body, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Source != api.DNCSourceMANUAL || entry.CreatedBy == nil {
		t.Errorf("entry = %+v, want MANUAL and the account that added it", entry)
	}

	// A key has no account behind it, so the row has no author; the audit
	// trail is where the key is named.
	keyed := add(call{bearer: writeKey, body: map[string]string{"number": "+8613900139000", "source": "IMPORT"}})
	if keyed.status != http.StatusCreated {
		t.Fatalf("a write key adding: %d %s", keyed.status, keyed.body)
	}
	var keyedEntry api.DNCNumber
	_ = json.Unmarshal(keyed.body, &keyedEntry)
	if keyedEntry.Source != api.DNCSourceIMPORT || keyedEntry.CreatedBy != nil {
		t.Errorf("keyed entry = %+v, want IMPORT and no author", keyedEntry)
	}
	if got := add(call{bearer: writeKey, body: map[string]string{"number": "+8613900139000", "source": "CONVERSATION"}}); got.status != http.StatusBadRequest {
		t.Errorf("a client claiming CONVERSATION: %d, want 400", got.status)
	}

	// Adding again is idempotent: 200, and the first author stays.
	again := add(call{cookie: admin, body: map[string]string{"number": "+86 138 0013 8000", "source": "IMPORT"}})
	if again.status != http.StatusOK {
		t.Fatalf("adding twice: %d %s, want 200", again.status, again.body)
	}
	var existing api.DNCNumber
	_ = json.Unmarshal(again.body, &existing)
	if existing.Source != api.DNCSourceMANUAL || existing.Number != "+8613800138000" {
		t.Errorf("the existing entry was rewritten: %+v", existing)
	}

	list := f.do(call{method: http.MethodGet, path: "/dnc-numbers?prefix=%2B8613800", bearer: readKey})
	var page api.DNCNumberList
	if err := json.Unmarshal(list.body, &page); err != nil || list.status != http.StatusOK {
		t.Fatalf("list: %d %s", list.status, list.body)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Number != "+8613800138000" {
		t.Errorf("list by prefix = %+v, want just +8613800138000", page)
	}

	// The + of the number is encoded in the path; a read key cannot remove.
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/%2B8613800138000", bearer: readKey}); got.status != http.StatusForbidden {
		t.Errorf("a read key removing: %d, want 403", got.status)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/%2B8613800138000", bearer: writeKey}); got.status != http.StatusNoContent {
		t.Errorf("a write key removing (encoded +): %d %s, want 204", got.status, got.body)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/+8613900139000", cookie: admin}); got.status != http.StatusNoContent {
		t.Errorf("an administrator removing (literal +): %d %s, want 204", got.status, got.body)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/%2B8613900139000", cookie: admin}); got.status != http.StatusNotFound {
		t.Errorf("removing a number that is gone: %d, want 404", got.status)
	}

	// The audit trail names the credential for each change.
	var n int
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'POST /api/v1/dnc-numbers' AND subject_kind = 'API_KEY'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no audit row names the key that added a number")
	}
}
