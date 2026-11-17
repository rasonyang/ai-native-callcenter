// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

func TestDailyCapScenarioCoversTheCase(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		if err := CheckScenarioCoversCases(loadScenarioFor(t, form), []Region{DailyCapRegion}, DailyCapCases, form); err != nil {
			t.Fatal(err)
		}
	})
	if got := len(DailyCapCase.Steps); got <= DailyCapMax {
		t.Fatalf("the case plays %d behaviours, want more than the cap %d so a fourth INVITE would show", got, DailyCapMax)
	}
}

func TestChooseDailyCapZone(t *testing.T) {
	// 12:00 UTC: Shanghai is 20:00, London 12:00 (winter), New York 07:00,
	// Los Angeles 04:00, Kolkata 17:30, Auckland 01:00 next day.
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	z, d, err := ChooseDailyCapZone(now, dailyCapZones)
	if err != nil {
		t.Fatal(err)
	}
	if z != "Europe/London" || d != 12*time.Hour {
		t.Errorf("got %s at %s from midnight, want Europe/London at 12h", z, d)
	}
	if _, _, err := ChooseDailyCapZone(now, []string{"Nowhere/Land"}); err == nil {
		t.Error("an unknown zone was accepted")
	}
}

func TestNextLocalMidnight(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	got := NextLocalMidnight(time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC), loc) // 04:00 on the 18th
	if want := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}
