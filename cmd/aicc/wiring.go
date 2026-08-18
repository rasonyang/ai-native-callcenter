// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
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
}

type presenceWiring interface {
	AttachStaffing(agents.Staffing)
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
	Transcripts   transcriptRetirer
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

	// The switch forgets its agents when it restarts, and we are the source of
	// truth, so every reconnect rebuilds its view.
	c.Link.OnConnect(c.onSwitchConnected)

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
