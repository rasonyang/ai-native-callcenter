// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/aicall"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/egress"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/doubao"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
	"github.com/rasonyang/ai-native-callcenter/internal/wrapupdraft"
)

// The parts of the running system, as the composition uses them.
//
// Interfaces rather than the concrete services, and narrow ones, because the
// point of this file is to be able to hand the composition fakes and see what
// it did to them. A connection that is only made in run() is a connection
// nothing can observe: it compiles whether or not it is there, no test names
// it, and CI is green with it deleted. That is how a media tap came to be
// attached with nothing retiring it, how staffing came to be reconciled on
// registration but not on reconnect, and how the transcript panel came to be
// told a state nothing published.
type switchWiring interface {
	AttachTaps(telephony.Tapper)
	AttachCDR(*telephony.CDRAssembler)
	AttachAudiences(telephony.Audiences)
	AttachTranscripts(telephony.TranscriptStarter)
	AttachQueues(telephony.QueueCatalog)
	ReconcileWaiting(ctx context.Context)
}

type presenceWiring interface {
	AttachStaffing(agents.Staffing)
	AttachWrapUps(agents.WrapUpLedger)
	AttachDrafter(agents.WrapUpDrafter)
	SyncSwitch(ctx context.Context)
	// ReleaseAgentsWithoutPhones signs off every READY agent whose phone the
	// switch does not have registered. It is separate from SyncSwitch because
	// it is only truthful when the registrations were actually read.
	ReleaseAgentsWithoutPhones(ctx context.Context)
	ObserveDevice(ctx context.Context, extensionNumber string, signal agents.DeviceSignal)
	// NoteDevice records a phone without mirroring it, for the pass that has
	// to happen before presence is sent to the switch.
	NoteDevice(extensionNumber string, isRegistered, isInService bool)
}

// staffingWiring is the catalog in its two roles: the thing agent presence
// reconciles staffing through, and the thing a reconnect rebuilds tiers with.
type staffingWiring interface {
	agents.Staffing
	SyncTiers(ctx context.Context)
}

type connectHook interface {
	OnConnect(fn func(context.Context))
	// OnLost fires when the connection to the switch drops. Both halves are
	// needed to say anything useful about the link: an event that only ever
	// reports coming back cannot tell a reconnect from a first start.
	OnLost(fn func())
}

// composition is every connection between the parts, in one place that can be
// driven without a database, a switch or a listening socket.
//
// It holds no construction: run() builds the parts, this joins them. Taps is
// nil when transcription is disabled, which is the one connection that is
// conditional.
type composition struct {
	Registry    *telephony.Registry
	Coordinator switchWiring
	Agents      presenceWiring
	Catalog     staffingWiring
	Link        connectHook
	CDR         *telephony.CDRAssembler
	Queues      telephony.QueueCatalog
	WrapUps     agents.WrapUpLedger
	// Drafter writes the AI draft of a wrap-up. Nil when the text model is not
	// configured or live transcription is off, which is the second conditional
	// connection the composition has: after-call work is then what it was.
	Drafter     agents.WrapUpDrafter
	Transcripts transcriptRegistry
	Audiences   telephony.Audiences
	Taps        telephony.Tapper
	// Lines gives back the outbound line a placed call held when the call ends.
	// Nil is allowed: the AI path may be off.
	Lines         lineReleaser
	Registrations func() ([]telephony.Registration, error)
	Events        systemPublisher
	Log           *slog.Logger
	// Reconcile adopts the channels the switch holds and settles the ones
	// nobody is looking after. It runs on every connect, last: the screens are
	// told the link is up first, and nothing above depends on it.
	Reconcile func(context.Context)
}

// systemPublisher is the slice of the hub used to announce facts about the
// system rather than about any call.
type systemPublisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// announceLink tells every screen whether the switch is reachable.
//
// Broadcast, not scoped: an agent whose switch has gone cannot take a call,
// place one, or be told why their buttons have stopped working, and that is
// not a fact about any one call or queue. The type has been in the contract
// with nothing producing it, so a panel could only ever guess — and the hook
// to hang it on was written and never called, like several others found this
// week.
func (c composition) announceLink(ctx context.Context, isUp bool) {
	if c.Events == nil {
		return
	}
	c.Events.Publish(ctx, events.Event{
		Type:    events.TypeSystemLink,
		Payload: map[string]any{"isUp": isUp},
	}, events.Scope{IsBroadcast: true})
}

