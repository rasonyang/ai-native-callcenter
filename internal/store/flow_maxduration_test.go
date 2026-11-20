// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// defaultBotMaxCallDuration is AICC_BOT_MAX_CALL_DURATION's default (#637).
const defaultBotMaxCallDuration = 300 * time.Second

// supportSpec is an inbound support line that hands callers to a person, so
// no campaign could ever run it. LIMIT is replaced with a maxDurationSec
// entry, or with nothing.
func supportSpec(limit string) []byte {
	return []byte(strings.Replace(`{
	  "id": "harbour_support", "specVersion": "v2", "initialNode": "greet",
	  "global": {
	    "persona": {"en": "You are NovaNet Broadband's support line.", "zh": "你是 NovaNet 宽带的客服热线。"},
	    LIMIT
	    "closingTarget": "farewell", "alwaysAllowedTools": ["transfer_to_agent", "hangup"]
	  },
	  "nodes": {
	    "greet": {
	      "instruction": {"en": "Help with the Harbour District fibre outage; put the caller through to a person when they ask.", "zh": "处理海港区光纤故障的咨询；来电者要求时转人工。"},
	      "announce": {"en": "Thanks for calling NovaNet support.", "zh": "感谢致电 NovaNet 客服。"}
	    },
	    "farewell": {
	      "instruction": {"en": "Say goodbye.", "zh": "道别。"},
	      "announce": {"en": "Thank you for calling NovaNet, goodbye.", "zh": "感谢致电 NovaNet，再见。"},
	      "isTerminal": true
	    }
	  }
	}`, "LIMIT", limit, 1))
}

// A revision published before the deployment capped a flow's time limit
// (#637), with the field left out, with 0 (which turned the limit off) or with
// 900 (the old default), still loads on the runtime path, PublishedSpec through
// flow.LoadTolerant, and runs at the default cap's 300 s. Publishing such a
// draft again is refused naming the setting, and the refusal leaves the stored
// revision answering; 301 is refused too, and 300 and the field left out
// publish.
func TestAStoredRevisionPastTheCapLoadsAndRunsAtTheCap(t *testing.T) {
	ctx := context.Background()
	st := revisionStore(t)
	// The deployment before the cap: no rule on the time limit.
	before := st.Flows()
	st.FlowPublishRules = []flow.Rule{flow.RequireMaxDurationWithin(defaultBotMaxCallDuration)}
	after := st.Flows()

	for _, tc := range []struct {
		slug      string
		limit     string
		isRefused bool
	}{
		{"left-out", "", false},
		{"zero", `"maxDurationSec": 0,`, true},
		{"old-default", `"maxDurationSec": 900,`, true},
		{"one-over", `"maxDurationSec": 301,`, true},
		{"at-the-cap", `"maxDurationSec": 300,`, false},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			id, err := before.Create(ctx, tc.slug, "Harbour support "+tc.slug, supportSpec(tc.limit))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := before.Publish(ctx, id, "published before the cap"); err != nil {
				t.Fatalf("publish without the rule: %v", err)
			}
			assertRunsAtTheCap(t, after, id)

			err = after.Publish(ctx, id, "published under the cap")
			if !tc.isRefused {
				if err != nil {
					t.Fatalf("publish under the cap: %v", err)
				}
				assertRunsAtTheCap(t, after, id)
				return
			}
			var invalid *flow.ValidationError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "AICC_BOT_MAX_CALL_DURATION") ||
				!strings.Contains(err.Error(), "(300s)") {
				t.Fatalf("publish error = %v, want a ValidationError naming AICC_BOT_MAX_CALL_DURATION and 300s", err)
			}
			detail, err := after.Detail(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.Revisions) != 1 {
				t.Errorf("revisions = %d after the refused publish, want the one stored before", len(detail.Revisions))
			}
			assertRunsAtTheCap(t, after, id)
		})
	}
}

func assertRunsAtTheCap(t *testing.T, flows *FlowStore, id uuid.UUID) {
	t.Helper()
	spec, err := flows.PublishedSpec(context.Background(), id)
	if err != nil {
		t.Fatalf("the published revision does not load: %v", err)
	}
	if got := spec.Global.EffectiveMaxDuration(defaultBotMaxCallDuration); got != 300*time.Second {
		t.Errorf("runs at %v, want 300s", got)
	}
}
