// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

func TestCallerFirstScenarioCoversTheCases(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		if err := CheckCallerFirstScenario(loadScenarioFor(t, form), CallerFirstCases); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCallerFirstScenarioRefusesADrift(t *testing.T) {
	good := loadScenarioFor(t, phonenum.FormE164)
	drifted := *good
	drifted.Numbers = map[string][]calleesim.Behaviour{}
	for k, v := range good.Numbers {
		drifted.Numbers[k] = v
	}
	first := CallerFirstCases[0]
	b := drifted.Numbers[first.Number][0]
	b.Uplink = calleesim.Uplink{Kind: calleesim.UplinkBursts, Bursts: []calleesim.Burst{{AtMs: 100, DurationMs: 400}}}
	drifted.Numbers[first.Number] = []calleesim.Behaviour{b}
	if err := CheckCallerFirstScenario(&drifted, CallerFirstCases); err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("a drifted burst was not refused: %v", err)
	}
	delete(drifted.Numbers, CallerFirstCases[2].Number)
	if err := CheckCallerFirstScenario(&drifted, CallerFirstCases); err == nil || !strings.Contains(err.Error(), CallerFirstCases[2].ID) {
		t.Errorf("a missing number was not refused: %v", err)
	}
}

// The case table is a set of intervals that tell the three outcomes apart: a
// greeting's release, the timeout, and the callee who speaks too late to
// matter. A bound that overlapped the next outcome's would pass a regression.
func TestCallerFirstBoundsSeparateTheOutcomes(t *testing.T) {
	a, b, c := CallerFirstCases[0], CallerFirstCases[1], CallerFirstCases[2]
	if a.FirstAudioMaxMs >= c.FirstAudioMinMs {
		t.Errorf("a greeting's release (up to %.0f ms) is not apart from the timeout (from %.0f ms)", a.FirstAudioMaxMs, c.FirstAudioMinMs)
	}
	// The release comes 300 ms after the greeting's last voiced slot.
	end := a.Bursts[0].AtMs + a.Bursts[0].DurationMs
	if a.FirstAudioMinMs < float64(end) {
		t.Errorf("the first audio may come at %.0f ms, before the greeting ended at %d ms", a.FirstAudioMinMs, end)
	}
	if b.FirstAudioMaxMs > float64(b.Bursts[0].AtMs) {
		t.Errorf("the opening must start before the late burst at %d ms, bound is %.0f ms", b.Bursts[0].AtMs, b.FirstAudioMaxMs)
	}
	// B's burst sits inside the grace (800 ms after a release at 2000 ms,
	// extended while voice is heard, capped at 1500 ms after it).
	if at, dur := b.Bursts[0].AtMs, b.Bursts[0].DurationMs; at < 2000 || at+dur > 2000+1500 {
		t.Errorf("B's burst (%d-%d ms) is not inside the grace after a release at 2000 ms", at, at+dur)
	}
	for _, cs := range CallerFirstCases {
		if cs.IsUplinkHeard {
			if at := cs.Bursts[0].AtMs; at < 2000+1500+1000 {
				t.Errorf("%s: the control's burst at %d ms is not well past release and grace", cs.ID, at)
			}
		}
	}
}

func callerFirstRuns(spans ...[2]float64) []calleesim.AudioRun {
	var out []calleesim.AudioRun
	for _, s := range spans {
		out = append(out, calleesim.AudioRun{StartAfterInviteMs: s[0], EndAfterInviteMs: s[1], Frames: int((s[1]-s[0])/20) + 1})
	}
	return out
}

var callerFirstNow = time.Now()

func callerFirstDialog(cs CallerFirstCase, mediaStart float64, runs []calleesim.AudioRun) calleesim.DialogRecord {
	return calleesim.DialogRecord{
		Number: cs.Number, IsExpected: true,
		Final: &calleesim.FinalRecord{Status: 200}, AckAt: &callerFirstNow,
		End:                        &calleesim.EndRecord{Method: calleesim.EndedWithBye, By: calleesim.EndedByCallee},
		UplinkStartedAfterInviteMs: &mediaStart,
		Downlink:                   calleesim.Downlink{AudioRuns: runs},
	}
}

