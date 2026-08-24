// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// FlowStore keeps conversation flows.
//
// The draft is what an author edits; a revision is an immutable copy taken at
// publish time. Calls only ever run the published revision, so an edit in
// progress can never change what a live number does.
type FlowStore struct{ q *queries.Queries }

// Flows returns the flow store.
func (s *Store) Flows() *FlowStore { return &FlowStore{q: s.Queries} }

// ErrFlowNotPublished distinguishes a flow that exists but has never been
// published from one that does not exist.
var ErrFlowNotPublished = errors.New("store: flow has no published revision")

// ErrFlowNotFound is asked for a flow that is not there.
var ErrFlowNotFound = errors.New("store: no such flow")

// FlowSummary is a flow's identity and publication state, without its spec:
// a roster of flows has no business shipping every document.
type FlowSummary struct {
	ID                  uuid.UUID
	Slug                string
	Name                string
	PublishedRevisionID *uuid.UUID
	// PublishedAt is nil while the flow has never gone live.
	PublishedAt *time.Time
	// HasUnpublishedChanges is the gap between what is written and what
	// answers the phone: the draft differs from the published revision, or
	// nothing is published at all.
	HasUnpublishedChanges bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// FlowRevisionSummary is one publish that happened. The snapshot itself is not
// carried: this records that a publish occurred, not what to roll back to.
type FlowRevisionSummary struct {
	ID          uuid.UUID
	Note        string
	CreatedAt   time.Time
	IsPublished bool
}

// FlowDetail is one flow with the draft an author edits and the publishes
// behind it.
type FlowDetail struct {
	Flow      FlowSummary
	DraftSpec []byte
	Revisions []FlowRevisionSummary
}

// PublishedSpec loads and parses the published revision of a flow.
//
// Validation runs again here even though publishing validated: the parsed spec
// is what a live call will follow, and trusting stored bytes over the current
// loader's rules is how version drift becomes a mid-call surprise.
func (f *FlowStore) PublishedSpec(ctx context.Context, flowID uuid.UUID) (*flow.Spec, error) {
	raw, err := f.q.GetPublishedSpec(ctx, flowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFlowNotPublished
		}
		return nil, fmt.Errorf("load published flow %s: %w", flowID, err)
	}
	spec, err := flow.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("published flow %s: %w", flowID, err)
	}
	return spec, nil
}

// Create stores a new flow with a draft. The draft must already be a valid
// spec: there is no point storing what could never publish.
func (f *FlowStore) Create(ctx context.Context, slug, name string, draft []byte) (uuid.UUID, error) {
	if _, err := flow.Load(draft); err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	_, err = f.q.CreateFlow(ctx, queries.CreateFlowParams{
		ID: id, Slug: slug, Name: name, DraftSpec: draft,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create flow %s: %w", slug, err)
	}
	return id, nil
}

// Publish snapshots the current draft as an immutable revision and points the
// flow at it.
func (f *FlowStore) Publish(ctx context.Context, flowID uuid.UUID, note string) error {
	record, err := f.q.GetFlow(ctx, flowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFlowNotFound
		}
		return fmt.Errorf("publish flow %s: %w", flowID, err)
	}
	if _, err := flow.Load(record.DraftSpec); err != nil {
		return fmt.Errorf("publish flow %s: draft is not publishable: %w", flowID, err)
	}

	revisionID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := f.q.CreateFlowRevision(ctx, queries.CreateFlowRevisionParams{
		ID: revisionID, FlowID: flowID, Spec: record.DraftSpec, Note: note,
	}); err != nil {
		return fmt.Errorf("snapshot flow %s: %w", flowID, err)
	}
	if err := f.q.SetPublishedRevision(ctx, queries.SetPublishedRevisionParams{
		ID: flowID, PublishedRevisionID: &revisionID,
	}); err != nil {
		return fmt.Errorf("point flow %s at its revision: %w", flowID, err)
	}
	return nil
}

// UpdateDraft replaces a flow's draft.
//
// It validates for the same reason Create does: a draft that cannot publish is
// a spec nobody can use, and storing it only moves the failure to whoever
// presses Publish. The name travels with it because both are what an author
// edits; the slug does not, being the identity automation updates the flow by.
func (f *FlowStore) UpdateDraft(ctx context.Context, flowID uuid.UUID, name string, draft []byte) error {
	if _, err := flow.Load(draft); err != nil {
		return err
	}
	if _, err := f.q.UpdateFlowDraft(ctx, queries.UpdateFlowDraftParams{
		ID: flowID, Name: name, DraftSpec: draft,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFlowNotFound
		}
		return fmt.Errorf("update flow %s: %w", flowID, err)
	}
	return nil
}

// List returns every flow's identity and publication state.
func (f *FlowStore) List(ctx context.Context) ([]FlowSummary, error) {
	rows, err := f.q.ListFlowSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	out := make([]FlowSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, FlowSummary{
			ID: row.ID, Slug: row.Slug, Name: row.Name,
			PublishedRevisionID:   row.PublishedRevisionID,
			PublishedAt:           timeOrNil(row.PublishedAt),
			HasUnpublishedChanges: row.HasUnpublishedChanges,
			CreatedAt:             row.CreatedAt.Time,
			UpdatedAt:             row.UpdatedAt.Time,
		})
	}
	return out, nil
}

// Summary is one flow's identity and publication state.
func (f *FlowStore) Summary(ctx context.Context, flowID uuid.UUID) (FlowSummary, error) {
	row, err := f.q.GetFlowSummary(ctx, flowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FlowSummary{}, ErrFlowNotFound
		}
		return FlowSummary{}, fmt.Errorf("read flow %s: %w", flowID, err)
	}
	return FlowSummary{
		ID: row.ID, Slug: row.Slug, Name: row.Name,
		PublishedRevisionID:   row.PublishedRevisionID,
		PublishedAt:           timeOrNil(row.PublishedAt),
		HasUnpublishedChanges: row.HasUnpublishedChanges,
		CreatedAt:             row.CreatedAt.Time,
		UpdatedAt:             row.UpdatedAt.Time,
	}, nil
}

// Detail is one flow with its draft and its publication history.
func (f *FlowStore) Detail(ctx context.Context, flowID uuid.UUID) (FlowDetail, error) {
	summary, err := f.Summary(ctx, flowID)
	if err != nil {
		return FlowDetail{}, err
	}
	draft, err := f.q.GetFlowDraft(ctx, flowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FlowDetail{}, ErrFlowNotFound
		}
		return FlowDetail{}, fmt.Errorf("read draft of flow %s: %w", flowID, err)
	}
	rows, err := f.q.ListFlowRevisions(ctx, flowID)
	if err != nil {
		return FlowDetail{}, fmt.Errorf("read revisions of flow %s: %w", flowID, err)
	}
	revisions := make([]FlowRevisionSummary, 0, len(rows))
	for _, row := range rows {
		revisions = append(revisions, FlowRevisionSummary{
			ID: row.ID, Note: row.Note,
			CreatedAt: row.CreatedAt.Time, IsPublished: row.IsPublished,
		})
	}
	return FlowDetail{Flow: summary, DraftSpec: draft, Revisions: revisions}, nil
}

// timeOrNil keeps "never happened" distinct from the zero instant.
func timeOrNil(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
