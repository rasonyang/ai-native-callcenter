// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// answeredAt is optional on the wire: a call nobody on the far side answered
// leaves it out, and the contract says so. The assembler stopped writing the
// agent's own phone's auto-answer there (#330); this holds the other half, that
// a row stored without one reaches GET /cdrs/{callId} without the key (not a
// zero time), and one stored with one carries it.
func TestACallNobodyAnsweredHasNoAnsweredAtOnTheWire(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, secret := f.issueKey(f.seedAdmin(), "history:read:all")

	started := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	row := func(callType string, answeredAfter time.Duration, status string) store.CDR {
		cdr := store.CDR{
			CallID: uuid.Must(uuid.NewV7()), StartedAt: started, EndedAt: started.Add(46 * time.Second),
			CallType: callType, FromNumber: "1008", ToNumber: "1002", Status: status,
		}
		if answeredAfter > 0 {
			cdr.AnsweredAt = started.Add(answeredAfter)
		}
		return cdr
	}

	for _, tc := range []struct {
		name string
		cdr  store.CDR
		want *time.Time
	}{
		{"1 click-to-dial that reached nobody", row("OUTBOUND", 0, store.CDRStatusNoAnswer), nil},
		{"8 1002 answered at 6 s", row("INTERNAL", 6*time.Second, store.CDRStatusAnswered), ptr(started.Add(6 * time.Second))},
		{"11 the bot answered an agent's call at 1.5 s", row("OUTBOUND", 1500*time.Millisecond, store.CDRStatusAnswered), ptr(started.Add(1500 * time.Millisecond))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.st.Ledger().InsertCDR(ctx, tc.cdr); err != nil {
				t.Fatalf("InsertCDR: %v", err)
			}
			got := f.do(call{method: http.MethodGet, path: "/cdrs/" + tc.cdr.CallID.String(), bearer: secret})
			if got.status != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200; body %.200s", got.status, got.code, got.body)
			}
			// The detail wraps the row under "cdr". Looking for the key one
			// level up would find it absent on every row and pass the first.
			var envelope struct {
				CDR map[string]json.RawMessage `json:"cdr"`
			}
			if err := json.Unmarshal(got.body, &envelope); err != nil || envelope.CDR == nil {
				t.Fatalf("decode: %v; body %.200s", err, got.body)
			}
			fields := envelope.CDR
			if _, ok := fields["callId"]; !ok {
				t.Fatalf("the row has no callId: %.200s", got.body)
			}
			raw, present := fields["answeredAt"]
			if tc.want == nil {
				if present {
					t.Errorf("answeredAt = %s, want the key absent", raw)
				}
				return
			}
			if !present {
				t.Fatalf("answeredAt is absent, want %v", *tc.want)
			}
			var at time.Time
			if err := json.Unmarshal(raw, &at); err != nil || !at.Equal(*tc.want) {
				t.Errorf("answeredAt = %s (%v), want %v", raw, err, *tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