// connect makes every connection. Order is load-bearing in one place and
// stated there.
func (c composition) connect() {
	// An agent becoming addressable is the first moment a tier for them can
	// succeed, so registration reconciles their staffing. Without this, an
	// agent staffed while signed out stays unroutable until the next reconnect
	// — Available, in a queue, offered nothing.
	c.Agents.AttachStaffing(c.Catalog)

	// After-call work opens a record the moment it begins, so a finished call
	// always has one. Without this the ledger only ever hears about the calls
	// somebody remembered to write up, and "nobody filed this" and "nobody
	// finished typing" are the same absence.
	c.Agents.AttachWrapUps(c.WrapUps)
	if c.Drafter != nil {
		c.Agents.AttachDrafter(c.Drafter)
	}

	// The switch forgets its agents when it restarts, and we are the source of
	// truth, so every reconnect rebuilds its view.
	c.Link.OnConnect(c.onSwitchConnected)

	// And the screens are told either way. Losing the switch is the one
	// failure an agent can neither see nor work around, and until now the
	// only sign of it was that nothing happened any more.
	c.Link.OnConnect(func(ctx context.Context) { c.announceLink(ctx, true) })
	c.Link.OnLost(func() { c.announceLink(context.Background(), false) })

	// What the switch is holding that this process knows nothing about: calls
	// that outlived a restart, and whatever happened while the link was down.
	if c.Reconcile != nil {
		c.Link.OnConnect(c.Reconcile)
	}

	// Who may see a call's live transcript is decided from who is on the call,
	// which only this side knows. Without it every transcript event falls
	// through the hub's scope check as an unscoped system notice.
	c.Coordinator.AttachAudiences(c.Audiences)

	// The switch names a queue; every screen names an id, a display name and
	// a target to measure a wait against. Without this the waiting line stays
	// empty and the agent's queue panel has nothing to show.
	c.Coordinator.AttachQueues(c.Queues)

	c.Coordinator.AttachCDR(c.CDR)
	// After AttachCDR, because that is what sets OnCallFinished: this composes
	// on top of the assembler rather than replacing it, and a CDR is not
	// something to lose in order to free a goroutine.
	c.Registry.OnCallFinished = retireTranscriptWithCall(c.Registry.OnCallFinished, c.Transcripts)

	// A line on a DID is held until the call's last leg is gone, on whichever
	// of the two hooks ends the call: finished (the last party released) or
	// retired (the actor exits, however it ended). Releasing is idempotent.
	if c.Lines != nil {
		c.Registry.OnCallFinished = releaseLinesOnFinish(c.Registry.OnCallFinished, c.Lines)
		c.Registry.OnCallRetired = releaseLinesOnRetire(c.Registry.OnCallRetired, c.Lines)
	}

	if c.Taps != nil {
		c.Coordinator.AttachTaps(c.Taps)
		// Attached with the tap, because the two are one decision: the
		// coordinator opens the actor at the moment it attaches the tap and
		// under the same gates, so a call that is transcribed always has
		// somewhere for the transcript to go. A deployment that turns
		// transcription off has no tap and therefore wants no actor — which is
		// why this is inside the same branch rather than beside it.
		c.Coordinator.AttachTranscripts(transcriptStarter{reg: c.Transcripts})
		// Switch events start and stop the tap in the ordinary case; the
		// call's own retirement is what makes it converge in every other one.
		c.Registry.OnCallRetired = detachTapsWithCall(c.Registry.OnCallRetired, c.Taps)
	}
}

