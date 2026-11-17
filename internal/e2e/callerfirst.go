// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
)

// The caller-first case of issue #594, as data. An AI outbound call on a flow
// with global.isCallerFirst listens to the callee before it opens: the opening
// is requested at once and generated during the wait, but its audio is held
// until the callee's greeting has ended (300 ms of silence after the voice), at
// 2000 ms after the answer when nothing was said, or at the 4000 ms cap. And
// nothing the callee says reaches the provider during the wait or the grace
// after it.
//
// Four calls, one after the other, each to a number of its own that the
// simulator answers with a different uplink (deploy/e2e/scenario.json). Every
// interval is measured on the simulator's monotonic clock (clocks.go: an order
// between two processes' wall clocks is never read), counted from the moment
// the simulator began sending, which is its media start. What the mock
// provider saw of the uplink is read as counts, never ordered against the
// simulator's events.

// CallerFirstScenarioID names the case in reports.
const CallerFirstScenarioID = "F1"

// The tolerances. The wait's constants are the application's (internal/aicall/
// callerfirst.go: end of greeting 300 ms after the voice, 2000 ms without
// any); the bounds leave a loaded CI runner a few hundred milliseconds of
// path latency (switch, relay, RTP pacing) on top of them, and no more: a
// timeout that fired where the greeting should have ended is 500 ms later
// than the greeting's release.
const (
	// callerFirstRunMinMs is the shortest the opening may be as one run. The
	// mock speaks four seconds; a lost frame or two does not split it, a gap
	// of 100 ms or more does.
	callerFirstRunMinMs = 3000
	// callerFirstEarliestMs and callerFirstTimeoutMaxMs bound the first audio
	// of a call whose callee never speaks within the wait: the wait ends at
	// 2000 ms (the first audio cannot come before) and the path adds latency.
	callerFirstEarliestMs   = 1900
	callerFirstTimeoutMaxMs = 3000
	// callerFirstGreetingMaxMs is the latest the first audio may come after a
	// greeting that ended at 1200 ms: its release is at 1500 ms and the path
	// adds latency; the bound sits 300 ms above it and 100 ms under the
	// earliest a timeout can come, so the two are told apart.
	callerFirstGreetingMaxMs = 1800
	// callerFirstMinLoudFrames is how many loud frames the mock must have
	// seen from a burst of 400 ms (20 frames) that came after the wait.
	callerFirstMinLoudFrames = 5
	// callerFirstByeAfterMs is when the callee hangs up: after the opening
	// (about 4 s from 2 s) and the late burst of the control call.
	callerFirstByeAfterMs = 10000
)

// CallerFirstCase is one call of the case.
type CallerFirstCase struct {
	ID string
	// Number is dialled as sent (POST /calls), so the simulator's key is the
	// same in every dial form.
	Number string
	// Callee describes what the simulator does, for the report.
	Callee string
	Bursts []calleesim.Burst
	// FirstAudioMinMs and FirstAudioMaxMs bound the first downlink audio, in
	// milliseconds after the simulator began sending.
	FirstAudioMinMs, FirstAudioMaxMs float64
	// IsRunWhole asserts the opening as one continuous run of at least
	// callerFirstRunMinMs.
	IsRunWhole bool
	// IsUplinkHeard says the mock must have seen the callee's voice (a burst
	// after the wait, which proves the mock's measure is not blind); false says
	// it must have seen none at all.
	IsUplinkHeard bool
}

