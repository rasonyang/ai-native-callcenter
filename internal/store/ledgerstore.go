// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// LedgerStore persists what calls leave behind: the CDR, the transcript, the
// artifacts. It is written once per call at retirement and read by everything
// after the fact — the explorer, reports, the wallboard's history.
type LedgerStore struct{ q *queries.Queries }

// Ledger returns the call ledger.
func (s *Store) Ledger() *LedgerStore { return &LedgerStore{q: s.Queries} }

// CDR is one finished call. Field vocabulary follows the naming spec; the
// enum values are byte-identical to what the API serves.
type CDR struct {
	CallID     uuid.UUID `json:"callId"`
	StartedAt  time.Time `json:"startedAt"`
	AnsweredAt time.Time `json:"answeredAt,omitzero"` // zero when never answered
	EndedAt    time.Time `json:"endedAt"`

	CallType string `json:"callType"`
	Language string `json:"language,omitempty"`

	FromNumber string     `json:"fromNumber"`
	ToNumber   string     `json:"toNumber"`
	DID        string     `json:"did,omitempty"`
	FlowID     *uuid.UUID `json:"flowId,omitempty"`
	QueueID    *uuid.UUID `json:"queueId,omitempty"`

	AgentIDs       []uuid.UUID `json:"agentIds,omitempty"`
	PrimaryAgentID *uuid.UUID  `json:"primaryAgentId,omitempty"`

	RingSec      int `json:"ringSec"`
	BotSec       int `json:"botSec"`
	QueueWaitSec int `json:"queueWaitSec"`
	TalkSec      int `json:"talkSec"`
	// BillSec is what the carrier bills: the caller's own leg answering
	// through to the end of the call, whoever did or did not take it.
	BillSec  int `json:"billSec"`
	TotalSec int `json:"totalSec"`

	Status       string `json:"status"`
	HangupCause  string `json:"hangupCause,omitempty"`
	MissedReason string `json:"missedReason,omitempty"` // empty when the call was not missed
	Disposition  string `json:"disposition,omitempty"`
	IsContained  bool   `json:"isContained"`
	HasRecording bool   `json:"hasRecording"`

	UserData map[string]any `json:"userData,omitempty"`
	Tech     map[string]any `json:"tech,omitempty"`
	Legs     []Leg          `json:"legs"`

	// WrapUp is the after-call work filed against this call: the requesting
	// agent's own on an agent's listing, the primary agent's (else the latest)
	// on everyone else's. Nil when nobody filed one. It is attached on read,
	// not written with the row, because it can be filed before the row exists.
	WrapUp *WrapUp `json:"wrapUp,omitempty"`
}

// WrapUp is one agent's after-call work for one call.
//
// The platform opens it when the call ends and the agent confirms it, so a
// finished call always has one and IsConfirmed is what separates a record
// somebody looked at from one still standing on its defaults. The label is
// captured at filing time, so the record survives later edits to the
// vocabulary.
type WrapUp struct {
	CallID           uuid.UUID `json:"callId"`
	AgentID          uuid.UUID `json:"agentId"`
	DispositionCode  string    `json:"dispositionCode"`
	DispositionLabel string    `json:"dispositionLabel"`
	Note             string    `json:"note"`
	IsConfirmed      bool      `json:"isConfirmed"`
	CreatedAt        time.Time `json:"createdAt"`
}

// Leg is one hop of a call's journey, in order, for the detail view.
type Leg struct {
	Kind        string `json:"kind"` // TRUNK | DIALING | BOT | QUEUE | AGENT
	Label       string `json:"label"`
	DurationSec int    `json:"durationSec"`
	Note        string `json:"note,omitempty"`
}

// CDR statuses.
const (
	CDRStatusAnswered = "ANSWERED"
	CDRStatusNoAnswer = "NO_ANSWER"
	CDRStatusBusy     = "BUSY"
	CDRStatusFailed   = "FAILED"
)

