// SPDX-License-Identifier: Apache-2.0

package httpapi

// A callback names who handled it (#469): the response of every operation that
// returns one, and the list, carry handledByName for an agent, for an
// administrator and for a user who is not an agent; a handler the database no
// longer knows has none.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type namedCallback struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	HandledBy     string  `json:"handledBy"`
	HandledByName *string `json:"handledByName"`
}

func TestCallbacksNameTheirHandler(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.seedAdmin()
	_, key := f.issueKey(admin, "calls:read:own")

	insert := func() uuid.UUID {
		cb, err := f.st.Ledger().InsertCallback(ctx, nil, nil, "+8619900000001", "[v469] ring me")
		if err != nil {
			t.Fatal(err)
		}
		return cb.ID
	}
	decode := func(t *testing.T, r reply) namedCallback {
		t.Helper()
		var cb namedCallback
		if err := json.Unmarshal(r.body, &cb); err != nil {
			t.Fatal(err)
		}
		return cb
	}

	// An agent claims it: the claim answers with the agent's name.
	claimed := insert()
	r := f.do(call{method: http.MethodPost, path: "/callbacks/" + claimed.String() + "/claim", cookie: f.agent.cookie})
	if r.status != http.StatusOK {
		t.Fatalf("claim: %d %s", r.status, r.body)
	}
	if cb := decode(t, r); cb.HandledByName == nil || *cb.HandledByName != f.agent.name {
		t.Errorf("claim answered handledByName %v, want %q", cb.HandledByName, f.agent.name)
	}

	// An administrator, who is not an agent, dismisses another.
	dismissed := insert()
	r = f.do(call{method: http.MethodPost, path: "/callbacks/" + dismissed.String() + "/complete", cookie: admin,
		body: map[string]string{"status": "DISMISSED"}})
	if r.status != http.StatusOK {
		t.Fatalf("complete: %d %s", r.status, r.body)
	}
	if cb := decode(t, r); cb.HandledByName == nil || *cb.HandledByName != "root" {
		t.Errorf("complete answered handledByName %v, want root", cb.HandledByName)
	}

	// A released callback has no handler and so no name.
	released := insert()
	f.do(call{method: http.MethodPost, path: "/callbacks/" + released.String() + "/claim", cookie: f.other.cookie})
	r = f.do(call{method: http.MethodPost, path: "/callbacks/" + released.String() + "/release", cookie: f.other.cookie})
	if r.status != http.StatusOK {
		t.Fatalf("release: %d %s", r.status, r.body)
	}
	if cb := decode(t, r); cb.HandledByName != nil || cb.HandledBy != "" {
		t.Errorf("a released callback names %q / %v", cb.HandledBy, cb.HandledByName)
	}

	// One held by a user the database has never heard of: a handler, no name.
	orphan := insert()
	if _, err := f.st.Ledger().ClaimCallback(ctx, orphan, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}

	list := func(t *testing.T, cookie, bearer string) map[string]namedCallback {
		t.Helper()
		r := f.in(t).do(call{method: http.MethodGet, path: "/callbacks?limit=50", cookie: cookie, bearer: bearer})
		if r.status != http.StatusOK {
			t.Fatalf("list: %d %s", r.status, r.body)
		}
		var page struct {
			Items []namedCallback `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		out := map[string]namedCallback{}
		for _, item := range page.Items {
			out[item.ID] = item
		}
		return out
	}
	check := func(t *testing.T, rows map[string]namedCallback) {
		t.Helper()
		f := f.in(t)
		for id, want := range map[uuid.UUID]string{claimed: f.agent.name, dismissed: "root"} {
			if got := rows[id.String()]; got.HandledByName == nil || *got.HandledByName != want {
				t.Errorf("%s: handledByName %v, want %q", id, got.HandledByName, want)
			}
		}
		if got := rows[orphan.String()]; got.HandledBy == "" || got.HandledByName != nil {
			t.Errorf("an unknown handler: handledBy %q, handledByName %v", got.HandledBy, got.HandledByName)
		}
		if got := rows[released.String()]; got.HandledBy != "" || got.HandledByName != nil {
			t.Errorf("an unhandled callback names %q / %v", got.HandledBy, got.HandledByName)
		}
	}
	t.Run("a session lists the names", func(t *testing.T) { check(t, list(t, admin, "")) })
	t.Run("a key lists the same names", func(t *testing.T) { check(t, list(t, "", key)) })
}