func assessOne(t *testing.T, cs CallerFirstCase, d calleesim.DialogRecord, m *mockprovider.SessionStat) []string {
	t.Helper()
	var col collector
	assessCallerFirst(&col, cs, d, m)
	return col.failures
}

func TestCallerFirstAssessmentAcceptsTheRightCalls(t *testing.T) {
	quiet := &mockprovider.SessionStat{UplinkFrames: 300}
	heard := &mockprovider.SessionStat{UplinkFrames: 300, UplinkLoudFrames: 18}
	for _, c := range []struct {
		cs    CallerFirstCase
		first float64
		m     *mockprovider.SessionStat
	}{
		{CallerFirstCases[0], 1550, quiet},
		{CallerFirstCases[1], 2150, quiet},
		{CallerFirstCases[2], 2100, quiet},
		{CallerFirstCases[3], 2100, heard},
	} {
		// The media start is 400 ms after the INVITE; the opening runs 4 s.
		d := callerFirstDialog(c.cs, 400, callerFirstRuns([2]float64{400 + c.first, 400 + c.first + 3980}))
		if fails := assessOne(t, c.cs, d, c.m); len(fails) > 0 {
			t.Errorf("%s: %v", c.cs.ID, fails)
		}
	}
}

func TestCallerFirstAssessmentCatchesTheRegressions(t *testing.T) {
	a, b := CallerFirstCases[0], CallerFirstCases[1]
	quiet := &mockprovider.SessionStat{UplinkFrames: 300}
	whole := func(first float64) []calleesim.AudioRun {
		return callerFirstRuns([2]float64{400 + first, 400 + first + 3980})
	}
	for name, c := range map[string]struct {
		cs   CallerFirstCase
		d    calleesim.DialogRecord
		m    *mockprovider.SessionStat
		want string
	}{
		"the opening ignored the wait":        {a, callerFirstDialog(a, 400, whole(150)), quiet, "first audio"},
		"a greeting was not detected":         {a, callerFirstDialog(a, 400, whole(2100)), quiet, "first audio"},
		"the opening waited for a late burst": {b, callerFirstDialog(b, 400, whole(3100)), quiet, "first audio"},
		"the opening was split":               {a, callerFirstDialog(a, 400, callerFirstRuns([2]float64{1950, 3000}, [2]float64{3300, 5900})), quiet, "run"},
		"the greeting reached the provider": {a, callerFirstDialog(a, 400, whole(1550)),
			&mockprovider.SessionStat{UplinkFrames: 300, UplinkLoudFrames: 20}, "no frame of the callee's voice"},
		"the mock saw no uplink at all": {a, callerFirstDialog(a, 400, whole(1550)), &mockprovider.SessionStat{}, "uplink"},
		"no audio":                      {a, callerFirstDialog(a, 400, nil), quiet, "heard the opening"},
		"no session":                    {a, callerFirstDialog(a, 400, whole(1550)), nil, "mock provider has the call's session"},
	} {
		fails := assessOne(t, c.cs, c.d, c.m)
		if len(fails) == 0 {
			t.Errorf("%s: no assertion failed", name)
			continue
		}
		if !strings.Contains(strings.Join(fails, "|"), c.want) {
			t.Errorf("%s: failures %v, want one about %q", name, fails, c.want)
		}
	}
	// The control must hear the late burst.
	d := CallerFirstCases[3]
	if fails := assessOne(t, d, callerFirstDialog(d, 400, whole(2100)), quiet); len(fails) == 0 {
		t.Error("the control call passed with a provider that heard nothing")
	}
}

func TestCallerFirstFlowIsCallerFirstWithAQuestionOpening(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(scenarioDir, "flow.callerfirst.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		InitialNode string `json:"initialNode"`
		Global      struct {
			IsCallerFirst bool `json:"isCallerFirst"`
		} `json:"global"`
		Nodes map[string]struct {
			Announce map[string]string `json:"announce"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.Global.IsCallerFirst {
		t.Error("the flow is not caller first")
	}
	for lang, text := range spec.Nodes[spec.InitialNode].Announce {
		if !strings.HasSuffix(text, "?") && !strings.HasSuffix(text, "？") {
			t.Errorf("the %s opening does not end in a question: %q", lang, text)
		}
	}
}
