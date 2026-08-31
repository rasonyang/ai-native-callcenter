// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// patchingCalls records the patch the handler built and answers with whatever
// the test wants back.
type patchingCalls struct {
	stubCalls
	got    map[string]any
	result map[string]any
	change telephony.UserDataChange
	err    error
}

func (c *patchingCalls) PatchUserData(_, _ uuid.UUID, patch map[string]any) (
	map[string]any, telephony.UserDataChange, error) {
	c.got = patch
	return c.result, c.change, c.err
}

func patchUserData(t *testing.T, calls *patchingCalls, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{calls: calls, agentDir: dialerDirectory{}}
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/calls/"+uuid.New().String()+"/user-data",
		strings.NewReader(body))
	r = r.WithContext(contextWithIdentity(r.Context(), auth.Identity{
		UserID: uuid.New(), Role: auth.RoleAgent,
	}, uuid.New()))
	w := httptest.NewRecorder()
	s.PatchUserData(w, r, uuid.New())
	return w
}

// A JSON null and an absent key are different instructions, and the wire is
// the only place that distinction can be lost: Go's zero value for a string is
// "" either way, so the patch travels as *string and nil means delete.
func TestANullInThePatchIsADeletionAndNotAnEmptyValue(t *testing.T) {
	calls := &patchingCalls{
		result: map[string]any{"orderId": "A-4471"},
		change: telephony.UserDataChange{Changed: []string{"orderId"}, Deleted: []string{"ticketId"}},
	}
	w := patchUserData(t, calls, `{"userData":{"orderId":"A-4471","ticketId":null,"note":""}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}

	if v, ok := calls.got["ticketId"]; !ok || v != nil {
		t.Errorf("ticketId reached the call as %#v, want a nil meaning delete", v)
	}
	if calls.got["note"] != "" {
		t.Errorf("an empty string became %#v; it is a value, not a deletion", calls.got["note"])
	}
	if calls.got["orderId"] != "A-4471" {
		t.Errorf("orderId = %#v", calls.got["orderId"])
	}

	var body struct {
		UserData    map[string]string `json:"userData"`
		ChangedKeys []string          `json:"changedKeys"`
		DeletedKeys []string          `json:"deletedKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.UserData["orderId"] != "A-4471" {
		t.Errorf("the answer carries %v, want the resulting data", body.UserData)
	}
	if len(body.ChangedKeys) != 1 || len(body.DeletedKeys) != 1 {
		t.Errorf("changed=%v deleted=%v", body.ChangedKeys, body.DeletedKeys)
	}
}

// The two lists are required by the contract, so they are [] and never null:
// a client iterating them should not have to check first.
func TestAPatchThatMovedNothingStillAnswersWithBothLists(t *testing.T) {
	calls := &patchingCalls{result: map[string]any{"orderId": "A-4471"}}
	w := patchUserData(t, calls, `{"userData":{"orderId":"A-4471"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if got := w.Body.String(); !strings.Contains(got, `"changedKeys":[]`) ||
		!strings.Contains(got, `"deletedKeys":[]`) {
		t.Errorf("body = %s, want empty arrays rather than nulls", got)
	}
}

func TestTheAnswerToAPatchSaysWhichKeysWereTheProblem(t *testing.T) {
	t.Run("a value too long is refused before the call is touched", func(t *testing.T) {
		calls := &patchingCalls{}
		w := patchUserData(t, calls,
			`{"userData":{"note":"`+strings.Repeat("x", userDataMaxValueBytes+1)+`"}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
		if calls.got != nil {
			t.Error("the call was asked to merge a patch the handler could already see was too big")
		}
	})

	t.Run("a call with no room answers 409 and names what would not fit", func(t *testing.T) {
		calls := &patchingCalls{
			change: telephony.UserDataChange{Dropped: []string{"mike", "zulu"}},
			err:    telephony.ErrUserDataWouldNotFit,
		}
		w := patchUserData(t, calls, `{"userData":{"alpha":"a","mike":"m","zulu":"z"}}`)
		// 409 rather than 400: the patch is well formed, the call is full.
		if w.Code != http.StatusConflict {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
		if body := w.Body.String(); !strings.Contains(body, "mike") || !strings.Contains(body, "zulu") {
			t.Errorf("body = %s, want the keys that would not fit named", body)
		}
		// alpha had room and will have again — naming it would send the client
		// looking for a problem that is not there.
		if strings.Contains(w.Body.String(), "alpha") {
			t.Errorf("body = %s, names a key that fitted", w.Body)
		}
	})

	t.Run("a call the agent is not on is not theirs to write to", func(t *testing.T) {
		calls := &patchingCalls{err: telephony.ErrNotCallParty}
		if w := patchUserData(t, calls, `{"userData":{"orderId":"A"}}`); w.Code != http.StatusForbidden {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("a call that does not exist", func(t *testing.T) {
		calls := &patchingCalls{err: telephony.ErrCallNotFound}
		if w := patchUserData(t, calls, `{"userData":{"orderId":"A"}}`); w.Code != http.StatusNotFound {
			t.Fatalf("http = %d: %s", w.Code, w.Body)
		}
	})
}
