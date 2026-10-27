// SPDX-License-Identifier: Apache-2.0

// Package flowsim replays scripted conversations against a flow, using the
// real flow.Engine and flow.Runtime and the flow's real HTTP tools. It is a
// test tool outside the call path: nothing here is imported by the server.
//
// This is tier 1 of issue #85, the deterministic tier. No model plays the bot
// or the caller; a scenario states which tools the bot calls, with which
// arguments, and when the caller speaks, keys or goes quiet. What the
// simulator checks is the flow's logic: which phase each event leads to, what
// each phase permits, what a tool returned and what the call ended with. It
// says nothing about audio, barge-in, playback timing or what a model would
// have chosen to do; those stay with real calls.
package flowsim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// File is a scenario file: the scenarios for one flow.
type File struct {
	Scenarios []Scenario `json:"scenarios"`
}

// Scenario is one scripted call.
type Scenario struct {
	// Name identifies the scenario in reports; unique within its file.
	Name string `json:"name"`
	// Description says what the scenario is meant to prove.
	Description string `json:"description,omitempty"`
	// Lang is the call's language, "en" or "zh". Empty is the default.
	Lang string `json:"lang,omitempty"`
	// Caller, Entry and DID seed the slots of the same name; UserData seeds
	// userData.<k>.
	Caller   string         `json:"caller,omitempty"`
	Entry    string         `json:"entry,omitempty"`
	DID      string         `json:"did,omitempty"`
	UserData map[string]any `json:"userData,omitempty"`
	// IsCampaign makes this a campaign call: record_lead is offered, the
	// leads phases declare are recorded on entry, and transfer_to_agent and
	// take_message are withdrawn, as on a call the campaign dialer placed.
	IsCampaign bool `json:"isCampaign,omitempty"`
	// Queues are the queue names the simulated deployment has. A transfer to
	// any other name is refused, as on a real deployment. Empty accepts any.
	Queues []string `json:"queues,omitempty"`
	// Actions scripts the built-in tools by name (transfer_to_agent,
	// take_message, hangup). An unscripted built-in succeeds.
	Actions map[string]ActionResult `json:"actions,omitempty"`
	// Steps happen in order.
	Steps []Step `json:"steps"`
	// Expect is checked after the last step.
	Expect Final `json:"expect,omitempty"`

	// CallerPersona and Model make the scenario a tier 2 scenario: a text
	// model plays this caller against a text model playing the bot. A scenario
	// with Steps runs in tier 1; one with a CallerPersona runs in tier 2; one
	// may have both.
	CallerPersona *Persona     `json:"callerPersona,omitempty"`
	Model         *ModelExpect `json:"model,omitempty"`
}

// Persona is who the simulated caller is and how they behave.
type Persona struct {
	// Goal is what the caller wants out of the call.
	Goal string `json:"goal"`
	// Persona is who they are: facts they know (an order number, a service
	// number) and how they talk.
	Persona string `json:"persona,omitempty"`
	// Behaviour is how they act: cooperative, declining, off-topic, silent.
	Behaviour string `json:"behaviour,omitempty"`
	// MaxTurns caps the caller's turns for this scenario (default 20).
	MaxTurns int `json:"maxTurns,omitempty"`
}

// ModelExpect is what a tier 2 run must end up with. A model varies, so it
// states outcomes (where the call ended, what ended it, what it must have
// passed through), not an exact path. Everything is optional.
type ModelExpect struct {
	// PassRate is the share of runs that must meet every expectation
	// (default 0.67).
	PassRate float64 `json:"passRate,omitempty"`
	// Phase is the phase the call ends in.
	Phase string `json:"phase,omitempty"`
	// Ending is the last built-in that succeeded, as in Final.
	Ending string `json:"ending,omitempty"`
	// Transfer is the queue and reason of the last successful transfer.
	Transfer *TransferExpect `json:"transfer,omitempty"`
	// Visits are phases the call must have entered.
	Visits []string `json:"visits,omitempty"`
	// Avoids are phases the call must not have entered.
	Avoids []string `json:"avoids,omitempty"`
	// Slots is a subset of the final slots.
	Slots map[string]any `json:"slots,omitempty"`
	// Lead is the lead the call ends with, as in Final.
	Lead *LeadExpect `json:"lead,omitempty"`
}

