// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// leadActions is the part of a call's actions record_lead reaches, wired to
// the real attempt store the way aicall's callActions is (Leads: st.Attempts()).
type leadActions struct {
	flow.Actions
	attempts *AttemptStore
	callID   uuid.UUID
}

func (a leadActions) RecordLead(ctx context.Context, r flow.LeadRequest) (flow.Result, error) {
	ok, err := a.attempts.SetLead(ctx, a.callID, campaign.LeadIntent(r.Intent), r.Notes)
	if err != nil || !ok {
		return flow.Failed("SAVE_FAILED", ""), nil
	}
	return flow.Succeeded(nil, ""), nil
}

const leadE2EFlow = `{
	"id": "lead-e2e", "specVersion": "v2", "initialNode": "talk",
	"global": {"persona": "You call people.", "alwaysAllowedTools": ["hangup"]},
	"nodes": {"talk": {"instruction": "Talk.", "tools": []}}
}`

// The whole path of a lead, from the model's tool call to the rows left behind
// when the CDR lands: Runtime.Dispatch -> SetLead -> InsertCDR's finish routine.
func TestARecordedLeadReachesTheContactTheCallbackAndTheDNCList(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Lead e2e", "5", 6, nil)
	start := ny(t, "2026-06-01 10:00")

	spec, err := flow.Load([]byte(leadE2EFlow))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// call runs one campaign call: the model's record_lead calls in order,
	// then the CDR. It returns the model's answers. It takes the subtest's t:
	// a FailNow on the parent from a subtest would end the parent and skip,
	// unreported, every later subtest.
	call := func(t *testing.T, contact campaign.Contact, calls ...string) (campaign.Attempt, []map[string]any) {
		t.Helper()
		a := e.begin(t, contact, start)
		rt := flow.NewRuntime(flow.NewEngine(spec, "en", nil, log),
			leadActions{attempts: e.attempts, callID: a.CallID}, flow.NewBackend(""), nil, log)
		rt.Deny("transfer_to_agent", "take_message")
		rt.OfferRecordLead(ctx)
		var answers []map[string]any
		for _, args := range calls {
			out, _ := rt.Dispatch(ctx, flow.ToolRecordLead, args)
			var decoded map[string]any
			if err := json.Unmarshal([]byte(out), &decoded); err != nil {
				t.Fatalf("answer %q: %v", out, err)
			}
			answers = append(answers, decoded)
		}
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
		return a, answers
	}
	callbacksOf := func(callID uuid.UUID) (n int, message string) {
		_ = e.st.Pool.QueryRow(ctx,
			`SELECT count(*), coalesce(max(message), '') FROM callbacks WHERE call_id=$1`, callID).Scan(&n, &message)
		return n, message
	}

	t.Run("HIGH files one callback with the notes", func(t *testing.T) {
		a, answers := call(t, contacts[0], `{"intent":"HIGH","notes":"send the offer"}`)
		if answers[0]["ok"] != "1" {
			t.Fatalf("answer = %v", answers[0])
		}
		if n, msg := callbacksOf(a.CallID); n != 1 || msg != "send the offer" {
			t.Errorf("callbacks = %d %q", n, msg)
		}
		if ct := e.contact(t, c, contacts[0].ID); ct.LeadIntent != campaign.LeadHigh || ct.LeadNotes != "send the offer" {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("HIGH then MEDIUM leaves MEDIUM and no callback", func(t *testing.T) {
		a, _ := call(t, contacts[1], `{"intent":"HIGH","notes":"yes"}`, `{"intent":"MEDIUM","notes":"later"}`)
		if n, _ := callbacksOf(a.CallID); n != 0 {
			t.Errorf("%d callbacks", n)
		}
		if ct := e.contact(t, c, contacts[1].ID); ct.LeadIntent != campaign.LeadMedium || ct.LeadNotes != "later" {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("MEDIUM then HIGH files exactly one callback", func(t *testing.T) {
		a, _ := call(t, contacts[2], `{"intent":"MEDIUM"}`, `{"intent":"HIGH","notes":"ok send it"}`)
		if n, msg := callbacksOf(a.CallID); n != 1 || msg != "ok send it" {
			t.Errorf("callbacks = %d %q", n, msg)
		}
	})
	t.Run("DO_NOT_CALL lists the number as CONVERSATION with the call id", func(t *testing.T) {
		a, _ := call(t, contacts[3], `{"intent":"DO_NOT_CALL","notes":"stop"}`)
		listed, _, err := e.st.DNC().List(ctx, DNCFilter{Prefix: contacts[3].Number})
		if err != nil || len(listed) != 1 || listed[0].Source != DNCSourceConversation ||
			listed[0].CallID == nil || *listed[0].CallID != a.CallID {
			t.Fatalf("dnc = %+v, %v", listed, err)
		}
		if ct := e.contact(t, c, contacts[3].ID); ct.Status != campaign.ContactDoNotCall {
			t.Errorf("contact status = %s", ct.Status)
		}
	})
	t.Run("an invalid intent is refused to the model and nothing is written", func(t *testing.T) {
		a, answers := call(t, contacts[4], `{"intent":"BANANAS","notes":"x"}`)
		if answers[0]["ok"] != "0" || !strings.Contains(answers[0]["error"].(string), "not a lead intent") {
			t.Errorf("answer = %v", answers[0])
		}
		got, err := e.attempts.Get(ctx, a.CallID)
		if err != nil || got.LeadIntent != "" {
			t.Errorf("attempt lead = %q, %v", got.LeadIntent, err)
		}
		if ct := e.contact(t, c, contacts[4].ID); ct.LeadIntent != "" {
			t.Errorf("contact lead = %q", ct.LeadIntent)
		}
	})
	t.Run("a call that never calls record_lead leaves NULL, not NONE", func(t *testing.T) {
		a, _ := call(t, contacts[5])
		var intent *string
		if err := e.st.Pool.QueryRow(ctx,
			`SELECT lead_intent FROM campaign_attempts WHERE call_id=$1`, a.CallID).Scan(&intent); err != nil || intent != nil {
			t.Errorf("attempt lead_intent = %v, %v, want NULL", intent, err)
		}
		if err := e.st.Pool.QueryRow(ctx,
			`SELECT lead_intent FROM campaign_contacts WHERE id=$1`, contacts[5].ID).Scan(&intent); err != nil || intent != nil {
			t.Errorf("contact lead_intent = %v, %v, want NULL", intent, err)
		}
	})
	t.Run("the contacts list filters by leadIntent over a mix", func(t *testing.T) {
		for intent, want := range map[campaign.LeadIntent]int{
			campaign.LeadHigh: 2, campaign.LeadMedium: 1, campaign.LeadDoNotCall: 1, campaign.LeadLow: 0,
		} {
			list, total, err := e.cs.ListContacts(ctx, c.ID, campaign.ContactFilter{LeadIntent: intent, Limit: 100})
			if err != nil || len(list) != want || int(total) != want {
				t.Errorf("%s: %d rows (total %d), %v, want %d", intent, len(list), total, err, want)
			}
		}
	})
}

// declaredLeadE2EFlow classifies the customer with a slot tool and closes in
// the same move, each closing phase declaring the lead (#352): the model never
// has to call record_lead.
const declaredLeadE2EFlow = `{
	"id": "declared-lead-e2e", "specVersion": "v2", "initialNode": "talk",
	"global": {"persona": "You call people."},
	"tools": {
		"said": {"kind": "slot", "slot": "said", "description": "What they said."},
		"classify": {"kind": "slot", "slot": "answer", "description": "Their answer.", "enum": ["yes", "stop", "no"]}
	},
	"nodes": {
		"talk": {"instruction": "Talk.", "tools": ["said", "classify"], "transitions": [
			{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "yes"}, "target": "close_high"},
			{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "stop"}, "target": "close_dnc"},
			{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "no"}, "target": "close_none"}
		]},
		"close_high": {"instruction": "Close.", "isTerminal": true, "lead": {"intent": "HIGH", "notes": "Agreed: {slots.said}"}},
		"close_dnc": {"instruction": "Close.", "isTerminal": true, "lead": {"intent": "DO_NOT_CALL", "notes": "{slots.said}"}},
		"close_none": {"instruction": "Close.", "isTerminal": true, "lead": {"intent": "NONE"}}
	}
}`

// A lead a phase declares takes the model's path: entering the phase ->
// SetLead -> InsertCDR's finish routine, which files the callback or lists the
// number from the final value, once.
func TestADeclaredLeadReachesTheContactTheCallbackAndTheDNCList(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Declared lead e2e", "6", 4, nil)
	start := ny(t, "2026-06-01 10:00")

	spec, err := flow.Load([]byte(declaredLeadE2EFlow))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// call runs one campaign call: the model's tool calls in order (name,
	// arguments), then the CDR, then a revision of it when isRevised. Every
	// call must succeed, except a record_lead made in a phase that declares
	// its own lead, which must be refused (#358). It takes the subtest's t, as
	// the other lead test's call does.
	call := func(t *testing.T, contact campaign.Contact, isRevised bool, calls ...[2]string) campaign.Attempt {
		t.Helper()
		a := e.begin(t, contact, start)
		rt := flow.NewRuntime(flow.NewEngine(spec, "en", nil, log),
			leadActions{attempts: e.attempts, callID: a.CallID}, flow.NewBackend(""), nil, log)
		rt.Deny("transfer_to_agent", "take_message")
		rt.OfferRecordLead(ctx)
		for _, tool := range calls {
			want := `"ok":"1"`
			if tool[0] == flow.ToolRecordLead && rt.IsLeadSetByPhase() {
				want = `"ok":"0"`
			}
			if out, _ := rt.Dispatch(ctx, tool[0], tool[1]); !strings.Contains(out, want) {
				t.Fatalf("%s %s: %s", tool[0], tool[1], out)
			}
		}
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
		if isRevised {
			if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(2*time.Minute))); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	callbacksOf := func(callID uuid.UUID) (n int, message string) {
		_ = e.st.Pool.QueryRow(ctx,
			`SELECT count(*), coalesce(max(message), '') FROM callbacks WHERE call_id=$1`, callID).Scan(&n, &message)
		return n, message
	}

	t.Run("HIGH files exactly one callback with the rendered notes, even when the CDR is revised", func(t *testing.T) {
		a := call(t, contacts[0], true,
			[2]string{"said", `{"value": "send it to me, \"today\" 好的"}`}, [2]string{"classify", `{"value": "yes"}`})
		if n, msg := callbacksOf(a.CallID); n != 1 || msg != `Agreed: send it to me, "today" 好的` {
			t.Errorf("callbacks = %d %q", n, msg)
		}
		if ct := e.contact(t, c, contacts[0].ID); ct.LeadIntent != campaign.LeadHigh {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("DO_NOT_CALL lists the number as CONVERSATION with the call id", func(t *testing.T) {
		a := call(t, contacts[1], false,
			[2]string{"said", `{"value": "stop calling"}`}, [2]string{"classify", `{"value": "stop"}`})
		listed, _, err := e.st.DNC().List(ctx, DNCFilter{Prefix: contacts[1].Number})
		if err != nil || len(listed) != 1 || listed[0].Source != DNCSourceConversation ||
			listed[0].CallID == nil || *listed[0].CallID != a.CallID {
			t.Fatalf("dnc = %+v, %v", listed, err)
		}
		if ct := e.contact(t, c, contacts[1].ID); ct.Status != campaign.ContactDoNotCall || ct.LeadNotes != "stop calling" {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("the model's HIGH, then the phase's NONE, files nothing", func(t *testing.T) {
		a := call(t, contacts[2], false,
			[2]string{flow.ToolRecordLead, `{"intent": "HIGH", "notes": "keen"}`}, [2]string{"classify", `{"value": "no"}`})
		if n, _ := callbacksOf(a.CallID); n != 0 {
			t.Errorf("%d callbacks", n)
		}
		if ct := e.contact(t, c, contacts[2].ID); ct.LeadIntent != campaign.LeadNone || ct.LeadNotes != "" {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("the phase's NONE, then the model's HIGH, which is refused, files nothing", func(t *testing.T) {
		a := call(t, contacts[3], false,
			[2]string{"classify", `{"value": "no"}`}, [2]string{flow.ToolRecordLead, `{"intent": "HIGH", "notes": "changed their mind"}`})
		if n, msg := callbacksOf(a.CallID); n != 0 {
			t.Errorf("callbacks = %d %q", n, msg)
		}
		if ct := e.contact(t, c, contacts[3].ID); ct.LeadIntent != campaign.LeadNone || ct.LeadNotes != "" {
			t.Errorf("contact = %+v", ct)
		}
	})
}
