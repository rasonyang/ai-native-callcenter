// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// Tier 2 of issue #85: a text model plays the bot and a second plays the
// caller. Everything the bot's model sees comes from the real runtime, the way
// the voice path takes it: Runtime.Instructions as the system prompt (platform
// rules included), Runtime.Tools as the tool list, Runtime.Announce as the
// line said as written, and every tool call goes through Runtime.Dispatch, so
// phase moves, hints, allowlist refusals and the guards are the real ones. The
// voice path uses these same public methods; nothing was extracted from it.
//
// What differs from a call: text, not audio, so there is no barge-in, no
// playback and no turn-end versus playback-end distinction, and the model is a
// chat model rather than the deployment's voice provider. A pass predicts a
// call only as far as that model resembles the provider.

const (
	// DefaultModelRuns is how many times each tier 2 scenario is run.
	DefaultModelRuns = 3
	// DefaultCallerTurns caps the caller's turns in one run.
	DefaultCallerTurns = 20
	// maxToolRounds caps the model calls inside one bot turn (a tool call
	// answered, the model called again, ...), so a model that calls tools
	// forever cannot spin inside a single turn.
	maxToolRounds = 6
	// defaultPassRate is the share of runs that must meet the expectations.
	defaultPassRate = 0.67

	callerHangup  = "<hangup>"
	callerSilence = "<silence>"
)

// ModelConfig configures a tier 2 invocation.
type ModelConfig struct {
	// Bot plays the bot, Caller plays the caller. They may be the same Chat.
	Bot, Caller Chat
	// BotModel and CallerModel name the models sent in requests.
	BotModel, CallerModel string
	// BotTemperature and CallerTemperature are optional.
	BotTemperature, CallerTemperature *float64
	// Runs per scenario; DefaultModelRuns when zero.
	Runs int
	// MaxCallerTurns caps one run; the scenario's persona may lower it.
	MaxCallerTurns int
	// Log receives progress lines as runs finish; nil is silent.
	Log io.Writer
}

// ModelReport is the outcome of one tier 2 scenario.
type ModelReport struct {
	Scenario string
	// Runs are the individual runs, each a Report whose Log is the transcript.
	Runs []Report
	// Passed is the number of runs that met every expectation.
	Passed int
	// Required is the pass rate the scenario needs.
	Required float64
	// Error is set when the invocation stopped before or during this
	// scenario (a model API error, the budget); the report is then partial.
	Error string
	// Skipped is true when the scenario was never started.
	Skipped bool
}

// Rate is the share of runs that passed.
func (r ModelReport) Rate() float64 {
	if len(r.Runs) == 0 {
		return 0
	}
	return float64(r.Passed) / float64(len(r.Runs))
}

// Succeeded reports whether the scenario met its pass rate without an error.
func (r ModelReport) Succeeded() bool {
	return !r.Skipped && r.Error == "" && len(r.Runs) > 0 && r.Rate()+1e-9 >= r.Required
}

// RunModel runs every tier 2 scenario cfg.Runs times. It stops at the first
// model API error or when the budget is spent, marking what it did not reach.
func RunModel(ctx context.Context, spec *flow.Spec, backend BackendSource, cfg ModelConfig, scenarios []Scenario) []ModelReport {
	if cfg.Runs <= 0 {
		cfg.Runs = DefaultModelRuns
	}
	if cfg.MaxCallerTurns <= 0 {
		cfg.MaxCallerTurns = DefaultCallerTurns
	}
	var reports []ModelReport
	stopped := ""
	for _, sc := range scenarios {
		if sc.CallerPersona == nil {
			continue
		}
		rep := ModelReport{Scenario: sc.Name, Required: defaultPassRate}
		if sc.Model.PassRate > 0 {
			rep.Required = sc.Model.PassRate
		}
		if stopped != "" {
			rep.Skipped, rep.Error = true, stopped
			reports = append(reports, rep)
			continue
		}
		for n := 0; n < cfg.Runs; n++ {
			run, err := runOnce(ctx, spec, backend, cfg, sc)
			rep.Runs = append(rep.Runs, run)
			if run.Passed() {
				rep.Passed++
			}
			if cfg.Log != nil {
				status := "pass"
				if !run.Passed() {
					status = "FAIL"
				}
				fmt.Fprintf(cfg.Log, "  %s run %d/%d: %s\n", sc.Name, n+1, cfg.Runs, status)
			}
			if err != nil {
				stopped = err.Error()
				rep.Error = stopped
				break
			}
		}
		reports = append(reports, rep)
	}
	return reports
}