// onSwitchConnected rebuilds the switch's view of us and ours of it.
//
// In one direction the switch forgot its agents when it restarted and we are
// the source of truth. In the other the switch knows which phones are
// registered, which live events alone never tell us: a phone that registered
// before this process started would otherwise look missing and its agent
// unroutable. An agent the switch knows but has no tier for is not routable
// either — the queue has nobody to offer to and the caller abandons — and
// presence and staffing are both ours, so both are rebuilt here.
//
// Who is waiting is the third such fact and the one that was missing. A queue
// join is announced once; a caller who joined while this process was down was
// announced to nobody, and no later event says it again. The switch has been
// holding them the whole time.
func (c composition) onSwitchConnected(ctx context.Context) {
	// The phones first, and quietly. Presence comes back from the database
	// with them unknown, unknown reads as unreachable, and an agent who is
	// perfectly fine therefore computes as On Break. Mirroring that before
	// learning better is not a harmless flicker: measured on 2026-08-23, the
	// queue had already begun offering a caller to that agent, this set them
	// On Break mid-delivery, and the caller waited out the queue's no-answer
	// delay — eighty seconds — before anybody was tried again.
	regs, err := c.Registrations()
	isKnown := err == nil
	if err != nil {
		// Not fatal — the rest of the reconciliation still has to run, and an
		// agent whose phone we could not ask about is better mirrored from
		// what we know than left off the switch entirely. But a read that
		// failed has told us nothing, whatever it handed back with the error.
		c.Log.WarnContext(ctx, "could not read registrations", "error", err)
		regs = nil
	}
	for _, reg := range regs {
		c.Agents.NoteDevice(reg.Extension, true, reg.IsReachable)
	}

	// Only now, and only if we actually heard back, is "this agent's phone is
	// not registered" a fact. An empty list from a switch that answered means
	// no phones are registered and every READY agent is unroutable; an error
	// means we do not know, and signing off the whole room on the strength of
	// one failed ESL command is a far worse answer than leaving presence as it
	// stands until the next connect.
	if isKnown {
		c.Agents.ReleaseAgentsWithoutPhones(ctx)
	}

	c.Agents.SyncSwitch(ctx)
	c.Catalog.SyncTiers(ctx)
	c.Coordinator.ReconcileWaiting(ctx)

	// Again, now out loud: the same observations, published this time, so a
	// phone that changed while we were away reaches the screens watching it.
	for _, reg := range regs {
		// Reachability is the whole of what this pass has to say: every
		// endpoint the switch listed is registered by definition, so the
		// question left is whether it is answering, and that is the axis with
		// a name for both answers.
		signal := agents.SignalReachable
		if !reg.IsReachable {
			signal = agents.SignalUnreachable
		}
		c.Agents.ObserveDevice(ctx, reg.Extension, signal)
	}
	c.Log.InfoContext(ctx, "registrations reconciled", "endpoints", len(regs))
}

// transcriptRetirer is the part of the transcript registry this file needs.
type transcriptRetirer interface {
	Close(callID uuid.UUID)
}

// transcriptRegistry is the registry as the composition holds it: the
// coordinator opens a call's actor through it, and the call's own end closes it.
type transcriptRegistry interface {
	transcriptRetirer
	For(callID uuid.UUID, callType events.CallType, answeredAt time.Time) *transcript.Actor
}

// transcriptStarter names the coordinator's half of For.
//
// The coordinator says "this call has an agent on it now" and the registry
// turns that into the actor the ingest will look for; neither needs the other's
// vocabulary, which is why the registry's For is not simply renamed. It is
// deliberately paired with the tap in the coordinator rather than wired
// somewhere of its own — see AttachTranscripts.
type transcriptStarter struct{ reg transcriptRegistry }

func (s transcriptStarter) Start(callID uuid.UUID, callType events.CallType, answeredAt time.Time) {
	s.reg.For(callID, callType, answeredAt)
}

