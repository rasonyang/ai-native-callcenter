// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Runtime runs one call's tools under the flow's control.
//
// It is where the division of labour is enforced. The model decides to call a
// tool; the runtime decides whether that is allowed in the current phase, runs
// it, and answers with a hint saying what to do next. The hint is how the flow
// steers without ever writing dialogue: it travels back inside the tool result,
// so the model reads it as something it has just learned rather than as an
// instruction arriving out of nowhere.
type Runtime struct {
	engine  *Engine
	actions Actions
	backend *Backend
	log     *slog.Logger

	builtins map[string]Tool
}

// NewRuntime prepares the tools for one call.
//
// queues are the names this deployment has, offered to the model as the only
// answers transfer_to_agent's queue argument accepts.
func NewRuntime(engine *Engine, actions Actions, backend *Backend,
	queues []string, log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{
		engine:   engine,
		actions:  actions,
		backend:  backend,
		log:      log.With("flowId", engine.Spec().ID),
		builtins: builtinSchemas(engine.Lang(), queues),
	}
}

// Engine exposes the phase machine.
func (r *Runtime) Engine() *Engine { return r.engine }

// Tools is what the model is offered, in the call's language.
//
// Only tools the flow actually references are offered. A model given a tool no
// phase permits will eventually call it, and be refused — better not to
// mention it.
func (r *Runtime) Tools() []provider.ToolSpec {
	referenced := r.referencedTools()
	specs := make([]provider.ToolSpec, 0, len(referenced))

	for _, name := range slices.Sorted(maps.Keys(referenced)) {
		tool, ok := r.builtins[name]
		if !ok {
			tool, ok = r.engine.Spec().Tools[name]
		}
		if !ok {
			continue
		}
		specs = append(specs, provider.ToolSpec{
			Name:        name,
			Description: tool.Description.For(r.engine.Lang()),
			Parameters:  tool.Parameters,
		})
	}
	return specs
}

// referencedTools is every tool some phase can reach.
func (r *Runtime) referencedTools() map[string]bool {
	spec := r.engine.Spec()
	referenced := map[string]bool{}

	isEverythingAllowed := false
	for _, node := range spec.Nodes {
		for _, name := range node.Tools {
			if name == "*" {
				isEverythingAllowed = true
				continue
			}
			referenced[name] = true
		}
	}
	for _, name := range spec.Global.AlwaysAllowedTools {
		referenced[name] = true
	}
	if isEverythingAllowed {
		for name := range spec.Tools {
			referenced[name] = true
		}
		for name := range r.builtins {
			referenced[name] = true
		}
	}
	return referenced
}

