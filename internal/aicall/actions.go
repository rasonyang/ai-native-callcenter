// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// callActions is what one call's flow tools actually do.
//
// The invariant all of them share: an action that ends or moves the call never
// runs while the bot is mid-sentence. The tool returns immediately — the model
// needs the result to speak its closing line — and the action itself is armed,
// to run when that line has finished playing, with a cap so a stalled provider
// cannot hold the caller hostage.
type callActions struct {
	orchestrator *Orchestrator
	session      *Session
	log          *slog.Logger

	// callerChannel is the caller's leg on the switch — the one that survives
	// this conversation and goes on to a queue.
	callerChannel string
	fallbackQueue *uuid.UUID

	mu    sync.Mutex
	armed func()
	// armedInTurn is the model turn the arming tool call arrived in. The
	// action waits for the playback of a LATER turn: the model speaks its
	// closing line in the turn created by the tool's own result, and the turn
	// that carried the tool call may finish playing after arming — acting on
	// that one would cut in before the line is spoken.
	armedInTurn int
	// isLineSpoken records that a later turn has finished generating — the
	// closing line exists and is playing out or already played.
	isLineSpoken bool
	// armedGeneration lets the cap timer recognise the action it belongs to.
	armedGeneration int
}

// TransferToAgent checks the queue can take the caller, then arms the
// transfer to run once the bridge line has been heard.
func (a *callActions) TransferToAgent(ctx context.Context, request flow.TransferRequest) (flow.Result, error) {
	queue, ok := a.orchestrator.findQueue(ctx, request.Queue)
	if !ok {
		// The refusal is a conversation, not an error: the bot explains and
		// offers what it can still do.
		return flow.Failed("QUEUE_UNKNOWN", a.refusalHint()), nil
	}
	if !queue.IsEnabled {
		return flow.Failed("QUEUE_CLOSED", a.refusalHint()), nil
	}
	if a.callerChannel == "" {
		return flow.Result{}, errNoCallerChannel
	}

	// The summary rides the caller's channel into the queue, so the agent who
	// answers already knows what the conversation established.
	a.stampChannel("aicc_bot_summary", request.Summary)
	a.stampChannel("aicc_bot_reason", request.Reason)
	if len(request.Slots) > 0 {
		if encoded, err := json.Marshal(request.Slots); err == nil {
			a.stampChannel("aicc_bot_slots", string(encoded))
		}
	}

	a.arm(ctx, func() {
		a.log.Info("transferring the caller", "queue", queue.Name, "ext", queue.ExtNumber)
		if err := a.orchestrator.cfg.Switch.TransferToExtension(
			a.callerChannel, queue.ExtNumber, "default"); err != nil {
			a.log.Error("transfer failed", "queue", queue.Name, "error", err)
		}
		// Our SIP leg's job is done either way; the caller's leg has moved on.
		a.session.Close(context.Background())
	})

	return flow.Succeeded(map[string]any{"queue": queue.Name}, ""), nil
}

// TakeMessage records what the caller wants passed on.
//
// Durable storage for callbacks arrives with the CDR work; until then the
// message is at least in the structured log rather than lost.
func (a *callActions) TakeMessage(_ context.Context, request flow.MessageRequest) (flow.Result, error) {
	a.log.Info("message taken",
		"message", request.Message, "callbackNumber", request.CallbackNumber)
	return flow.Succeeded(nil, ""), nil
}

// Hangup ends the call once the goodbye has been heard.
func (a *callActions) Hangup(ctx context.Context, _ flow.HangupRequest) (flow.Result, error) {
	a.arm(ctx, func() {
		a.log.Info("hanging up after the farewell")
		a.session.Close(context.Background())
	})
	return flow.Succeeded(nil, ""), nil
}

// arm schedules an action for the end of the closing line the model is about
// to speak, with a cap in case that line never finishes.
func (a *callActions) arm(_ context.Context, action func()) {
	a.mu.Lock()
	a.armed = action
	a.armedInTurn = a.session.currentTurn()
	a.isLineSpoken = false
	a.armedGeneration++
	generation := a.armedGeneration
	a.mu.Unlock()

	time.AfterFunc(actionGraceCap, func() {
		a.mu.Lock()
		isStillArmed := a.armed != nil && a.armedGeneration == generation
		armed := a.armed
		if isStillArmed {
			a.armed = nil
		}
		a.mu.Unlock()
		if isStillArmed {
			a.log.Warn("running an armed action at the cap; playback never finished")
			armed()
		}
	})
}

// onPlaybackDone runs whatever was armed, now that the caller has heard the
// line that precedes it. turn says whose playback this was: the turn the tool
// call arrived in does not count, only a turn the tool's result created.
func (a *callActions) onPlaybackDone(turn int) {
	a.mu.Lock()
	if a.armed == nil || turn <= a.armedInTurn {
		a.mu.Unlock()
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	armed()
}

// onTurnDone records that the closing line has finished generating.
func (a *callActions) onTurnDone(turn int) {
	a.mu.Lock()
	if a.armed != nil && turn > a.armedInTurn {
		a.isLineSpoken = true
	}
	a.mu.Unlock()
}

// onBargeIn fires the armed action when the caller talks over or after the
// closing line.
//
// This exists because of what real calls showed: the caller answers the
// goodbye — "bye now" — the detector reports speech, the playback watch is
// superseded, and PLAYBACK_DONE never arrives; every call then ended on the
// grace cap, five silent seconds late. A caller speaking after the closing
// line is not a reason to wait longer; it is the moment to act.
func (a *callActions) onBargeIn() {
	a.mu.Lock()
	if a.armed == nil || !a.isLineSpoken {
		a.mu.Unlock()
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	a.log.Info("caller spoke after the closing line; running the armed action now")
	armed()
}

// rescueCaller moves the caller to the number's fallback queue after a failed
// conversation.
func (a *callActions) rescueCaller() {
	if a.callerChannel == "" || a.fallbackQueue == nil {
		a.session.Close(context.Background())
		return
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(context.Background())
	if err == nil {
		for _, queue := range queues {
			if queue.ID == *a.fallbackQueue {
				a.log.Info("rescuing the caller to the fallback queue", "queue", queue.Name)
				if err := a.orchestrator.cfg.Switch.TransferToExtension(
					a.callerChannel, queue.ExtNumber, "default"); err != nil {
					a.log.Error("rescue transfer failed", "error", err)
				}
				break
			}
		}
	}
	a.session.Close(context.Background())
}

func (a *callActions) stampChannel(name, value string) {
	if value == "" {
		return
	}
	if err := a.orchestrator.cfg.Switch.SetVariable(a.callerChannel, name, value); err != nil {
		a.log.Warn("could not stamp the caller's channel", "variable", name, "error", err)
	}
}

func (a *callActions) refusalHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "这个队列现在无法接听。请向来电者说明，并主动提出为他们留言。"
	}
	return "That queue cannot take the call right now. Explain that to the caller " +
		"and offer to take a message instead."
}

var errNoCallerChannel = errors.New(
	"the bot leg carried no caller channel header, so there is nothing to transfer")
