// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"net/netip"

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
	TotalSec     int `json:"totalSec"`

	Status       string `json:"status"`
	HangupCause  string `json:"hangupCause,omitempty"`
	MissedReason string `json:"missedReason,omitempty"` // empty when the call was not missed
	Disposition  string `json:"disposition,omitempty"`
	IsContained  bool   `json:"isContained"`
	HasRecording bool   `json:"hasRecording"`

	UserData map[string]any `json:"userData,omitempty"`
	Tech     map[string]any `json:"tech,omitempty"`
	Legs     []Leg          `json:"legs"`
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

// InsertCDR writes the ledger row. A second insert for the same call is a
// no-op: the first writer wins, which is what makes call retirement safe to
// run from more than one place.
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
	return out, total, nil
}

// GetCDR reads one call.
func (l *LedgerStore) GetCDR(ctx context.Context, callID uuid.UUID) (CDR, error) {
	row, err := l.q.GetCDR(ctx, callID)
	if err != nil {
		return CDR{}, err
	}
	return fromRow(row), nil
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

// Transcript roles and kinds, byte-identical across the stack.
const (
	TranscriptRoleBot    = "BOT"
	TranscriptRoleCaller = "CALLER"

	TranscriptKindText       = "TEXT"
	TranscriptKindToolCall   = "TOOL_CALL"
	TranscriptKindToolResult = "TOOL_RESULT"
)

// TranscriptEntry is one thing said or done on an AI leg.
type TranscriptEntry struct {
	Seq        int            `json:"seq"`
	OccurredAt time.Time      `json:"occurredAt"`
	Role       string         `json:"role"`
	Kind       string         `json:"kind"`
	Content    map[string]any `json:"content"`
}

// InsertTranscript writes a call's transcript in one batch at call end.
func (l *LedgerStore) InsertTranscript(ctx context.Context, callID uuid.UUID, entries []TranscriptEntry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]queries.InsertTranscriptParams, 0, len(entries))
	for _, e := range entries {
		content, err := marshalOr(e.Content, "{}")
		if err != nil {
			return fmt.Errorf("encode transcript %d: %w", e.Seq, err)
		}
		rows = append(rows, queries.InsertTranscriptParams{
			CallID:     callID,
			Seq:        int32(e.Seq),
			OccurredAt: stamp(e.OccurredAt),
			Role:       e.Role,
			Kind:       e.Kind,
			Content:    content,
		})
	}
	_, err := l.q.InsertTranscript(ctx, rows)
	return err
}

// ListTranscript reads a call's transcript in order.
func (l *LedgerStore) ListTranscript(ctx context.Context, callID uuid.UUID) ([]TranscriptEntry, error) {
	rows, err := l.q.ListTranscripts(ctx, callID)
	if err != nil {
		return nil, err
	}
	out := make([]TranscriptEntry, 0, len(rows))
	for _, row := range rows {
		entry := TranscriptEntry{
			Seq:        int(row.Seq),
			OccurredAt: row.OccurredAt.Time,
			Role:       row.Role,
			Kind:       row.Kind,
		}
		_ = json.Unmarshal(row.Content, &entry.Content)
		out = append(out, entry)
	}
	return out, nil
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
