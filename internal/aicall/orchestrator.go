// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
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
	headerCallType  = "X-Aicc-Call-Type"
	// Sent only by the AI-outbound bridge: the platform placed this call to a
	// customer, so the ANI is the person answering and the DID is our number.
	headerPlacedToCustomer = "X-Aicc-Placed-To-Customer"
)

// actionGraceCap bounds the wait between arming a transfer or hangup and the
// bridge line finishing playback. If playback never completes — the provider
// stalled mid-goodbye — the action runs anyway rather than holding the caller.
//
// The clock starts at the tool call, before the closing line even begins to
// generate, so the cap has to cover generation plus playback plus drain on the
// slower provider. Five seconds proved too tight on real calls: the line was
// still playing when the cap cut it off.
//
// Once the closing line has finished generating, the cap is moved out to
// cover the audio still queued ahead of and within it, plus lineDrainMargin;
// it never moves earlier.
const actionGraceCap = 10 * time.Second

// lineDrainMargin is the slack over the queued audio's own length that a
// generated closing line gets to finish playing.
const lineDrainMargin = 2 * time.Second

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
	// EndCallerWithTheirBridge restores the switch's own teardown rule before
	// the caller is handed on. The inbound script suspends it for the bot
	// bridge; everything downstream of us expects it back.
	EndCallerWithTheirBridge(channelID string) error
}

// CallDataSource answers what business data a call was placed with. Only a
// call this application placed has any, and it never travels through the
// switch — so on a call the bot finishes alone, this is the only way that data
// reaches the ledger row the bot writes.
type CallDataSource interface {
	CallData(callID uuid.UUID) map[string]any
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
	// CallData answers what business data a placed call was asked to carry.
	// Nil leaves the bot's ledger row without it, which is what happened
	// before there was anywhere to ask.
	CallData CallDataSource
	// Transcripts hands out the per-call actor that owns transcript order for
	// both phases of a call; nil disables transcripts entirely.
	Transcripts *transcript.Registry
	// AnnounceCallback tells the live event stream about a callback the bot
	// just created; nil means nobody is watching.
	AnnounceCallback func(callback store.Callback)
	// AnnounceBotSession tells the live event stream that the conversation on
	// a call has actually begun — which the bot leg answering does not say,
	// because the model session is opened after that and can fail. Nil means
	// nobody is watching.
	AnnounceBotSession func(callID, flowID uuid.UUID)
	// BackendBase is the base URL for flows' declarative HTTP tools.
	BackendBase string
	// IsGreetingGated and GreetingMediaWait hold each call's greeting until
	// the caller's inbound media has started; see Config. False is the
	// original behaviour, greeting the moment the model session is ready.
	IsGreetingGated   bool
	GreetingMediaWait time.Duration
	// Profile is the provider this deployment runs, resolved at startup. The
	// same one answers every call, whatever language it is in.
	Profile provider.Profile
	Logger  *slog.Logger
}

