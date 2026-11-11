// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"time"
)

// maxTrackedItems bounds the per-call sets of utterance ids. A call has a few
// hundred utterances at most; the bound keeps a pathological one from growing
// without limit.
const maxTrackedItems = 256

// transcriptionLanguage is the call's language as the ISO-639-1 code the
// transcriber takes: "zh-CN" and "zh_CN" are "zh". Empty stays empty.
func transcriptionLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if i := strings.IndexAny(language, "-_"); i >= 0 {
		language = language[:i]
	}
	return language
}

// AnswerCallerTurn asks for the turn that answers the caller's utterance. It
// is the half of the contract a provider with Profile.ControlsInterruption
// leaves to this client: with create_response off, nothing answers the caller
// until this is called.
//
// The request is made only once the provider has committed the audio buffer of
// that utterance (input_audio_buffer.committed), because a response created
// before the item is in the conversation would answer what came before it.
// Server VAD commits at the end of speech, so the wait is normally nothing.
// When the commit has already been seen the turn is owed at once; otherwise it
// is owed when the commit arrives. Owing a turn is the mechanism a tool result
// or a cue uses (oweCallerTurn): one flag, so one response.create however much
// is owed, and a response that still holds the floor (the one the caller just
// cut) is waited for. An empty id means the next commit.
//
// It is a no-op on a profile that does not control interruption: that provider
// answers the caller itself.
func (r *Realtime) AnswerCallerTurn(itemID string) error {
	if !r.profile.ControlsInterruption {
		return nil
	}
	r.mu.Lock()
	_, isCommitted := r.committedItems[itemID]
	if itemID == "" || !isCommitted {
		if r.wantedItems == nil || len(r.wantedItems) >= maxTrackedItems {
			r.wantedItems = map[string]struct{}{}
		}
		r.wantedItems[itemID] = struct{}{}
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	r.oweCallerTurn("answering the caller's turn: the model no longer answers it by itself")
	return nil
}

// DeclineCallerTurn takes an utterance the model is not to read out of the
// conversation, and cancels a request to answer it that was waiting for the
// commit. The delete is acknowledged with conversation.item.deleted; a
// provider that refuses it (the item is already gone) is tolerated like any
// delete of ours (isAnsweredDelete).
func (r *Realtime) DeclineCallerTurn(itemID string) error {
	r.mu.Lock()
	delete(r.wantedItems, itemID)
	r.mu.Unlock()
	if itemID == "" {
		return nil
	}
	r.mu.Lock()
	r.deleteSentAt = time.Now()
	r.mu.Unlock()
	if err := r.sendEvent(map[string]any{
		"type":    "conversation.item.delete",
		"item_id": itemID,
	}); err != nil {
		return err
	}
	r.log.Info("declined the caller's acknowledgement: it is out of the conversation", "itemId", itemID)
	return nil
}

// onBufferCommitted notes that the provider committed an utterance's audio and
// owes the caller's turn if the call had asked for it already.
func (r *Realtime) onBufferCommitted(itemID string) {
	if !r.profile.ControlsInterruption {
		return
	}
	r.mu.Lock()
	if r.committedItems == nil || len(r.committedItems) >= maxTrackedItems {
		r.committedItems = map[string]struct{}{}
	}
	r.committedItems[itemID] = struct{}{}
	_, isWanted := r.wantedItems[itemID]
	_, isNextWanted := r.wantedItems[""]
	if isWanted {
		delete(r.wantedItems, itemID)
	} else if isNextWanted {
		delete(r.wantedItems, "")
	}
	r.mu.Unlock()
	if isWanted || isNextWanted {
		r.oweCallerTurn("the caller's turn is committed; answering it")
	}
}