type modelRun struct {
	cfg     ModelConfig
	sc      Scenario
	engine  *flow.Engine
	runtime *flow.Runtime
	actions *scriptedActions
	report  Report

	// system is the bot's standing instructions as last pinned.
	system   string
	botMsgs  []ChatMessage
	callMsgs []ChatMessage
	// spoken is what the bot said since the caller last spoke.
	spoken []string
}

// runOnce plays one call. The error is non-nil only for a model API failure
// or the budget, which end the whole invocation; a call that merely went
// wrong is a failed Report.
func runOnce(ctx context.Context, spec *flow.Spec, source BackendSource, cfg ModelConfig, sc Scenario) (Report, error) {
	backend, release := source()
	defer release()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	actions := newActions(sc)
	engine := flow.NewEngine(spec, sc.Lang, flow.SeedSlots(sc.Caller, sc.Entry, sc.DID, sc.UserData), quiet)
	runtime := flow.NewRuntime(engine, actions, backend, sc.Queues, quiet)
	applyCampaign(ctx, runtime, sc)
	m := &modelRun{cfg: cfg, sc: sc, engine: engine, runtime: runtime, actions: actions}
	m.report = Report{Scenario: sc.Name, Path: []string{engine.NodeID()}}
	m.system = runtime.Instructions()
	m.callMsgs = []ChatMessage{{Role: "system", Content: callerPrompt(sc, engine.Lang())}}

	err := m.play(ctx)
	m.report.Ending = actions.ending()
	m.report.Transfer = actions.transfer
	m.report.Lead = actions.lead
	if err != nil {
		m.report.Failure = "model error: " + err.Error()
		return m.report, err
	}
	if m.report.Failure == "" {
		m.report.Failure = judge(*sc.Model, m.report, engine)
	}
	return m.report, nil
}

func (m *modelRun) play(ctx context.Context) error {
	maxTurns := m.cfg.MaxCallerTurns
	if p := m.sc.CallerPersona; p.MaxTurns > 0 && p.MaxTurns < maxTurns {
		maxTurns = p.MaxTurns
	}

	// The call opens with the entry phase's line, said as written; a phase
	// without one leaves the model to speak first.
	if line := m.runtime.Announce(); line != "" {
		m.say(line)
		m.botMsgs = append(m.botMsgs, ChatMessage{Role: "assistant", Content: line})
	} else {
		m.botMsgs = append(m.botMsgs, ChatMessage{Role: "user", Content: "(The call has just connected. Greet the caller.)"})
		if err := m.botTurn(ctx); err != nil {
			return err
		}
	}

	for turn := 1; ; turn++ {
		if m.isOver() {
			return nil
		}
		if turn > maxTurns {
			m.report.Failure = fmt.Sprintf("the call was still going after %d caller turns", maxTurns)
			return nil
		}
		said, err := m.callerTurn(ctx)
		if err != nil {
			return err
		}
		switch said {
		case callerHangup:
			m.log("CALLER hangs up")
			return nil
		case callerSilence, "":
			m.log("CALLER is silent")
			before := m.engine.NodeID()
			m.runtime.OnNoInput()
			m.afterMove(before)
			m.botMsgs = append(m.botMsgs, ChatMessage{Role: "user", Content: "(The caller says nothing.)"})
		default:
			m.log("CALLER: " + said)
			m.runtime.OnCallerSpoke(said)
			m.botMsgs = append(m.botMsgs, ChatMessage{Role: "user", Content: said})
		}
		if m.isOver() {
			return nil
		}
		if err := m.botTurn(ctx); err != nil {
			return err
		}
		if m.report.Failure != "" {
			return nil
		}
	}
}

// isOver is whether the call has ended: a terminal phase reached or a hangup
// that succeeded.
func (m *modelRun) isOver() bool {
	return m.engine.IsTerminal() || m.actions.last == flow.ToolHangup
}

func (m *modelRun) log(line string) { m.report.Log = append(m.report.Log, line) }

// say records something the caller heard.
func (m *modelRun) say(text string) {
	m.log("BOT: " + text)
	m.spoken = append(m.spoken, text)
}

