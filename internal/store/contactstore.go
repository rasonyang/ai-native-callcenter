// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// ContactStore holds the customer records: who a phone number belongs to, and
// what the last agent noted about them.
type ContactStore struct{ q *queries.Queries }

// Contacts returns the contact book.
func (s *Store) Contacts() *ContactStore { return &ContactStore{q: s.Queries} }

// Contact is one customer record.
type Contact struct {
	ID          uuid.UUID  `json:"id"`
	PhoneNumber string     `json:"phoneNumber"`
	Name        string     `json:"name"`
	Company     string     `json:"company"`
	Email       string     `json:"email"`
	Tags        []string   `json:"tags"`
	Notes       string     `json:"notes"`
	LastCallAt  *time.Time `json:"lastCallAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// ContactWrite is what a create or update carries. Nil fields on an update
// leave the stored value alone.
type ContactWrite struct {
	PhoneNumber string
	Name        *string
	Company     *string
	Email       *string
	Tags        *[]string
	Notes       *string
}

// ContactFilter narrows a listing. Zero values mean "any".
type ContactFilter struct {
	// Query matches the phone number, name and company as a substring.
	Query string
	// PhoneNumber matches exactly: the cockpit's "who is this" lookup.
	PhoneNumber string
	Limit       int
	Offset      int
}

// Errors returned by the contact book.
var (
	ErrContactExists   = errors.New("a contact with that phone number already exists")
	ErrContactNotFound = errors.New("no such contact")
	ErrContactInvalid  = errors.New("invalid contact")
)

// normalize trims what people type and refuses the one thing a contact cannot
// do without.
func (w ContactWrite) normalize() (ContactWrite, error) {
	w.PhoneNumber = strings.TrimSpace(w.PhoneNumber)
	if w.PhoneNumber == "" {
		return w, errors.New("phoneNumber is required")
	}
	if len(w.PhoneNumber) > 32 {
		return w, errors.New("phoneNumber is too long")
	}
	if w.Tags != nil {
		cleaned := make([]string, 0, len(*w.Tags))
		for _, tag := range *w.Tags {
			if tag = strings.TrimSpace(tag); tag != "" {
				cleaned = append(cleaned, tag)
			}
		}
		w.Tags = &cleaned
	}
	return w, nil
}

// Create stores a new contact. The phone number is unique across the book.
func (c *ContactStore) Create(ctx context.Context, w ContactWrite, by *uuid.UUID) (Contact, error) {
	w, err := w.normalize()
	if err != nil {
		return Contact{}, errors.Join(ErrContactInvalid, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Contact{}, err
	}
	tags := []string{}
	if w.Tags != nil {
		tags = *w.Tags
	}
	row, err := c.q.InsertContact(ctx, queries.InsertContactParams{
		ID: id, PhoneNumber: w.PhoneNumber,
		Name: valueOr(w.Name), Company: valueOr(w.Company), Email: valueOr(w.Email),
		Tags: tags, Notes: valueOr(w.Notes), UpdatedBy: by,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Contact{}, ErrContactExists
		}
		return Contact{}, err
	}
	return contactFromRow(row, nil), nil
}

// Update rewrites a contact. Fields the caller did not send keep their value.
func (c *ContactStore) Update(ctx context.Context, id uuid.UUID, w ContactWrite, by *uuid.UUID) (Contact, error) {
	w, err := w.normalize()
	if err != nil {
		return Contact{}, errors.Join(ErrContactInvalid, err)
	}
	current, err := c.q.GetContact(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contact{}, ErrContactNotFound
	}
	if err != nil {
		return Contact{}, err
	}
	arg := queries.UpdateContactParams{
		ID: id, PhoneNumber: w.PhoneNumber,
		Name: current.Name, Company: current.Company, Email: current.Email,
		Tags: current.Tags, Notes: current.Notes, UpdatedBy: by,
	}
	if w.Name != nil {
		arg.Name = *w.Name
	}
	if w.Company != nil {
		arg.Company = *w.Company
	}
	if w.Email != nil {
		arg.Email = *w.Email
	}
	if w.Tags != nil {
		arg.Tags = *w.Tags
	}
	if w.Notes != nil {
		arg.Notes = *w.Notes
	}
	row, err := c.q.UpdateContact(ctx, arg)
	if err != nil {
		if isUniqueViolation(err) {
			return Contact{}, ErrContactExists
		}
		return Contact{}, err
	}
	return contactFromRow(row, nil), nil
}

// Delete removes a contact.
func (c *ContactStore) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := c.q.DeleteContact(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrContactNotFound
	}
	return nil
}

// List pages the book, newest edits first, with each contact's last call.
func (c *ContactStore) List(ctx context.Context, filter ContactFilter) ([]Contact, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	rows, err := c.q.ListContacts(ctx, queries.ListContactsParams{
		PhoneNumber: strings.TrimSpace(filter.PhoneNumber),
		Q:           strings.TrimSpace(filter.Query),
		PageLimit:   int32(filter.Limit),
		PageOffset:  int32(filter.Offset),
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := c.q.CountContacts(ctx, queries.CountContactsParams{
		PhoneNumber: strings.TrimSpace(filter.PhoneNumber),
		Q:           strings.TrimSpace(filter.Query),
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]Contact, 0, len(rows))
	for _, row := range rows {
		var last *time.Time
		if row.LastCallAt.Valid {
			at := row.LastCallAt.Time
			last = &at
		}
		out = append(out, contactFromRow(row.Contact, last))
	}
	return out, total, nil
}

func contactFromRow(row queries.Contact, lastCallAt *time.Time) Contact {
	tags := row.Tags
	if tags == nil {
		tags = []string{}
	}
	return Contact{
		ID: row.ID, PhoneNumber: row.PhoneNumber, Name: row.Name, Company: row.Company,
		Email: row.Email, Tags: tags, Notes: row.Notes, LastCallAt: lastCallAt,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

// isUniqueViolation reports a PostgreSQL unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func valueOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
