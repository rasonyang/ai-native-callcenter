// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// Every field the API emits is camelCase (docs/design/07-naming.md). The CDR
// struct once shipped without tags and leaked CallID/StartedAt to browsers.
func TestLedgerTypesMarshalPerTheNamingSpec(t *testing.T) {
	for name, value := range map[string]any{
		"CDR":            CDR{},
		"Leg":            Leg{},
		"TranscriptLine": TranscriptLine{},
		"Callback":       Callback{},
		"Recording":      Recording{},
		"QualityReview":  QualityReview{},
		"Overview":       Overview{},
		"QueueReport":    QueueReport{},
		"DailyReport":    DailyReport{},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for key := range decoded {
			if key == "" || unicode.IsUpper(rune(key[0])) || strings.Contains(key, "_") {
				t.Errorf("%s emits %q — the JSON contract is camelCase", name, key)
			}
		}
	}
}
