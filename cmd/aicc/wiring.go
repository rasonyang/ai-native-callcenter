// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

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