// IsModelOnly reports whether the scenario has nothing for tier 1 to replay.
func (s Scenario) IsModelOnly() bool { return len(s.Steps) == 0 && s.CallerPersona != nil }

// ActionResult scripts what a built-in tool answers.
type ActionResult struct {
	// IsOK defaults to true.
	IsOK   *bool          `json:"isOk,omitempty"`
	Error  string         `json:"error,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
	Hint   string         `json:"hint,omitempty"`
}

// Step is one event of the call. Exactly one of the event fields is set.
type Step struct {
	// Say is a finished caller utterance (its transcript).
	Say *string `json:"say,omitempty"`
	// Keypress is a DTMF digit or string.
	Keypress *string `json:"keypress,omitempty"`
	// NoInput is a silence the dead-air timer reported.
	NoInput bool `json:"noInput,omitempty"`
	// Exchange is the caller saying this and the bot answering in words with
	// no tool call: the unit the maxTurnsWithoutTool wall counts.
	Exchange *string `json:"exchange,omitempty"`
	// BotTurn is a bot turn that finished without a tool call: "reply", or
	// "interrupted" for one cut short. It feeds the maxTurnsWithoutTool wall.
	BotTurn string `json:"botTurn,omitempty"`
	// SessionLimit is the call reaching global.maxDurationSec.
	SessionLimit bool `json:"sessionLimit,omitempty"`
	// Call is a tool the bot calls, with Args. One response, one tool.
	Call string         `json:"call,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	// Batch is several tools called in one response, in order. The tools are
	// judged in the phase the response was made in, and the last move wins.
	Batch []ToolCall `json:"batch,omitempty"`
	// Repeat runs the step this many times (default once), for a loop such as
	// a model that keeps retrying a tool. Its expectations hold every time.
	Repeat int `json:"repeat,omitempty"`

	Expect StepExpect `json:"expect,omitempty"`
}

// ToolCall is one member of a Batch.
type ToolCall struct {
	Call string         `json:"call"`
	Args map[string]any `json:"args,omitempty"`
}

// StepExpect is checked after its step. Everything is optional, except that a
// tool call is expected to succeed and to be allowed in the current phase
// unless the step says otherwise.
type StepExpect struct {
	// Phase is the phase the call is in after the step.
	Phase string `json:"phase,omitempty"`
	// IsOK is whether the tool succeeded (default true). Applies to the last
	// tool of a step.
	IsOK *bool `json:"isOk,omitempty"`
	// IsRefused expects the phase to refuse the tool as not allowed.
	IsRefused bool `json:"isRefused,omitempty"`
	// Result is a subset of the tool output the model receives (fields as
	// the flow maps them, plus ok/error).
	Result map[string]any `json:"result,omitempty"`
	// Slots is a subset of the slots after the step.
	Slots map[string]any `json:"slots,omitempty"`
	// Announce is the exact line the call's phase announces after the step.
	Announce *string `json:"announce,omitempty"`
	// InstructionContains are substrings the phase's rendered instruction
	// must contain, so a slot reference that fell out of the flow is caught.
	InstructionContains []string `json:"instructionContains,omitempty"`
}

