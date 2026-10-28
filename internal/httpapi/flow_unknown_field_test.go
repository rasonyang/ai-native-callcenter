// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// unknownFieldSpec is a NovaNet closing phase whose line was keyed "anounce".
// Before #362 it saved, published, and the caller never heard the line.
func unknownFieldSpec(extra string) json.RawMessage {
	return json.RawMessage(`{
	  "id": "novanet_upgrade", "specVersion": "v2", "initialNode": "offer",
	  "global": {"persona": "You are NovaNet Broadband's upgrade assistant."` + extra + `},
	  "nodes": {
	    "offer": {"instruction": "Offer the 500 Mbps plan.", "tools": ["hangup"]},
	    "confirm_upgrade": {
	      "instruction": "The upgrade is booked. Do not repeat it.",
	      "anounce": "Your plan changes to 500 Mbps on the 1st. Thank you for calling NovaNet, goodbye.",
	      "isTerminal": true
	    }
	  }
	}`)
}

// Saving a draft with a field the spec does not declare is refused through
// the API as every other load problem is: 422, VALIDATION_FAILED, and every
// such field in params.problems, named with where it is.
func TestADraftWithAnUnknownFieldIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	admin := f.seedAdmin()

	created := f.do(call{method: http.MethodPost, path: "/flows", cookie: admin, body: map[string]any{
		"slug": "novanet-upgrade", "name": "Upgrade",
		"spec": json.RawMessage(strings.Replace(string(unknownFieldSpec("")), `"anounce"`, `"announce"`, 1)),
	}})
	if created.status != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.status, created.body)
	}
	var flow struct {
		FlowID string `json:"flowId"`
	}
	if err := json.Unmarshal(created.body, &flow); err != nil || flow.FlowID == "" {
		t.Fatalf("create body %s: %v", created.body, err)
	}

	for _, tc := range []struct {
		name  string
		extra string
		want  []string
	}{
		{"one misspelling", "", []string{
			`phase "confirm_upgrade" has unknown field "anounce"; did you mean "announce"?`,
		}},
		{"three at once", `, "maxDurationSecs": 600, "x-comment": "draft for review"`, []string{
			`phase "confirm_upgrade" has unknown field "anounce"; did you mean "announce"?`,
			`global has unknown field "maxDurationSecs"; did you mean "maxDurationSec"?`,
			`global has unknown field "x-comment"`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.do(call{method: http.MethodPut, path: "/flows/" + flow.FlowID, cookie: admin, body: map[string]any{
				"name": "Upgrade", "spec": unknownFieldSpec(tc.extra),
			}})
			if got.status != http.StatusUnprocessableEntity || got.code != string(CodeValidationFailed) {
				t.Fatalf("update = %d %s, want 422 VALIDATION_FAILED: %s", got.status, got.code, got.body)
			}
			var body struct {
				Error struct {
					Params struct {
						Problems []string `json:"problems"`
					} `json:"params"`
				} `json:"error"`
			}
			if err := json.Unmarshal(got.body, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			problems := body.Error.Params.Problems
			if len(problems) != len(tc.want) {
				t.Errorf("params.problems = %q, want %d", problems, len(tc.want))
			}
			for _, want := range tc.want {
				if !slices.Contains(problems, want) {
					t.Errorf("params.problems = %q, lack %q", problems, want)
				}
			}
		})
	}
}