// InsertCDR writes the ledger row, or replaces one that saw less of the call.
//
// Retirement runs from more than one place, so a second write for the same
// call has to be safe. It used to be safe by being a no-op — first writer
// wins — and that discarded the truth in the one case where the two writers
// disagreed: a bot leg killed by a restart wrote the call off as ended, and
// the four minutes the caller then spent with an agent had nowhere to go. The
// rule is now that a row ending later replaces one ending earlier, because a
// later ending means more of the call is known.
func (l *LedgerStore) InsertCDR(ctx context.Context, cdr CDR) error {
	userData, err := marshalOr(cdr.UserData, "{}")
	if err != nil {
		return fmt.Errorf("encode userData: %w", err)
	}
	tech, err := marshalOr(cdr.Tech, "{}")
	if err != nil {
		return fmt.Errorf("encode tech: %w", err)
	}
	legs, err := marshalOr(cdr.Legs, "[]")
	if err != nil {
		return fmt.Errorf("encode legs: %w", err)
	}

	var missed *string
	if cdr.MissedReason != "" {
		missed = &cdr.MissedReason
	}
	agentIDs := cdr.AgentIDs
	if agentIDs == nil {
		agentIDs = []uuid.UUID{}
	}

	return l.q.InsertCDR(ctx, queries.InsertCDRParams{
		CallID:         cdr.CallID,
		StartedAt:      stamp(cdr.StartedAt),
		AnsweredAt:     stamp(cdr.AnsweredAt),
		EndedAt:        stamp(cdr.EndedAt),
		CallType:       cdr.CallType,
		Language:       cdr.Language,
		FromNumber:     cdr.FromNumber,
		ToNumber:       cdr.ToNumber,
		Did:            cdr.DID,
		FlowID:         cdr.FlowID,
		QueueID:        cdr.QueueID,
		AgentIds:       agentIDs,
		PrimaryAgentID: cdr.PrimaryAgentID,
		RingSec:        int32(cdr.RingSec),
		BotSec:         int32(cdr.BotSec),
		QueueWaitSec:   int32(cdr.QueueWaitSec),
		TalkSec:        int32(cdr.TalkSec),
		BillSec:        int32(cdr.BillSec),
		TotalSec:       int32(cdr.TotalSec),
		Status:         cdr.Status,
		HangupCause:    cdr.HangupCause,
		MissedReason:   missed,
		Disposition:    cdr.Disposition,
		IsContained:    cdr.IsContained,
		HasRecording:   cdr.HasRecording,
		UserData:       userData,
		Tech:           tech,
		Legs:           legs,
	})
}

// CDRFilter narrows a ledger listing. Zero values mean "any".
type CDRFilter struct {
	From    time.Time
	To      time.Time
	QueueID *uuid.UUID
	// AgentID keeps the calls this agent was a party to. It also decides
	// whose wrap-up rides on each row: an agent's own listing shows what
	// they filed, everyone else's shows the primary agent's.
	AgentID *uuid.UUID
	Status  string
	DID     string
	// FromNumber matches as a substring, for finding a caller.
	FromNumber string
	Limit      int
	Offset     int
}