// Final is checked after the last step.
type Final struct {
	// Path is the phases entered, in order, starting with the initial one.
	Path []string `json:"path,omitempty"`
	// Phase is the phase the call ends in.
	Phase string `json:"phase,omitempty"`
	// Ending is the last built-in action that succeeded: "hangup",
	// "transfer_to_agent" or "take_message"; "none" when no built-in did.
	Ending string `json:"ending,omitempty"`
	// Transfer is the queue and reason of the last transfer that succeeded.
	Transfer *TransferExpect `json:"transfer,omitempty"`
	// ToolsRun lists the tools that ran (allowed ones), in order.
	ToolsRun []string `json:"toolsRun,omitempty"`
	// Slots is a subset of the final slots.
	Slots map[string]any `json:"slots,omitempty"`
	// Lead is the lead the call ended with: the last write that succeeded,
	// a record_lead call or the lead a phase declares, recorded on entry.
	Lead *LeadExpect `json:"lead,omitempty"`
}

// LeadExpect is the lead a campaign call concluded. Notes is checked only when
// given. IsUnrecorded expects that no lead was ever recorded, by record_lead or
// by a phase's declaration, which is not the same as the intent NONE.
type LeadExpect struct {
	Intent       string `json:"intent,omitempty"`
	Notes        string `json:"notes,omitempty"`
	IsUnrecorded bool   `json:"isUnrecorded,omitempty"`
}

// TransferExpect names what a transfer asked for; an empty field is not checked.
type TransferExpect struct {
	Queue  string `json:"queue,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ParseScenarios decodes a scenario file. Unknown fields are an error: a
// misspelt expectation would otherwise pass by checking nothing.
func ParseScenarios(data []byte) (*File, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("flowsim: cannot parse scenarios: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("flowsim: trailing data after the scenarios")
	}
	if len(file.Scenarios) == 0 {
		return nil, fmt.Errorf("flowsim: the file has no scenarios")
	}
	seen := map[string]bool{}
	for i := range file.Scenarios {
		s := &file.Scenarios[i]
		if s.Name == "" {
			return nil, fmt.Errorf("flowsim: scenario %d has no name", i+1)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("flowsim: two scenarios are called %q", s.Name)
		}
		seen[s.Name] = true
		if len(s.Steps) == 0 && s.CallerPersona == nil {
			return nil, fmt.Errorf("flowsim: scenario %q has neither steps nor a callerPersona", s.Name)
		}
		if (s.CallerPersona == nil) != (s.Model == nil) {
			return nil, fmt.Errorf("flowsim: scenario %q: callerPersona and model go together", s.Name)
		}
		if s.CallerPersona != nil && s.CallerPersona.Goal == "" {
			return nil, fmt.Errorf("flowsim: scenario %q: callerPersona has no goal", s.Name)
		}
		if s.Model != nil && (s.Model.PassRate < 0 || s.Model.PassRate > 1) {
			return nil, fmt.Errorf("flowsim: scenario %q: model.passRate is outside 0..1", s.Name)
		}
		for j, step := range s.Steps {
			if err := step.validate(); err != nil {
				return nil, fmt.Errorf("flowsim: scenario %q step %d: %w", s.Name, j+1, err)
			}
		}
	}
	return &file, nil
}

// LoadScenarios reads and parses a scenario file.
func LoadScenarios(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	file, err := ParseScenarios(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

func (s Step) validate() error {
	events := 0
	for _, set := range []bool{s.Say != nil, s.Keypress != nil, s.Exchange != nil, s.NoInput, s.BotTurn != "",
		s.SessionLimit, s.Call != "", len(s.Batch) > 0} {
		if set {
			events++
		}
	}
	if s.Repeat < 0 {
		return fmt.Errorf("repeat is negative")
	}
	if events != 1 {
		return fmt.Errorf("a step is exactly one of say, keypress, exchange, noInput, botTurn, sessionLimit, call, batch (found %d)", events)
	}
	if s.BotTurn != "" && s.BotTurn != "reply" && s.BotTurn != "interrupted" {
		return fmt.Errorf("botTurn is %q; want \"reply\" or \"interrupted\"", s.BotTurn)
	}
	for _, c := range s.Batch {
		if c.Call == "" {
			return fmt.Errorf("a batch member has no call")
		}
	}
	return nil
}