// retireTranscriptWithCall composes the call-finished hook so that a call's
// transcript actor is retired when the call is, without displacing whatever
// already listens — the CDR assembler is on that hook first, and a CDR is not
// something to lose in order to free a goroutine.
//
// It is a named function rather than a closure in run() because it is the one
// piece of that wiring with behaviour to get wrong: the ordering, the nil
// predecessor, and the retirement itself. A transcript actor outlives both of
// its producers — the bot's leg ends at the transfer while the human phase
// keeps writing to the same actor — so it is retired with the call and nowhere
// earlier. If it is not retired at all, every call leaves a goroutine and its
// mailbox behind for the life of the process.
func retireTranscriptWithCall(prior func(telephony.Snapshot), transcripts transcriptRetirer) func(telephony.Snapshot) {
	return func(snap telephony.Snapshot) {
		if prior != nil {
			prior(snap)
		}
		transcripts.Close(snap.CallID)
	}
}

// lineReleaser is the part of the outbound service that gives lines back.
type lineReleaser interface {
	CallEnded(callID uuid.UUID)
}

// releaseLinesOnFinish composes the call-finished hook so that a call's
// outbound line is given back, without displacing the CDR writer before it.
func releaseLinesOnFinish(prior func(telephony.Snapshot), lines lineReleaser) func(telephony.Snapshot) {
	return func(snap telephony.Snapshot) {
		if prior != nil {
			prior(snap)
		}
		lines.CallEnded(snap.CallID)
	}
}

// releaseLinesOnRetire is the same for the hook that always runs when a call's
// actor exits, which is the net under a call that never finished.
func releaseLinesOnRetire(prior func(uuid.UUID), lines lineReleaser) func(uuid.UUID) {
	return func(callID uuid.UUID) {
		if prior != nil {
			prior(callID)
		}
		lines.CallEnded(callID)
	}
}

// callTapper is the part of the transcription tap this file needs.
type callTapper interface {
	DetachCall(callID uuid.UUID)
}

// detachTapsWithCall composes the call-retired hook so that a call's media
// taps stop when the call's actor does, without displacing whatever already
// listens.
//
// It is here, and named, for the same reason retireTranscriptWithCall is: it
// is a connection that nothing else would notice was missing. The tap is
// otherwise driven entirely by CHANNEL_UNBRIDGE and CHANNEL_HANGUP, and
// neither event is owed to us — a leg transferred away, an ESL link that
// reconnected across the hangup, a call absorbed into another and retired
// mid-life. Every one of those leaves mod_audio_stream pumping a call's audio
// at an ingest whose transcript actor has been closed, for the life of the
// process, with nothing anywhere reporting a fault.
//
// The hook runs on the call actor's own goroutine as it exits, so the ESL
// round trip delays only that actor's teardown.
func detachTapsWithCall(prior func(uuid.UUID), taps callTapper) func(uuid.UUID) {
	return func(callID uuid.UUID) {
		if prior != nil {
			prior(callID)
		}
		taps.DetachCall(callID)
	}
}

// The two struct literals that assemble a whole subsystem's dependencies are
// assembled here instead, through parameters.
//
// A keyed struct literal may omit any field and still compile, so dropping one
// line disables a dependency silently — httpapi.Deps.Transcripts is how the
// snapshot learns what state transcription is in, and it is one line in a
// twelve-line literal. Parameters cannot be omitted: leaving one out is a
// compile error, which is a stronger guarantee than any test. The reflection
// tests beside these cover the other half, that every field of the result is
// populated, so a field added to either config fails until it is plumbed.

