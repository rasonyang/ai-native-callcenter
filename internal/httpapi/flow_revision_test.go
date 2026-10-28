// SPDX-License-Identifier: Apache-2.0

package httpapi

// Reading an earlier revision and restoring it (#363), on a real PostgreSQL
// and the real routing tree. Restoring is getFlowRevision followed by the
// ordinary draft save, and the ordinary publish puts it live: these tests
// drive exactly those calls, the ones an integration would make.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// The history of NovaNet's outage line, 30 publishes long.
const (
	outageRevisions = 30
	// transferRevision could hand the caller to an engineer. It went live
	// before the outage campaign existed; publishing it again would give a
	// campaign call a way to a person.
	transferRevision = 2
	// refusedRevision was stored by an older version: a misspelt line and a
	// tool nobody declared, both of which today's loader refuses.
	refusedRevision = 18
	// restoredRevision is the one these tests take back.
	restoredRevision = 5
)

// outageFlow is revision n of the outage line. Every publish moved the
// credit it announces.
func outageFlow(n int) map[string]any {
	greet := map[string]any{
		"instruction": map[string]any{
			"en": "Answer questions about the Harbour District outage and the credit for it.",
			"zh": "回答关于海港区停网和补偿的问题。",
		},
		"announce": map[string]any{
			"en": fmt.Sprintf("Thanks for calling NovaNet. Fibre in Harbour District is back, and %d days of service have been credited to your account.", n),
			"zh": fmt.Sprintf("感谢致电 NovaNet。海港区光纤已恢复，已为您的账户补偿 %d 天服务费。", n),
		},
		"tools": []any{},
		"transitions": []any{
			map[string]any{"on": "TOOL_RESULT", "tool": "hangup", "target": "farewell"},
		},
	}
	nodes := map[string]any{
		"greet": greet,
		"farewell": map[string]any{
			"instruction": map[string]any{"en": "Say goodbye.", "zh": "道别。"},
			"announce":    map[string]any{"en": "Thank you for calling NovaNet, goodbye.", "zh": "感谢致电 NovaNet，再见。"},
			"isTerminal":  true,
		},
	}
	switch n {
	case transferRevision:
		greet["tools"] = []any{"transfer_to_agent"}
		greet["transitions"] = append(greet["transitions"].([]any),
			map[string]any{"on": "TOOL_RESULT", "tool": "transfer_to_agent", "target": "handoff"})
		nodes["handoff"] = map[string]any{
			"instruction": map[string]any{"en": "Tell the caller an engineer is taking the call.", "zh": "告知来电者将由工程师接听。"},
			"announce":    map[string]any{"en": "Putting you through to an engineer now.", "zh": "正在为您转接工程师。"},
			"isTerminal":  true,
		}
	case refusedRevision:
		greet["anounce"] = greet["announce"]
		delete(greet, "announce")
		greet["tools"] = []any{"lookup_outage_ticket"}
	}
	return map[string]any{
		"id": "novanet_outage", "specVersion": "v2", "initialNode": "greet",
		"global": map[string]any{
			"persona": map[string]any{
				"en": "You are NovaNet Broadband's outage line for Harbour District.",
				"zh": "你是 NovaNet 宽带海港区的故障热线。",
			},
			"rules": map[string]any{
				"en": []any{"This is a phone call: keep each reply to one or two short sentences."},
				"zh": []any{"这是电话语音：每次只说一两句短句。"},
			},
			"closingTarget":      "farewell",
			"alwaysAllowedTools": []any{"hangup"},
			"maxDurationSec":     600,
		},
		"nodes": nodes,
	}
}

// noteTwoHundred is a note exactly as long as a note may be.
var noteTwoHundred = strings.Repeat("Credit wording agreed with the Harbour District field team after the second fibre cut. ", 3)[:200]

func outageNote(n int) string {
	switch n {
	case 1:
		return ""
	case transferRevision:
		return "Callers can ask for an engineer"
	case 7:
		return noteTwoHundred
	case 12:
		return "海港区补偿改为十二天，按客服组意见调整话术"
	case refusedRevision:
		return "从旧版本导入"
	case 24:
		return "Holiday wording / 节日话术"
	default:
		return fmt.Sprintf("Credit set to %d days", n)
	}
}

