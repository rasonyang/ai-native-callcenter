// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// TestNumberService is the list of test numbers as the API uses it. Numbers
// are canonical E.164 by the time they reach it; this layer canonicalises.
type TestNumberService interface {
	Add(ctx context.Context, number, note string, by *uuid.UUID) (store.TestNumber, bool, error)
	Remove(ctx context.Context, number string) error
	List(ctx context.Context, filter store.TestNumberFilter) ([]store.TestNumber, int64, error)
}

// ListTestNumbers pages the list, newest first.
func (s *Server) ListTestNumbers(w http.ResponseWriter, r *http.Request, params api.ListTestNumbersParams) {
	// See ListDNCNumbers: an unencoded "+" arrives as a space.
	prefix := strings.TrimSpace(stringOr(params.Prefix))
	if prefix != "" && !strings.HasPrefix(prefix, "+") {
		prefix = "+" + prefix
	}
	items, total, err := s.testNumbers.List(r.Context(), store.TestNumberFilter{
		Prefix: prefix,
		Limit:  intOr(params.Limit, 50),
		Offset: intOr(params.Offset, 0),
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot list test numbers", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list the test numbers", nil)
		return
	}
	out := make([]api.TestNumber, 0, len(items))
	for _, it := range items {
		out = append(out, testNumberOf(it))
	}
	writeJSON(w, http.StatusOK, api.TestNumberList{Items: out, Total: int(total)})
}

// CreateTestNumber puts a number on the list. 201 when it is new, 200 with the
// existing entry when it was already there (a full list included), 409
// TEST_NUMBER_LIMIT_REACHED when it is new and the list is full.
func (s *Server) CreateTestNumber(w http.ResponseWriter, r *http.Request) {
	var in api.TestNumberWrite
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
	note := strings.TrimSpace(stringOr(in.Note))
	if utf8.RuneCountInString(note) > store.MaxTestNumberNote {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, "the note is too long",
			map[string]any{"field": "note", "rule": "TOO_LONG", "max": store.MaxTestNumberNote})
		return
	}
	entry, isNew, err := s.testNumbers.Add(r.Context(), number, note, ac.actorOrNil())
	if err != nil {
		if errors.Is(err, store.ErrTestNumberListFull) {
			writeError(w, http.StatusConflict, CodeTestNumberLimitReached,
				"the list of test numbers is full; take a number off it first",
				map[string]any{"limit": store.MaxTestNumbers})
			return
		}
		slog.ErrorContext(r.Context(), "cannot add a test number", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot save the number", nil)
		return
	}
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	writeJSON(w, status, testNumberOf(entry))
}

// DeleteTestNumber takes a canonical number off the list.
func (s *Server) DeleteTestNumber(w http.ResponseWriter, r *http.Request, number string) {
	if !phonenum.IsCanonical(number) {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"the path must carry a canonical E.164 number",
			map[string]any{"field": "number", "rule": string(phonenum.NumberInvalid)})
		return
	}
	if err := s.testNumbers.Remove(r.Context(), number); err != nil {
		if errors.Is(err, store.ErrTestNumberNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "that number is not on the list", nil)
			return
		}
		slog.ErrorContext(r.Context(), "cannot remove a test number", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot remove the number", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func testNumberOf(e store.TestNumber) api.TestNumber {
	out := api.TestNumber{Number: e.Number, CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy}
	if e.Note != "" {
		note := e.Note
		out.Note = &note
	}
	return out
}
