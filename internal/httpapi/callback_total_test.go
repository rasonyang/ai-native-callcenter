// SPDX-License-Identifier: Apache-2.0

package httpapi

// listCallbacks answers a total (#469): the rows matching the status and
// callId filters, whatever the page, so a screen can page the whole queue.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type callbackPage struct {
	Items []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"items"`
	Total *int `json:"total"`
}

func TestListCallbacksTotalsTheFilterNotThePage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.seedAdmin()
	_, key := f.issueKey(admin, "calls:read:own")

	// 12 OPEN, 5 CLAIMED, 3 DONE, 2 DISMISSED; one OPEN row carries a call id.
	wantByStatus := map[string]int{"OPEN": 12, "CLAIMED": 5, "DONE": 3, "DISMISSED": 2}
	filedBy := uuid.Must(uuid.NewV7())
	var filed *uuid.UUID
	n := 0
	for _, status := range []string{"OPEN", "CLAIMED", "DONE", "DISMISSED"} {
		for i := 0; i < wantByStatus[status]; i++ {
			var callID *uuid.UUID
			if status == "OPEN" && i == 0 {
				callID = &filedBy
				filed = callID
			}
			n++
			cb, err := f.st.Ledger().InsertCallback(ctx, callID, nil, fmt.Sprintf("+86138%08d", n), "msg")
			if err != nil {
				t.Fatal(err)
			}
			switch status {
			case "CLAIMED":
				_, err = f.st.Ledger().ClaimCallback(ctx, cb.ID, f.agent.userID)
			case "DONE", "DISMISSED":
				_, err = f.st.Ledger().HandleCallback(ctx, cb.ID, status, f.agent.userID)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	const all = 22

	get := func(t *testing.T, query, cookie, bearer string) callbackPage {
		t.Helper()
		r := f.in(t).do(call{method: http.MethodGet, path: "/callbacks" + query, cookie: cookie, bearer: bearer})
		if r.status != 200 {
			t.Fatalf("GET /callbacks%s: %d %s", query, r.status, r.body)
		}
		var page callbackPage
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		if page.Total == nil || page.Items == nil {
			t.Fatalf("GET /callbacks%s lacks total or items: %s", query, r.body)
		}
		return page
	}

	for status, want := range wantByStatus {
		t.Run(status, func(t *testing.T) {
			page := get(t, "?status="+status+"&limit=2", admin, "")
			if *page.Total != want {
				t.Errorf("total = %d, want %d", *page.Total, want)
			}
			if len(page.Items) != min(2, want) {
				t.Errorf("a page of 2 holds %d rows", len(page.Items))
			}
			for _, item := range page.Items {
				if item.Status != status {
					t.Errorf("filter %s returned a %s row", status, item.Status)
				}
			}
		})
	}

	t.Run("no status", func(t *testing.T) {
		if page := get(t, "?limit=5", admin, ""); *page.Total != all || len(page.Items) != 5 {
			t.Errorf("total %d with %d items, want %d with 5", *page.Total, len(page.Items), all)
		}
	})
	t.Run("offset past the end", func(t *testing.T) {
		if page := get(t, "?offset=500", admin, ""); *page.Total != all || len(page.Items) != 0 {
			t.Errorf("total %d with %d items, want %d with none", *page.Total, len(page.Items), all)
		}
	})
	t.Run("limit zero is the default page", func(t *testing.T) {
		if page := get(t, "?limit=0", admin, ""); *page.Total != all || len(page.Items) != all {
			t.Errorf("total %d with %d items, want %d with %d", *page.Total, len(page.Items), all, all)
		}
	})
	t.Run("a call that filed one", func(t *testing.T) {
		page := get(t, "?callId="+filed.String(), admin, "")
		if *page.Total != 1 || len(page.Items) != 1 {
			t.Errorf("total %d with %d items, want 1", *page.Total, len(page.Items))
		}
	})
	t.Run("a call that filed none", func(t *testing.T) {
		page := get(t, "?callId="+uuid.Must(uuid.NewV7()).String(), admin, "")
		if *page.Total != 0 || len(page.Items) != 0 {
			t.Errorf("total %d with %d items, want 0", *page.Total, len(page.Items))
		}
	})
	t.Run("a key reads what a session reads", func(t *testing.T) {
		for _, query := range []string{"", "?status=CLAIMED", "?limit=3&offset=2"} {
			s, k := get(t, query, admin, ""), get(t, query, "", key)
			if *s.Total != *k.Total || len(s.Items) != len(k.Items) {
				t.Errorf("%q: session %d/%d, key %d/%d", query, *s.Total, len(s.Items), *k.Total, len(k.Items))
			}
		}
	})
}
