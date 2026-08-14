// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// The SIP headers the dialplan attaches to a bot leg. They are the only thing
// correlating the SIP dialog with the call the rest of the system knows.
const (
	headerCallID    = "X-Aicc-Call-Id"
	headerDID       = "X-Aicc-Did"
	headerLanguage  = "X-Aicc-Language"
	headerANI       = "X-Aicc-Ani"
	headerChannelID = "X-Aicc-Channel-Id"
)

// actionGraceCap bounds the wait between arming a transfer or hangup and the
// bridge line finishing playback. If playback never completes — the provider
// stalled mid-goodbye — the action runs anyway rather than holding the caller.
//
// The clock starts at the tool call, before the closing line even begins to
// generate, so the cap has to cover generation plus playback plus drain on the
// slower provider. Five seconds proved too tight on real calls: the line was
// still playing when the cap cut it off.
const actionGraceCap = 10 * time.Second

// Catalog is what the orchestrator needs to know about numbers and queues.
type Catalog interface {
	DIDs(ctx context.Context) ([]catalog.DID, error)
	Queues(ctx context.Context) ([]catalog.Queue, error)
}

// FlowSource resolves a DID's flow to a runnable spec.
type FlowSource interface {
	PublishedSpec(ctx context.Context, flowID uuid.UUID) (*flow.Spec, error)
}

// Switch is what the orchestrator does to the caller's own leg on the
// telephone switch — the leg bridged to us, not our SIP dialog.
type Switch interface {
	// TransferToExtension moves the caller's channel to a dialplan extension.
	TransferToExtension(channelID, extension, context string) error
	// SetVariable stamps business context onto the caller's channel, so it
	// survives into the queue and the answering agent's screen pop.
	SetVariable(channelID, name, value string) error
}

// SessionFactory builds a provider session; swapped out in tests.
type SessionFactory func(profile provider.Profile, log *slog.Logger) (provider.VoiceSession, error)

// OrchestratorConfig wires the orchestrator.
type OrchestratorConfig struct {
	UAS      voice.Config
	Catalog  Catalog
	Flows    FlowSource
	Switch   Switch
	Sessions SessionFactory
	// Ledger receives finished calls; nil disables writing.
	Ledger Ledger
	// BackendBase is the base URL for flows' declarative HTTP tools.
	BackendBase string
	Logger      *slog.Logger
}

// Orchestrator answers bot legs and runs a conversation on each.
//
// It is the composition root of the AI side: the UAS accepts the call, a flow
// is chosen by the number dialled, a provider by the call's language, and the
// bridge moves audio while the flow steers. Nothing below it knows the whole
// shape; nothing above it needs to.
type Orchestrator struct {
	cfg OrchestratorConfig
	uas *voice.UAS
	log *slog.Logger

	mu    sync.Mutex
	calls map[string]*activeCall
}

type activeCall struct {
	session *Session
	cancel  context.CancelFunc
}

// NewOrchestrator prepares the AI side. Nothing listens until Start.
func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Catalog == nil || cfg.Flows == nil || cfg.Switch == nil {
		return nil, errors.New("aicall: the orchestrator needs a catalog, flows and a switch")
	}
	if cfg.Sessions == nil {
		cfg.Sessions = func(profile provider.Profile, log *slog.Logger) (provider.VoiceSession, error) {
			return provider.New(profile, log)
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	o := &Orchestrator{
		cfg:   cfg,
		log:   cfg.Logger,
		calls: map[string]*activeCall{},
	}
	uasCfg := cfg.UAS
	uasCfg.Logger = cfg.Logger
	o.uas = voice.NewUAS(uasCfg)
	o.uas.OnCallStarted = o.onCallStarted
	o.uas.OnCallEnded = o.onCallEnded
	o.uas.OnCallFailed = func(dialog *voice.Dialog, err error) {
		o.log.Error("bot leg failed before media", "callId", dialog.CallID, "error", err)
	}
	return o, nil
}

// Start begins accepting bot legs.
func (o *Orchestrator) Start() error { return o.uas.Start() }

// Stop refuses new calls and ends the ones in progress.
func (o *Orchestrator) Stop() {
	o.uas.Stop()

	o.mu.Lock()
	calls := make([]*activeCall, 0, len(o.calls))
	for _, call := range o.calls {
		calls = append(calls, call)
	}
	o.mu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

// ActiveCalls reports how many conversations are running.
func (o *Orchestrator) ActiveCalls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.calls)
}

func (o *Orchestrator) onCallStarted(dialog *voice.Dialog) {
	ctx, cancel := context.WithCancel(context.Background())

	o.mu.Lock()
	o.calls[dialog.CallID] = &activeCall{cancel: cancel}
	o.mu.Unlock()

	go func() {
		defer cancel()
		if err := o.runCall(ctx, dialog); err != nil {
			o.log.Error("ai call could not run", "callId", dialog.CallID, "error", err)
			o.rescue(dialog)
		}
	}()
}