// afterMove records a phase change and, as the voice path does, re-pins the
// instructions and says the new phase's line as written.
func (m *modelRun) afterMove(before string) {
	after := m.engine.NodeID()
	if after != before {
		m.report.Path = append(m.report.Path, after)
		m.report.Moves = append(m.report.Moves, Move{From: before, To: after})
		m.log(fmt.Sprintf("PHASE %s -> %s", before, after))
	}
	if after != before || m.runtime.IsInstructionStale() {
		m.system = m.runtime.Instructions()
	}
	if after != before {
		if line := m.runtime.Announce(); line != "" {
			m.say(line)
			m.botMsgs = append(m.botMsgs, ChatMessage{Role: "assistant", Content: line})
		}
	}
}

// botTurn lets the bot model respond to the conversation so far: it may call
// tools, read their results, and call again, until it answers in words.
func (m *modelRun) botTurn(ctx context.Context) error {
	for round := 0; round < maxToolRounds; round++ {
		resp, err := m.cfg.Bot.Complete(ctx, ChatRequest{
			Model:       m.cfg.BotModel,
			Messages:    append([]ChatMessage{{Role: "system", Content: m.system}}, m.botMsgs...),
			Tools:       chatTools(m.runtime),
			Temperature: m.cfg.BotTemperature,
		})
		if err != nil {
			return err
		}
		m.botMsgs = append(m.botMsgs, ChatMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			if text := strings.TrimSpace(resp.Content); text != "" {
				m.say(text)
			}
			if m.runtime.OnBotTurnDone(false, false) {
				before := m.engine.NodeID()
				m.runtime.CloseAtTurnsWithoutToolWall()
				m.afterMove(before)
			}
			return nil
		}

		if text := strings.TrimSpace(resp.Content); text != "" {
			m.say(text)
		}
		before := m.engine.NodeID()
		m.runtime.BeginToolBatch()
		for _, call := range resp.ToolCalls {
			isAllowed := m.runtime.IsToolAllowed(call.Name)
			output, _ := m.runtime.Dispatch(ctx, call.Name, call.Arguments)
			m.log(fmt.Sprintf("TOOL %s(%s) -> %s", call.Name, call.Arguments, flow.TranscriptOutput(output)))
			if isAllowed {
				m.report.ToolsRun = append(m.report.ToolsRun, call.Name)
			}
			m.botMsgs = append(m.botMsgs, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: output})
		}
		m.runtime.EndToolBatch()
		m.runtime.OnBotTurnDone(true, false)
		m.afterMove(before)
		if m.engine.IsTerminal() && m.runtime.Announce() != "" {
			// The closing phase has its own line, said as written; the call
			// ends after it, so the model is not asked for another.
			return nil
		}
	}
	m.report.Failure = fmt.Sprintf("the bot made tool calls for %d model rounds in one turn without answering", maxToolRounds)
	return nil
}

// callerTurn asks the caller model what the caller says next.
func (m *modelRun) callerTurn(ctx context.Context) (string, error) {
	heard := "(The line is connected. The assistant says nothing.)"
	if len(m.spoken) > 0 {
		heard = strings.Join(m.spoken, " ")
	}
	m.spoken = nil
	m.callMsgs = append(m.callMsgs, ChatMessage{Role: "user", Content: heard})
	resp, err := m.cfg.Caller.Complete(ctx, ChatRequest{
		Model: m.cfg.CallerModel, Messages: m.callMsgs, Temperature: m.cfg.CallerTemperature,
	})
	if err != nil {
		return "", err
	}
	said := strings.TrimSpace(resp.Content)
	m.callMsgs = append(m.callMsgs, ChatMessage{Role: "assistant", Content: said})
	return said, nil
}

func chatTools(rt *flow.Runtime) []ChatTool {
	specs := rt.Tools()
	tools := make([]ChatTool, len(specs))
	for i, s := range specs {
		tools[i] = ChatTool{Name: s.Name, Description: s.Description, Parameters: s.Parameters}
	}
	return tools
}

