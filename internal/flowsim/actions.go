// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"fmt"
	"slices"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// scriptedActions stands in for the application's call control. It records
// what the flow asked the call to do and answers as the scenario scripts.
type scriptedActions struct {
	sc       Scenario
	transfer *flow.TransferRequest
	// lead is the last lead record_lead stored, nil if none was.
	lead *flow.LeadRequest
	// last is the name of the last built-in that succeeded.
	last string
	// isEndingArmed says a transfer or a hangup has succeeded: in the
	// application the call's ending is then armed, and a later hangup is the
	// closing paths' to answer, its reason unjudged (flow.EndingState).
	isEndingArmed bool
}

func newActions(sc Scenario) *scriptedActions { return &scriptedActions{sc: sc} }

func (a *scriptedActions) ending() string {
	if a.last == "" {
		return "none"
	}
	return a.last
}

func (a *scriptedActions) answer(name string) flow.Result {
	script, ok := a.sc.Actions[name]
	if !ok {
		a.last = name
		return flow.Succeeded(nil, "")
	}
	if script.IsOK != nil && !*script.IsOK {
		return flow.Result{Error: script.Error, Fields: script.Fields, Hint: script.Hint}
	}
	a.last = name
	return flow.Succeeded(script.Fields, script.Hint)
}

func (a *scriptedActions) TransferToAgent(_ context.Context, req flow.TransferRequest) (flow.Result, error) {
	if len(a.sc.Queues) > 0 && !slices.Contains(a.sc.Queues, req.Queue) {
		return flow.Failed(fmt.Sprintf("there is no queue called %q", req.Queue), ""), nil
	}
	result := a.answer(flow.ToolTransferToAgent)
	if result.IsOK {
		a.transfer = &req
		a.isEndingArmed = true
	}
	return result, nil
}

func (a *scriptedActions) TakeMessage(context.Context, flow.MessageRequest) (flow.Result, error) {
	return a.answer(flow.ToolTakeMessage), nil
}

func (a *scriptedActions) Hangup(context.Context, flow.HangupRequest) (flow.Result, error) {
	result := a.answer(flow.ToolHangup)
	if result.IsOK {
		a.isEndingArmed = true
	}
	return result, nil
}

// IsEndingUnderway is flow.EndingState: true once a transfer or a hangup has
// succeeded, as the application's armed ending is.
func (a *scriptedActions) IsEndingUnderway() bool { return a.isEndingArmed }

// RecordLead keeps the last lead, as the attempt row does, whether record_lead
// or a phase's declaration wrote it; a scripted record_lead failure refuses
// both, as an attempt no longer in flight would. It never becomes the
// call's ending: recording a lead does not end a call.
func (a *scriptedActions) RecordLead(_ context.Context, req flow.LeadRequest) (flow.Result, error) {
	script, ok := a.sc.Actions[flow.ToolRecordLead]
	if ok && script.IsOK != nil && !*script.IsOK {
		return flow.Result{Error: script.Error, Fields: script.Fields, Hint: script.Hint}, nil
	}
	a.lead = &req
	return flow.Succeeded(nil, ""), nil
}