func (o *Orchestrator) onCallEnded(dialog *voice.Dialog) {
	o.mu.Lock()
	call := o.calls[dialog.CallID]
	delete(o.calls, dialog.CallID)
	o.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

// runCall is one conversation, from headers to hangup.
func (o *Orchestrator) runCall(ctx context.Context, dialog *voice.Dialog) error {
	headers := dialog.CustomHeaders
	didNumber := headers[headerDID]
	language := headers[headerLanguage]
	callerChannel := headers[headerChannelID]

	log := o.log.With("callId", dialog.CallID,
		"aiccCallId", headers[headerCallID], "did", didNumber)

	did, err := o.findDID(ctx, didNumber)
	if err != nil {
		return err
	}
	if language == "" {
		language = did.Language
	}
	if did.FlowID == nil {
		return fmt.Errorf("number %s has no flow assigned", didNumber)
	}
	spec, err := o.cfg.Flows.PublishedSpec(ctx, *did.FlowID)
	if err != nil {
		return err
	}

	// The ledger identity is the call id minted before any leg existed, so the
	// bot's rows and the human path's rows meet on the same key.
	ledgerCallID, err := uuid.Parse(headers[headerCallID])
	if err != nil {
		ledgerCallID = uuid.New()
		log.Warn("bot leg carried no parseable call id; minted one",
			"header", headers[headerCallID], "callId", ledgerCallID)
	}
	recorder := newCallRecorder(ledgerCallID, time.Now())
	facts := &callFacts{
		callType:           callTypeInbound,
		language:           flow.Lang(language),
		fromNumber:         headers[headerANI],
		did:                didNumber,
		flowID:             did.FlowID,
		flowSlug:           spec.ID,
		isRecordingEnabled: did.IsRecordingEnabled,
		tech: map[string]any{
			"sipCallId":     dialog.CallID,
			"codec":         dialog.RTP.Law().String(),
			"remoteRtpAddr": dialog.RemoteRTPAddr.String(),
		},
	}
	defer recorder.finish(o.cfg.Ledger, facts, log)

	// The flow's phase machine, and the actions its tools perform on this call.
	engine := flow.NewEngine(spec, language, map[string]any{
		"caller": headers[headerANI],
		"entry":  spec.Entry,
		"did":    didNumber,
	}, log)
	actions := &callActions{
		orchestrator:  o,
		log:           log,
		callerChannel: callerChannel,
		fallbackQueue: did.FallbackQueueID,
		recorder:      recorder,
		facts:         facts,
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(o.cfg.BackendBase), log)

	profile := provider.ProfileForLanguage(language)
	model, err := o.cfg.Sessions(profile, log)
	if err != nil {
		return fmt.Errorf("no %s provider: %w", profile.Name, err)
	}

	session, err := New(FromDialog(dialog), model, profile, Config{
		Session: provider.SessionConfig{
			Instructions: runtime.Instructions(),
			Language:     language,
			Turn:         provider.DefaultTurnDetection(),
			Tools:        runtime.Tools(),
		},
		Logger: log,
	})
	if err != nil {
		return err
	}
	actions.session = session

	o.mu.Lock()
	if call := o.calls[dialog.CallID]; call != nil {
		call.session = session
	}
	o.mu.Unlock()

	if err := session.Start(ctx); err != nil {
		return err
	}
	log.Info("ai conversation started", "flowId", spec.ID,
		"provider", profile.Name, "language", language)

	o.drive(ctx, session, runtime, actions, recorder, log)
	return nil
}

// drive consumes the bridge's events and lets the flow steer.
func (o *Orchestrator) drive(ctx context.Context, session *Session,
	runtime *flow.Runtime, actions *callActions, recorder *callRecorder, log *slog.Logger) {

	for event := range session.Events() {
		switch event.Type {
		case EventTypeCallerSaid:
			if event.IsFinal {
				recorder.say(store.TranscriptRoleCaller, event.Text)
			}

		case EventTypeBotSaid:
			if event.IsFinal {
				recorder.say(store.TranscriptRoleBot, event.Text)
			}

		case EventTypeDigit:
			recorder.say(store.TranscriptRoleCaller, "[keypad] "+event.Text)

		case EventTypeToolCall:
			recorder.toolCall(event.ToolName, event.ToolArgs)
			output, moved := runtime.Dispatch(ctx, event.ToolName, event.ToolArgs)
			recorder.toolResult(event.ToolName, output)
			if err := session.AnswerTool(event.ToolCallID, output, ""); err != nil {
				log.Warn("could not answer a tool call", "tool", event.ToolName, "error", err)
			}
			// A phase change re-pins the standing instructions, which is what
			// keeps collected facts alive past a provider's context limits.
			o.afterMove(moved, session, runtime, actions, log)

		case EventTypeNoInput:
			o.handleDeadAir(session, runtime, actions, log)

		case EventTypeTurnDone:
			actions.onTurnDone(event.Turn)

		case EventTypeBargeIn:
			actions.onBargeIn()

		case EventTypePlaybackDone:
			actions.onPlaybackDone(event.Turn)

		case EventTypeFailed:
			// The conversation cannot continue; the caller still can.
			log.Warn("conversation failed, rescuing the caller", "reason", event.Text)
			recorder.markFailed("MEDIA_OR_PROVIDER_FAILURE")
			actions.rescueCaller()
			return

		case EventTypeEnded:
			return
		}
	}
}

// afterMove follows up a phase change: the standing instructions are re-pinned
// so collected facts survive a provider's context limits, and a terminal phase
// ends the call once its closing words have been heard. Without that last rule
// a conversation that reaches goodbye simply stays open, with the bot politely
// re-engaging the silence forever.
func (o *Orchestrator) afterMove(moved string, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) {

	if moved == "" {
		return
	}
	if err := session.Reinstruct(runtime.Instructions()); err != nil {
		log.Warn("could not update instructions", "error", err)
	}
	if runtime.Engine().IsTerminal() {
		log.Info("flow reached a terminal phase; the call ends after the closing line",
			"node", moved)
		actions.arm(context.Background(), func() {
			session.Close(context.Background())
		})
	}
}

// handleDeadAir asks the model to re-engage a silent caller, or moves the flow
// on when its rules treat silence as an answer.
func (o *Orchestrator) handleDeadAir(session *Session, runtime *flow.Runtime,
	actions *callActions, log *slog.Logger) {

	moved := runtime.OnNoInput()
	o.afterMove(moved, session, runtime, actions, log)

	cue := "(The caller has been silent. Gently check whether they are still there " +
		"and repeat the current question.)"
	if runtime.Engine().Lang() == flow.LangZH {
		cue = "（来电者一直没有说话。请温和地确认对方是否还在线，并重复当前的问题。）"
	}
	if moved != "" && runtime.Engine().IsTerminal() {
		// The flow has decided the conversation is over; the model's next words
		// are the goodbye, not another prompt.
		cue = "(The caller seems to have gone. Say a brief goodbye; the call will end.)"
		if runtime.Engine().Lang() == flow.LangZH {
			cue = "（来电者似乎已离开。请简短道别，通话随后会结束。）"
		}
	}
	if err := session.model.SendUserText(cue); err != nil {
		log.Warn("could not prompt a silent caller", "error", err)
	}
}

// findDID resolves a dialled number.
func (o *Orchestrator) findDID(ctx context.Context, number string) (catalog.DID, error) {
	dids, err := o.cfg.Catalog.DIDs(ctx)
	if err != nil {
		return catalog.DID{}, fmt.Errorf("read numbers: %w", err)
	}
	for _, did := range dids {
		if did.Number == number && did.IsEnabled {
			return did, nil
		}
	}
	return catalog.DID{}, fmt.Errorf("no enabled number %q", number)
}

// findQueue resolves a flow's queue name to the queue.
func (o *Orchestrator) findQueue(ctx context.Context, name string) (catalog.Queue, bool) {
	queues, err := o.cfg.Catalog.Queues(ctx)
	if err != nil {
		o.log.Error("read queues", "error", err)
		return catalog.Queue{}, false
	}
	for _, queue := range queues {
		if queue.Name == name || queue.ExtNumber == name {
			return queue, true
		}
	}
	return catalog.Queue{}, false
}

// rescue sends a caller whose conversation never started to the number's
// fallback queue, so a broken provider degrades to a longer wait rather than
// to a dead line.
func (o *Orchestrator) rescue(dialog *voice.Dialog) {
	headers := dialog.CustomHeaders
	defer dialog.Stop()

	channel := headers[headerChannelID]
	if channel == "" {
		return
	}
	did, err := o.findDID(context.Background(), headers[headerDID])
	if err != nil || did.FallbackQueueID == nil {
		return
	}
	queues, err := o.cfg.Catalog.Queues(context.Background())
	if err != nil {
		return
	}
	for _, queue := range queues {
		if queue.ID == *did.FallbackQueueID {
			o.log.Info("transferring the caller to the fallback queue",
				"callId", dialog.CallID, "queue", queue.Name)
			if err := o.cfg.Switch.TransferToExtension(channel, queue.ExtNumber, "default"); err != nil {
				o.log.Error("fallback transfer failed", "callId", dialog.CallID, "error", err)
			}
			return
		}
	}
}
