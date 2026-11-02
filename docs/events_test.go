// SPDX-License-Identifier: Apache-2.0

package docs

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// typeRow matches a row of the event-type table in events.md: a line whose
// first cell is exactly one backticked event name.
var typeRow = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]*)`\\s*\\|")

// TestEventsGuideListsEverySseEventType keeps docs/events.md in step with the
// contract: every SseEventType value has a row in the guide's table, and the
// table names no type the contract does not have.
func TestEventsGuideListsEverySseEventType(t *testing.T) {
	var contract struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `json:"enum"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(Contract, &contract); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	want := contract.Components.Schemas["SseEventType"].Enum
	if len(want) == 0 {
		t.Fatal("contract has no SseEventType enum")
	}

	guide, err := os.ReadFile("events.md")
	if err != nil {
		t.Fatalf("read events.md: %v", err)
	}
	var have []string
	for _, line := range strings.Split(string(guide), "\n") {
		if m := typeRow.FindStringSubmatch(line); m != nil {
			have = append(have, m[1])
		}
	}

	for _, typ := range want {
		if !slices.Contains(have, typ) {
			t.Errorf("SseEventType %s is in docs/openapi.json but has no row in docs/events.md", typ)
		}
	}
	seen := map[string]bool{}
	for _, typ := range have {
		if !slices.Contains(want, typ) {
			t.Errorf("docs/events.md has a row for %s, which is not an SseEventType in docs/openapi.json (stale row)", typ)
		}
		if seen[typ] {
			t.Errorf("docs/events.md lists %s more than once", typ)
		}
		seen[typ] = true
	}
}
