// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Defaults for the backfill cursor, matching the contract.
const (
	transcriptDefaultLimit = 500
	transcriptMaxLimit     = 1000
)

// GetCallTranscript serves the snapshot half of the live transcript contract.
//
// The client subscribes to the event stream *first* and then calls this, which
// looks backwards but is the only order without a hole: subscribing first can
// duplicate lines, and duplicates are removable because seq is dense, while
// snapshotting first can lose the lines that arrive between the read and the
// subscribe, and loss is not removable.
func (s *Server) GetCallTranscript(w http.ResponseWriter, r *http.Request, callID uuid.UUID, params api.GetCallTranscriptParams) {
	if s.ledger == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such call", nil)
		return
	}
	if !s.mayReadTranscript(w, r, callID) {
		return
	}

	sinceSeq := 0
	if params.SinceSeq != nil && *params.SinceSeq > 0 {
		sinceSeq = int(*params.SinceSeq)
	}
	limit := transcriptDefaultLimit
	if params.Limit != nil && *params.Limit > 0 {
		limit = min(*params.Limit, transcriptMaxLimit)
	}

	lines, err := s.ledger.ListTranscriptSince(r.Context(), callID, sinceSeq, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the transcript", nil)
		return
	}

	// The cursor is the last seq actually returned, so a client that asks
	// again gets what came after it and nothing twice. An empty page leaves
	// the cursor where the caller had it.
	next := int64(sinceSeq)
	items := make([]api.TranscriptLine, 0, len(lines))
	for _, line := range lines {
		items = append(items, transcriptLineToAPI(line))
		next = int64(line.Seq)
	}

	isLive := s.isCallLive(callID)
	state := api.TranscriptionStateENDED
	if isLive {
		state = api.TranscriptionStateLIVE
	}
	writeJSON(w, http.StatusOK, api.CallTranscript{
		Items:        items,
		NextSinceSeq: next,
		IsLive:       isLive,
		State:        state,
	})
}

// mayReadTranscript answers who can see a conversation. A supervisor or an
// administrator can see any of them, as they can everywhere else; an agent can
// see the calls they are actually on, because a call id in the path is never
// authority on its own.
func (s *Server) mayReadTranscript(w http.ResponseWriter, r *http.Request, callID uuid.UUID) bool {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return false
	}
	if id.Role.AtLeast(auth.RoleSupervisor) {
		return true
	}
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return false
	}
	if s.calls == nil {
		writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
		return false
	}
	for _, snap := range s.calls.CallsForAgent(agentID) {
		if snap.CallID == callID {
			return true
		}
	}
	// A finished call is not an agent's to re-read: the transcript panel exists
	// for the call in front of them, and history is supervision.
	writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
	return false
}

// isCallLive reports whether more of this transcript is still coming, so a
// client knows to tail the stream rather than poll this endpoint.
func (s *Server) isCallLive(callID uuid.UUID) bool {
	if s.calls == nil {
		return false
	}
	for _, snap := range s.calls.AllCalls() {
		if snap.CallID == callID {
			return true
		}
	}
	return false
}

func transcriptLineToAPI(line store.TranscriptLine) api.TranscriptLine {
	out := api.TranscriptLine{
		Seq:        line.Seq,
		OccurredAt: line.OccurredAt,
		Speaker:    api.Speaker(line.Speaker),
		Kind:       api.TranscriptKind(line.Kind),
		Content:    line.Content,
		OffsetMs:   int64(line.OffsetMs),
		Source:     api.TranscriptSource(line.Source),
		PartyID:    line.PartyID,
		AgentID:    line.AgentID,
	}
	if out.Content == nil {
		out.Content = map[string]any{}
	}
	if line.Language != "" {
		out.Language = &line.Language
	}
	if line.Provider != "" {
		out.Provider = &line.Provider
	}
	if line.UtteranceID != "" {
		out.UtteranceID = &line.UtteranceID
	}
	return out
}