// Orchestrator answers bot legs and runs a conversation on each.
//
// It is the composition root of the AI side: the UAS accepts the call, a flow
// is chosen by the number dialled, the provider is the deployment's, and the
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
	if cfg.Profile.Name == "" {
		return nil, errors.New("aicall: the orchestrator needs a provider profile")
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
	obs.CallStarted(obs.CallKindBot)

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

	obs.CallEnded(obs.CallKindBot)
	// The media counters are atomics the session kept all along; teardown is
	// where reading them costs nothing and where they are finally complete.
	if dialog.RTP != nil {
		sent, _, late := dialog.RTP.Health()
		lost, dropped, filled := dialog.RTP.JitterStats()
		obs.RecordRTPHealth(sent, late, lost, dropped, filled)
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
	// An outbound conversation is the same machinery with the direction
	// stamped by whoever originated it; the default is a caller dialing in.
	direction := callTypeFromHeader(headers[headerCallType])
	recorder := newCallRecorder(ledgerCallID, time.Now(), o.transcriptActor(ledgerCallID, direction))
	facts := &callFacts{
		callType:           direction,
		isPlacedToCustomer: headers[headerPlacedToCustomer] == "true",
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
	// A call this application placed may have been asked to carry business
	// data. On a call the bot finishes alone this row is the only one written,
	// so without this the data would reach the agent's screen on a transfer
	// and vanish from the ledger on every call that never needed one.
	if o.cfg.CallData != nil {
		facts.userData = o.cfg.CallData.CallData(ledgerCallID)
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
	// An agent who hangs up first leaves the bot no chance to say what it ran,
	// and it is their call's row the human path writes, so the flow and the
	// number go onto their channel now rather than at an ending that may never
	// come. Not aicc_bot_sec: that variable alone is what says a call was
	// handed over.
	if facts.isPlacedByAgent() && callerChannel != "" {
		actions.stampBotShare(facts)
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(o.cfg.BackendBase),
		o.queueNames(ctx, did.FallbackQueueID), log)

	profile := o.cfg.Profile
	model, err := o.cfg.Sessions(profile, log)
	if err != nil {
		return fmt.Errorf("no %s provider: %w", profile.Name, err)
	}

	session, err := New(FromDialog(dialog), model, profile, Config{
		Session:           sessionConfigFor(spec, runtime, language),
		IsGreetingGated:   o.cfg.IsGreetingGated,
		GreetingMediaWait: o.cfg.GreetingMediaWait,
		IsEndingArmed:     actions.isArmed,
		Logger:            log,
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
	// Only now. Everything before this point could still have ended with the
	// caller being rescued to a queue without a word being said to them.
	if announce := o.cfg.AnnounceBotSession; announce != nil {
		// The flow's database identity rather than the slug in the spec: it is
		// what the ledger row will carry and what the flows screen is keyed
		// by, so a client can follow it to the revision that answered.
		announce(ledgerCallID, *did.FlowID)
	}

	o.drive(ctx, session, runtime, actions, recorder,
		budgetFor(spec).since(recorder.answeredAt, time.Now()), log)
	// The call is over. Whatever was waiting for a closing line to be heard
	// will never hear it, and the caller's channel is gone with the call.
	actions.disarm()
	return nil
}

// sessionConfigFor is everything the model needs before it hears anything: who
// it is, what phase it is in, what it may call — and the entry phase's own
// opening line, where the flow named one.
//
// The opening line belongs here rather than beside the other announcements
// because the first turn is asked for while the session is being started.
// There is no moment afterwards early enough to catch it: by the time the
// bridge is running, the greeting is already being made. Session.Start adds
// the opening gate (SessionConfig.OpeningGate) when the deployment holds the
// greeting for the caller's media; it is not part of what a flow decides.
func sessionConfigFor(spec *flow.Spec, runtime *flow.Runtime, language string) provider.SessionConfig {
	return provider.SessionConfig{
		Instructions: runtime.Instructions(),
		Language:     language,
		// The bot's own voice, published with the flow. Empty falls back
		// to the provider profile's default.
		Voice:       spec.Global.Voice,
		Turn:        provider.DefaultTurnDetection(),
		Tools:       runtime.Tools(),
		OpeningText: runtime.Announce(),
	}
}

// drive consumes the bridge's events and lets the flow steer, and keeps the
// call inside its time budget (sessionBudget). The budget's clocks are
// delivered here, on the call's own goroutine, because the engine and the
// runtime they act on belong to it; stopping them when drive returns is all
// the cancelling they need.
func (o *Orchestrator) drive(ctx context.Context, session *Session,
	runtime *flow.Runtime, actions *callActions, recorder *callRecorder,
	budget sessionBudget, log *slog.Logger) {

	wall := turnsWithoutToolWatch{toolCallTurn: -1}
	var claims claimWatch
	reportClaims := func(turn int, found []claim) {
		for _, c := range found {
			o.reportUnbackedClaim(turn, c, recorder, log)
		}
	}
	// deadline is when the limit falls due, fixed once: the wrap-up steer
	// counts the time left from it whenever the instructions are rendered.
	deadline := time.Now().Add(budget.limitAfter)
	var wrapUpDue, limitDue <-chan time.Time
	if budget.wrapUpAfter > 0 {
		timer := time.NewTimer(budget.wrapUpAfter)
		defer timer.Stop()
		wrapUpDue = timer.C
	}
	var limitTimer *time.Timer
	if budget.limitAfter > 0 {
		limitTimer = time.NewTimer(budget.limitAfter)
		defer limitTimer.Stop()
		limitDue = limitTimer.C
	}
	// isLimitWaiting is a limit that came due while the bot held the floor,
	// waiting for it to be free; limitDue is then its backstop.
	isLimitWaiting := false
	enforceLimit := func(how limitEnforcement) {
		isLimitWaiting = false
		limitDue = nil
		o.enforceSessionLimit(ctx, session, runtime, actions, recorder, how, log)
	}

	// batch is the tool calls of the response being answered, from the first
	// of them to that response's TURN_DONE.
	var batch toolBatch
	events := session.Events()
	for {
		select {
		case <-wrapUpDue:
			wrapUpDue = nil
			o.handleWrapUpDue(ctx, session, runtime, actions, deadline, log)
			continue

		case <-limitDue:
			switch {
			case isLimitWaiting:
				log.Warn("the bot kept the floor past the time limit; ending the call anyway")
				enforceLimit(limitForced)
			case session.isHoldingTheFloor():
				// Asking for a turn over one in progress is refused or cuts
				// it off; the limit waits for the floor, but not forever.
				log.Info("call reached its time limit; waiting for the bot to finish its turn")
				isLimitWaiting = true
				limitTimer.Reset(cmp.Or(budget.floorWait, actionGraceCap))
			default:
				enforceLimit(limitFloorFree)
			}
			continue

		case event, ok := <-events:
			if !ok {
				return
			}
			switch event.Type {
			case EventTypeCustomerSaid:
				if event.IsFinal {
					recorder.say(store.SpeakerCustomer, event.Text)
					runtime.OnCallerSpoke(event.Text)
				}

			case EventTypeBotSaid:
				if event.IsFinal {
					recorder.say(store.SpeakerBot, event.Text)
					reportClaims(event.Turn, claims.onBotSaid(event.Turn, event.Text))
				}

			case EventTypeDigit:
				recorder.say(store.SpeakerCustomer, "[keypad] "+event.Text)
				runtime.OnCallerKeyed()

			case EventTypeToolCall:
				wall.toolCallTurn = event.Turn
				if !batch.isOpen {
					batch.isOpen = true
					runtime.BeginToolBatch()
				}
				o.answerToolCall(ctx, event, &batch, session, runtime, actions, recorder, log)

			case EventTypeNoInput:
				if isLimitWaiting {
					enforceLimit(limitCallerSilent)
					continue
				}
				o.handleDeadAir(session, runtime, actions, log)

			case EventTypeTurnDone:
				reportClaims(event.Turn, claims.onTurnDone(event.Turn,
					event.Turn == wall.toolCallTurn, actions.isArmed()))
				actions.onTurnDone(event.Turn, event.IsInterrupted)
				wall.onTurnDone(event.Turn, event.IsInterrupted, runtime)
				o.closeToolBatch(&batch, session, runtime, log)

			case EventTypeBargeIn:
				actions.onBargeIn()

			case EventTypePlaybackDone:
				// A limit waiting for the floor goes first, ahead of the
				// turns-without-a-tool wall: both would close the call, and
				// the limit is the one that says why.
				if isLimitWaiting {
					if !actions.isArmed() {
						// A PLAYBACK_DONE already queued when the next turn
						// began is stale: the floor is held again, and the
						// backstop still bounds the wait.
						if session.isHoldingTheFloor() {
							log.Info("time limit still waiting: the bot holds the floor again")
							continue
						}
						enforceLimit(limitFloorFree)
						continue
					}
					// The turn that held the floor armed an ending of its
					// own; that ending, bounded by its grace cap, is the one.
					log.Info("call reached its time limit while an ending was armed; the armed action ends it")
					isLimitWaiting = false
					limitDue = nil
				}
				o.handlePlaybackDone(event.Turn, &wall, session, runtime, actions, log)

			case EventTypeFailed:
				// The conversation cannot continue; the caller still can.
				log.Warn("conversation failed, rescuing the caller",
					"reason", event.Text, "cause", event.FailureCause)
				recorder.markFailed(hangupCauseFor(event.FailureCause))
				actions.rescueCaller()
				return

			case EventTypeEnded:
				return
			}
		}
	}
}

// reportUnbackedClaim records a claim the bot made with no tool call behind
// it (claimWatch). It changes nothing about the call: it is counted, logged
// and written to the call's record, so the rate can be measured — and the
// engines compared — before anything is made to act on it (#44). The log
// carries the words that matched and not the line, which is the transcript's
// to keep.
func (o *Orchestrator) reportUnbackedClaim(turn int, c claim, recorder *callRecorder,
	log *slog.Logger) {
	log.Warn("the bot claimed an action no tool call backs",
		"claim", c.kind, "turn", turn, "phrase", c.phrase)
	obs.RecordUnbackedClaim(c.kind, o.cfg.Profile.Name)
	recorder.markUnbackedClaim(c.kind)
}

// hangupCauseMediaOrProvider is what a failed AI call has always been released
// with: the bot could not go on, and nothing said which half of it broke.
const hangupCauseMediaOrProvider = "MEDIA_OR_PROVIDER_FAILURE"

// hangupCauseFor is what the CDR says about a failure.
//
// A provider that named its own cause is taken at its word — the whole point of
// naming one is that it reaches whoever reads the call afterwards — and a
// failure nobody could explain is still a failure of the media or the provider,
// which is what this path has always recorded and what it records now.
func hangupCauseFor(cause provider.FailureCause) string {
	if cause == "" {
		return hangupCauseMediaOrProvider
	}
	return string(cause)
}

// answerToolCall runs a tool the model asked for, answers it, and follows up
// whatever phase change the result caused. A result that stays in the phase
// but changes what the phase's instruction renders (a lookup that overwrites a
// slot the instruction quotes) re-pins the standing instructions too, before
// the answer, so the turn it asks for already runs under the fresh text and
// not under the previous record's.
//
// One move is answered differently: into a terminal phase that has a line of
// its own, on a profile that puts that line in the tool result
// (Profile.PutsTerminalAnnounceInToolResult — the Realtime ones). There the
// tool result's hint IS the line — SayExactly in place of the new phase's
// instruction — and the turn the result produces is the line's turn; nothing
// else is asked for. Asking for the line in a turn of its own on top of the
// result, as every other move does, lost to the result on qwen every time it
// was measured (0 of 7): the model answers the last thing on the caller's side
// of the conversation, which is the tool result, over a per-response override.
// With the line in the result it was said 7 of 7 on qwen, and as often as the
// other path on openai (docs/design/qwen-findings.md, W-Q1).
//
// Doubao keeps its own SpeakText, because that is exact by construction and a
// direction to a model is not. Gemini keeps it for now: that model sometimes
// answers a tool result with nothing (gemini-findings W-G6), and the SpeakText
// after the result is what still says the line then. A phase that is not
// terminal keeps it on every client: the direction would stay in the history
// and nobody has measured what it does to the turns after it.
//
// That SpeakText waits for the batch: the call belongs to a response that may
// ask for more tools, and a line pre-empts. Said now, it would cancel the
// response before its later calls arrived, and on doubao and gemini, which
// wait for every call's answer, it would go out while one was still owed.
// closeToolBatch says it once the response is over and every call answered.
func (o *Orchestrator) answerToolCall(ctx context.Context, event Event, batch *toolBatch,
	session *Session, runtime *flow.Runtime, actions *callActions, recorder *callRecorder,
	log *slog.Logger) {

	recorder.toolCall(event.ToolName, event.ToolArgs)
	output, moved := runtime.Dispatch(ctx, event.ToolName, event.ToolArgs)

	if !o.isLineTheToolAnswer(moved, runtime) {
		recorder.toolResult(event.ToolName, output, moved)
		if moved == "" && runtime.IsInstructionStale() {
			// Nothing is armed and no line is said: the phase did not change.
			// A move later in the batch re-pins through enterPhase anyway.
			log.Info("a tool result changed the phase's instruction; re-pinning",
				"tool", event.ToolName, "node", runtime.Engine().NodeID())
			if err := session.Reinstruct(runtime.Instructions()); err != nil {
				log.Warn("could not update instructions", "error", err)
			}
		}
		if err := session.AnswerTool(event.ToolCallID, output, ""); err != nil {
			log.Warn("could not answer a tool call", "tool", event.ToolName, "error", err)
		}
		if moved != "" {
			// A phase change re-pins the standing instructions, which is what
			// keeps collected facts alive past a provider's context limits.
			// A later move in the batch replaces this one's line.
			o.enterPhase(moved, session, runtime, actions, log)
			batch.lineOwedIn = ""
			if runtime.Announce() != "" {
				batch.lineOwedIn = moved
			}
		}
		return
	}
	// The line rides in this answer; an earlier move's line is superseded.
	batch.lineOwedIn = ""

	// The flow's own hint for the move is the new phase's instruction; the
	// line replaces it, in the language the line is written in. The phase's
	// instruction still reaches the model, as the standing instructions.
	output = provider.MergeHint(output,
		provider.SayExactly(runtime.Announce(), runtime.Engine().Lang()))
	recorder.toolResult(event.ToolName, output, moved)

	// Before the answer, both of them. The instructions first, so the turn the
	// answer asks for runs under the terminal phase's. The ending second: it
	// remembers the turn in progress and waits for the playback of a later
	// one, and the answer is what brings the line's turn into existence —
	// arming after it would race that turn into being before it was recorded,
	// and no playback would ever count.
	o.enterPhase(moved, session, runtime, actions, log)
	log.Info("the tool's answer asks for the phase's own line", "node", moved)
	if err := session.AnswerTool(event.ToolCallID, output, ""); err != nil {
		log.Warn("could not answer a tool call", "tool", event.ToolName, "error", err)
	}
}

// isLineTheToolAnswer reports whether a tool result that moved the call to
// moved should carry the new phase's line itself, rather than have it asked for
// in a turn of its own: only into a terminal phase with a line, and only on a
// profile that puts that line in the result. See answerToolCall.
func (o *Orchestrator) isLineTheToolAnswer(moved string, runtime *flow.Runtime) bool {
	return moved != "" &&
		o.cfg.Profile.PutsTerminalAnnounceInToolResult &&
		runtime.Engine().IsTerminal() &&
		runtime.Announce() != ""
}

// afterMove follows up a phase change: the standing instructions are re-pinned
// so collected facts survive a provider's context limits, the phase's own line
// is said where it has one, and a terminal phase ends the call once its closing
// words have been heard. Without that last rule a conversation that reaches
// goodbye simply stays open, with the bot politely re-engaging the silence
// forever.
//
// It reports whether the new phase's own line was asked for, because a caller
// that was about to ask for a turn of its own must not: the line is that turn.
func (o *Orchestrator) afterMove(moved string, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) (isLineAsked bool) {

	if moved == "" {
		return false
	}
	o.enterPhase(moved, session, runtime, actions, log)
	// Last, and that ordering is load-bearing on a terminal phase. Arming
	// remembers the turn it happened in and waits for the playback of a later
	// one, because the closing line is spoken in a turn of its own. Asking for
	// the line first would race that turn into existence before the turn is
	// recorded, and then no playback would ever count: the call would end ten
	// seconds later on the grace cap, in silence the caller has to sit through.
	return o.sayPhaseLine(session, runtime, log)
}

// sayPhaseLine asks for the current phase's own line, where it has one, and
// reports whether it did.
func (o *Orchestrator) sayPhaseLine(session *Session, runtime *flow.Runtime,
	log *slog.Logger) bool {

	line := runtime.Announce()
	if line == "" {
		return false
	}
	if err := session.Speak(line, runtime.Engine().IsTerminal()); err != nil {
		log.Warn("could not say the phase's own line",
			"node", runtime.Engine().NodeID(), "error", err)
		return false
	}
	return true
}

// toolBatch is the tool calls of one model response, answered one at a time as
// they arrive and closed by the response's TURN_DONE (Engine.BeginToolBatch).
type toolBatch struct {
	isOpen bool
	// lineOwedIn is the phase the batch's last move went to, when that phase
	// has a line of its own that waits for the batch to close
	// (answerToolCall). Empty is no line owed.
	lineOwedIn string
}

// closeToolBatch ends the batch of the response that has just finished, if
// there is one, and says the line its last move left owed. Every call of the
// response has been answered by now: they all arrive before its TURN_DONE. A
// call that has moved on since, other than by a tool (the time limit), said
// its own phase's line on the way, and owes nothing more.
func (o *Orchestrator) closeToolBatch(batch *toolBatch, session *Session,
	runtime *flow.Runtime, log *slog.Logger) {

	if !batch.isOpen {
		return
	}
	runtime.EndToolBatch()
	lineOwedIn := batch.lineOwedIn
	*batch = toolBatch{}
	if lineOwedIn != "" && lineOwedIn == runtime.Engine().NodeID() {
		o.sayPhaseLine(session, runtime, log)
	}
}

// enterPhase re-pins the standing instructions for the phase the call has just
// moved to and, when that phase is terminal, arms the call's ending.
func (o *Orchestrator) enterPhase(moved string, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) {

	if err := session.Reinstruct(runtime.Instructions()); err != nil {
		log.Warn("could not update instructions", "error", err)
	}
	if runtime.Engine().IsTerminal() {
		o.armTheEnding(moved, session, actions, log)
	}
}

// armTheEnding schedules the end of a call the flow has concluded, for once the
// caller has heard what the terminal phase had to say.
func (o *Orchestrator) armTheEnding(moved string, session *Session,
	actions *callActions, log *slog.Logger) {

	// Unless the tool that moved us here already armed the call's ending.
	// A phase is usually terminal *because* of that tool — transfer_to_agent
	// lands in a "we're putting you through" phase, hangup in a goodbye.
	// Arming keeps what is armed, but the flow's own ending would still mark
	// the ledger a hangup over a transfer, so it is not attempted at all.
	if actions.isArmed() {
		log.Info("flow reached a terminal phase; the armed action ends the call",
			"node", moved)
		return
	}
	log.Info("flow reached a terminal phase; the call ends after the closing line",
		"node", moved)
	// The flow concluding the call is containment, exactly like the
	// hangup tool concluding it — the ledger must not tell them apart.
	if actions.recorder != nil {
		actions.recorder.markHangup()
	}
	actions.arm(context.Background(), func() {
		actions.markFinished("FLOW_END")
		session.Close(context.Background())
	})
}

// handleDeadAir asks the model to re-engage a silent caller, or moves the flow
// on when its rules treat silence as an answer.
func (o *Orchestrator) handleDeadAir(session *Session, runtime *flow.Runtime,
	actions *callActions, log *slog.Logger) {

	moved := runtime.OnNoInput()
	// A move that carried words of its own has already asked for the next
	// turn, and those words are the flow's answer to the silence. The cue is
	// sent only when no line was: asking for both is two turns at once — one
	// provider refuses the second, another says both — and the cue would
	// contradict a closing line the flow has just chosen.
	if o.afterMove(moved, session, runtime, actions, log) {
		return
	}

	cue := "(The caller has been silent. Gently check whether they are still there " +
		"and repeat the current question.)"
	if runtime.Engine().Lang() == flow.LangZH {
		cue = "（来电者一直没有说话。请温和地确认对方是否还在线，并重复当前的问题。）"
	}
	if moved != "" && runtime.Engine().IsTerminal() {
		// The flow has decided the conversation is over; the model's next words
		// are the goodbye, not another prompt.
		cue = goodbyeCue(runtime.Engine().Lang(), true)
	}
	if err := session.SendCue(cue); err != nil {
		log.Warn("could not prompt a silent caller", "error", err)
	}
}

// goodbyeCue asks the model for the goodbye of a call the flow has closed on
// its own, when the closing phase has no line of its own to say instead. A
// caller who went silent is told goodbye as someone who may have gone; one
// who is still talking, as someone who is there.
func goodbyeCue(lang string, isCallerSilent bool) string {
	switch {
	case isCallerSilent && lang == flow.LangZH:
		return "（来电者似乎已离开。请简短道别，通话随后会结束。）"
	case isCallerSilent:
		return "(The caller seems to have gone. Say a brief goodbye; the call will end.)"
	case lang == flow.LangZH:
		return "（通话即将结束。请感谢来电者并简短道别，通话随后会结束。）"
	default:
		return "(The call is ending now. Thank the caller and say a brief goodbye; the call will end.)"
	}
}

// turnsWithoutToolWatch follows the model's turns for the flow's
// maxTurnsWithoutTool wall, on the call's own goroutine. The engine counts;
// this knows which turn is which, which the engine cannot.
type turnsWithoutToolWatch struct {
	// toolCallTurn is the turn the latest tool call arrived in, so the turn's
	// TURN_DONE can tell the engine it made one. -1 before any.
	toolCallTurn int
	// crossedInTurn is the completed turn after which the call stood past the
	// wall, and whose playback the move waits for. 0 is none: turns are
	// numbered from 1.
	crossedInTurn int
}

// onTurnDone accounts for one finished turn and records whether the call now
// stands past the wall, and after which turn.
//
// Every turn re-decides it. A turn that follows the crossing one supersedes it
// — the caller talked over the reply, or answered it and got another — and the
// one after it is what the caller must hear before the goodbye; if that turn
// was a tool call, the count is back at zero and there is nothing to wait for.
func (w *turnsWithoutToolWatch) onTurnDone(turn int, isInterrupted bool, runtime *flow.Runtime) {
	isPast := runtime.OnBotTurnDone(turn == w.toolCallTurn, isInterrupted)
	w.crossedInTurn = 0
	if isPast {
		w.crossedInTurn = turn
	}
}

// handlePlaybackDone runs what waited for the caller to hear a turn: an armed
// action first, then the maxTurnsWithoutTool wall.
//
// The wall's move is made here, on the playback of the reply that crossed it,
// and not when the count crossed. Then the model had only just finished that
// reply, the caller had not heard it, and — with the provider's own turn
// detection answering the same utterance — the goodbye asked for on top of it
// was refused on qwen, so the call ended on the reply after it with no goodbye
// at all. Here the floor is free and the reply has been heard. A caller who
// talks over the reply first supersedes it: barge-in flushes the turn, its
// playback never completes, and the turn the caller's words produce decides
// again (turnsWithoutToolWatch.onTurnDone).
//
// The move itself is any other terminal arrival's: afterMove re-pins the
// instructions and arms the ending before the closing line is asked for. A
// closing phase with no line gets the goodbye cue instead, as handleDeadAir's
// terminal move does — without it the model has nothing to answer and the call
// ends on the grace cap in silence.
func (o *Orchestrator) handlePlaybackDone(turn int, wall *turnsWithoutToolWatch,
	session *Session, runtime *flow.Runtime, actions *callActions, log *slog.Logger) {

	// An action already armed ends the call its own way; the wall has nothing
	// to add to it.
	wasArmed := actions.isArmed()
	actions.onPlaybackDone(turn)
	if wasArmed || wall.crossedInTurn == 0 || turn != wall.crossedInTurn {
		return
	}
	wall.crossedInTurn = 0

	moved := runtime.CloseAtTurnsWithoutToolWall()
	if moved == "" || o.afterMove(moved, session, runtime, actions, log) {
		return
	}
	if err := session.SendCue(goodbyeCue(runtime.Engine().Lang(), false)); err != nil {
		log.Warn("could not ask for the goodbye", "error", err)
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
// queueNames is what a transfer on this number may name, offered to the model
// as an enum.
//
// The number's own queue when it has one, and nothing else. A caller on a
// mobile-support line asking for a person wants that line's agents, and the
// number is where an operator said which those are — the model has no way to
// know and no business guessing. Offered the whole catalogue it guessed
// reasonably and wrongly: on a Chinese call it picked support-zh, a real queue
// staffed by nobody who works this number, and the caller waited on hold music
// for an agent who was sitting in another queue.
//
// Every queue only when the number names none. Then there is nothing better to
// go on, and a model choosing among real queues still beats one inventing a
// name.
//
// Disabled queues are included either way: the tool refuses them with a reason
// the bot can explain ("we are closed"), which is a better conversation than a
// model that cannot name the queue the caller is asking for.
func (o *Orchestrator) queueNames(ctx context.Context, fallbackQueueID *uuid.UUID) []string {
	queues, err := o.cfg.Catalog.Queues(ctx)
	if err != nil {
		o.log.Error("read queues", "error", err)
		return nil
	}
	if fallbackQueueID != nil {
		for _, queue := range queues {
			if queue.ID == *fallbackQueueID {
				return []string{queue.Name}
			}
		}
	}
	names := make([]string, 0, len(queues))
	for _, queue := range queues {
		names = append(names, queue.Name)
	}
	return names
}

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
			if err := o.cfg.Switch.TransferToExtension(channel, queue.ExtNumber, ""); err != nil {
				o.log.Error("fallback transfer failed", "callId", dialog.CallID, "error", err)
			}
			return
		}
	}
}

// callTypeFromHeader reads the type hint a dialplan or the platform stamped on
// the bot leg. Anything else, including no header (a carrier's caller), is
// INBOUND.
func callTypeFromHeader(v string) callType {
	switch callType(v) {
	case callTypeOutbound, callTypeInternal:
		return callType(v)
	}
	return callTypeInbound
}
