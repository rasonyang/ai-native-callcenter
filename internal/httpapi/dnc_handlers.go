// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// DNCService is the do-not-call list as the API uses it. Numbers are canonical
// E.164 by the time they reach it; this layer canonicalises.
type DNCService interface {
	Add(ctx context.Context, number, source string, callID, by *uuid.UUID) (store.DNCNumber, bool, error)
	Remove(ctx context.Context, number string) error
	List(ctx context.Context, filter store.DNCFilter) ([]store.DNCNumber, int64, error)
}

// ListDNCNumbers pages the list, newest first.
func (s *Server) ListDNCNumbers(w http.ResponseWriter, r *http.Request, params api.ListDNCNumbersParams) {
	// A canonical number starts with "+", which an unencoded query string
	// turns into a space. A prefix with no "+" can only mean the plus was lost
	// (or omitted), so it is put back rather than matching nothing.
	prefix := strings.TrimSpace(stringOr(params.Prefix))
	if prefix != "" && !strings.HasPrefix(prefix, "+") {
		prefix = "+" + prefix
	}
	items, total, err := s.dnc.List(r.Context(), store.DNCFilter{
		Prefix: prefix,
		Limit:  intOr(params.Limit, 50),
		Offset: intOr(params.Offset, 0),
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot list do-not-call numbers", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list the do-not-call list", nil)
		return
	}
	out := make([]api.DNCNumber, 0, len(items))
	for _, it := range items {
		out = append(out, dncNumberOf(it))
	}
	writeJSON(w, http.StatusOK, api.DNCNumberList{Items: out, Total: int(total)})
}

// CreateDNCNumber puts a number on the list. 201 when it is new, 200 with the
// existing entry when it was already there: adding twice is not an error.
func (s *Server) CreateDNCNumber(w http.ResponseWriter, r *http.Request) {
	var in api.DNCNumberWrite
	if !decode(w, r, &in) {
		return
	}
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	number, err := phonenum.Canonical(in.Number, s.cfg.DefaultCountryCode)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error(),
			map[string]any{"field": "number", "rule": string(phonenum.ReasonOf(err))})
		return
	}
	// CONVERSATION is written only by the platform, when a customer asks on a
	// call; a client may say MANUAL or IMPORT and nothing else.
	source := store.DNCSourceManual
	if in.Source != nil {
		switch *in.Source {
		case api.DNCNumberWriteSourceMANUAL:
		case api.DNCNumberWriteSourceIMPORT:
			source = store.DNCSourceImport
		default:
			writeError(w, http.StatusBadRequest, CodeValidationFailed, "unknown source",
				map[string]any{"allowed": []string{
					string(api.DNCNumberWriteSourceMANUAL), string(api.DNCNumberWriteSourceIMPORT)}})
			return
		}
	}
	entry, isNew, err := s.dnc.Add(r.Context(), number, source, nil, ac.actorOrNil())
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot add a do-not-call number", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot save the number", nil)
		return
	}
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	writeJSON(w, status, dncNumberOf(entry))
}

// DeleteDNCNumber takes a canonical number off the list.
func (s *Server) DeleteDNCNumber(w http.ResponseWriter, r *http.Request, number string) {
	// Exactly canonical: the path is the key, and a number that merely
	// canonicalises is a different request than the one this operation names.
	if !phonenum.IsCanonical(number) {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"the path must carry a canonical E.164 number",
			map[string]any{"field": "number", "rule": string(phonenum.NumberInvalid)})
		return
	}
	if err := s.dnc.Remove(r.Context(), number); err != nil {
		if errors.Is(err, store.ErrDNCNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "that number is not on the list", nil)
			return
		}
		slog.ErrorContext(r.Context(), "cannot remove a do-not-call number", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot remove the number", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func dncNumberOf(e store.DNCNumber) api.DNCNumber {
	return api.DNCNumber{
		Number: e.Number, Source: api.DNCSource(e.Source), CallID: e.CallID,
		CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy,
	}
}