// apiDeps assembles what the HTTP server is given.
func apiDeps(
	authSvc *auth.Service,
	hub *events.Hub,
	agentSvc httpapi.AgentService,
	agentDir httpapi.AgentDirectory,
	calls httpapi.CallService,
	transcripts httpapi.TranscriptStates,
	catalogSvc httpapi.CatalogService,
	contacts httpapi.ContactService,
	dnc httpapi.DNCService,
	campaigns httpapi.CampaignService,
	ledger *store.LedgerStore,
	flows httpapi.FlowService,
	accounts httpapi.AccountService,
	trunks httpapi.TrunkReader,
	recordings httpapi.RecordingStreamer,
	auditor httpapi.Auditor,
	outboundSvc httpapi.OutboundService,
	webhooks httpapi.WebhookService,
	keys httpapi.KeyService,
	sipSessions httpapi.SIPSessionService,
	postCall httpapi.PostCallTranscriber,
	spa http.Handler,
) httpapi.Deps {
	return httpapi.Deps{
		Auth:        authSvc,
		Hub:         hub,
		Agents:      agentSvc,
		AgentDir:    agentDir,
		Calls:       calls,
		Transcripts: transcripts,
		Catalog:     catalogSvc,
		Contacts:    contacts,
		DNC:         dnc,
		Campaigns:   campaigns,
		Ledger:      ledger,
		Flows:       flows,
		Accounts:    accounts,
		Trunks:      trunks,
		Recordings:  recordings,
		Auditor:     auditor,
		Outbound:    outboundSvc,
		Webhooks:    webhooks,
		Keys:        keys,
		SIPSessions: sipSessions,
		PostCall:    postCall,
		SPA:         spa,
	}
}

// voiceProfile resolves the one provider this deployment's conversations run
// on, with its connection details applied.
//
// A deployment with the AI leg switched off still calls this, and still fails
// on a name nobody recognises: the setting is either meant or a typo, and a
// typo that only surfaces when somebody turns the bot on is a typo that
// surfaces during an incident.
func voiceProfile(cfg config.Config) (provider.Profile, error) {
	transcribeOff := strings.EqualFold(cfg.ProviderTranscribeModel, "off")
	transcribeModel := cfg.ProviderTranscribeModel
	if transcribeOff {
		transcribeModel = ""
	}
	return provider.ProfileFor(cfg.Provider, provider.Override{
		Endpoint:        cfg.ProviderEndpoint,
		Model:           cfg.ProviderModel,
		TranscribeModel: transcribeModel,
		TranscribeOff:   transcribeOff,
	})
}

// flowPublishRules is what this deployment demands of a flow before it will let
// one answer a call, beyond the flow being loadable.
//
// With the AI leg off there is no demand to make: nothing here answers a call,
// and refusing a flow over the shortcomings of a provider this process will
// never open a session with would be a rule inventing its own reason.
//
// With it on, a flow whose HTTP tools reference a secret this process does not
// have is refused: the tool would fail on every call (issue #286).
func flowPublishRules(cfg config.Config, profile provider.Profile) []flow.Rule {
	if !cfg.IsBotEnabled {
		return nil
	}
	rules := flowDraftChecks(cfg)
	if profile.RequiresTerminalAnnounce {
		rules = append(rules, flow.RequireTerminalAnnounce)
	}
	return rules
}

// flowDraftChecks are the publish rules a saved draft is warned about rather
// than refused for: a secret the operator may set after the draft is written.
func flowDraftChecks(cfg config.Config) []flow.Rule {
	if !cfg.IsBotEnabled {
		return nil
	}
	return []flow.Rule{flow.RequireToolSecrets(os.LookupEnv)}
}

// toolBackend is the one Backend every bot call's HTTP tools go through: the
// deployment's base URL, AICC_TOOL_ALLOWED_HOSTS as the outbound host policy,
// and secrets from the environment.
func toolBackend(cfg config.Config) (*flow.Backend, error) {
	allowed, err := egress.ParseAllowList(cfg.ToolAllowedHosts)
	if err != nil {
		return nil, fmt.Errorf("AICC_TOOL_ALLOWED_HOSTS: %w", err)
	}
	listed := make([]string, 0, len(allowed))
	for _, entry := range allowed {
		listed = append(listed, entry.String())
	}
	slog.Info("http tool outbound host policy",
		"allowedHosts", listed,
		"note", "private and loopback addresses and the AICC_BOT_BACKEND_BASE host need no entry; link-local and unspecified addresses are always refused")
	return flow.NewBackend(cfg.BotBackendBase, flow.WithAllowedHosts(allowed)), nil
}

