// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// List returns every flow's identity and publication state.
func (f *FlowStore) List(ctx context.Context) ([]queries.Flow, error) {
	return f.q.ListFlows(ctx)
}