// CallerFirstCases are the calls, in the order they are placed.
var CallerFirstCases = []CallerFirstCase{
	{
		ID: "F1-A", Number: "+12025550201",
		Callee: "says a greeting at 800-1200 ms, then is silent",
		Bursts: []calleesim.Burst{{AtMs: 800, DurationMs: 400}},
		// The opening starts after the greeting has ended, as one run, and
		// not at the 2000 ms timeout.
		FirstAudioMinMs: 1400, FirstAudioMaxMs: callerFirstGreetingMaxMs, IsRunWhole: true,
	},
	{
		ID: "F1-B", Number: "+12025550202",
		Callee: "says nothing for 2.6 s, then speaks for 400 ms",
		Bursts: []calleesim.Burst{{AtMs: 2600, DurationMs: 400}},
		// The opening started at the timeout, before the burst; the burst
		// falls in the grace after the release, which the provider never hears.
		FirstAudioMinMs: callerFirstEarliestMs, FirstAudioMaxMs: 2600, IsRunWhole: true,
	},
	{
		ID: "F1-C", Number: "+12025550203",
		Callee:          "says nothing at all",
		FirstAudioMinMs: callerFirstEarliestMs, FirstAudioMaxMs: callerFirstTimeoutMaxMs, IsRunWhole: true,
	},
	{
		ID: "F1-D", Number: "+12025550204",
		Callee: "says nothing for 7 s, then speaks for 400 ms (control: the provider hears it)",
		Bursts: []calleesim.Burst{{AtMs: 7000, DurationMs: 400}},
		// The opening is the silent callee's; the burst long after the wait
		// reaches the provider, so a mock that could not see voice would show.
		FirstAudioMinMs: callerFirstEarliestMs, FirstAudioMaxMs: callerFirstTimeoutMaxMs, IsUplinkHeard: true,
	},
}

// CallerFirstFlowSlug names the flow the case publishes.
const CallerFirstFlowSlug = "e2e-callerfirst"

// CallerFirstConfig is what the run needs of its environment.
type CallerFirstConfig struct {
	API, ReadyURL string
	// CalleeSim is the simulator's report base; MockProvider the mock
	// provider's HTTP base.
	CalleeSim, MockProvider string
	ESLAddr, ESLPassword    string
	ComposeConfig           string
	// ScenarioFile is the simulator's scenario; every case must be in it.
	ScenarioFile string
	// FlowFile is the caller-first flow (deploy/e2e/flow.callerfirst.json).
	FlowFile           string
	Username, Password string
	OutDir             string
	Wait               time.Duration
}

// CallerFirstResult is one call's measurements.
type CallerFirstResult struct {
	Case   CallerFirstCase `json:"case"`
	CallID string          `json:"callId"`
	DID    string          `json:"did"`
	// FirstAudioAfterMediaStartMs is the first downlink audio, on the
	// simulator's monotonic clock from the moment it began sending; absent
	// when there was none.
	FirstAudioAfterMediaStartMs *float64 `json:"firstAudioAfterMediaStartMs,omitempty"`
	// RunMs is the length of the first run of audio.
	RunMs float64                   `json:"firstRunMs"`
	Runs  []calleesim.AudioRun      `json:"audioRuns"`
	Mock  *mockprovider.SessionStat `json:"mockSession,omitempty"`
	// Dialog is the simulator's record.
	Dialog *calleesim.DialogRecord `json:"dialog,omitempty"`
}

// CallerFirstReport is what a run writes to report.json.
type CallerFirstReport struct {
	Scenario    string              `json:"scenario"`
	Passed      bool                `json:"passed"`
	StartedAt   time.Time           `json:"startedAt"`
	FinishedAt  time.Time           `json:"finishedAt"`
	DurationSec float64             `json:"durationSec"`
	DialForm    string              `json:"dialForm"`
	Results     []CallerFirstResult `json:"results"`
	Timeline    []string            `json:"timeline"`
	Checks      []string            `json:"checks"`
	Failures    []string            `json:"failures"`
}