// botUAS is the AI leg's SIP listener, which starts from the deployment's
// defaults and overrides only what configuration supplies.
//
// It used to be a literal that copied four fields out of voice.DefaultConfig()
// by hand. A field added to that default reaches this process only if somebody
// remembers to copy it too, and a field dropped from the copy silently becomes
// the zero value — which for RTPDeadTimeout means the dead-media check is off
// and for RTCPInterval means no reporting at all. Overriding a default cannot
// fail that way.
func botUAS(cfg config.Config, slots *voice.BotSlots) voice.Config {
	uas := voice.DefaultConfig()
	uas.SIPHost = cfg.BotSIPHost
	uas.SIPPort = cfg.BotSIPPort
	// Validated when the configuration was loaded, so a list that does not
	// parse never reaches here.
	uas.AllowedPeers, _ = cfg.BotAllowedPeerPrefixes()
	uas.RTPPortRange = [2]int{cfg.BotRTPPortLow, cfg.BotRTPPortHigh}
	uas.MaxCalls = cfg.BotMaxCalls
	// The counter the outbound originator reserves from; MaxCalls above is
	// only what a UAS without one would build for itself.
	uas.Slots = slots
	uas.RTPDeadTimeout = cfg.BotRTPDeadTimeout
	uas.FirstMediaTimeout = cfg.BotFirstMediaTimeout
	return uas
}

// voiceSession opens one conversation with the provider this deployment runs.
//
// Which client answers is decided here, in the composition root, because it
// cannot be decided anywhere lower: the clients that are not the Realtime one
// live in sub-packages of internal/provider, and a package cannot import one of
// its own children. This is the only place where all of them are already in
// scope. It is still just a SessionFactory — the orchestrator keeps its own
// default for the tests that never name a provider, and a test that wants a
// fake still swaps this one out.
//
// Every other name is a dialect of the Realtime protocol and reaches the one
// client that speaks it, which is the rule this file has always followed; the
// switch has a case per protocol, not per vendor, and it has one because one
// other protocol is spoken here.
func voiceSession(profile provider.Profile, log *slog.Logger) (provider.VoiceSession, error) {
	obs.RecordProviderSessionStarted(profile.Name)
	switch profile.Name {
	case provider.NameDoubao:
		return doubao.New(profile, log)
	default:
		return provider.New(profile, log)
	}
}

// reportMissingProviderKey says at startup what the first bot call would
// otherwise find out alone: the provider's credential is not in this process's
// environment. It reports whether it was missing.
//
// It does not refuse to start. A stack with no key is a supported way to run —
// agents, queues and the screens all work — and only the bot is silent. But
// silent is the problem: every client reads its key when a session opens, so
// without this the process logs a healthy start, and the first sign of trouble
// is a caller hearing hold music, because the orchestrator hands a call whose
// session cannot open to the number's fallback queue. The variable comes from
// the profile, which is where every client (Realtime, doubao) looks.
func reportMissingProviderKey(log *slog.Logger, profile provider.Profile, getenv func(string) string) bool {
	if profile.APIKeyEnv == "" || getenv(profile.APIKeyEnv) != "" {
		return false
	}
	log.Error(profile.APIKeyEnv+" is not set: every AI call will be transferred straight to the number's fallback queue",
		"provider", profile.Name,
		"missing", profile.APIKeyEnv,
		"hint", "set it in deploy/.env for the compose stack and recreate the container with `docker compose up -d` (restart does not re-read .env)")
	return true
}

// botConfig assembles what the AI voice leg is given.
//
// Logger is deliberately not a parameter: the orchestrator fills it with its
// own default, and passing it from here would mean this process could disagree
// with every test that builds one. Sessions used to be left the same way, and
// is not any more — see voiceSession for why the choice of client belongs to
// the composition root.
// botSwitch is the switch as the bot uses it: the adapter, with the caller's
// transfer taken through the coordinator, so the bot's handover and an agent's
// transfer of the same leg share one guard and cannot race (#303).
type botSwitch struct {
	*telephony.Adapter
	coordinator *telephony.Coordinator
}

func (b botSwitch) TransferToExtension(channelID, extension, dialplanContext string) error {
	return b.coordinator.TransferLeg(channelID, extension, dialplanContext)
}

