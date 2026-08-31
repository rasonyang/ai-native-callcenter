// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// ContactService is the customer record book as the API uses it.
//
// Deliberately small: a contact here answers one question — who is this phone
// number, and what did the last person who spoke to them write down. It is not
// a CRM, and a screen that needs one integrates it rather than growing this.
type ContactService interface {
	List(ctx context.Context, filter store.ContactFilter) ([]store.Contact, int64, error)
	Create(ctx context.Context, w store.ContactWrite, by *uuid.UUID) (store.Contact, error)
	Update(ctx context.Context, id uuid.UUID, w store.ContactWrite, by *uuid.UUID) (store.Contact, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// ListContacts searches the book. The cockpit's caller lookup is the same
// endpoint with an exact phoneNumber.
func (s *Server) ListContacts(w http.ResponseWriter, r *http.Request, params api.ListContactsParams) {
	items, total, err := s.contacts.List(r.Context(), store.ContactFilter{
		Query:       stringOr(params.Q),
		PhoneNumber: stringOr(params.PhoneNumber),
		Limit:       intOr(params.Limit, 50),
		Offset:      intOr(params.Offset, 0),
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot list contacts", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list contacts", nil)
		return
	}
	writeJSON(w, http.StatusOK, api.ContactList{Items: contactsOf(items), Total: int(total)})
}

// CreateContact adds a customer record.
func (s *Server) CreateContact(w http.ResponseWriter, r *http.Request) {
	var in api.ContactWrite
	if !decode(w, r, &in) {
		return
	}
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	contact, err := s.contacts.Create(r.Context(), contactWriteFrom(in), &ac.SubjectID)
	s.writeContact(w, r, contact, err, http.StatusCreated)
}

// UpdateContact rewrites a customer record.
func (s *Server) UpdateContact(w http.ResponseWriter, r *http.Request, contactID uuid.UUID) {
	var in api.ContactWrite
	if !decode(w, r, &in) {
		return
	}
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	contact, err := s.contacts.Update(r.Context(), contactID, contactWriteFrom(in), &ac.SubjectID)
	s.writeContact(w, r, contact, err, http.StatusOK)
}

// DeleteContact removes a customer record.
func (s *Server) DeleteContact(w http.ResponseWriter, r *http.Request, contactID uuid.UUID) {
	if err := s.contacts.Delete(r.Context(), contactID); err != nil {
		s.writeContactError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func contactWriteFrom(in api.ContactWrite) store.ContactWrite {
	return store.ContactWrite{
		PhoneNumber: in.PhoneNumber,
		Name:        in.Name,
		Company:     in.Company,
		Email:       in.Email,
		Tags:        in.Tags,
		Notes:       in.Notes,
	}
}

func contactsOf(items []store.Contact) []api.Contact {
	out := make([]api.Contact, 0, len(items))
	for _, c := range items {
		out = append(out, contactOf(c))
	}
	return out
}

func contactOf(c store.Contact) api.Contact {
	return api.Contact{
		ID: c.ID, PhoneNumber: c.PhoneNumber, Name: c.Name, Company: c.Company,
		Email: c.Email, Tags: c.Tags, Notes: c.Notes, LastCallAt: c.LastCallAt,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (s *Server) writeContact(w http.ResponseWriter, r *http.Request, c store.Contact, err error, status int) {
	if err != nil {
		s.writeContactError(w, r, err)
		return
	}
	writeJSON(w, status, contactOf(c))
}

// writeContactError maps the book's failures onto the API vocabulary. The one
// that matters is the duplicate: the phone number is how a caller is found, so
// two records for one number would make the lookup a coin toss.
func (s *Server) writeContactError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrContactInvalid):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error(),
			map[string]any{"field": "phoneNumber", "rule": "PHONE_INVALID"})
	case errors.Is(err, store.ErrContactNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such contact", nil)
	case errors.Is(err, store.ErrContactExists):
		writeError(w, http.StatusConflict, CodeConflict,
			"that phone number already has a contact", map[string]any{"field": "phoneNumber", "rule": "PHONE_TAKEN"})
	default:
		slog.ErrorContext(r.Context(), "contact write failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot save the contact", nil)
	}
}
