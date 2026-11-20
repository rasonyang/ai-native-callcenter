// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// A messy flow: several phases, three ways to end the call, and exactly one of
// those endings ("handoff") with no line of its own. Phases the call passes
// through carry no announce, which is fine on every engine.
const flowWithOneSilentEnding = `{
	"id": "billing", "specVersion": "v2", "initialNode": "welcome",
	"global": {
		"persona": {"en": "You answer for NovaNet billing.", "zh": "你是 NovaNet 账务热线客服。"},
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"]
	},
	"nodes": {
		"welcome": {
			"instruction": {"en": "Ask what the call is about.", "zh": "询问来电事由。"},
			"announce": {"en": "Thanks for calling.", "zh": "感谢来电。"},
			"transitions": [
				{"on": "NO_INPUT", "condition": {"slot": "noInput.count", "op": "GTE", "value": 2}, "target": "farewell"}
			]
		},
		"triage": {
			"instruction": {"en": "Find out whether it is billing or a fault.", "zh": "判断是账务还是故障。"}
		},
		"report": {
			"instruction": {"en": "Explain the charge.", "zh": "解释费用。"}
		},
		"handoff": {
			"instruction": {"en": "Tell the caller they are being put through.", "zh": "告知来电者正在转接。"},
			"isTerminal": true
		},
		"farewell": {
			"instruction": {"en": "Say goodbye.", "zh": "道别。"},
			"announce": {"en": "Goodbye.", "zh": "再见。"},
			"isTerminal": true
		},
		"callback_done": {
			"instruction": {"en": "Confirm the callback.", "zh": "确认回电。"},
			"announce": {"en": "We will call you back.", "zh": "我们会回电给您。"},
			"isTerminal": true
		}
	}
}`

// The publish rule follows the deployment's engine through the API: where the
// profile requires a terminal announce (doubao) a flow with a silent ending is
// refused with TERMINAL_ANNOUNCE_REQUIRED naming that phase and only that one;
// the same document publishes on qwen, whose engine takes a text cue.
func TestThePublishRuleFollowsTheDeploymentsEngine(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		isRefused bool
	}{
		{provider.NameDoubao, true},
		{provider.NameQwen, false},
		{provider.NameGrok, false},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, testdb.ScratchDSN(t, "pubrule"), 4)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(st.Close)
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			profile, err := provider.ProfileFor(tc.provider, provider.Override{})
			if err != nil {
				t.Fatalf("profile: %v", err)
			}
			// What cmd/aicc/wiring.go's flowPublishRules derives for a
			// deployment with the AI leg on.
			if profile.RequiresTerminalAnnounce {
				st.FlowPublishRules = []flow.Rule{flow.RequireTerminalAnnounce}
			}
			s := &Server{flows: st.Flows()}

			created := httptest.NewRecorder()
			s.CreateFlow(created, post(`{"slug":"billing","name":"Billing","spec":`+flowWithOneSilentEnding+`}`))
			if created.Code != http.StatusCreated {
				t.Fatalf("create: status = %d: %s", created.Code, created.Body.String())
			}
			var summary struct {
				ID uuid.UUID `json:"flowId"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &summary); err != nil {
				t.Fatalf("decode summary: %v", err)
			}

			rec := httptest.NewRecorder()
			s.PublishFlow(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")), summary.ID)

			if !tc.isRefused {
				if rec.Code != http.StatusOK {
					t.Fatalf("publish on %s: status = %d, want 200: %s", tc.provider, rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("publish on %s: status = %d, want 422: %s", tc.provider, rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Code != "TERMINAL_ANNOUNCE_REQUIRED" {
				t.Errorf("code = %q, want TERMINAL_ANNOUNCE_REQUIRED", body.Error.Code)
			}
			nodes, _ := body.Error.Params["nodes"].([]any)
			if len(nodes) != 1 || nodes[0] != "handoff" {
				t.Errorf("params.nodes = %v, want only the ending with no line", body.Error.Params["nodes"])
			}
		})
	}
}

// A flow whose time limit is above the deployment's AICC_BOT_MAX_CALL_DURATION
// is refused at publish as a malformed spec would be (#637): 422
// VALIDATION_FAILED, with a problem that names the setting and its value. The
// draft itself saves, as any loadable draft does.
func TestAPublishAboveTheCallCapIsAValidationFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "pubcap"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st.FlowPublishRules = []flow.Rule{flow.RequireMaxDurationWithin(300 * time.Second)}
	s := &Server{flows: st.Flows()}

	spec := strings.Replace(flowWithOneSilentEnding, `"alwaysAllowedTools"`, `"maxDurationSec": 900, "alwaysAllowedTools"`, 1)
	created := httptest.NewRecorder()
	s.CreateFlow(created, post(`{"slug":"billing","name":"Billing","spec":`+spec+`}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d: %s", created.Code, created.Body.String())
	}
	var summary struct {
		ID uuid.UUID `json:"flowId"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}

	rec := httptest.NewRecorder()
	s.PublishFlow(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")), summary.ID)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code   string `json:"code"`
			Params struct {
				Problems []string `json:"problems"`
			} `json:"params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "VALIDATION_FAILED" {
		t.Errorf("code = %q, want VALIDATION_FAILED", body.Error.Code)
	}
	if p := body.Error.Params.Problems; len(p) != 1 || !strings.Contains(p[0], "AICC_BOT_MAX_CALL_DURATION") ||
		!strings.Contains(p[0], "(300s)") {
		t.Errorf("params.problems = %q, want one naming AICC_BOT_MAX_CALL_DURATION and 300s", p)
	}
}