func specBytesOf(t *testing.T, spec map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encode spec: %v", err)
	}
	return raw
}

// outageHistory is the fixture: the flow, its 30 revisions oldest first, what
// each one was written as, and a DRAFT campaign dialling from a number the
// flow answers.
type outageHistory struct {
	flowID     uuid.UUID
	revisions  []uuid.UUID // revisions[n-1] is revision n
	written    map[uuid.UUID][]byte
	campaignID string
	admin      string
}

func buildOutageHistory(t *testing.T, f *fixture) outageHistory {
	t.Helper()
	ctx := context.Background()
	flows := f.st.Flows()
	h := outageHistory{written: map[uuid.UUID][]byte{}, admin: f.seedAdmin()}

	if noteTwoHundred != strings.TrimSpace(noteTwoHundred) || utf8.RuneCountInString(noteTwoHundred) != 200 {
		t.Fatalf("the long note is %d characters, want 200", utf8.RuneCountInString(noteTwoHundred))
	}

	id, err := flows.Create(ctx, "novanet-outage", "Outage line", specBytesOf(t, outageFlow(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h.flowID = id
	// Each revision is known by the id its publish created, never by its
	// place in the history: that order is the database clock's.
	for n := 1; n <= outageRevisions; n++ {
		spec := specBytesOf(t, outageFlow(n))
		revisionID := uuid.Must(uuid.NewV7())
		if n == refusedRevision {
			// Straight into the table, as an older version stored and
			// published it.
			if _, err := f.st.Pool.Exec(ctx,
				`INSERT INTO flow_revisions (id, flow_id, spec, note) VALUES ($1, $2, $3, $4)`,
				revisionID, id, spec, outageNote(n)); err != nil {
				t.Fatalf("write revision %d: %v", n, err)
			}
			if _, err := f.st.Pool.Exec(ctx,
				`UPDATE flows SET published_revision_id = $2, published_at = now() WHERE id = $1`,
				id, revisionID); err != nil {
				t.Fatalf("point at revision %d: %v", n, err)
			}
		} else {
			if n > 1 {
				if err := flows.UpdateDraft(ctx, id, "Outage line", spec); err != nil {
					t.Fatalf("draft %d: %v", n, err)
				}
			}
			if err := flows.Publish(ctx, id, outageNote(n)); err != nil {
				t.Fatalf("publish %d: %v", n, err)
			}
			live, err := flows.Summary(ctx, id)
			if err != nil || live.PublishedRevisionID == nil {
				t.Fatalf("publish %d left nothing live: %+v %v", n, live, err)
			}
			revisionID = *live.PublishedRevisionID
		}
		h.revisions = append(h.revisions, revisionID)
		h.written[revisionID] = spec
	}

	detail, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Revisions) != outageRevisions {
		t.Fatalf("revisions = %d, want %d", len(detail.Revisions), outageRevisions)
	}

	// The campaign is a DRAFT on a number the flow answers. It could be
	// created because revision 30, which is live, cannot transfer.
	did, err := f.st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: "95363", Language: "zh", FlowID: &id,
		IsEnabled: true, AllowInbound: true, AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("create did: %v", err)
	}
	h.campaignID = f.createCampaign(h.admin, did.ID, nil).ID
	return h
}

func (h outageHistory) revision(n int) uuid.UUID { return h.revisions[n-1] }

// number is which publish a revision was, 1 to 30.
func (h outageHistory) number(t *testing.T, revisionID uuid.UUID) int {
	t.Helper()
	n := slices.Index(h.revisions, revisionID)
	if n < 0 {
		t.Fatalf("revision %s is not one the fixture wrote", revisionID)
	}
	return n + 1
}

func (h outageHistory) path(revisionID uuid.UUID) string {
	return "/flows/" + h.flowID.String() + "/revisions/" + revisionID.String()
}

// revisionReply is getFlowRevision's body with the spec left as JSON.
type revisionReply struct {
	RevisionID  uuid.UUID       `json:"revisionId"`
	Note        string          `json:"note"`
	CreatedAt   time.Time       `json:"createdAt"`
	IsPublished bool            `json:"isPublished"`
	Spec        json.RawMessage `json:"spec"`
}

func readRevision(t *testing.T, f *fixture, h outageHistory, revisionID uuid.UUID) revisionReply {
	t.Helper()
	got := f.do(call{method: http.MethodGet, path: h.path(revisionID), cookie: h.admin})
	if got.status != http.StatusOK {
		t.Fatalf("GET revision %s = %d: %s", revisionID, got.status, got.body)
	}
	var out revisionReply
	if err := json.Unmarshal(got.body, &out); err != nil {
		t.Fatalf("decode revision: %v: %s", err, got.body)
	}
	return out
}

type flowReply struct {
	Flow struct {
		PublishedRevisionID   uuid.UUID `json:"publishedRevisionId"`
		HasUnpublishedChanges bool      `json:"hasUnpublishedChanges"`
	} `json:"flow"`
	DraftSpec json.RawMessage   `json:"draftSpec"`
	Revisions []revisionSummary `json:"revisions"`
}

type revisionSummary struct {
	RevisionID  uuid.UUID `json:"revisionId"`
	Note        string    `json:"note"`
	CreatedAt   time.Time `json:"createdAt"`
	IsPublished bool      `json:"isPublished"`
}

func readFlow(t *testing.T, f *fixture, h outageHistory) flowReply {
	t.Helper()
	got := f.do(call{method: http.MethodGet, path: "/flows/" + h.flowID.String(), cookie: h.admin})
	if got.status != http.StatusOK {
		t.Fatalf("GET flow = %d: %s", got.status, got.body)
	}
	var out flowReply
	if err := json.Unmarshal(got.body, &out); err != nil {
		t.Fatalf("decode flow: %v", err)
	}
	return out
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

// Every revision is served with the spec it stored, the refused one included,
// and the fields the history lists. A revision is found only through its own
// flow; reading needs config:read and nothing more.
func TestEveryRevisionIsServedWithTheSpecItStored(t *testing.T) {
	f := newFixture(t)
	h := buildOutageHistory(t, f)
	ctx := context.Background()

	listed := readFlow(t, f, h)
	if len(listed.Revisions) != outageRevisions {
		t.Fatalf("listed revisions = %d, want %d", len(listed.Revisions), outageRevisions)
	}
	for _, summary := range listed.Revisions {
		n := h.number(t, summary.RevisionID)
		got := readRevision(t, f, h, summary.RevisionID)
		var stored []byte
		if err := f.st.Pool.QueryRow(ctx, `SELECT spec FROM flow_revisions WHERE id = $1`,
			summary.RevisionID).Scan(&stored); err != nil {
			t.Fatalf("read revision %d: %v", n, err)
		}
		if !sameJSON(t, got.Spec, stored) || !sameJSON(t, got.Spec, h.written[summary.RevisionID]) {
			t.Errorf("revision %d spec = %s, want what was stored", n, got.Spec)
		}
		if got.RevisionID != summary.RevisionID || got.Note != summary.Note ||
			!got.CreatedAt.Equal(summary.CreatedAt) || got.IsPublished != summary.IsPublished {
			t.Errorf("revision %d = %+v, want what the history lists: %+v", n, got, summary)
		}
		if got.Note != outageNote(n) {
			t.Errorf("revision %d note = %q, want %q", n, got.Note, outageNote(n))
		}
		if got.IsPublished != (n == outageRevisions) {
			t.Errorf("revision %d isPublished = %v", n, got.IsPublished)
		}
	}
	refused := readRevision(t, f, h, h.revision(refusedRevision))
	for _, want := range []string{`"anounce"`, `"lookup_outage_ticket"`} {
		if !strings.Contains(string(refused.Spec), want) {
			t.Errorf("the refused revision lost %s: %s", want, refused.Spec)
		}
	}

	// Another flow and its revision, to ask for through the wrong flow.
	billing, err := f.st.Flows().Create(ctx, "novanet-billing", "Billing", []byte(validSpecBody))
	if err != nil {
		t.Fatalf("create the other flow: %v", err)
	}
	if err := f.st.Flows().Publish(ctx, billing, "billing"); err != nil {
		t.Fatalf("publish the other flow: %v", err)
	}
	billingDetail, err := f.st.Flows().Detail(ctx, billing)
	if err != nil {
		t.Fatalf("detail of the other flow: %v", err)
	}
	for _, tc := range []struct {
		name, path string
		status     int
		code       string
	}{
		{"a revision that does not exist", h.path(uuid.Must(uuid.NewV7())), http.StatusNotFound, "NOT_FOUND"},
		{"another flow's revision", h.path(billingDetail.Revisions[0].ID), http.StatusNotFound, "NOT_FOUND"},
		{"a flow that does not exist",
			"/flows/" + uuid.Must(uuid.NewV7()).String() + "/revisions/" + h.revision(1).String(),
			http.StatusNotFound, "NOT_FOUND"},
		{"a revision id that is not one", "/flows/" + h.flowID.String() + "/revisions/0199e1c0",
			http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantReply(t, "GET", f.do(call{method: http.MethodGet, path: tc.path, cookie: h.admin}), tc.status, tc.code)
		})
	}

	// config:read reads, through a key as through a session; nothing else does.
	_, reader := f.issueKey(h.admin, "config:read")
	_, other := f.issueKey(h.admin, "history:read:all", "campaigns:read")
	wantReply(t, "a config:read key", f.do(call{method: http.MethodGet, path: h.path(h.revision(3)), bearer: reader}),
		http.StatusOK, "")
	wantReply(t, "a key without config:read", f.do(call{method: http.MethodGet, path: h.path(h.revision(3)), bearer: other}),
		http.StatusForbidden, "INSUFFICIENT_SCOPE")
	if got := f.do(call{method: http.MethodGet, path: h.path(h.revision(3)), cookie: f.agent.cookie}); got.status != http.StatusForbidden {
		t.Errorf("an agent's session: %d %s, want 403", got.status, got.body)
	}
}

// Restoring is the read and the ordinary draft save; the ordinary publish puts
// it live and appends to the history without rewriting any of it. A revision
// the loader refuses is refused as a draft with its problems, and one that can
// transfer saves as a draft but does not publish while the campaign is a
// DRAFT. Both saves are in the audit trail as draft saves.
func TestARevisionIsRestoredThroughTheDraftAndTheUsualPublish(t *testing.T) {
	f := newFixture(t)
	h := buildOutageHistory(t, f)
	ctx := context.Background()
	flowPath := "/flows/" + h.flowID.String()

	before := map[uuid.UUID]revisionReply{}
	for _, id := range h.revisions {
		before[id] = readRevision(t, f, h, id)
	}
	put := func(spec json.RawMessage) reply {
		return f.do(call{method: http.MethodPut, path: flowPath, cookie: h.admin,
			body: map[string]any{"name": "Outage line", "spec": spec}})
	}

	// Restore revision 5.
	five := readRevision(t, f, h, h.revision(restoredRevision))
	restored := put(five.Spec)
	wantReply(t, "restore to the draft", restored, http.StatusOK, "")
	var flowAfterPut struct {
		HasUnpublishedChanges bool      `json:"hasUnpublishedChanges"`
		PublishedRevisionID   uuid.UUID `json:"publishedRevisionId"`
	}
	if err := json.Unmarshal(restored.body, &flowAfterPut); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !flowAfterPut.HasUnpublishedChanges || flowAfterPut.PublishedRevisionID != h.revision(outageRevisions) {
		t.Errorf("after the restore: %+v, want unpublished changes over revision 30", flowAfterPut)
	}
	if draft := readFlow(t, f, h).DraftSpec; !sameJSON(t, draft, h.written[h.revision(restoredRevision)]) {
		t.Errorf("draft = %s, want revision 5", draft)
	}

	// The draft save is audited as one, against the flow.
	var targetKind, targetID, request string
	if err := f.st.Pool.QueryRow(ctx,
		`SELECT target_kind, target_id, coalesce(detail->>'request', '') FROM audit_logs
		 WHERE action = 'PUT /api/v1/flows/{flowId}' ORDER BY id DESC LIMIT 1`).Scan(&targetKind, &targetID, &request); err != nil {
		t.Fatalf("the draft save left no audit row: %v", err)
	}
	if targetKind != "flow" || targetID != h.flowID.String() || !strings.Contains(request, "5 days of service") {
		t.Errorf("audit row = %s %s %.120s, want the flow and the restored spec", targetKind, targetID, request)
	}

	// The usual publish appends revision 31, which is revision 5 again.
	note := "Restore of " + h.revision(restoredRevision).String()[:8]
	wantReply(t, "publish", f.do(call{method: http.MethodPost, path: flowPath + "/publish", cookie: h.admin,
		body: map[string]any{"note": note}}), http.StatusOK, "")
	after := readFlow(t, f, h)
	if len(after.Revisions) != outageRevisions+1 {
		t.Fatalf("revisions = %d, want %d", len(after.Revisions), outageRevisions+1)
	}
	added := slices.DeleteFunc(slices.Clone(after.Revisions), func(r revisionSummary) bool {
		_, existed := before[r.RevisionID]
		return existed
	})
	if len(added) != 1 {
		t.Fatalf("the publish added %d revisions, want 1", len(added))
	}
	newest := added[0]
	if !newest.IsPublished || newest.Note != note ||
		after.Flow.PublishedRevisionID != newest.RevisionID || after.Flow.HasUnpublishedChanges {
		t.Errorf("the publish did not append a live revision: %+v, flow %+v", newest, after.Flow)
	}
	if got := readRevision(t, f, h, newest.RevisionID); !sameJSON(t, got.Spec, five.Spec) {
		t.Errorf("revision 31 = %s, want revision 5", got.Spec)
	}
	for n, id := range h.revisions {
		was, now := before[id], readRevision(t, f, h, id)
		was.IsPublished = false
		if !sameJSON(t, now.Spec, was.Spec) || now.Note != was.Note || !now.CreatedAt.Equal(was.CreatedAt) ||
			now.RevisionID != was.RevisionID || now.IsPublished {
			t.Errorf("revision %d changed: %+v, was %+v", n+1, now, before[id])
		}
	}

	// The refused revision: 422 naming both problems, and the draft is still
	// revision 5.
	old := readRevision(t, f, h, h.revision(refusedRevision))
	refused := put(old.Spec)
	wantReply(t, "restore the refused revision", refused, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	var body struct {
		Error struct {
			Params struct {
				Problems []string `json:"problems"`
			} `json:"params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(refused.body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{
		`phase "greet" has unknown field "anounce"; did you mean "announce"?`,
		`phase "greet" allows unknown tool "lookup_outage_ticket"`,
	} {
		if !slices.Contains(body.Error.Params.Problems, want) {
			t.Errorf("problems = %q, lack %q", body.Error.Params.Problems, want)
		}
	}
	if draft := readFlow(t, f, h).DraftSpec; !sameJSON(t, draft, five.Spec) {
		t.Errorf("a refused restore changed the draft: %s", draft)
	}

	// The revision that can transfer: a draft is free to say anything, but
	// it does not go live while the DRAFT campaign dials with this flow.
	transfer := readRevision(t, f, h, h.revision(transferRevision))
	wantReply(t, "restore the transfer revision", put(transfer.Spec), http.StatusOK, "")
	blocked := f.do(call{method: http.MethodPost, path: flowPath + "/publish", cookie: h.admin,
		body: map[string]any{"note": "Restore of " + h.revision(transferRevision).String()[:8]}})
	wantReply(t, "publish the transfer revision", blocked, http.StatusConflict, "CAMPAIGN_FLOW_HAS_TRANSFER")
	var refusal errView
	if err := json.Unmarshal(blocked.body, &refusal); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if refusal.Error.Params["rule"] != "PUBLISH_BLOCKED" || refusal.Error.Params["campaigns"] != float64(1) {
		t.Errorf("params = %v, want rule PUBLISH_BLOCKED and the one campaign", refusal.Error.Params)
	}
	if final := readFlow(t, f, h); len(final.Revisions) != outageRevisions+1 ||
		final.Flow.PublishedRevisionID != newest.RevisionID {
		t.Errorf("a refused publish moved the flow: %d revisions, live %s", len(final.Revisions), final.Flow.PublishedRevisionID)
	}
	if got := f.do(call{method: http.MethodGet, path: "/campaigns/" + h.campaignID, cookie: h.admin}); decodeCampaign(t, got).Status != "DRAFT" {
		t.Errorf("the campaign is %s, want it untouched", decodeCampaign(t, got).Status)
	}
}
