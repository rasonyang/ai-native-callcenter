// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// CatalogService is the configuration surface used by the API.
type CatalogService interface {
	Extensions(ctx context.Context) ([]catalog.Extension, error)
	CreateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error)
	UpdateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error)
	DeleteExtension(ctx context.Context, id uuid.UUID) error

	Queues(ctx context.Context) ([]catalog.Queue, error)
	CreateQueue(ctx context.Context, q catalog.Queue) (catalog.Queue, error)
	UpdateQueue(ctx context.Context, q catalog.Queue) (catalog.Queue, error)
	DeleteQueue(ctx context.Context, id uuid.UUID) error

	QueueAgents(ctx context.Context, queueID uuid.UUID) ([]catalog.QueueAgent, error)
	StaffQueue(ctx context.Context, queueID, agentID uuid.UUID, level, position int) error
	UnstaffQueue(ctx context.Context, queueID, agentID uuid.UUID) error

	DIDs(ctx context.Context) ([]catalog.DID, error)
	CreateDID(ctx context.Context, d catalog.DID) (catalog.DID, error)
	UpdateDID(ctx context.Context, d catalog.DID) (catalog.DID, error)
	DeleteDID(ctx context.Context, id uuid.UUID) error
}

//
// Extensions.
//

func (s *Server) ListExtensions(w http.ResponseWriter, r *http.Request) {
	items, err := s.catalog.Extensions(r.Context())
	s.writeList(w, r, items, err)
}

func (s *Server) CreateExtension(w http.ResponseWriter, r *http.Request) {
	var in catalog.Extension
	if !decode(w, r, &in) {
		return
	}
	out, err := s.catalog.CreateExtension(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusCreated)
}

func (s *Server) UpdateExtension(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var in catalog.Extension
	if !decode(w, r, &in) {
		return
	}
	in.ID = id
	out, err := s.catalog.UpdateExtension(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusOK)
}

func (s *Server) DeleteExtension(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	s.writeDeleted(w, r, s.catalog.DeleteExtension(r.Context(), id))
}

//
// Queues.
//

func (s *Server) ListQueues(w http.ResponseWriter, r *http.Request) {
	items, err := s.catalog.Queues(r.Context())
	s.writeList(w, r, items, err)
}

func (s *Server) CreateQueue(w http.ResponseWriter, r *http.Request) {
	var in catalog.Queue
	if !decode(w, r, &in) {
		return
	}
	out, err := s.catalog.CreateQueue(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusCreated)
}

func (s *Server) UpdateQueue(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var in catalog.Queue
	if !decode(w, r, &in) {
		return
	}
	in.ID = id
	out, err := s.catalog.UpdateQueue(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusOK)
}

func (s *Server) DeleteQueue(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	s.writeDeleted(w, r, s.catalog.DeleteQueue(r.Context(), id))
}

func (s *Server) ListQueueAgents(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	items, err := s.catalog.QueueAgents(r.Context(), id)
	s.writeList(w, r, items, err)
}

func (s *Server) StaffQueue(w http.ResponseWriter, r *http.Request, queueID uuid.UUID) {
	var in struct {
		AgentID  uuid.UUID `json:"agentId"`
		Level    int       `json:"level"`
		Position int       `json:"position"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Level == 0 {
		in.Level = 1
	}
	if in.Position == 0 {
		in.Position = 1
	}
	s.writeDeleted(w, r, s.catalog.StaffQueue(r.Context(), queueID, in.AgentID, in.Level, in.Position))
}

func (s *Server) UnstaffQueue(w http.ResponseWriter, r *http.Request, queueID, agentID uuid.UUID) {
	s.writeDeleted(w, r, s.catalog.UnstaffQueue(r.Context(), queueID, agentID))
}

//
// Numbers.
//

func (s *Server) ListDIDs(w http.ResponseWriter, r *http.Request) {
	items, err := s.catalog.DIDs(r.Context())
	s.writeList(w, r, items, err)
}

func (s *Server) CreateDID(w http.ResponseWriter, r *http.Request) {
	var in catalog.DID
	if !decode(w, r, &in) {
		return
	}
	out, err := s.catalog.CreateDID(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusCreated)
}

func (s *Server) UpdateDID(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var in catalog.DID
	if !decode(w, r, &in) {
		return
	}
	in.ID = id
	out, err := s.catalog.UpdateDID(r.Context(), in)
	s.writeCatalog(w, r, out, err, http.StatusOK)
}

func (s *Server) DeleteDID(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	s.writeDeleted(w, r, s.catalog.DeleteDID(r.Context(), id))
}

//
// Shared plumbing.
//

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return false
	}
	return true
}

func (s *Server) writeList(w http.ResponseWriter, r *http.Request, items any, err error) {
	if err != nil {
		s.writeCatalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) writeCatalog(w http.ResponseWriter, r *http.Request, item any, err error, status int) {
	if err != nil {
		s.writeCatalogError(w, r, err)
		return
	}
	writeJSON(w, status, item)
}

func (s *Server) writeDeleted(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.writeCatalogError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeCatalogError maps service errors onto the API vocabulary. A unique
// violation from the database is reported as a conflict rather than an
// internal error, because it means the operator picked a number that is taken.
func (s *Server) writeCatalogError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrValidation):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error(), nil)
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "not found", nil)
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, CodeConflict, "that identifier is already in use", nil)
	default:
		slog.ErrorContext(r.Context(), "catalog request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot complete the change", nil)
	}
}