func botConfig(
	uas voice.Config,
	catalogSvc aicall.Catalog,
	flows aicall.FlowSource,
	sw aicall.Switch,
	ledger aicall.Ledger,
	transcripts *transcript.Registry,
	backend *flow.Backend,
	profile provider.Profile,
	announce func(store.Callback),
	announceBot func(callID, flowID uuid.UUID),
	callData aicall.CallDataSource,
	timeline aicall.TimelineSource,
	leads aicall.LeadSink,
	isGreetingGated bool,
	greetingMediaWait time.Duration,
	noInput time.Duration,
	countryCode string,
	dialForm phonenum.DialForm,
) aicall.OrchestratorConfig {
	return aicall.OrchestratorConfig{
		UAS:                uas,
		Sessions:           voiceSession,
		Catalog:            catalogSvc,
		Flows:              flows,
		Switch:             sw,
		Ledger:             ledger,
		Transcripts:        transcripts,
		Backend:            backend,
		Profile:            profile,
		AnnounceCallback:   announce,
		AnnounceBotSession: announceBot,
		// A call the bot finishes alone writes the only ledger row it will
		// ever have, so the business data has to reach the bot too.
		CallData: callData,
		// ...and so does when the call began, which the bot session starts too
		// late to know.
		Timeline: timeline,
		// record_lead on a campaign call writes the attempt's lead.
		Leads: leads,
		// Unset is false and zero, which is today's behaviour; the pair is
		// the deployment's AICC_BOT_GREETING_MEDIA_WAIT.
		IsGreetingGated:   isGreetingGated,
		GreetingMediaWait: greetingMediaWait,
		// Dead air before NO_INPUT: the deployment's AICC_BOT_NO_INPUT_TIMEOUT.
		NoInput: noInput,
		// take_message writes a callback number the way this deployment
		// dials: AICC_DEFAULT_COUNTRY_CODE and AICC_OUTBOUND_DIAL_FORM.
		DefaultCountryCode: countryCode,
		DialForm:           dialForm,
	}
}

// announceBotSession tells the live event stream that a conversation has
// actually begun on a call.
//
// Call-scoped and addressed to no agent, which under a default-deny hub means
// supervisors and administrators — the whole audience there is, because a call
// the bot is answering has nobody else on it. By the time a person joins, this
// has already happened.
//
// It is the one thing the bot's half has to say that nothing else says. Its
// leg answering is PARTY_ESTABLISHED, and the model session is opened after
// that: it can fail, the caller is rescued to a queue, and from the stream
// alone that was indistinguishable from a bot that talked and handed over.
func announceBotSession(ctx context.Context, pub callbackPublisher) func(callID, flowID uuid.UUID) {
	return func(callID, flowID uuid.UUID) {
		pub.Publish(ctx, events.Event{
			Type:    events.TypeBotSessionStarted,
			CallID:  &callID,
			Payload: map[string]any{"flowId": flowID},
		}, events.Scope{})
	}
}

// settledCallback tells the live event stream that a callback's last attempt
// has an outcome — a call placed from the row has ended, and the row now says
// whether anybody answered.
func settledCallback(ctx context.Context, pub callbackPublisher) func(store.Callback) {
	return func(callback store.Callback) {
		pub.Publish(ctx, events.Event{
			Type:    events.TypeCallbackUpdated,
			CallID:  callback.CallID,
			Payload: map[string]any{"callback": callback},
		}, events.Scope{IsBroadcast: true})
	}
}

// callbackPublisher is the slice of the event hub the bot's announcement uses.
type callbackPublisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// announceCallback tells the live event stream about a callback the bot just
// created.
//
// A callback is one of the few things that is genuinely everybody's — it
// appears on every screen that can act on one — and under a default-deny hub
// that has to be said rather than left to an empty scope. It is a named
// function so the saying of it is something a test can observe; as a closure
// in run() it was neither reachable nor asserted.
func announceCallback(ctx context.Context, pub callbackPublisher) func(store.Callback) {
	return func(callback store.Callback) {
		pub.Publish(ctx, events.Event{
			Type:    events.TypeCallbackCreated,
			CallID:  callback.CallID,
			Payload: map[string]any{"callback": callback},
		}, events.Scope{IsBroadcast: true})
	}
}

