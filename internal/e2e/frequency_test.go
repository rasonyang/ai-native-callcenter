// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

func TestHoldCheck(t *testing.T) {
	touch := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	at := func(d time.Duration) *time.Time { v := touch.Add(window + d); return &v }
	for _, tc := range []struct {
		name    string
		got     *time.Time
		wantErr string
	}{
		{"exact", at(0), ""},
		{"within slack", at(1500 * time.Millisecond), ""},
		{"early", at(-5 * time.Second), "off by"},
		{"late", at(time.Minute), "off by"},
		{"absent", nil, "absent"},
	} {
		err := HoldCheck(tc.got, touch, window, 2*time.Second)
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestFrequencyScenarioCoversTheCases(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		if err := CheckScenarioCoversCases(loadScenarioFor(t, form), []Region{FrequencyRegion}, FrequencyCases, form); err != nil {
			t.Error(err)
		}
	})
}