// ListCDRs pages through the ledger, newest first, with the total for paging.
func (l *LedgerStore) ListCDRs(ctx context.Context, filter CDRFilter) ([]CDR, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}

	rows, err := l.q.ListCDRs(ctx, queries.ListCDRsParams{
		FromAt:     stamp(filter.From),
		ToAt:       stamp(filter.To),
		QueueID:    filter.QueueID,
		AgentID:    filter.AgentID,
		Status:     filter.Status,
		Did:        filter.DID,
		FromNumber: filter.FromNumber,
		PageLimit:  int32(filter.Limit),
		PageOffset: int32(filter.Offset),
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := l.q.CountCDRs(ctx, queries.CountCDRsParams{
		FromAt:     stamp(filter.From),
		ToAt:       stamp(filter.To),
		QueueID:    filter.QueueID,
		AgentID:    filter.AgentID,
		Status:     filter.Status,
		Did:        filter.DID,
		FromNumber: filter.FromNumber,
	})
	if err != nil {
		return nil, 0, err
	}

	out := make([]CDR, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	if err := l.attachWrapUps(ctx, out, filter.AgentID); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// attachWrapUps puts the wrap-up filed against each call on its row: the
// named agent's own when one is asked for, otherwise the primary agent's,
// otherwise the most recent. One query for the page, matched in memory.
func (l *LedgerStore) attachWrapUps(ctx context.Context, cdrs []CDR, forAgent *uuid.UUID) error {
	if len(cdrs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(cdrs))
	for _, cdr := range cdrs {
		ids = append(ids, cdr.CallID)
	}
	rows, err := l.q.ListWrapUpsForCalls(ctx, ids)
	if err != nil {
		return err
	}
	byCall := make(map[uuid.UUID][]WrapUp, len(rows))
	for _, row := range rows {
		byCall[row.CallID] = append(byCall[row.CallID], wrapUpFromRow(row))
	}
	for i := range cdrs {
		cdrs[i].WrapUp = pickWrapUp(byCall[cdrs[i].CallID], forAgent, cdrs[i].PrimaryAgentID)
	}
	return nil
}

// pickWrapUp chooses which agent's filing represents a call. Candidates
// arrive newest first, so the fallback is the latest one.
func pickWrapUp(candidates []WrapUp, forAgent, primaryAgent *uuid.UUID) *WrapUp {
	if len(candidates) == 0 {
		return nil
	}
	if forAgent != nil {
		for i := range candidates {
			if candidates[i].AgentID == *forAgent {
				return &candidates[i]
			}
		}
		// An agent's own listing shows their own filing or none: somebody
		// else's disposition is not what they said about the call.
		return nil
	}
	if primaryAgent != nil {
		for i := range candidates {
			if candidates[i].AgentID == *primaryAgent {
				return &candidates[i]
			}
		}
	}
	return &candidates[0]
}

// HasCDR reports whether a call already finished into the ledger; outbound
// idempotency reads it before ever redialing.
func (l *LedgerStore) HasCDR(ctx context.Context, callID uuid.UUID) (bool, error) {
	_, err := l.q.GetCDR(ctx, callID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetCDR reads one call, with the primary agent's wrap-up on it.
func (l *LedgerStore) GetCDR(ctx context.Context, callID uuid.UUID) (CDR, error) {
	row, err := l.q.GetCDR(ctx, callID)
	if err != nil {
		return CDR{}, err
	}
	out := []CDR{fromRow(row)}
	if err := l.attachWrapUps(ctx, out, nil); err != nil {
		return CDR{}, err
	}
	return out[0], nil
}

func fromRow(row queries.Cdr) CDR {
	cdr := CDR{
		CallID:         row.CallID,
		StartedAt:      row.StartedAt.Time,
		EndedAt:        row.EndedAt.Time,
		CallType:       row.CallType,
		Language:       row.Language,
		FromNumber:     row.FromNumber,
		ToNumber:       row.ToNumber,
		DID:            row.Did,
		FlowID:         row.FlowID,
		QueueID:        row.QueueID,
		AgentIDs:       row.AgentIds,
		PrimaryAgentID: row.PrimaryAgentID,
		RingSec:        int(row.RingSec),
		BotSec:         int(row.BotSec),
		QueueWaitSec:   int(row.QueueWaitSec),
		TalkSec:        int(row.TalkSec),
		BillSec:        int(row.BillSec),
		TotalSec:       int(row.TotalSec),
		Status:         row.Status,
		HangupCause:    row.HangupCause,
		Disposition:    row.Disposition,
		IsContained:    row.IsContained,
		HasRecording:   row.HasRecording,
	}
	if row.AnsweredAt.Valid {
		cdr.AnsweredAt = row.AnsweredAt.Time
	}
	if row.MissedReason != nil {
		cdr.MissedReason = *row.MissedReason
	}
	_ = json.Unmarshal(row.UserData, &cdr.UserData)
	_ = json.Unmarshal(row.Tech, &cdr.Tech)
	_ = json.Unmarshal(row.Legs, &cdr.Legs)
	return cdr
}

//
// Transcripts.
//

// Speakers, kinds and sources, byte-identical across the stack.
const (
	SpeakerCustomer   = "CUSTOMER"
	SpeakerBot        = "BOT"
	SpeakerHumanAgent = "HUMAN_AGENT"

	TranscriptKindText       = "TEXT"
	TranscriptKindToolCall   = "TOOL_CALL"
	TranscriptKindToolResult = "TOOL_RESULT"

	// TranscriptSourceModel is the conversational engine's own transcript of
	// the AI leg; TranscriptSourceASR is a separate recognition of streamed
	// audio. The hybrid of the two is only auditable if the line says which.
	TranscriptSourceModel = "MODEL"
	TranscriptSourceASR   = "ASR"
)

// TranscriptLine is one thing said or done on a call, in either phase.
type TranscriptLine struct {
	Seq         int            `json:"seq"`
	OccurredAt  time.Time      `json:"occurredAt"`
	Speaker     string         `json:"speaker"`
	Kind        string         `json:"kind"`
	Content     map[string]any `json:"content"`
	PartyID     *uuid.UUID     `json:"partyId,omitempty"`
	AgentID     *uuid.UUID     `json:"agentId,omitempty"`
	OffsetMs    int            `json:"offsetMs"`
	Language    string         `json:"language,omitempty"`
	Source      string         `json:"source"`
	Provider    string         `json:"provider,omitempty"`
	UtteranceID string         `json:"utteranceId,omitempty"`
}

func (l *TranscriptLine) params(callID uuid.UUID) (queries.InsertTranscriptLineParams, error) {
	content, err := marshalOr(l.Content, "{}")
	if err != nil {
		return queries.InsertTranscriptLineParams{}, fmt.Errorf("encode transcript %d: %w", l.Seq, err)
	}
	source := l.Source
	if source == "" {
		source = TranscriptSourceModel
	}
	return queries.InsertTranscriptLineParams{
		CallID:      callID,
		Seq:         int32(l.Seq),
		OccurredAt:  stamp(l.OccurredAt),
		Speaker:     l.Speaker,
		Kind:        l.Kind,
		Content:     content,
		PartyID:     l.PartyID,
		AgentID:     l.AgentID,
		OffsetMs:    int32(l.OffsetMs),
		Language:    l.Language,
		Source:      source,
		Provider:    l.Provider,
		UtteranceID: l.UtteranceID,
	}, nil
}

// InsertTranscriptLine writes one line as it is spoken. A redelivered final is
// dropped by the idempotency index rather than duplicated.
func (l *LedgerStore) InsertTranscriptLine(ctx context.Context, callID uuid.UUID, line TranscriptLine) error {
	arg, err := line.params(callID)
	if err != nil {
		return err
	}
	return l.q.InsertTranscriptLine(ctx, arg)
}

// ListTranscript reads a call's transcript in order.
func (l *LedgerStore) ListTranscript(ctx context.Context, callID uuid.UUID) ([]TranscriptLine, error) {
	rows, err := l.q.ListTranscripts(ctx, callID)
	if err != nil {
		return nil, err
	}
	return transcriptLines(rows), nil
}

// ListTranscriptSince reads the lines after a cursor, for the backfill that
// closes the gap between an agent's snapshot and their live tail.
func (l *LedgerStore) ListTranscriptSince(ctx context.Context, callID uuid.UUID, sinceSeq, limit int) ([]TranscriptLine, error) {
	rows, err := l.q.ListTranscriptSince(ctx, queries.ListTranscriptSinceParams{
		CallID: callID,
		Seq:    int32(sinceSeq),
		Limit:  int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return transcriptLines(rows), nil
}

func transcriptLines(rows []queries.Transcript) []TranscriptLine {
	out := make([]TranscriptLine, 0, len(rows))
	for _, row := range rows {
		line := TranscriptLine{
			Seq:         int(row.Seq),
			OccurredAt:  row.OccurredAt.Time,
			Speaker:     row.Speaker,
			Kind:        row.Kind,
			PartyID:     row.PartyID,
			AgentID:     row.AgentID,
			OffsetMs:    int(row.OffsetMs),
			Language:    row.Language,
			Source:      row.Source,
			Provider:    row.Provider,
			UtteranceID: row.UtteranceID,
		}
		_ = json.Unmarshal(row.Content, &line.Content)
		out = append(out, line)
	}
	return out
}

//
// After-call work: the vocabulary, and what agents file against calls.
//

// Disposition is one word an agent can file a call under.
type Disposition struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// ListDispositions reads the enabled vocabulary in the order agents see it.
// One flat list: with a handful of words there is nothing to group, and a
// grouping is one more thing to navigate before reaching the word they want.
func (l *LedgerStore) ListDispositions(ctx context.Context) ([]Disposition, error) {
	rows, err := l.q.ListEnabledDispositions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Disposition, 0, len(rows))
	for _, d := range rows {
		out = append(out, Disposition{Code: d.Code, Label: d.Label})
	}
	return out, nil
}

// ErrUnknownDisposition reports a code the vocabulary does not have, or has
// disabled: an agent cannot file under a word that is not on the list.
var ErrUnknownDisposition = errors.New("unknown disposition")

// DefaultDispositionCode is what the platform files on an agent's behalf when
// after-call work begins. It is the ordinary outcome, so confirming it is one
// press for the ordinary call; anything else the agent has to say, they say by
// changing it.
const DefaultDispositionCode = "RESOLVED"

// OpenWrapUp starts the record for a call whose after-call work has just
// begun: the default disposition, no note, unconfirmed.
//
// This is what makes "a finished call always has a wrap-up" true. It is not an
// overwrite — a record that already exists may carry what the agent typed, and
// a second opening must not take that from them.
func (l *LedgerStore) OpenWrapUp(ctx context.Context, callID, agentID uuid.UUID) error {
	code, label, err := l.defaultDisposition(ctx)
	if err != nil {
		return err
	}
	return l.q.OpenWrapUp(ctx, queries.OpenWrapUpParams{
		CallID: callID, AgentID: agentID,
		DispositionCode: code, DispositionLabel: label,
	})
}

// GetWrapUp reads one agent's record for one call.
func (l *LedgerStore) GetWrapUp(ctx context.Context, callID, agentID uuid.UUID) (WrapUp, error) {
	row, err := l.q.GetWrapUp(ctx, queries.GetWrapUpParams{CallID: callID, AgentID: agentID})
	if err != nil {
		return WrapUp{}, err
	}
	return wrapUpFromRow(row), nil
}

// ConfirmWrapUp records the agent's confirmation, changing only what they sent.
//
// A nil field means "leave it": the record already carries a disposition, and
// pressing Done without touching anything is an agent saying the defaults are
// right. The record is created here too, for the case where after-call work
// never opened one — a restart between the call ending and the agent
// answering — so a confirmation is never refused for want of a row.
func (l *LedgerStore) ConfirmWrapUp(ctx context.Context, callID, agentID uuid.UUID,
	dispositionCode, note *string) (WrapUp, error) {
	code, label, err := l.defaultDisposition(ctx)
	if err != nil {
		return WrapUp{}, err
	}
	noteValue := ""

	current, err := l.GetWrapUp(ctx, callID, agentID)
	switch {
	case err == nil:
		code, label, noteValue = current.DispositionCode, current.DispositionLabel, current.Note
	case !errors.Is(err, pgx.ErrNoRows):
		return WrapUp{}, err
	}

	if dispositionCode != nil && *dispositionCode != "" {
		d, err := l.q.GetDisposition(ctx, *dispositionCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return WrapUp{}, fmt.Errorf("%w: %q", ErrUnknownDisposition, *dispositionCode)
		}
		if err != nil {
			return WrapUp{}, err
		}
		code, label = d.Code, d.Label
	}
	if note != nil {
		noteValue = *note
	}

	row, err := l.q.UpsertWrapUp(ctx, queries.UpsertWrapUpParams{
		CallID: callID, AgentID: agentID, Note: noteValue,
		DispositionCode: code, DispositionLabel: label, IsConfirmed: true,
	})
	if err != nil {
		return WrapUp{}, err
	}
	return wrapUpFromRow(row), nil
}

// defaultDisposition is what a record is opened with: the configured default
// where the installation still has it, otherwise the first word on its own
// list. A vocabulary somebody emptied leaves the code blank rather than
// blocking the call from being recorded at all.
func (l *LedgerStore) defaultDisposition(ctx context.Context) (code, label string, err error) {
	d, err := l.q.GetDisposition(ctx, DefaultDispositionCode)
	if err == nil {
		return d.Code, d.Label, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	items, err := l.q.ListEnabledDispositions(ctx)
	if err != nil {
		return "", "", err
	}
	if len(items) == 0 {
		return "", "", nil
	}
	return items[0].Code, items[0].Label, nil
}

func wrapUpFromRow(row queries.WrapUp) WrapUp {
	return WrapUp{
		CallID: row.CallID, AgentID: row.AgentID,
		DispositionCode: row.DispositionCode, DispositionLabel: row.DispositionLabel,
		Note: row.Note, IsConfirmed: row.IsConfirmed, CreatedAt: row.CreatedAt.Time,
	}
}

//
// Callbacks.
//

// Callback is a promise to ring someone back.
type Callback struct {
	ID          uuid.UUID  `json:"id"`
	CallID      *uuid.UUID `json:"callId,omitempty"`
	QueueID     *uuid.UUID `json:"queueId,omitempty"`
	PhoneNumber string     `json:"phoneNumber"`
	Message     string     `json:"message"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	HandledBy   *uuid.UUID `json:"handledBy,omitempty"`
	HandledAt   *time.Time `json:"handledAt,omitempty"`
}

// Callback statuses.
const (
	CallbackStatusOpen      = "OPEN"
	CallbackStatusDone      = "DONE"
	CallbackStatusDismissed = "DISMISSED"
)

// InsertCallback records a promise to call back.
func (l *LedgerStore) InsertCallback(ctx context.Context, callID, queueID *uuid.UUID, phoneNumber, message string) (Callback, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Callback{}, err
	}
	row, err := l.q.InsertCallback(ctx, queries.InsertCallbackParams{
		ID: id, CallID: callID, QueueID: queueID,
		PhoneNumber: phoneNumber, Message: message,
	})
	if err != nil {
		return Callback{}, err
	}
	return callbackFromRow(row), nil
}

// ListCallbacks pages the work queue; an empty status lists everything.
func (l *LedgerStore) ListCallbacks(ctx context.Context, status string, limit, offset int) ([]Callback, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := l.q.ListCallbacks(ctx, queries.ListCallbacksParams{
		Status: status, PageLimit: int32(limit), PageOffset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Callback, 0, len(rows))
	for _, row := range rows {
		out = append(out, callbackFromRow(row))
	}
	return out, nil
}

// ClaimCallback marks an OPEN callback as being worked by one agent. A second
// claim loses: the row is only updated while still OPEN.
func (l *LedgerStore) ClaimCallback(ctx context.Context, id, userID uuid.UUID) (Callback, error) {
	row, err := l.q.ClaimCallback(ctx, queries.ClaimCallbackParams{ID: id, HandledBy: &userID})
	if err != nil {
		return Callback{}, err
	}
	return callbackFromRow(row), nil
}

// HandleCallback closes a callback as done or dismissed.
func (l *LedgerStore) HandleCallback(ctx context.Context, id uuid.UUID, status string, handledBy uuid.UUID) (Callback, error) {
	row, err := l.q.HandleCallback(ctx, queries.HandleCallbackParams{
		ID: id, Status: status, HandledBy: &handledBy,
	})
	if err != nil {
		return Callback{}, err
	}
	return callbackFromRow(row), nil
}

func callbackFromRow(row queries.Callback) Callback {
	cb := Callback{
		ID: row.ID, CallID: row.CallID, QueueID: row.QueueID,
		PhoneNumber: row.PhoneNumber, Message: row.Message,
		Status: row.Status, CreatedAt: row.CreatedAt.Time,
		HandledBy: row.HandledBy,
	}
	if row.HandledAt.Valid {
		at := row.HandledAt.Time
		cb.HandledAt = &at
	}
	return cb
}

//
// Recordings, queue events, audit.
//

// Recording backends.
const (
	RecordingBackendFS = "FS"
	RecordingBackendS3 = "S3"
)

// Recording is one stored audio artifact.
type Recording struct {
	ID          uuid.UUID `json:"id"`
	CallID      uuid.UUID `json:"callId"`
	Backend     string    `json:"backend"`
	Bucket      string    `json:"bucket,omitempty"`
	ObjectKey   string    `json:"objectKey"`
	SizeBytes   int64     `json:"sizeBytes"`
	DurationSec int       `json:"durationSec"`
	Format      string    `json:"format"`
	CreatedAt   time.Time `json:"createdAt"`
}

// InsertRecording records where a call's audio landed.
func (l *LedgerStore) InsertRecording(ctx context.Context, r Recording) (Recording, error) {
	if r.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return Recording{}, err
		}
		r.ID = id
	}
	if r.Format == "" {
		r.Format = "WAV"
	}
	row, err := l.q.InsertRecording(ctx, queries.InsertRecordingParams{
		ID: r.ID, CallID: r.CallID, Backend: r.Backend, Bucket: r.Bucket,
		ObjectKey: r.ObjectKey, SizeBytes: r.SizeBytes,
		DurationSec: int32(r.DurationSec), Format: r.Format,
	})
	if err != nil {
		return Recording{}, err
	}
	return recordingFromRow(row), nil
}

// GetRecording reads one artifact that still exists.
func (l *LedgerStore) GetRecording(ctx context.Context, id uuid.UUID) (Recording, error) {
	row, err := l.q.GetRecording(ctx, id)
	if err != nil {
		return Recording{}, err
	}
	return recordingFromRow(row), nil
}

// RecordingsByCall lists a call's artifacts.
func (l *LedgerStore) RecordingsByCall(ctx context.Context, callID uuid.UUID) ([]Recording, error) {
	rows, err := l.q.ListRecordingsByCall(ctx, callID)
	if err != nil {
		return nil, err
	}
	out := make([]Recording, 0, len(rows))
	for _, row := range rows {
		out = append(out, recordingFromRow(row))
	}
	return out, nil
}

func recordingFromRow(row queries.Recording) Recording {
	return Recording{
		ID: row.ID, CallID: row.CallID, Backend: row.Backend, Bucket: row.Bucket,
		ObjectKey: row.ObjectKey, SizeBytes: row.SizeBytes,
		DurationSec: int(row.DurationSec), Format: row.Format,
		CreatedAt: row.CreatedAt.Time,
	}
}

// QualityReview is one reviewer's scoring of one recording.
type QualityReview struct {
	ID          uuid.UUID      `json:"id"`
	RecordingID uuid.UUID      `json:"recordingId"`
	CallID      uuid.UUID      `json:"callId"`
	ReviewerID  uuid.UUID      `json:"reviewerId"`
	Scores      map[string]int `json:"scores"`
	TotalScore  int            `json:"totalScore"`
	Notes       string         `json:"notes"`
	CreatedAt   time.Time      `json:"createdAt"`
}

// InsertQualityReview records a scoring.
func (l *LedgerStore) InsertQualityReview(ctx context.Context, review QualityReview) (QualityReview, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return QualityReview{}, err
	}
	scores, err := marshalOr(review.Scores, "{}")
	if err != nil {
		return QualityReview{}, err
	}
	row, err := l.q.InsertQualityReview(ctx, queries.InsertQualityReviewParams{
		ID: id, RecordingID: review.RecordingID, CallID: review.CallID,
		ReviewerID: review.ReviewerID, Scores: scores,
		TotalScore: int16(review.TotalScore), Notes: review.Notes,
	})
	if err != nil {
		return QualityReview{}, err
	}
	return reviewFromRow(row), nil
}

// ReviewsByCall lists a call's reviews, newest first.
func (l *LedgerStore) ReviewsByCall(ctx context.Context, callID uuid.UUID) ([]QualityReview, error) {
	rows, err := l.q.ListQualityReviewsByCall(ctx, callID)
	if err != nil {
		return nil, err
	}
	out := make([]QualityReview, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewFromRow(row))
	}
	return out, nil
}

func reviewFromRow(row queries.QualityReview) QualityReview {
	review := QualityReview{
		ID: row.ID, RecordingID: row.RecordingID, CallID: row.CallID,
		ReviewerID: row.ReviewerID, TotalScore: int(row.TotalScore),
		Notes: row.Notes, CreatedAt: row.CreatedAt.Time,
	}
	_ = json.Unmarshal(row.Scores, &review.Scores)
	return review
}

// MarkRecorded flips the ledger row's recording flag once audio is stored.
func (l *LedgerStore) MarkRecorded(ctx context.Context, callID uuid.UUID) error {
	return l.q.UpdateCDRHasRecording(ctx, callID)
}

// Queue event names.
const (
	QueueEventJoined    = "JOINED"
	QueueEventLeft      = "LEFT"
	QueueEventOffered   = "OFFERED"
	QueueEventBridged   = "BRIDGED"
	QueueEventAbandoned = "ABANDONED"
)

// InsertQueueEvent records one member movement.
func (l *LedgerStore) InsertQueueEvent(ctx context.Context, occurredAt time.Time,
	callID *uuid.UUID, queueID uuid.UUID, event string, agentID *uuid.UUID, waitMs int) error {
	return l.q.InsertQueueEvent(ctx, queries.InsertQueueEventParams{
		OccurredAt: stamp(occurredAt), CallID: callID, QueueID: queueID,
		Event: event, AgentID: agentID, WaitMs: int32(waitMs),
	})
}

// Audit records an administrative action against who did it.
func (l *LedgerStore) Audit(ctx context.Context, actorID *uuid.UUID,
	action, targetKind, targetID string, detail map[string]any, ip string) error {
	encoded, err := marshalOr(detail, "{}")
	if err != nil {
		return err
	}
	var addr *netip.Addr
	if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil {
		addr = &parsed
	}
	return l.q.InsertAuditLog(ctx, queries.InsertAuditLogParams{
		ActorID: actorID, Action: action,
		TargetKind: targetKind, TargetID: targetID,
		Detail: encoded, IP: addr,
	})
}

//
// Reports.
//

// Overview is the headline numbers for a period.
type Overview struct {
	TotalCalls        int     `json:"totalCalls"`
	AnsweredCalls     int     `json:"answeredCalls"`
	AbandonedCalls    int     `json:"abandonedCalls"`
	ContainedCalls    int     `json:"containedCalls"`
	QueueCalls        int     `json:"queueCalls"`
	AnsweredWithinSLA int     `json:"answeredWithinSla"`
	AvgWaitSec        float64 `json:"avgWaitSec"`
	AvgTalkSec        float64 `json:"avgTalkSec"`
	AvgBotSec         float64 `json:"avgBotSec"`
}

// QueueReport is one queue's period numbers.
type QueueReport struct {
	QueueID           *uuid.UUID `json:"queueId"`
	TotalCalls        int        `json:"totalCalls"`
	AnsweredCalls     int        `json:"answeredCalls"`
	AbandonedCalls    int        `json:"abandonedCalls"`
	AnsweredWithinSLA int        `json:"answeredWithinSla"`
	AvgWaitSec        float64    `json:"avgWaitSec"`
	MaxWaitSec        int        `json:"maxWaitSec"`
	AvgTalkSec        float64    `json:"avgTalkSec"`
}

// DailyReport is one day's counts, for charts.
type DailyReport struct {
	Day            time.Time `json:"day"`
	TotalCalls     int       `json:"totalCalls"`
	AnsweredCalls  int       `json:"answeredCalls"`
	ContainedCalls int       `json:"containedCalls"`
	AbandonedCalls int       `json:"abandonedCalls"`
}

// ReportOverview aggregates the period's headline numbers.
func (l *LedgerStore) ReportOverview(ctx context.Context, from, to time.Time, queueID *uuid.UUID) (Overview, error) {
	row, err := l.q.ReportOverview(ctx, queries.ReportOverviewParams{
		StartedAt: stamp(from), StartedAt_2: stamp(to), QueueID: queueID,
	})
	if err != nil {
		return Overview{}, err
	}
	return Overview{
		TotalCalls:        int(row.TotalCalls),
		AnsweredCalls:     int(row.AnsweredCalls),
		AbandonedCalls:    int(row.AbandonedCalls),
		ContainedCalls:    int(row.ContainedCalls),
		QueueCalls:        int(row.QueueCalls),
		AnsweredWithinSLA: int(row.AnsweredWithinSla),
		AvgWaitSec:        row.AvgWaitSec,
		AvgTalkSec:        row.AvgTalkSec,
		AvgBotSec:         row.AvgBotSec,
	}, nil
}

// ReportByQueue aggregates per queue.
func (l *LedgerStore) ReportByQueue(ctx context.Context, from, to time.Time) ([]QueueReport, error) {
	rows, err := l.q.ReportByQueue(ctx, queries.ReportByQueueParams{
		StartedAt: stamp(from), StartedAt_2: stamp(to),
	})
	if err != nil {
		return nil, err
	}
	out := make([]QueueReport, 0, len(rows))
	for _, row := range rows {
		out = append(out, QueueReport{
			QueueID:           row.QueueID,
			TotalCalls:        int(row.TotalCalls),
			AnsweredCalls:     int(row.AnsweredCalls),
			AbandonedCalls:    int(row.AbandonedCalls),
			AnsweredWithinSLA: int(row.AnsweredWithinSla),
			AvgWaitSec:        row.AvgWaitSec,
			MaxWaitSec:        int(row.MaxWaitSec),
			AvgTalkSec:        row.AvgTalkSec,
		})
	}
	return out, nil
}

// AgentDay is one agent's own day: what they handled, and what the time went
// on. The totals ride along with the averages so every number on the screen
// can be checked against them rather than believed.
type AgentDay struct {
	CallsHandled int `json:"callsHandled"`
	TalkSec      int `json:"talkSec"`
	WrapUpSec    int `json:"wrapUpSec"`
	SignedInSec  int `json:"signedInSec"`
	AvgHandleSec int `json:"avgHandleSec"`
	AvgWrapUpSec int `json:"avgWrapUpSec"`
	OccupancyPct int `json:"occupancyPct"`

	// The after-call records opened for this agent, and how many of them they
	// confirmed. Since the platform opens one per finished call, the share is
	// how much of the day's after-call work somebody actually looked at.
	WrapUpsOpened    int `json:"wrapUpsOpened"`
	WrapUpsConfirmed int `json:"wrapUpsConfirmed"`
	ConfirmedPct     int `json:"confirmedPct"`
}

// ReportAgentDay aggregates one agent's window from the ledger and their
// presence history.
//
// Handle time is talk plus after-call work, which is what the agent was busy
// with; occupancy is that against the time they were signed in at all. The
// averages are derived here rather than in SQL so the arithmetic — including
// what happens on a day with no calls — is testable without a database.
func (l *LedgerStore) ReportAgentDay(ctx context.Context, agentID uuid.UUID, from, to time.Time) (AgentDay, error) {
	row, err := l.q.ReportAgentToday(ctx, queries.ReportAgentTodayParams{
		FromAt: stamp(from), ToAt: stamp(to), AgentID: &agentID,
	})
	if err != nil {
		return AgentDay{}, err
	}
	return agentDay(int(row.CallsHandled), int(row.TalkSec), int(row.WrapUpSec),
		int(row.WrapUps), int(row.SignedInSec),
		int(row.WrapUpsOpened), int(row.WrapUpsConfirmed)), nil
}

// agentDay derives the averages. Separated from the query because every
// interesting case is a division by something that can be zero.
func agentDay(callsHandled, talkSec, wrapUpSec, wrapUps, signedInSec,
	wrapUpsOpened, wrapUpsConfirmed int) AgentDay {
	day := AgentDay{
		CallsHandled: callsHandled, TalkSec: talkSec,
		WrapUpSec: wrapUpSec, SignedInSec: signedInSec,
		WrapUpsOpened: wrapUpsOpened, WrapUpsConfirmed: wrapUpsConfirmed,
	}
	if wrapUpsOpened > 0 {
		day.ConfirmedPct = wrapUpsConfirmed * 100 / wrapUpsOpened
	}
	busy := talkSec + wrapUpSec
	if callsHandled > 0 {
		day.AvgHandleSec = busy / callsHandled
	}
	if wrapUps > 0 {
		day.AvgWrapUpSec = wrapUpSec / wrapUps
	}
	if signedInSec > 0 {
		// Busy can exceed signed-in time by a second or two at the edges of
		// the window — a call that started before it, a rounded interval — and
		// an occupancy over 100% reads as a bug rather than as rounding.
		day.OccupancyPct = min(100, busy*100/signedInSec)
	}
	return day
}

// ReportDaily aggregates per day.
func (l *LedgerStore) ReportDaily(ctx context.Context, from, to time.Time) ([]DailyReport, error) {
	rows, err := l.q.ReportDaily(ctx, queries.ReportDailyParams{
		StartedAt: stamp(from), StartedAt_2: stamp(to),
	})
	if err != nil {
		return nil, err
	}
	out := make([]DailyReport, 0, len(rows))
	for _, row := range rows {
		out = append(out, DailyReport{
			Day:            row.Day.Time,
			TotalCalls:     int(row.TotalCalls),
			AnsweredCalls:  int(row.AnsweredCalls),
			ContainedCalls: int(row.ContainedCalls),
			AbandonedCalls: int(row.AbandonedCalls),
		})
	}
	return out, nil
}

//
// Helpers.
//

// stamp converts a time to its column form; the zero time becomes NULL.
func stamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// marshalOr encodes v, mapping nil to a JSON zero value so columns never hold
// the string "null".
func marshalOr(v any, empty string) ([]byte, error) {
	if v == nil {
		return []byte(empty), nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(encoded) == "null" {
		return []byte(empty), nil
	}
	return encoded, nil
}

// QueueEvent is one movement of a caller through a queue.
//
// The CDR says how a call ended; these say how routing behaved on the way
// there. A call offered once and a call offered thirty-three times are the
// same abandoned row in the CDR.
type QueueEvent struct {
	OccurredAt time.Time
	QueueID    uuid.UUID
	Event      string
	AgentID    *uuid.UUID
	WaitMs     int
}

// QueueEventsByCall reads one call's journey through the queues, oldest first.
func (l *LedgerStore) QueueEventsByCall(ctx context.Context, callID uuid.UUID) ([]QueueEvent, error) {
	rows, err := l.q.QueueEventsByCall(ctx, &callID)
	if err != nil {
		return nil, err
	}
	out := make([]QueueEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, QueueEvent{
			OccurredAt: row.OccurredAt.Time,
			QueueID:    row.QueueID,
			Event:      row.Event,
			AgentID:    row.AgentID,
			WaitMs:     int(row.WaitMs),
		})
	}
	return out, nil
}