// announceCampaign tells the live event stream how a campaign stands.
//
// Supervision only: the zero Scope reaches the subjects that see every call
// (calls:read:all) and nobody else, so an agent's stream never carries it. It
// is called by campaign.Reporter, which publishes only when something moved
// and at most once a second per campaign.
func announceCampaign(ctx context.Context, pub callbackPublisher) func(campaign.Snapshot) {
	return func(s campaign.Snapshot) {
		pub.Publish(ctx, events.Event{
			Type:    events.TypeCampaignUpdated,
			Payload: campaignUpdatedPayload(s),
		}, events.Scope{})
	}
}

// campaignUpdatedPayload is SseCampaignUpdatedPayload.
func campaignUpdatedPayload(s campaign.Snapshot) map[string]any {
	p := map[string]any{
		"campaignId": s.CampaignID,
		"status":     s.Status,
		"contacts":   s.Contacts,
	}
	if s.PauseReason != "" {
		p["pauseReason"] = s.PauseReason
	}
	if s.Activity != "" {
		p["activity"] = s.Activity
		p["inFlight"] = s.InFlight
	}
	if s.NextDialAt != nil {
		p["nextDialAt"] = s.NextDialAt
	}
	return p
}

// wrapUpDrafter builds the AI draft of after-call work, or returns nil.
//
// Nil is the whole of "off": the text model is not configured, its key is
// missing, the feature is switched off, or live transcription is disabled (the
// agent's part of a call is only transcribed by the live tap, so there would
// be nothing to read). The caller then attaches nothing and after-call work is
// exactly what it was. A configured model with a missing key is logged, because
// a deployment that meant to turn the feature on should not find out from the
// absence of drafts.
func wrapUpDrafter(cfg config.Config, st wrapupdraft.Store, pub wrapupdraft.Publisher,
	getenv func(string) string, log *slog.Logger) agents.WrapUpDrafter {
	if !cfg.IsWrapUpDraftEnabled || cfg.TextLLMEndpoint == "" || cfg.TextLLMModel == "" {
		return nil
	}
	if !cfg.IsTranscriptionEnabled {
		log.Info("wrap-up drafts are off: live transcription is disabled")
		return nil
	}
	key := getenv("TEXT_LLM_API_KEY")
	if key == "" {
		log.Error("wrap-up drafts are off: TEXT_LLM_API_KEY is not set",
			"endpoint", cfg.TextLLMEndpoint)
		return nil
	}
	llm, err := textllm.New(textllm.Config{
		Endpoint: cfg.TextLLMEndpoint, Model: cfg.TextLLMModel, APIKey: key,
	}, nil)
	if err != nil {
		log.Error("wrap-up drafts are off", "error", err)
		return nil
	}
	log.Info("wrap-up drafts are on", "model", cfg.TextLLMModel)
	return wrapupdraft.New(st, llm, pub, wrapupdraft.Config{
		Timeout:     cfg.TextLLMTimeout,
		BudgetChars: cfg.TextLLMContextChars,
		Settle:      wrapupdraft.DefaultSettle,
	}, log)
}

// removedSettings are AICC_* variables an earlier release read and this one
// does not, with what replaced them. A deployment that still sets one starts
// and is told, once.
var removedSettings = map[string]string{
	"AICC_BOT_ADVERTISE_IP": "the bot answers with the address the switch sent the INVITE to, and binds its media there",
}

// warnRemovedSettings logs one WARN per removed setting the environment still
// carries. Call it after config.Load, so a value set only in .env is seen.
func warnRemovedSettings(lookup func(string) (string, bool)) {
	for _, name := range slices.Sorted(maps.Keys(removedSettings)) {
		if value, ok := lookup(name); ok && value != "" {
			slog.Warn("setting is no longer read and is ignored", "setting", name,
				"value", value, "now", removedSettings[name])
		}
	}
}