// CheckCallerFirstScenario refuses a run whose simulator would not play what
// the cases expect: each number in the scenario, answered 200 after a ring,
// with the case's bursts as its uplink, and a callee that hangs up on its own.
func CheckCallerFirstScenario(s *calleesim.Scenario, cases []CallerFirstCase) error {
	var problems []string
	for _, cs := range cases {
		list := s.Numbers[cs.Number]
		if len(list) != 1 {
			problems = append(problems, fmt.Sprintf("%s: the scenario has %d behaviours for %s, want 1", cs.ID, len(list), cs.Number))
			continue
		}
		b := list[0]
		if b.Final() != 200 {
			problems = append(problems, fmt.Sprintf("%s: the callee answers %d, want 200", cs.ID, b.Final()))
		}
		if b.ByeAfterMs == nil || *b.ByeAfterMs != callerFirstByeAfterMs {
			problems = append(problems, fmt.Sprintf("%s: byeAfterMs is %v, want %d", cs.ID, b.ByeAfterMs, callerFirstByeAfterMs))
		}
		for _, p := range b.Provisionals {
			if p.EarlyMedia {
				problems = append(problems, fmt.Sprintf("%s: the callee sends early media, which would start the uplink before the answer", cs.ID))
			}
		}
		if len(cs.Bursts) == 0 {
			if k := b.Uplink.Kind; k != "" && k != calleesim.UplinkSilence {
				problems = append(problems, fmt.Sprintf("%s: the uplink is %s, want silence", cs.ID, k))
			}
			continue
		}
		if b.Uplink.Kind != calleesim.UplinkBursts || !reflect.DeepEqual(b.Uplink.Bursts, cs.Bursts) {
			problems = append(problems, fmt.Sprintf("%s: the uplink is %s %+v, want BURSTS %+v", cs.ID, b.Uplink.Kind, b.Uplink.Bursts, cs.Bursts))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the simulator's scenario does not match the caller-first cases: %s", strings.Join(problems, "; "))
	}
	return nil
}

// assessCallerFirst makes every assertion about one call, from the
// simulator's record of it and the mock's record of its session.
func assessCallerFirst(col *collector, cs CallerFirstCase, d calleesim.DialogRecord, m *mockprovider.SessionStat) CallerFirstResult {
	res := CallerFirstResult{Case: cs, Runs: d.Downlink.AudioRuns, Mock: m}
	pre := cs.ID + ": "

	col.that(d.IsExpected && d.Number == cs.Number, "%sthe dialog is the scenario's for %s (%q, expected %v)", pre, cs.Number, d.Number, d.IsExpected)
	col.that(d.Final != nil && d.Final.Status == 200 && d.AckAt != nil, "%sthe call was answered and acknowledged", pre)
	col.that(d.End != nil, "%sthe dialog ended", pre)
	col.that(d.UplinkStartedAfterInviteMs != nil, "%sthe simulator recorded when it began sending", pre)

	if d.UplinkStartedAfterInviteMs != nil && len(d.Downlink.AudioRuns) > 0 {
		run := d.Downlink.AudioRuns[0]
		first := run.StartAfterInviteMs - *d.UplinkStartedAfterInviteMs
		res.FirstAudioAfterMediaStartMs = &first
		res.RunMs = run.EndAfterInviteMs - run.StartAfterInviteMs
		col.that(first >= cs.FirstAudioMinMs && first <= cs.FirstAudioMaxMs,
			"%sthe first audio came %.0f ms after the callee's media start, within %.0f-%.0f ms", pre, first, cs.FirstAudioMinMs, cs.FirstAudioMaxMs)
		if cs.IsRunWhole {
			col.that(res.RunMs >= callerFirstRunMinMs,
				"%sthe opening played as one run of %.0f ms, at least %d ms", pre, res.RunMs, callerFirstRunMinMs)
			col.that(len(d.Downlink.AudioRuns) == 1,
				"%sthe downlink holds one run of audio, no gap in the opening (%d runs)", pre, len(d.Downlink.AudioRuns))
		}
	} else {
		col.that(false, "%sthe callee heard the opening (audio runs: %d)", pre, len(d.Downlink.AudioRuns))
	}

	if m == nil {
		col.that(false, "%sthe mock provider has the call's session", pre)
		return res
	}
	col.that(m.UplinkFrames > 0, "%sthe mock saw the call's uplink (%d frames)", pre, m.UplinkFrames)
	if cs.IsUplinkHeard {
		col.that(m.UplinkLoudFrames >= callerFirstMinLoudFrames,
			"%sthe callee's late burst reached the provider (%d loud frames, at least %d)", pre, m.UplinkLoudFrames, callerFirstMinLoudFrames)
	} else {
		col.that(m.UplinkLoudFrames == 0,
			"%sno frame of the callee's voice reached the provider (%d loud of %d frames)", pre, m.UplinkLoudFrames, m.UplinkFrames)
	}
	return res
}

type callerFirstRun struct {
	cfg CallerFirstConfig
	c   *httpc
	rep *CallerFirstReport
	col collector
}

func (r *callerFirstRun) note(format string, args ...any) {
	r.rep.Timeline = append(r.rep.Timeline, fmt.Sprintf("%s  %s", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// RunCallerFirst runs the cases against the environment. It writes
// report.json to cfg.OutDir and returns the report, with an error when the run
// was refused, could not proceed, or an assertion failed.
func RunCallerFirst(ctx context.Context, cfg CallerFirstConfig) (*CallerFirstReport, error) {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	composeJSON, err := os.ReadFile(cfg.ComposeConfig)
	if err != nil {
		return nil, fmt.Errorf("read compose config: %w", err)
	}
	if err := CheckComposeConfig(composeJSON); err != nil {
		return nil, err
	}
	form, err := appSetting(composeJSON, "AICC_OUTBOUND_DIAL_FORM")
	if err != nil {
		return nil, err
	}
	scenario, err := calleesim.LoadScenario(cfg.ScenarioFile)
	if err != nil {
		return nil, err
	}
	if err := CheckCallerFirstScenario(scenario, CallerFirstCases); err != nil {
		return nil, err
	}
	if err := checkSwitch(ctx, cfg.ESLAddr, cfg.ESLPassword); err != nil {
		return nil, err
	}
	c := &httpc{base: cfg.API, client: newClient()}
	if err := waitReady(ctx, c.client, cfg.ReadyURL, cfg.Wait); err != nil {
		return nil, err
	}
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, "e2e-callerfirst", keyScopes); err != nil {
		return nil, err
	}

	run := &callerFirstRun{cfg: cfg, c: c, rep: &CallerFirstReport{
		Scenario: CallerFirstScenarioID, StartedAt: time.Now().UTC(), DialForm: form, Results: []CallerFirstResult{},
	}}
	runErr := run.run(ctx)

	rep := run.rep
	rep.FinishedAt = time.Now().UTC()
	rep.DurationSec = rep.FinishedAt.Sub(rep.StartedAt).Seconds()
	rep.Checks, rep.Failures = run.col.checks, append([]string{}, run.col.failures...)
	if runErr != nil {
		rep.Failures = append(rep.Failures, runIncomplete+runErr.Error())
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return rep, runErr
	}
	if !rep.Passed {
		return rep, fmt.Errorf("%d assertion(s) failed: %s", len(rep.Failures), strings.Join(rep.Failures, "; "))
	}
	return rep, nil
}

func (r *callerFirstRun) run(ctx context.Context) error {
	number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	did, err := r.c.publishFlowAndDID(ctx, r.cfg.FlowFile, CallerFirstFlowSlug+"-"+number, "e2e caller first "+number, number, nil)
	if err != nil {
		return err
	}
	r.note("published the caller-first flow on DID %s", did.Number)

	for _, cs := range CallerFirstCases {
		res, err := r.call(ctx, cs, did.Number)
		if res != nil {
			r.rep.Results = append(r.rep.Results, *res)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", cs.ID, err)
		}
	}
	return nil
}

// call places one case's call and measures it.
func (r *callerFirstRun) call(ctx context.Context, cs CallerFirstCase, did string) (*CallerFirstResult, error) {
	var simBefore calleesim.Report
	if err := r.c.getJSON(ctx, r.cfg.CalleeSim+"/report", &simBefore); err != nil {
		return nil, fmt.Errorf("simulator report: %w", err)
	}
	var mockBefore mockprovider.Stats
	if err := r.c.getJSON(ctx, r.cfg.MockProvider+mockprovider.StatsPath, &mockBefore); err != nil {
		return nil, fmt.Errorf("mock provider stats: %w", err)
	}
	if mockBefore.Live != 0 {
		return nil, fmt.Errorf("the mock provider has %d live session(s) before the call, so the call's session could not be told apart", mockBefore.Live)
	}

	callID := uuid.Must(uuid.NewV7())
	kind := api.CreateCallRequestKind("AI_OUTBOUND")
	var created api.CreateCallResponse
	if err := r.c.do(ctx, http.MethodPost, "/calls", api.CreateCallRequest{CallID: &callID, Kind: &kind, To: cs.Number, DID: &did}, &created, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("place call: %w", err)
	}
	r.note("%s: placed call %s to %s (%s)", cs.ID, callID, cs.Number, cs.Callee)

	rep, err := r.waitDialogEnded(ctx, len(simBefore.Dialogs), cs.Number)
	if err != nil {
		return nil, err
	}
	var dialog *calleesim.DialogRecord
	for i := len(simBefore.Dialogs); i < len(rep.Dialogs); i++ {
		if rep.Dialogs[i].Number == cs.Number {
			dialog = &rep.Dialogs[i]
		}
	}
	r.col.that(rep.UnexpectedCount == simBefore.UnexpectedCount, "%sno unexpected dialog at the callee (%d)", cs.ID+": ", rep.UnexpectedCount-simBefore.UnexpectedCount)

	mockSession, err := r.waitMockSession(ctx, mockBefore.Total+1)
	if err != nil {
		return nil, err
	}
	res := assessCallerFirst(&r.col, cs, *dialog, mockSession)
	res.CallID, res.DID, res.Dialog = callID.String(), did, dialog
	r.note("%s: first audio %s ms after the callee's media start, first run %.0f ms, mock saw %s",
		cs.ID, msStr(res.FirstAudioAfterMediaStartMs), res.RunMs, uplinkStr(mockSession))
	return &res, nil
}

func msStr(v *float64) string {
	if v == nil {
		return "none"
	}
	return fmt.Sprintf("%.0f", *v)
}

func uplinkStr(m *mockprovider.SessionStat) string {
	if m == nil {
		return "no session"
	}
	return fmt.Sprintf("%d loud of %d uplink frames", m.UplinkLoudFrames, m.UplinkFrames)
}

// waitDialogEnded waits for the simulator's dialog for number, past the first
// prior ones, to have ended.
func (r *callerFirstRun) waitDialogEnded(ctx context.Context, prior int, number string) (calleesim.Report, error) {
	var rep calleesim.Report
	deadline := time.Now().Add(r.cfg.Wait)
	for {
		if err := r.c.getJSON(ctx, r.cfg.CalleeSim+"/report", &rep); err == nil {
			for i := prior; i < len(rep.Dialogs); i++ {
				if rep.Dialogs[i].Number == number && rep.Dialogs[i].End != nil {
					return rep, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return rep, fmt.Errorf("the simulator did not record a dialog for %s that ended within %s", number, r.cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return rep, err
		}
	}
}

// waitMockSession waits for the mock's session number seq to have closed (the
// call is over) and returns its record.
func (r *callerFirstRun) waitMockSession(ctx context.Context, seq int64) (*mockprovider.SessionStat, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st mockprovider.Stats
		if err := r.c.getJSON(ctx, r.cfg.MockProvider+mockprovider.StatsPath+"?sessions=1", &st); err == nil {
			for i := range st.Sessions {
				if s := st.Sessions[i]; s.Seq == seq && !s.IsLive {
					return &s, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the mock provider's session %d did not close within 30 s of the call ending", seq)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
	}
}
