// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// Report is the outcome of one scenario.
type Report struct {
	Scenario string
	// Path is the phases entered, starting with the initial one.
	Path []string
	// Ending is the last built-in action that succeeded, or "none".
	Ending string
	// Lead is the last lead stored, by record_lead or by a phase that
	// declares one (flow.DeclaredLead), if any.
	Lead *flow.LeadRequest
	// Transfer is the last successful transfer, if any.
	Transfer *flow.TransferRequest
	// Moves are the phase changes that happened, as from -> to.
	Moves []Move
	// ToolsRun are the allowed tools that ran, in order.
	ToolsRun []string
	// Log is one line per step, for a verbose report.
	Log []string
	// Failure is empty when the scenario passed; otherwise it says which step
	// failed and why. A scenario stops at its first failure.
	Failure string
}

// Passed reports whether the scenario passed.
func (r Report) Passed() bool { return r.Failure == "" }

// Move is one phase change.
type Move struct{ From, To string }

// Run replays every scenario of a file against the flow. backend serves the
// flow's HTTP tools, fresh for each scenario.
func Run(ctx context.Context, spec *flow.Spec, backend BackendSource, scenarios []Scenario) []Report {
	reports := make([]Report, 0, len(scenarios))
	for _, scenario := range scenarios {
		if scenario.IsModelOnly() {
			continue // tier 2 only
		}
		reports = append(reports, RunScenario(ctx, spec, backend, scenario))
	}
	return reports
}

// RunScenario replays one scenario on a fresh engine and runtime.
func RunScenario(ctx context.Context, spec *flow.Spec, source BackendSource, sc Scenario) Report {
	backend, release := source()
	defer release()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	actions := newActions(sc)
	engine := flow.NewEngine(spec, sc.Lang, flow.SeedSlots(sc.Caller, sc.Entry, sc.DID, sc.UserData), quiet)
	runtime := flow.NewRuntime(engine, actions, backend, sc.Queues, quiet)
	applyCampaign(ctx, runtime, sc)

	r := &runner{ctx: ctx, spec: spec, sc: sc, engine: engine, runtime: runtime, actions: actions}
	r.report = Report{Scenario: sc.Name, Path: []string{engine.NodeID()}}
	// The standing instructions are rendered (and pinned) once at the start of
	// a call, as the application does.
	runtime.Instructions()
	for i, step := range sc.Steps {
		for n := 0; n < max(1, step.Repeat) && r.report.Failure == ""; n++ {
			if err := r.step(step); err != nil {
				r.report.Failure = fmt.Sprintf("step %d (%s), run %d: %v", i+1, describe(step), n+1, err)
			}
		}
		if r.report.Failure != "" {
			break
		}
	}
	if r.report.Failure == "" {
		if err := r.final(); err != nil {
			r.report.Failure = "end of scenario: " + err.Error()
		}
	}
	r.report.Ending = actions.ending()
	r.report.Transfer = actions.transfer
	r.report.Lead = actions.lead
	return r.report
}

type runner struct {
	ctx     context.Context
	spec    *flow.Spec
	sc      Scenario
	engine  *flow.Engine
	runtime *flow.Runtime
	actions *scriptedActions
	report  Report
	// lastOutput is the output of the last tool the current step ran.
	lastOutput map[string]any
	// lastRefused records the last tool of the step being refused by the phase.
	lastRefused bool
	// currentExpect is the expectation of the step being run.
	currentExpect StepExpect
}

func describe(s Step) string {
	switch {
	case s.Say != nil:
		return fmt.Sprintf("say %q", *s.Say)
	case s.Keypress != nil:
		return fmt.Sprintf("keypress %q", *s.Keypress)
	case s.Exchange != nil:
		return fmt.Sprintf("exchange %q", *s.Exchange)
	case s.NoInput:
		return "noInput"
	case s.BotTurn != "":
		return "botTurn " + s.BotTurn
	case s.SessionLimit:
		return "sessionLimit"
	case s.Call != "":
		return "call " + s.Call
	default:
		names := make([]string, len(s.Batch))
		for i, c := range s.Batch {
			names[i] = c.Call
		}
		return "batch " + strings.Join(names, ", ")
	}
}

