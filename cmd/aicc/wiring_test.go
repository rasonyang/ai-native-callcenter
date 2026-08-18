// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type retirer struct{ closed []uuid.UUID }

func (r *retirer) Close(id uuid.UUID) { r.closed = append(r.closed, id) }

// The composition has two ways to be wrong and both are silent.
//
// Drop the retirement and every call leaves a goroutine and its mailbox behind
// for the life of the process — invisible until a long shift runs out of
// memory. Drop the predecessor and the CDR assembler stops being called, which
// costs a call record to free a goroutine: the worse trade of the two.
func TestRetiringATranscriptDoesNotDisplaceWhatAlreadyListens(t *testing.T) {
	var priorSaw []uuid.UUID
	prior := func(s telephony.Snapshot) { priorSaw = append(priorSaw, s.CallID) }
	transcripts := &retirer{}

	callID := uuid.New()
	retireTranscriptWithCall(prior, transcripts)(telephony.Snapshot{CallID: callID})

	if len(priorSaw) != 1 || priorSaw[0] != callID {
		t.Errorf("the existing hook saw %v, want the finished call — a CDR is not "+
			"something to lose in order to free a goroutine", priorSaw)
	}
	if len(transcripts.closed) != 1 || transcripts.closed[0] != callID {
		t.Errorf("retired %v, want the finished call", transcripts.closed)
	}
}

// Nothing has to be listening first.
func TestRetiringATranscriptWorksWithNoPredecessor(t *testing.T) {
	transcripts := &retirer{}
	callID := uuid.New()

	retireTranscriptWithCall(nil, transcripts)(telephony.Snapshot{CallID: callID})

	if len(transcripts.closed) != 1 || transcripts.closed[0] != callID {
		t.Errorf("retired %v, want the finished call", transcripts.closed)
	}
}

type detacher struct{ calls []uuid.UUID }

func (d *detacher) DetachCall(id uuid.UUID) { d.calls = append(d.calls, id) }

// Same two silent failures as the transcript hook, on the other resource.
//
// Drop the detach and mod_audio_stream keeps pumping a finished call's audio
// at an ingest whose transcript actor has been closed, for the life of the
// process. Drop the predecessor and whatever was already listening on call
// retirement stops running.
func TestDetachingTapsDoesNotDisplaceWhatAlreadyListens(t *testing.T) {
	var priorSaw []uuid.UUID
	prior := func(id uuid.UUID) { priorSaw = append(priorSaw, id) }
	taps := &detacher{}

	callID := uuid.New()
	detachTapsWithCall(prior, taps)(callID)

	if len(priorSaw) != 1 || priorSaw[0] != callID {
		t.Errorf("the existing hook saw %v, want the retired call", priorSaw)
	}
	if len(taps.calls) != 1 || taps.calls[0] != callID {
		t.Errorf("detached %v, want the retired call", taps.calls)
	}
}

// Nothing has to be listening first — and nothing is, today.
func TestDetachingTapsWorksWithNoPredecessor(t *testing.T) {
	taps := &detacher{}
	callID := uuid.New()

	detachTapsWithCall(nil, taps)(callID)

	if len(taps.calls) != 1 || taps.calls[0] != callID {
		t.Errorf("detached %v, want the retired call", taps.calls)
	}
}
