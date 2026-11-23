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

// The greeting-cut case of issue #596, as data. The application runs the
// gateway profile — the one whose provider cancels its own response when the
// caller's voice is heard — against the mock in VAD mode: a burst of the
// callee's is detected by level, ends the turn in flight with
// response.done{status:"cancelled"}, and earns the scripted transcript and
// reply of the run's next utterance (deploy/e2e/provider-vad.json).
//
// On a greeting the answer the reply makes is declined: a moving tool inside
// it is refused, the opening is said again whole, and no third telling is
// earned by greeting the replay too. On real words the tool runs as today.
//
// Five calls, one after the other, each to a number of its own that the
// simulator answers with the case's uplink (deploy/e2e/scenario.json). The
// cases are read from three records: the simulator's downlink runs (the
// opening cut and its replay heard whole), the mock's session record (bursts
// detected, turns cancelled, transcripts, and what each tool result said —
// a refusal reads ok:"0", a carried-out call ok:"1"), and the call's CDR.
// As in the caller-first case, nothing is read across two processes' clocks.

// GreetingCutScenarioID names the case in reports.
const GreetingCutScenarioID = "G1"

const (
	// greetingCutWholeMs is the shortest a run of bot audio may be to count
	// as the opening (or the phase's announce) played whole: the mock speaks
	// four seconds a turn; a cut run ends far under this.
	greetingCutWholeMs = 3800
	// greetingCutRefusal is the flag a refused tool's result carries.
	greetingCutRefusal = `"ok":"0"`
	// greetingCutCarried is the flag a carried-out tool's result carries.
	greetingCutCarried = `"ok":"1"`
	// greetingCutByeAfterMs is when the callee hangs up: after the opening,
	// its replay, and the replies.
	greetingCutByeAfterMs = 15000
)

// GreetingCutCase is one call of the case.
type GreetingCutCase struct {
	ID string
	// Number is dialled as sent (POST /calls), so the simulator's key is the
	// same in every dial form.
	Number string
	// Callee describes what the simulator does, for the report.
	Callee string
	Bursts []calleesim.Burst
	// Texts are the transcripts the bursts earn, in order (provider-vad.json).
	Texts []string
	// Cancels is how many of the bursts land inside a turn.
	Cancels int
	// Refusals counts tool results read back as refusals (ok:"0"); Runs
	// counts the ones that ran (ok:"1"). Together they are the case's tool
	// outcomes — a refusal on a greeting, a run on real words.
	Refusals, Runs int
	// WholeRuns counts downlink runs at least greetingCutWholeMs — a telling
	// of the opening (or a full announce) the callee heard all of, never a
	// cut one. MinRuns and MaxRuns bound the total: the scripted reply and
	// the call's closing line are runs of their own, and a reply merged
	// into the run that follows shows as one run, not two. A muted reply or
	// a second replay would push the count past MaxRuns.
	WholeRuns, MinRuns, MaxRuns int
}

// GreetingCutCases are the calls, in the order they are placed: the same
// order the VAD file's utterance queue is consumed in.
var GreetingCutCases = []GreetingCutCase{
	{
		ID: "G1-A", Number: "+12025550210",
		Callee: "cuts the opening saying 喂？ at 1.5 s for 400 ms; the reply answers with a line and a moving tool",
		Bursts: []calleesim.Burst{{AtMs: 1500, DurationMs: 400}},
		Texts:  []string{"喂？"},
		// The opening is cut; its replay plays whole; the tool is refused.
		// Runs: the cut opening, the scripted reply, the replay, the close.
		Cancels: 1, Refusals: 1,
		WholeRuns: 1, MinRuns: 3, MaxRuns: 5,
	},
	{
		ID: "G1-B", Number: "+12025550211",
		Callee:    "says a combined greeting 喂，您好 at 1.2 s for 900 ms; the reply is an improvised line",
		Bursts:    []calleesim.Burst{{AtMs: 1200, DurationMs: 900}},
		Texts:     []string{"喂，您好"},
		Cancels:   1,
		WholeRuns: 1, MinRuns: 3, MaxRuns: 5,
	},
	{
		ID: "G1-C", Number: "+12025550212",
		Callee: "cuts the opening with real words 不需要，谢谢 at 1.5 s for 600 ms; the reply calls the moving tool",
		Bursts: []calleesim.Burst{{AtMs: 1500, DurationMs: 600}},
		Texts:  []string{"不需要，谢谢"},
		// Real words: no replay is owed, the tool runs as today; the whole
		// run is the moved-to phase's announce, not a second telling.
		Cancels: 1, Runs: 1,
		WholeRuns: 1, MinRuns: 2, MaxRuns: 4,
	},
	{
		ID: "G1-D", Number: "+12025550213",
		Callee: "cuts the opening saying 喂 at 1.5 s, then cuts its replay saying 喂？ at 6 s; the second reply calls the moving tool",
		Bursts: []calleesim.Burst{{AtMs: 1500, DurationMs: 400}, {AtMs: 6000, DurationMs: 400}},
		Texts:  []string{"喂", "喂？"},
		// Both bursts land inside bot audio: the opening and its replay are
		// each cut; the second greeting's tool is refused; the one whole
		// run is the second telling (a run under 3.8 s is a cut or a merge
		// of the short reply with part of one) — a third telling shows.
		Cancels: 2, Refusals: 1,
		WholeRuns: 1, MinRuns: 4, MaxRuns: 6,
	},
	{
		ID: "G1-E", Number: "+12025550214",
		Callee: "says nothing at all (control: the opening plays once, whole)",
		// No speech: nothing is detected, cancelled or answered. The callee
		// hears the opening whole, then the no-input re-ask, then the close.
		WholeRuns: 2, MinRuns: 2, MaxRuns: 3,
	},
}

