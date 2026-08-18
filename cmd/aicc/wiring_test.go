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
