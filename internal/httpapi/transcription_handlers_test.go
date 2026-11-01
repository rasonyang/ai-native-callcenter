// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/postcall"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type fakeTranscriber struct {
	run store.CallTranscription
	err error
}

func (f fakeTranscriber) Request(_ context.Context, callID uuid.UUID) (store.CallTranscription, error) {
	r := f.run
	r.CallID = callID
	return r, f.err
}

// seedTranscription gives a call of the agent's a finished run with two
// segments, written straight to the tables.
func (f *fixture) seedTranscription(agentID uuid.UUID) uuid.UUID {
	f.t.Helper()
	callID, recID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	ctx := context.Background()
	if _, err := f.st.Pool.Exec(ctx,
		`INSERT INTO cdrs (call_id, call_type, from_number, to_number, started_at, ended_at, status, primary_agent_id, agent_ids)
		 VALUES ($1, 'INBOUND', '1', '95001', now() - interval '1 minute', now(), 'ANSWERED', $2, ARRAY[$2]::uuid[])`,
		callID, agentID); err != nil {
		f.t.Fatal(err)
	}
	l := f.st.Ledger()
	if _, err := l.EnqueueTranscription(ctx, callID, recID, 61); err != nil {
		f.t.Fatal(err)
	}
	run, _, _ := l.ClaimTranscription(ctx, "fake-asr")
	if ok, err := l.CompleteTranscription(ctx, callID, run.Run, []store.TranscriptionSegment{
		{Seq: 1, Channel: store.ChannelCustomer, OffsetMs: 100, EndMs: 900, Text: "hello", Language: "en"},
		{Seq: 2, Channel: store.ChannelFarEnd, OffsetMs: 1000, EndMs: 1800, Text: "welcome"},
	}); err != nil || !ok {
		f.t.Fatal(ok, err)
	}
	return callID
}