// GreetingCutFlowSlug names the flow the case publishes.
const GreetingCutFlowSlug = "e2e-greeting-cut"

// GreetingCutConfig is what the run needs of its environment.
type GreetingCutConfig struct {
	API, ReadyURL string
	// CalleeSim is the simulator's report base; MockProvider the mock
	// provider's HTTP base.
	CalleeSim, MockProvider string
	ESLAddr, ESLPassword    string
	ComposeConfig           string
	// ScenarioFile is the simulator's scenario; every case must be in it.
	ScenarioFile string
	// FlowFile is the greeting-cut flow (deploy/e2e/flow.greetingcut.json);
	// VADFile the mock's utterance list (deploy/e2e/provider-vad.json), whose
	// shape the run checks against its cases.
	FlowFile, VADFile  string
	Username, Password string
	OutDir             string
	Wait               time.Duration
}

// GreetingCutResult is one call's measurements.
type GreetingCutResult struct {
	Case   GreetingCutCase `json:"case"`
	CallID string          `json:"callId"`
	DID    string          `json:"did"`
	// Runs is the downlink audio the callee heard, in order.
	Runs []calleesim.AudioRun `json:"audioRuns"`
	// ToolOutputs is what the application returned for the session's tool
	// calls (a refusal's reason or a result's), in order.
	ToolOutputs []string                  `json:"toolOutputs,omitempty"`
	Mock        *mockprovider.SessionStat `json:"mockSession,omitempty"`
	// Dialog is the simulator's record.
	Dialog *calleesim.DialogRecord `json:"dialog,omitempty"`
}

// GreetingCutReport is what a run writes to report.json.
type GreetingCutReport struct {
	Scenario    string              `json:"scenario"`
	Passed      bool                `json:"passed"`
	StartedAt   time.Time           `json:"startedAt"`
	FinishedAt  time.Time           `json:"finishedAt"`
	DurationSec float64             `json:"durationSec"`
	DialForm    string              `json:"dialForm"`
	Results     []GreetingCutResult `json:"results"`
	Timeline    []string            `json:"timeline"`
	Checks      []string            `json:"checks"`
	Failures    []string            `json:"failures"`
}

// CheckGreetingCutScenario refuses a run whose simulator would not play what
// the cases expect: each number in the scenario, answered 200 after a ring,
// with the case's bursts as its uplink, and a callee that hangs up on its own.
func CheckGreetingCutScenario(s *calleesim.Scenario, cases []GreetingCutCase) error {
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
		if b.ByeAfterMs == nil || *b.ByeAfterMs != greetingCutByeAfterMs {
			problems = append(problems, fmt.Sprintf("%s: byeAfterMs is %v, want %d", cs.ID, b.ByeAfterMs, greetingCutByeAfterMs))
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
		return fmt.Errorf("the simulator's scenario does not match the greeting-cut cases: %s", strings.Join(problems, "; "))
	}
	return nil
}

