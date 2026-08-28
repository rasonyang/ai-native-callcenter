// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Conversation flows, as an administrator edits them.
//
// The draft is what an author writes; a revision is an immutable copy taken at
// publish time, and calls only ever run the published revision. So every write
// here is safe against a live number until somebody publishes — which is the
// one operation that changes what a caller hears.

// FlowService is the flow surface the API edits.
type FlowService interface {
	List(ctx context.Context) ([]store.FlowSummary, error)
	Summary(ctx context.Context, flowID uuid.UUID) (store.FlowSummary, error)
	Detail(ctx context.Context, flowID uuid.UUID) (store.FlowDetail, error)
	Create(ctx context.Context, slug, name string, draft []byte) (uuid.UUID, error)
	UpdateDraft(ctx context.Context, flowID uuid.UUID, name string, draft []byte) error
	Publish(ctx context.Context, flowID uuid.UUID, note string) error
}

// A spec is a document, not a form: the 64KB body limit the catalogue writes
// use is about small records, and a flow with a full persona, a dozen phases
// and their tools is legitimately larger than that.
const maxFlowBodyBytes = 512 << 10

// slugPattern is the contract's, restated where it is enforced: the generated
// server binds parameters but does not validate bodies.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ListFlows serves every flow's identity and publication state.
func (s *Server) ListFlows(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read flows", nil)
		return
	}
	rows, err := s.flows.List(r.Context())
	if err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	items := make([]api.Flow, 0, len(rows))
	for _, row := range rows {
		items = append(items, flowOut(row))
	}
	writeJSON(w, http.StatusOK, api.FlowList{Items: items})
}

// GetFlow serves one flow with its draft and its publication history.
func (s *Server) GetFlow(w http.ResponseWriter, r *http.Request, flowID uuid.UUID) {
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read flows", nil)
		return
	}
	detail, err := s.flows.Detail(r.Context(), flowID)
	if err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	var spec api.FlowSpec
	if err := json.Unmarshal(detail.DraftSpec, &spec); err != nil {
		// The stored draft was validated on the way in, so this is corruption
		// rather than a bad request, and saying so beats an empty editor.
		slog.ErrorContext(r.Context(), "stored flow draft is not an object", "flowId", flowID, "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "the stored draft cannot be read", nil)
		return
	}
	revisions := make([]api.FlowRevision, 0, len(detail.Revisions))
	for _, rev := range detail.Revisions {
		revisions = append(revisions, api.FlowRevision{
			RevisionID:  rev.ID,
			Note:        rev.Note,
			CreatedAt:   rev.CreatedAt,
			IsPublished: rev.IsPublished,
		})
	}
	writeJSON(w, http.StatusOK, api.FlowDetail{
		Flow:      flowOut(detail.Flow),
		DraftSpec: spec,
		Revisions: revisions,
	})
}

// CreateFlow stores a new draft. It does not publish: a flow that answers a
// number the moment it is typed would make saving a deployment.
func (s *Server) CreateFlow(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot store flows", nil)
		return
	}
	var in api.FlowCreate
	if !decodeFlowBody(w, r, &in) {
		return
	}
	slug := strings.TrimSpace(in.Slug)
	if !slugPattern.MatchString(slug) || len(slug) > 64 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a slug is lowercase letters, digits, hyphen and underscore", map[string]any{"field": "slug", "rule": "SLUG_CHARSET"})
		return
	}
	name, ok := flowName(w, in.Name)
	if !ok {
		return
	}
	spec, ok := specBytes(w, r, in.Spec)
	if !ok {
		return
	}

	id, err := s.flows.Create(r.Context(), slug, name, spec)
	if err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	s.writeFlowSummary(w, r, id, http.StatusCreated)
}

// UpdateFlowDraft replaces the draft. The published revision keeps running:
// an edit in progress must never change what a live number does.
func (s *Server) UpdateFlowDraft(w http.ResponseWriter, r *http.Request, flowID uuid.UUID) {
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot store flows", nil)
		return
	}
	var in api.FlowDraftWrite
	if !decodeFlowBody(w, r, &in) {
		return
	}
	name, ok := flowName(w, in.Name)
	if !ok {
		return
	}
	spec, ok := specBytes(w, r, in.Spec)
	if !ok {
		return
	}

	if err := s.flows.UpdateDraft(r.Context(), flowID, name, spec); err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	s.writeFlowSummary(w, r, flowID, http.StatusOK)
}

// PublishFlow snapshots the draft and points the flow at it. This is the only
// operation on this surface a caller can hear.
func (s *Server) PublishFlow(w http.ResponseWriter, r *http.Request, flowID uuid.UUID) {
	if s.flows == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot publish flows", nil)
		return
	}
	// The note is optional, and so is the body that carries it: publishing
	// without saying why is a choice, not a malformed request.
	var in api.FlowPublish
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return
	}
	note := ""
	if in.Note != nil {
		note = strings.TrimSpace(*in.Note)
	}
	if len(note) > 200 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"the note is longer than 200 characters", map[string]any{"field": "note", "rule": "NOTE_TOO_LONG"})
		return
	}

	if err := s.flows.Publish(r.Context(), flowID, note); err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	s.writeFlowSummary(w, r, flowID, http.StatusOK)
}

// writeFlowSummary answers a write with the flow as it now stands, so a client
// never has to guess what publishing did to it.
func (s *Server) writeFlowSummary(w http.ResponseWriter, r *http.Request, flowID uuid.UUID, status int) {
	summary, err := s.flows.Summary(r.Context(), flowID)
	if err != nil {
		s.writeFlowError(w, r, err)
		return
	}
	writeJSON(w, status, flowOut(summary))
}

func flowOut(row store.FlowSummary) api.Flow {
	return api.Flow{
		FlowID:                row.ID,
		Slug:                  row.Slug,
		Name:                  row.Name,
		PublishedRevisionID:   row.PublishedRevisionID,
		PublishedAt:           row.PublishedAt,
		HasUnpublishedChanges: row.HasUnpublishedChanges,
		CreatedAt:             row.CreatedAt,
		UpdatedAt:             row.UpdatedAt,
	}
}

func flowName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > 120 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a flow needs a name of at most 120 characters", map[string]any{"field": "name", "rule": "NAME_TOO_LONG"})
		return "", false
	}
	return name, true
}

// specBytes turns the decoded document back into the bytes the loader reads.
// The column is jsonb, which normalizes key order and whitespace anyway, so
// nothing is lost that was ever going to be kept.
func specBytes(w http.ResponseWriter, r *http.Request, spec api.FlowSpec) ([]byte, bool) {
	raw, err := json.Marshal(spec)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot re-encode a decoded flow spec", "error", err)
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "the spec cannot be encoded", nil)
		return nil, false
	}
	return raw, true
}

func decodeFlowBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFlowBodyBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return false
	}
	return true
}

func (s *Server) writeFlowError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *flow.ValidationError
	switch {
	case errors.As(err, &invalid):
		// Every problem, not the first: an author fixing a spec should see
		// the whole report rather than discover it one save at a time.
		problems := make([]any, 0, len(invalid.Problems))
		for _, p := range invalid.Problems {
			problems = append(problems, p)
		}
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			invalid.Error(), map[string]any{"problems": problems})
	case errors.Is(err, store.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such flow", nil)
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, CodeConflict, "that slug is already in use", nil)
	default:
		slog.ErrorContext(r.Context(), "flow request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot complete the change", nil)
	}
}