func (r *runner) step(s Step) error {
	r.currentExpect = s.Expect
	before := r.engine.NodeID()
	r.lastOutput, r.lastRefused = nil, false

	switch {
	case s.Say != nil:
		r.runtime.OnCallerSpoke(*s.Say)
	case s.Keypress != nil:
		r.runtime.OnCallerKeyed()
	case s.Exchange != nil:
		r.runtime.OnCallerSpoke(*s.Exchange)
		r.botTurn(false)
	case s.NoInput:
		r.runtime.OnNoInput()
	case s.BotTurn != "":
		r.botTurn(s.BotTurn == "interrupted")
	case s.SessionLimit:
		r.runtime.CloseAtSessionLimit()
	case s.Call != "":
		if err := r.call(s.Call, s.Args); err != nil {
			return err
		}
	default:
		r.runtime.BeginToolBatch()
		var firstErr error
		for _, c := range s.Batch {
			if err := r.call(c.Call, c.Args); err != nil {
				firstErr = err
				break
			}
		}
		r.runtime.EndToolBatch()
		if firstErr != nil {
			return firstErr
		}
	}

	after := r.engine.NodeID()
	if after != before {
		r.report.Path = append(r.report.Path, after)
		r.report.Moves = append(r.report.Moves, Move{From: before, To: after})
	}
	line := fmt.Sprintf("%s: %s", describe(s), before)
	if after != before {
		line += " -> " + after
	} else {
		line += " (stays)"
	}
	r.report.Log = append(r.report.Log, line)
	return r.check(s, after)
}

// botTurn reports a finished bot turn that made no tool call, and applies the
// maxTurnsWithoutTool wall the way the application does when it is passed.
func (r *runner) botTurn(isInterrupted bool) {
	if r.runtime.OnBotTurnDone(false, isInterrupted) {
		r.runtime.CloseAtTurnsWithoutToolWall()
	}
}

// call dispatches one tool the way the application does, after the checks
// that make a mistake in the flow, not in the scenario, readable.
func (r *runner) call(name string, args map[string]any) error {
	if !r.isKnownTool(name) {
		return fmt.Errorf("the flow has no tool %q (its tools: %s)", name, strings.Join(r.knownTools(), ", "))
	}
	node := r.engine.NodeID()
	if !r.runtime.IsToolAllowed(name) {
		r.lastRefused = true
		if !r.expectsRefusal() {
			return fmt.Errorf("the tool %q is not allowed in phase %q (allowed there: %s); "+
				"a model would be refused the same way. Add it to the phase's \"tools\", or fix the transition that left the call in this phase",
				name, node, strings.Join(r.engine.AllowedTools(), ", "))
		}
	} else if name == flow.ToolRecordLead && r.runtime.IsLeadSetByPhase() {
		// Judged here, just before this tool, so inside a batch a move made
		// by an earlier tool counts.
		r.lastRefused = true
		if !r.expectsRefusal() {
			return fmt.Errorf("the tool %q is refused in phase %q: it declares its own lead, "+
				"so a model's record_lead there is refused and the declared lead stays. "+
				"Mark the step \"isRefused\", or remove the call", name, node)
		}
	} else if !r.isOffered(name) {
		return fmt.Errorf("the tool %q is allowed in phase %q but never offered to the model (Runtime.Tools omits it)", name, node)
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("arguments are not JSON: %w", err)
	}
	output, _ := r.runtime.Dispatch(r.ctx, name, string(encoded))
	r.lastOutput = map[string]any{}
	if err := json.Unmarshal([]byte(output), &r.lastOutput); err != nil {
		return fmt.Errorf("tool output is not a JSON object: %s", output)
	}
	if !r.lastRefused {
		r.report.ToolsRun = append(r.report.ToolsRun, name)
	}
	return nil
}

func (r *runner) expectsRefusal() bool {
	// Looked up on the step being run through the current step's expectation.
	return r.currentExpect.IsRefused
}

func (r *runner) isKnownTool(name string) bool {
	if slices.Contains(flow.BuiltinToolNames, name) {
		return true
	}
	_, ok := r.spec.Tools[name]
	return ok
}