// CheckGreetingCutVAD refuses a run whose mock would not answer the bursts
// the cases speak: the utterance queue must hold, in order, the texts and
// tools the cases expect, because burst n of the run consumes utterance n.
func CheckGreetingCutVAD(v *mockprovider.VAD, cases []GreetingCutCase) error {
	var problems []string
	var want []mockprovider.VADUtterance
	for _, cs := range cases {
		for _, text := range cs.Texts {
			want = append(want, mockprovider.VADUtterance{Text: text})
		}
	}
	if len(v.Utterances) < len(want) {
		return fmt.Errorf("the VAD file has %d utterances, the cases speak %d bursts", len(v.Utterances), len(want))
	}
	for i, w := range want {
		if v.Utterances[i].Text != w.Text {
			problems = append(problems, fmt.Sprintf("utterance %d is %q, want %q", i, v.Utterances[i].Text, w.Text))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the VAD file does not answer the greeting-cut bursts in order: %s", strings.Join(problems, "; "))
	}
	return nil
}

// assessGreetingCut makes every assertion about one call, from the
// simulator's record of it, the mock's record of its session, and the CDR.
func assessGreetingCut(col *collector, cs GreetingCutCase, d calleesim.DialogRecord,
	m *mockprovider.SessionStat, cdr *api.CDR) GreetingCutResult {
	res := GreetingCutResult{Case: cs, Runs: d.Downlink.AudioRuns, Mock: m}
	pre := cs.ID + ": "

	col.that(d.IsExpected && d.Number == cs.Number, "%sthe dialog is the scenario's for %s (%q, expected %v)", pre, cs.Number, d.Number, d.IsExpected)
	col.that(d.Final != nil && d.Final.Status == 200 && d.AckAt != nil, "%sthe call was answered and acknowledged", pre)
	col.that(d.End != nil, "%sthe dialog ended", pre)

	// The downlink, run by run: a run ends shortly after each burst's speech
	// start — the in-flight tail a cancelled turn still plays —
	// and the case's whole runs and total bound hold. The muted reply and a
	// second replay would each make a run the case does not count.
	var whole int
	for _, run := range d.Downlink.AudioRuns {
		if run.EndAfterInviteMs-run.StartAfterInviteMs >= greetingCutWholeMs {
			whole++
		}
	}
	col.that(len(d.Downlink.AudioRuns) > 0, "%sthe callee heard the bot at all (%d runs)", pre, len(d.Downlink.AudioRuns))
	for i, b := range cs.Bursts {
		lo, hi := float64(b.AtMs+200), float64(b.AtMs+b.DurationMs+2800)
		found := false
		for _, run := range d.Downlink.AudioRuns {
			if run.EndAfterInviteMs >= lo && run.EndAfterInviteMs <= hi {
				found = true
				break
			}
		}
		col.that(found, "%sburst %d cut a run of the bot's audio (a run ends in %d..%d ms)", pre, i+1, int(lo), int(hi))
	}
	col.that(whole == cs.WholeRuns, "%s%d run(s) played whole (>= %d ms), want %d", pre, whole, greetingCutWholeMs, cs.WholeRuns)
	col.that(len(d.Downlink.AudioRuns) >= cs.MinRuns && len(d.Downlink.AudioRuns) <= cs.MaxRuns,
		"%sthe downlink holds %d runs, want %d..%d — no muted reply, no second replay", pre, len(d.Downlink.AudioRuns), cs.MinRuns, cs.MaxRuns)

	if cdr == nil {
		col.that(false, "%sthe call's CDR is on record", pre)
	} else {
		res.CallID = cdr.CallID.String()
		col.that(cdr.ToNumber == cs.Number, "%sthe CDR is the call's own (to %s)", pre, cdr.ToNumber)
	}

	if m == nil {
		col.that(false, "%sthe mock provider has the call's session", pre)
		return res
	}
	res.ToolOutputs = m.ToolOutputs
	// The mock heard every burst as speech, cancelled the turns they cut,
	// and answered each with its scripted transcript.
	col.that(int(m.VADStarts) == len(cs.Texts), "%sthe mock detected %d burst(s) of speech, want %d", pre, m.VADStarts, len(cs.Texts))
	col.that(int(m.VADCancels) == cs.Cancels, "%s%d burst(s) landed inside a turn and cancelled it, want %d", pre, m.VADCancels, cs.Cancels)
	col.that(reflect.DeepEqual(m.VADTexts, cs.Texts), "%sthe transcripts are %q, want %q", pre, m.VADTexts, cs.Texts)

	// What the tool results said is the move's record: a refusal means the
	// greeting ran no tool and no phase move is owed; ok:"1" means the tool
	// ran as it does today. A greeting records neither a lead nor a phase
	// move because nothing it returns is carried out.
	var refusals, runs int
	for _, out := range m.ToolOutputs {
		if strings.Contains(out, greetingCutRefusal) {
			refusals++
		} else if strings.Contains(out, greetingCutCarried) {
			runs++
		}
	}
	col.that(refusals == cs.Refusals, "%s%d tool result(s) came back refused, want %d (%d outputs)", pre, refusals, cs.Refusals, len(m.ToolOutputs))
	col.that(runs == cs.Runs, "%s%d tool result(s) came back carried out, want %d", pre, runs, cs.Runs)
	return res
}

type greetingCutRun struct {
	cfg GreetingCutConfig
	c   *httpc
	rep *GreetingCutReport
	col collector
}

func (r *greetingCutRun) note(format string, args ...any) {
	r.rep.Timeline = append(r.rep.Timeline, fmt.Sprintf("%s  %s", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// RunGreetingCut runs the cases against the environment. It writes
// report.json to cfg.OutDir and returns the report, with an error when the
// run was refused, could not proceed, or an assertion failed.
func RunGreetingCut(ctx context.Context, cfg GreetingCutConfig) (*GreetingCutReport, error) {
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
	// The run's whole point is the gateway profile; the environment must be
	// the one e2e.sh greeting-cut recreated for it.
	provider, err := appSetting(composeJSON, "AICC_PROVIDER")
	if err != nil {
		return nil, err
	}
	if provider != "gateway" {
		return nil, fmt.Errorf("the application runs AICC_PROVIDER=%q; the greeting-cut cases want the server-cancels profile (gateway)", provider)
	}
	scenario, err := calleesim.LoadScenario(cfg.ScenarioFile)
	if err != nil {
		return nil, err
	}
	if err := CheckGreetingCutScenario(scenario, GreetingCutCases); err != nil {
		return nil, err
	}
	if cfg.VADFile != "" {
		vadData, err := os.ReadFile(cfg.VADFile)
		if err != nil {
			return nil, fmt.Errorf("read VAD file: %w", err)
		}
		vad, err := mockprovider.ParseVAD(vadData)
		if err != nil {
			return nil, fmt.Errorf("parse VAD file: %w", err)
		}
		if err := CheckGreetingCutVAD(vad, GreetingCutCases); err != nil {
			return nil, err
		}
	}
	if err := checkSwitch(ctx, cfg.ESLAddr, cfg.ESLPassword); err != nil {
		return nil, err
	}
	c := &httpc{base: cfg.API, client: newClient()}
	if err := waitReady(ctx, c.client, cfg.ReadyURL, cfg.Wait); err != nil {
		return nil, err
	}
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, "e2e-greeting-cut", keyScopes); err != nil {
		return nil, err
	}

	run := &greetingCutRun{cfg: cfg, c: c, rep: &GreetingCutReport{
		Scenario: GreetingCutScenarioID, StartedAt: time.Now().UTC(), DialForm: form, Results: []GreetingCutResult{},
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

func (r *greetingCutRun) run(ctx context.Context) error {
	number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	// The callee greets in Chinese, so the call runs in zh: a DID's language
	// is what the greeting and acknowledgement classifiers judge by.
	zh := "zh"
	did, err := r.c.publishFlowAndDIDWith(ctx, r.cfg.FlowFile, GreetingCutFlowSlug+"-"+number,
		"e2e greeting cut "+number, api.DIDWrite{Number: number, Language: &zh})
	if err != nil {
		return err
	}
	r.note("published the greeting-cut flow on DID %s", did.Number)

	for _, cs := range GreetingCutCases {
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
func (r *greetingCutRun) call(ctx context.Context, cs GreetingCutCase, did string) (*GreetingCutResult, error) {
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
	cdr, err := r.waitCDR(ctx, callID, did)
	if err != nil {
		return nil, err
	}
	res := assessGreetingCut(&r.col, cs, *dialog, mockSession, cdr)
	res.CallID, res.DID, res.Dialog = callID.String(), did, dialog
	r.note("%s: %d downlink runs, %d burst(s) heard, %d turn(s) cancelled, %d tool output(s)",
		cs.ID, len(res.Runs), mockSession.VADStarts, mockSession.VADCancels, len(mockSession.ToolOutputs))
	return &res, nil
}

// waitCDR waits for the call's CDR to be written (the call ended) and
// returns it. The ledger has no callId filter; the run's DID is unique to it.
func (r *greetingCutRun) waitCDR(ctx context.Context, callID uuid.UUID, did string) (*api.CDR, error) {
	deadline := time.Now().Add(30 * time.Second)
	var list api.CDRList
	for {
		if err := r.c.do(ctx, http.MethodGet, "/cdrs?limit=50&did="+did, nil, &list, http.StatusOK); err == nil {
			for i := range list.Items {
				if list.Items[i].CallID == callID {
					return &list.Items[i], nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the call's CDR was not written within 30 s of the call ending")
		}
		if err := sleep(ctx, time.Second); err != nil {
			return nil, err
		}
	}
}

// waitDialogEnded waits for the simulator's dialog for number, past the first
// prior ones, to have ended.
func (r *greetingCutRun) waitDialogEnded(ctx context.Context, prior int, number string) (calleesim.Report, error) {
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
func (r *greetingCutRun) waitMockSession(ctx context.Context, seq int64) (*mockprovider.SessionStat, error) {
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
