// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/aicall"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
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
	AttachQueues(telephony.QueueCatalog)
}

type presenceWiring interface {
	AttachStaffing(agents.Staffing)
	AttachWrapUps(agents.WrapUpLedger)
	SyncSwitch(ctx context.Context)
	ObserveDevice(ctx context.Context, extensionNumber string, isRegistered, isInService bool)
}

// staffingWiring is the catalog in its two roles: the thing agent presence
// reconciles staffing through, and the thing a reconnect rebuilds tiers with.
type staffingWiring interface {
	agents.Staffing
	SyncTiers(ctx context.Context)
}

type connectHook interface {
	OnConnect(fn func(context.Context))
}

// composition is every connection between the parts, in one place that can be
// driven without a database, a switch or a listening socket.
//
// It holds no construction: run() builds the parts, this joins them. Taps is
// nil when transcription is disabled, which is the one connection that is
// conditional.
type composition struct {
	Registry      *telephony.Registry
	Coordinator   switchWiring
	Agents        presenceWiring
	Catalog       staffingWiring
	Link          connectHook
	CDR           *telephony.CDRAssembler
	Queues        telephony.QueueCatalog
	WrapUps       agents.WrapUpLedger
	Transcripts   transcriptRetirer
	Audiences     telephony.Audiences
	Taps          telephony.Tapper
	Registrations func() ([]telephony.Registration, error)
	Log           *slog.Logger
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

	// The switch forgets its agents when it restarts, and we are the source of
	// truth, so every reconnect rebuilds its view.
	c.Link.OnConnect(c.onSwitchConnected)

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

	if c.Taps != nil {
		c.Coordinator.AttachTaps(c.Taps)
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
func (c composition) onSwitchConnected(ctx context.Context) {
	c.Agents.SyncSwitch(ctx)
	c.Catalog.SyncTiers(ctx)

	regs, err := c.Registrations()
	if err != nil {
		c.Log.WarnContext(ctx, "could not read registrations", "error", err)
		return
	}
	for _, reg := range regs {
		c.Agents.ObserveDevice(ctx, reg.Extension, true, reg.IsReachable)
	}
	c.Log.InfoContext(ctx, "registrations reconciled", "endpoints", len(regs))
}

// transcriptRetirer is the part of the transcript registry this file needs.
type transcriptRetirer interface {
	Close(callID uuid.UUID)
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
	ledger *store.LedgerStore,
	recordings httpapi.RecordingStreamer,
	auditor httpapi.Auditor,
	outboundSvc httpapi.OutboundService,
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
		Ledger:      ledger,
		Recordings:  recordings,
		Auditor:     auditor,
		Outbound:    outboundSvc,
		SPA:         spa,
	}
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
func botUAS(cfg config.Config) voice.Config {
	uas := voice.DefaultConfig()
	uas.SIPHost = cfg.BotSIPHost
	uas.SIPPort = cfg.BotSIPPort
	uas.AdvertiseIP = cfg.BotAdvertiseIP
	uas.RTPPortRange = [2]int{cfg.BotRTPPortLow, cfg.BotRTPPortHigh}
	uas.MaxCalls = cfg.BotMaxCalls
	return uas
}

// botConfig assembles what the AI voice leg is given.
//
// Sessions and Logger are deliberately not parameters: the orchestrator fills
// both with its own defaults, and passing them from here would mean this
// process could disagree with every test that builds one.
func botConfig(
	uas voice.Config,
	catalogSvc aicall.Catalog,
	flows aicall.FlowSource,
	sw aicall.Switch,
	ledger aicall.Ledger,
	transcripts *transcript.Registry,
	backendBase string,
	profile provider.Profile,
	announce func(store.Callback),
) aicall.OrchestratorConfig {
	return aicall.OrchestratorConfig{
		UAS:              uas,
		Catalog:          catalogSvc,
		Flows:            flows,
		Switch:           sw,
		Ledger:           ledger,
		Transcripts:      transcripts,
		BackendBase:      backendBase,
		Profile:          profile,
		AnnounceCallback: announce,
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