func TestReadingAPostCallTranscription(t *testing.T) {
	f := newFixture(t)
	callID := f.seedTranscription(f.agent.agentID)
	path := "/calls/" + callID.String() + "/transcription"

	// The agent who took the call reads it.
	got := f.do(call{method: http.MethodGet, path: path, cookie: f.agent.cookie})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (%s) body %.300s", got.status, got.code, got.body)
	}
	var body struct {
		Status   string `json:"status"`
		Model    string `json:"model"`
		AudioSec int    `json:"audioSec"`
		Segments []struct {
			Seq      int    `json:"seq"`
			Channel  string `json:"channel"`
			OffsetMs int    `json:"offsetMs"`
			EndMs    int    `json:"endMs"`
			Text     string `json:"text"`
			Language string `json:"language"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "DONE" || body.Model != "fake-asr" || body.AudioSec != 61 || len(body.Segments) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if s := body.Segments[1]; s.Channel != "FAR_END" || s.OffsetMs != 1000 || s.EndMs != 1800 || s.Text != "welcome" || s.Language != "" {
		t.Errorf("segment 2 = %+v", s)
	}

	// A colleague does not.
	if got := f.do(call{method: http.MethodGet, path: path, cookie: f.other.cookie}); got.status != http.StatusForbidden {
		t.Errorf("another agent: status %d, want 403", got.status)
	}

	// A system with history:read:all does.
	_, secret := f.issueKey(f.seedAdmin(), "history:read:own", "history:read:all")
	if got := f.do(call{method: http.MethodGet, path: path, bearer: secret}); got.status != http.StatusOK {
		t.Errorf("key with history:read:all: status %d (%s)", got.status, got.code)
	}

	// A call that has none is NOT_FOUND, and so is one that does not exist.
	empty := f.do(call{method: http.MethodGet, path: "/calls/" + uuid.NewString() + "/transcription", bearer: secret})
	if empty.status != http.StatusNotFound || empty.code != "NOT_FOUND" {
		t.Errorf("no run: status %d (%s)", empty.status, empty.code)
	}
}

// While the run has not finished the segments are empty, not absent.
func TestAPendingRunServesNoSegments(t *testing.T) {
	f := newFixture(t)
	callID := uuid.Must(uuid.NewV7())
	if _, err := f.st.Ledger().EnqueueTranscription(context.Background(), callID, uuid.Must(uuid.NewV7()), 30); err != nil {
		t.Fatal(err)
	}
	_, secret := f.issueKey(f.seedAdmin(), "history:read:own", "history:read:all")
	got := f.do(call{method: http.MethodGet, path: "/calls/" + callID.String() + "/transcription", bearer: secret})
	if got.status != http.StatusOK {
		t.Fatalf("status %d", got.status)
	}
	var body struct {
		Status   string          `json:"status"`
		Segments json.RawMessage `json:"segments"`
	}
	_ = json.Unmarshal(got.body, &body)
	if body.Status != "PENDING" || string(body.Segments) != "[]" {
		t.Errorf("body = %s", got.body)
	}
}

func TestRequestingAPostCallTranscription(t *testing.T) {
	callID := uuid.Must(uuid.NewV7()).String()
	path := "/calls/" + callID + "/transcription"

	t.Run("a deployment with it off says so", func(t *testing.T) {
		f := newFixture(t)
		_, secret := f.issueKey(f.seedAdmin(), "quality:review")
		got := f.do(call{method: http.MethodPost, path: path, bearer: secret})
		if got.status != http.StatusConflict || got.code != "TRANSCRIPTION_DISABLED" {
			t.Errorf("status %d (%s), want 409 TRANSCRIPTION_DISABLED", got.status, got.code)
		}
	})

	t.Run("the scope is quality:review, for a key and a session alike", func(t *testing.T) {
		f := newFixture(t, func(d *Deps) { d.PostCall = fakeTranscriber{} })
		admin := f.seedAdmin()
		_, readOnly := f.issueKey(admin, "history:read:own", "history:read:all")
		if got := f.do(call{method: http.MethodPost, path: path, bearer: readOnly}); got.code != "INSUFFICIENT_SCOPE" {
			t.Errorf("a read-only key: status %d (%s)", got.status, got.code)
		}
		// An agent's session does not carry quality:review.
		if got := f.do(call{method: http.MethodPost, path: path, cookie: f.agent.cookie}); got.status != http.StatusForbidden {
			t.Errorf("an agent session: status %d (%s), want 403", got.status, got.code)
		}
		if got := f.do(call{method: http.MethodPost, path: path, cookie: admin}); got.status != http.StatusAccepted {
			t.Errorf("an administrator's session: status %d (%s)", got.status, got.code)
		}
	})

	cases := []struct {
		name   string
		t      fakeTranscriber
		status int
		code   string
	}{
		{"queued", fakeTranscriber{run: store.CallTranscription{Status: store.TranscriptionPending, Run: 2, RequestedAt: time.Now()}}, http.StatusAccepted, ""},
		{"no recording", fakeTranscriber{err: postcall.ErrNoRecording}, http.StatusNotFound, "NOT_FOUND"},
		{"too long", fakeTranscriber{err: postcall.ErrTooLong}, http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		{"daily cap", fakeTranscriber{err: postcall.ErrCapReached}, http.StatusTooManyRequests, "RATE_LIMITED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(d *Deps) { d.PostCall = tc.t })
			_, secret := f.issueKey(f.seedAdmin(), "quality:review")
			got := f.do(call{method: http.MethodPost, path: path, bearer: secret})
			if got.status != tc.status || got.code != tc.code {
				t.Fatalf("status %d (%s), want %d (%s); body %.200s", got.status, got.code, tc.status, tc.code, got.body)
			}
			if tc.status == http.StatusAccepted {
				var body struct {
					Status   string            `json:"status"`
					CallID   string            `json:"callId"`
					Segments []json.RawMessage `json:"segments"`
				}
				_ = json.Unmarshal(got.body, &body)
				if body.Status != "PENDING" || body.CallID != callID || body.Segments == nil {
					t.Errorf("body = %s", got.body)
				}
			}
		})
	}
}