// callerPrompt is the caller model's brief.
func callerPrompt(sc Scenario, lang string) string {
	p := sc.CallerPersona
	var b strings.Builder
	b.WriteString("You are playing a person who has phoned a company and is talking to its automated voice assistant. ")
	b.WriteString("Stay in character for the whole call and never reveal that this is a simulation.\n\n")
	fmt.Fprintf(&b, "Your goal: %s\n", p.Goal)
	if p.Persona != "" {
		fmt.Fprintf(&b, "Who you are and what you know: %s\n", p.Persona)
	}
	if sc.Caller != "" {
		fmt.Fprintf(&b, "The number you are calling from: %s\n", sc.Caller)
	}
	if p.Behaviour != "" {
		fmt.Fprintf(&b, "How you behave: %s\n", p.Behaviour)
	}
	if lang == flow.LangZH {
		b.WriteString("You speak Chinese.\n")
	} else {
		b.WriteString("You speak English.\n")
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- Reply with only the words you say aloud: one or two short, natural spoken sentences. No stage directions.\n")
	b.WriteString("- Know only the facts above. If asked for something you do not know, say you do not know; never invent numbers.\n")
	fmt.Fprintf(&b, "- To stay silent this turn, reply exactly %s\n", callerSilence)
	fmt.Fprintf(&b, "- When the call is over for you (your goal is met, you were transferred, or you gave up), reply exactly %s\n", callerHangup)
	return b.String()
}

// judge checks a finished run against the scenario's model expectations.
func judge(want ModelExpect, r Report, engine *flow.Engine) string {
	var problems []string
	if want.Phase != "" && want.Phase != engine.NodeID() {
		problems = append(problems, fmt.Sprintf("ended in phase %q, want %q", engine.NodeID(), want.Phase))
	}
	if want.Ending != "" && want.Ending != r.Ending {
		problems = append(problems, fmt.Sprintf("ended with %q, want %q", r.Ending, want.Ending))
	}
	if problem := leadProblem(want.Lead, r.Lead); problem != "" {
		problems = append(problems, problem)
	}
	if t := want.Transfer; t != nil {
		switch {
		case r.Transfer == nil:
			problems = append(problems, "no transfer succeeded")
		case t.Queue != "" && t.Queue != r.Transfer.Queue:
			problems = append(problems, fmt.Sprintf("transferred to %q, want %q", r.Transfer.Queue, t.Queue))
		case t.Reason != "" && t.Reason != r.Transfer.Reason:
			problems = append(problems, fmt.Sprintf("transfer reason %q, want %q", r.Transfer.Reason, t.Reason))
		}
	}
	for _, phase := range want.Visits {
		if !slices.Contains(r.Path, phase) {
			problems = append(problems, fmt.Sprintf("never entered %q (path %s)", phase, strings.Join(r.Path, " > ")))
		}
	}
	for _, phase := range want.Avoids {
		if slices.Contains(r.Path, phase) {
			problems = append(problems, fmt.Sprintf("entered %q, which it must not (path %s)", phase, strings.Join(r.Path, " > ")))
		}
	}
	if diff := subsetDiff(want.Slots, engine.Slots()); diff != "" {
		problems = append(problems, "slots: "+diff)
	}
	return strings.Join(problems, "; ")
}

// WriteModel prints tier 2 reports. verbose adds each run's transcript. It
// returns the number of scenarios that missed their pass rate or errored.
func WriteModel(w io.Writer, flowID string, reports []ModelReport, verbose bool) int {
	failed := 0
	fmt.Fprintf(w, "flow %s (tier 2)\n", flowID)
	for _, r := range reports {
		status := "PASS"
		if !r.Succeeded() {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "  %s  %s  %d/%d runs passed (need %.0f%%)\n", status, r.Scenario, r.Passed, len(r.Runs), r.Required*100)
		for i, run := range r.Runs {
			result := "pass"
			if !run.Passed() {
				result = "FAIL"
			}
			fmt.Fprintf(w, "        run %d: %s  path: %s  ending: %s\n", i+1, result, strings.Join(run.Path, " > "), run.Ending)
			if !run.Passed() {
				fmt.Fprintf(w, "               %s\n", run.Failure)
			}
			if verbose || !run.Passed() {
				for _, line := range run.Log {
					fmt.Fprintf(w, "               | %s\n", line)
				}
			}
		}
		if r.Skipped {
			fmt.Fprintf(w, "        not run: %s\n", r.Error)
		} else if r.Error != "" {
			fmt.Fprintf(w, "        stopped: %s\n", r.Error)
		}
	}
	return failed
}