// Instructions are the model's standing instructions: who it is, the rules it
// works under, and what it is doing right now.
func (r *Runtime) Instructions() string {
	spec := r.engine.Spec()
	lang := r.engine.Lang()

	var b strings.Builder
	b.WriteString(spec.Global.Persona.For(lang))
	if rules := spec.Global.Rules.For(lang); len(rules) > 0 {
		b.WriteString("\n\n")
		for _, rule := range rules {
			b.WriteString("- ")
			b.WriteString(rule)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(r.phasePreamble(r.engine.NodeID()))
	b.WriteString(r.engine.Instruction())
	return b.String()
}

// Dispatch runs a tool the model asked for and returns what to send back.
//
// It also reports whether the phase changed, which is what the caller uses to
// re-pin the model's standing instructions.
func (r *Runtime) Dispatch(ctx context.Context, name, arguments string) (output string, newNode string) {
	args := parseArguments(arguments)

	// A tool used outside its phase is answered, not failed. The model gets
	// ok=0 and the current phase's instruction, which reads as being told it
	// is getting ahead of itself — and it carries on talking to the caller
	// instead of falling silent.
	if !r.engine.IsToolAllowed(name) {
		r.log.Info("tool refused by the current phase",
			"tool", name, "node", r.engine.NodeID())
		return Failed(
			fmt.Sprintf("the tool %s is not available in the current phase", name),
			r.phasePreamble(r.engine.NodeID())+r.engine.Instruction(),
		).asToolOutput(), ""
	}

	result, err := r.run(ctx, name, args)
	if err != nil {
		r.log.Error("tool failed", "tool", name, "node", r.engine.NodeID(), "error", err)
		result = Failed(err.Error(), r.recoveryHint())
	}

	// The outcome flag is recorded alongside the tool's own fields, so a rule
	// can distinguish a transfer that happened from one that was refused.
	recorded := map[string]any{"ok": boolAsFlag(result.IsOK)}
	maps.Copy(recorded, result.Fields)
	newNode = r.engine.OnToolResult(name, recorded)
	if newNode != "" {
		// The flow is authoritative on progression: whatever the tool wanted to
		// say next, the new phase's instruction replaces it.
		result.Hint = r.phasePreamble(newNode) + r.engine.Instruction()
	}

	output = result.asToolOutput()
	// The reason, not just the verdict: a refusal that logs only isOk=false
	// leaves the operator with a bot that would not put the caller through and
	// no way to learn why without reading the transcript table.
	fields := []any{"tool", name, "node", r.engine.NodeID(),
		"isOk", result.IsOK, "movedTo", newNode}
	if !result.IsOK && result.Error != "" {
		fields = append(fields, "error", result.Error)
	}
	r.log.Info("tool ran", fields...)
	return output, newNode
}

// OnNoInput reports dead air to the flow, returning the new phase if the flow
// treats silence as a reason to move on.
func (r *Runtime) OnNoInput() string { return r.engine.OnNoInput() }

// run executes one tool: a built-in acts on the call, anything else is the
// flow's own declarative backend call.
func (r *Runtime) run(ctx context.Context, name string, args map[string]any) (Result, error) {
	switch name {
	case ToolTransferToAgent:
		return r.actions.TransferToAgent(ctx, TransferRequest{
			Queue:   stringArg(args, "queue"),
			Reason:  stringArg(args, "reason"),
			Summary: stringArg(args, "summary"),
			Slots:   mapArg(args, "slots"),
		})
	case ToolTakeMessage:
		return r.actions.TakeMessage(ctx, MessageRequest{
			Message:        stringArg(args, "message"),
			CallbackNumber: stringArg(args, "callbackNumber"),
		})
	case ToolHangup:
		return r.actions.Hangup(ctx, HangupRequest{
			IsFarewellSpoken: boolArg(args, "isFarewellSpoken"),
		})
	}

	tool, ok := r.engine.Spec().Tools[name]
	if !ok {
		return Result{}, fmt.Errorf("there is no tool called %s", name)
	}
	if r.backend == nil {
		return Result{}, fmt.Errorf("no backend is configured for %s", name)
	}
	return r.backend.Call(ctx, tool, args, r.engine.Slots())
}

// phasePreamble labels an instruction so the model can tell a phase change
// from ordinary guidance.
func (r *Runtime) phasePreamble(node string) string {
	if r.engine.Lang() == LangZH {
		return "当前环节【" + node + "】：\n"
	}
	return "Current phase [" + node + "]:\n"
}

// recoveryHint tells the model what to do when a backend has failed. Saying
// nothing would leave it silent on the line.
func (r *Runtime) recoveryHint() string {
	if r.engine.Lang() == LangZH {
		return "系统暂时无法完成该操作。请向来电者说明，如再次失败请转人工。"
	}
	return "That could not be completed just now. Tell the caller so, and if it " +
		"fails again offer to put them through to a person."
}

func parseArguments(arguments string) map[string]any {
	if arguments == "" {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return map[string]any{}
	}
	return args
}

func stringArg(args map[string]any, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	return asString(value)
}

func mapArg(args map[string]any, name string) map[string]any {
	if value, ok := args[name].(map[string]any); ok {
		return value
	}
	return nil
}

func boolArg(args map[string]any, name string) bool {
	switch value := args[name].(type) {
	case bool:
		return value
	case string:
		return value == "true" || value == "1"
	default:
		return false
	}
}