func (r *runner) knownTools() []string {
	names := slices.Clone(flow.BuiltinToolNames)
	for name := range r.spec.Tools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (r *runner) isOffered(name string) bool {
	for _, tool := range r.runtime.Tools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// check applies a step's expectations.
func (r *runner) check(s Step, phase string) error {
	want := s.Expect
	if want.Phase != "" && want.Phase != phase {
		return fmt.Errorf("the call is in phase %q, want %q", phase, want.Phase)
	}
	isTool := s.Call != "" || len(s.Batch) > 0
	if isTool {
		wantOK := want.IsOK == nil || *want.IsOK
		if want.IsRefused {
			wantOK = false
		}
		gotOK := r.lastOutput["ok"] == "1"
		if gotOK != wantOK {
			return fmt.Errorf("the tool answered ok=%v, want %v; output: %s", gotOK, wantOK, compact(r.lastOutput))
		}
		if want.IsRefused != r.lastRefused {
			return fmt.Errorf("the tool was refused by the phase = %v, want %v", r.lastRefused, want.IsRefused)
		}
		if diff := subsetDiff(want.Result, r.lastOutput); diff != "" {
			return fmt.Errorf("tool result: %s; output: %s", diff, compact(r.lastOutput))
		}
	} else if len(want.Result) > 0 || want.IsOK != nil || want.IsRefused {
		return fmt.Errorf("result, isOk and isRefused apply to tool calls only")
	}
	if diff := subsetDiff(want.Slots, r.engine.Slots()); diff != "" {
		return fmt.Errorf("slots: %s", diff)
	}
	if want.Announce != nil && r.engine.Announce() != *want.Announce {
		return fmt.Errorf("announce is %q, want %q", r.engine.Announce(), *want.Announce)
	}
	if len(want.InstructionContains) > 0 {
		instruction := r.engine.Instruction()
		for _, fragment := range want.InstructionContains {
			if !strings.Contains(instruction, fragment) {
				return fmt.Errorf("the instruction of phase %q does not contain %q; it reads: %s", phase, fragment, instruction)
			}
		}
	}
	return nil
}

func (r *runner) final() error {
	want := r.sc.Expect
	if len(want.Path) > 0 && !slices.Equal(want.Path, r.report.Path) {
		return fmt.Errorf("the call went through %s, want %s",
			strings.Join(r.report.Path, " > "), strings.Join(want.Path, " > "))
	}
	if want.Phase != "" && want.Phase != r.engine.NodeID() {
		return fmt.Errorf("the call ended in phase %q, want %q", r.engine.NodeID(), want.Phase)
	}
	if want.Ending != "" && want.Ending != r.actions.ending() {
		return fmt.Errorf("the call ended with %q, want %q", r.actions.ending(), want.Ending)
	}
	if t := want.Transfer; t != nil {
		got := r.actions.transfer
		switch {
		case got == nil:
			return fmt.Errorf("no transfer succeeded, want queue %q reason %q", t.Queue, t.Reason)
		case t.Queue != "" && t.Queue != got.Queue:
			return fmt.Errorf("transferred to queue %q, want %q", got.Queue, t.Queue)
		case t.Reason != "" && t.Reason != got.Reason:
			return fmt.Errorf("transferred with reason %q, want %q", got.Reason, t.Reason)
		}
	}
	if len(want.ToolsRun) > 0 && !slices.Equal(want.ToolsRun, r.report.ToolsRun) {
		return fmt.Errorf("tools ran: %s; want %s",
			strings.Join(r.report.ToolsRun, ", "), strings.Join(want.ToolsRun, ", "))
	}
	if diff := subsetDiff(want.Slots, r.engine.Slots()); diff != "" {
		return fmt.Errorf("slots: %s", diff)
	}
	if problem := leadProblem(want.Lead, r.actions.lead); problem != "" {
		return errors.New(problem)
	}
	return nil
}

// leadProblem says how the lead a call ended with differs from what a scenario
// expects, or returns an empty string.
func leadProblem(want *LeadExpect, got *flow.LeadRequest) string {
	switch {
	case want == nil:
		return ""
	case want.IsUnrecorded && got != nil:
		return fmt.Sprintf("a lead %q was recorded, want none", got.Intent)
	case want.IsUnrecorded:
		return ""
	case got == nil:
		return fmt.Sprintf("no lead was recorded, want intent %q", want.Intent)
	case want.Intent != "" && want.Intent != got.Intent:
		return fmt.Sprintf("the lead is %q, want %q", got.Intent, want.Intent)
	case want.Notes != "" && want.Notes != got.Notes:
		return fmt.Sprintf("the lead notes are %q, want %q", got.Notes, want.Notes)
	}
	return ""
}

// applyCampaign gives the runtime a campaign call's tools: no way to a person
// and no message-taking, and record_lead (see aicall's campaign wiring). It is
// also what makes the leads phases declare count, the initial phase's at
// once, as on a campaign call.
func applyCampaign(ctx context.Context, runtime *flow.Runtime, sc Scenario) {
	if !sc.IsCampaign {
		return
	}
	runtime.Deny(flow.ToolTransferToAgent, flow.ToolTakeMessage)
	runtime.OfferRecordLead(ctx)
}

// subsetDiff reports the first key of want that is missing from, or differs
// in, got. Values are compared as JSON, so 3 equals 3.0.
func subsetDiff(want, got map[string]any) string {
	for _, key := range sortedKeys(want) {
		actual, ok := got[key]
		if !ok {
			return fmt.Sprintf("%q is not set, want %s", key, compact(want[key]))
		}
		if !jsonEqual(want[key], actual) {
			return fmt.Sprintf("%q is %s, want %s", key, compact(actual), compact(want[key]))
		}
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func normalize(v any) any {
	encoded, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(encoded, &out) != nil {
		return v
	}
	return out
}

func compact(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(encoded)
}
