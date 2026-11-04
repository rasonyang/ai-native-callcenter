// SPDX-License-Identifier: Apache-2.0

package httpapi

// POST /calls and the callId (#400): optional, minted as a UUIDv7 when left
// out, and a UUIDv7 or a 422 when given. Run against the real outbound
// service, a switch that counts originates and a real ledger, because "nothing
// was dialled and nothing was written" is the claim under test.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type createCallAnswer struct {
	status int
	body   []byte
	Error  struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	} `json:"error"`
	CallID      uuid.UUID `json:"callId"`
	IsDuplicate bool      `json:"isDuplicate"`
}

func (s *dncStack) post(ac AuthContext, body string) createCallAnswer {
	s.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.server.CreateCall(w, r)
	a := createCallAnswer{status: w.Code, body: w.Body.Bytes()}
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		s.t.Fatalf("answer is not JSON: %v: %s", err, w.Body)
	}
	return a
}

func newCallIDStack(t *testing.T) *dncStack {
	s := newDNCStack(t, "86")
	s.server.ledger = s.st.Ledger()
	return s
}

func v7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// createCallKinds lists the three shapes a placement takes, each with a body template
// taking the callId member (empty or `,"callId":"…"`).
var createCallKinds = []struct {
	name string
	ac   func() AuthContext
	body string
}{
	{"AI", keySubject, `{"kind":"AI_OUTBOUND","did":"95012","to":"13700137000"%s}`},
	{"agent, naming the phone", keySubject, `{"kind":"AGENT_OUTBOUND","extensionNumber":"1009","to":"13700137000"%s}`},
	{"agent, plain click-to-dial", agentSubject, `{"to":"13700137000"%s}`},
}

func TestAV7CallIDIsPlacedAndItsRetryIsADuplicate(t *testing.T) {
	for _, k := range createCallKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newCallIDStack(t)
			id := v7(t)
			ac := k.ac()
			body := fmt.Sprintf(k.body, `,"callId":"`+strings.ToUpper(id.String())+`"`)
			first := s.post(ac, body)
			if first.status != http.StatusCreated || first.CallID != id {
				t.Fatalf("first: %d %s, want 201 with %s", first.status, first.body, id)
			}
			again := s.post(ac, body)
			if again.status != http.StatusOK || !again.IsDuplicate || again.CallID != id {
				t.Fatalf("retry: %d %s, want 200 isDuplicate for %s", again.status, again.body, id)
			}
			if n := s.sw.count(); n != 1 {
				t.Errorf("originates = %d, want 1", n)
			}
		})
	}
}

func TestACallIDThatIsNotV7IsRefusedBeforeAnythingHappens(t *testing.T) {
	for _, k := range createCallKinds {
		for _, c := range []struct {
			name string
			id   uuid.UUID
		}{
			{"v4", uuid.New()},
			{"v1", uuid.Must(uuid.NewUUID())},
			{"nil", uuid.Nil},
			{"v7 digits with a non-RFC variant", uuid.MustParse("01890000-0000-7000-c000-000000000000")},
		} {
			t.Run(k.name+"/"+c.name, func(t *testing.T) {
				s := newCallIDStack(t)
				a := s.post(k.ac(), fmt.Sprintf(k.body, `,"callId":"`+c.id.String()+`"`))
				assertCallIDRefused(t, a)
				if s.sw.count() != 0 {
					t.Errorf("the switch was told to originate %d times", s.sw.count())
				}
				if s.hasCDR(c.id) {
					t.Errorf("a CDR exists for the refused id")
				}
			})
		}
	}
}

func assertCallIDRefused(t *testing.T, a createCallAnswer) {
	t.Helper()
	if a.status != http.StatusUnprocessableEntity || a.Error.Code != "VALIDATION_FAILED" {
		t.Fatalf("%d %s, want 422 VALIDATION_FAILED", a.status, a.body)
	}
	if a.Error.Params["field"] != "callId" || a.Error.Params["rule"] != "UUID_V7" {
		t.Errorf("params = %v, want field=callId rule=UUID_V7", a.Error.Params)
	}
}

func TestACallWithoutACallIDGetsAV7(t *testing.T) {
	for _, k := range createCallKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newCallIDStack(t)
			a := s.post(k.ac(), fmt.Sprintf(k.body, ""))
			if a.status != http.StatusCreated {
				t.Fatalf("%d %s", a.status, a.body)
			}
			if a.CallID.Version() != 7 || a.CallID.Variant() != uuid.RFC4122 {
				t.Errorf("callId = %s (version %d), want a UUIDv7", a.CallID, a.CallID.Version())
			}
		})
	}
}

// claimedCallback is an OPEN callback claimed by the person who will dial it.
func claimedCallback(t *testing.T, st *store.Store) (store.Callback, AuthContext) {
	t.Helper()
	ctx := context.Background()
	userID := v7(t)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (id, username, password_hash, display_name, role) VALUES ($1, $2, 'x', 'cb', 'AGENT')`,
		userID, "cb-"+userID.String()); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	cb, err := st.Ledger().InsertCallback(ctx, nil, nil, "13700137000", "call me back")
	if err != nil {
		t.Fatalf("insert callback: %v", err)
	}
	if cb, err = st.Ledger().ClaimCallback(ctx, cb.ID, userID); err != nil {
		t.Fatalf("claim callback: %v", err)
	}
	ac := agentSubject()
	ac.ActorUserID = userID
	return cb, ac
}

func lastAttempt(t *testing.T, st *store.Store, id uuid.UUID) *uuid.UUID {
	t.Helper()
	cbs, _, err := st.Ledger().ListCallbacks(context.Background(), "", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cbs {
		if c.ID == id {
			return c.LastAttemptCallID
		}
	}
	t.Fatalf("callback %s not listed", id)
	return nil
}

func TestAKeptCallbackNeverSeesANonV7CallID(t *testing.T) {
	s := newCallIDStack(t)
	cb, ac := claimedCallback(t, s.st)
	for _, bad := range []uuid.UUID{uuid.New(), uuid.Nil} {
		a := s.post(ac, fmt.Sprintf(`{"to":"13700137000","callbackId":"%s","callId":"%s"}`, cb.ID, bad))
		assertCallIDRefused(t, a)
	}
	if got := lastAttempt(t, s.st, cb.ID); got != nil {
		t.Errorf("the callback was marked with attempt %s by a refused request", got)
	}
	if s.sw.count() != 0 {
		t.Errorf("originates = %d, want 0", s.sw.count())
	}
}

func TestAKeptCallbackWithoutACallIDIsMarkedWithAMintedV7(t *testing.T) {
	s := newCallIDStack(t)
	cb, ac := claimedCallback(t, s.st)
	a := s.post(ac, fmt.Sprintf(`{"to":"13700137000","callbackId":"%s"}`, cb.ID))
	if a.status != http.StatusCreated {
		t.Fatalf("%d %s", a.status, a.body)
	}
	if a.CallID.Version() != 7 {
		t.Fatalf("callId = %s (version %d), want a UUIDv7", a.CallID, a.CallID.Version())
	}
	got := lastAttempt(t, s.st, cb.ID)
	if got == nil || *got != a.CallID {
		t.Errorf("callback's last attempt = %v, want the minted %s", got, a.CallID)
	}
}
