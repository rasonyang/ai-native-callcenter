// SPDX-License-Identifier: Apache-2.0

// Package catalog owns the configuration a call centre runs on: extensions,
// queues, the numbers that reach them, and who staffs what.
//
// Configuration lives in PostgreSQL and the switch reads most of it through
// the Lua handlers, so a change here is a database write plus, where the
// switch caches something, a targeted refresh.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// Store is the persistence this service needs.
type Store interface {
	ListExtensions(ctx context.Context) ([]Extension, error)
	CreateExtension(ctx context.Context, e Extension) (Extension, error)
	UpdateExtension(ctx context.Context, e Extension) (Extension, error)
	SetExtensionPassword(ctx context.Context, id uuid.UUID, password string) error
	DeleteExtension(ctx context.Context, id uuid.UUID) error

	ListQueues(ctx context.Context) ([]Queue, error)
	CreateQueue(ctx context.Context, q Queue) (Queue, error)
	UpdateQueue(ctx context.Context, q Queue) (Queue, error)
	DeleteQueue(ctx context.Context, id uuid.UUID) error
	QueueByID(ctx context.Context, id uuid.UUID) (Queue, error)

	ListQueueAgents(ctx context.Context, queueID uuid.UUID) ([]QueueAgent, error)
	SetQueueAgent(ctx context.Context, queueID, agentID uuid.UUID, level, position int) error
	RemoveQueueAgent(ctx context.Context, queueID, agentID uuid.UUID) error

	ListDIDs(ctx context.Context) ([]DID, error)
	CreateDID(ctx context.Context, d DID) (DID, error)
	UpdateDID(ctx context.Context, d DID) (DID, error)
	DeleteDID(ctx context.Context, id uuid.UUID) error
}

// SwitchControl is the part of the switch that caches configuration.
type SwitchControl interface {
	ReloadQueue(name string) error
	AddCallcenterTier(queue, agent string, level, position int) error
	DeleteCallcenterTier(queue, agent string) error
	IsUp() bool
}

// AgentNames resolves an agent's switch-side name for tier management.
type AgentNames interface {
	CallcenterName(ctx context.Context, agentID uuid.UUID) (string, error)
}

// Errors returned by the service.
var (
	ErrValidation = errors.New("validation failed")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("already exists")
)

// Service is the configuration catalogue.
type Service struct {
	store     Store
	switchCtl SwitchControl
	agents    AgentNames
}

// NewService builds a Service.
func NewService(store Store, switchCtl SwitchControl, agents AgentNames) *Service {
	return &Service{store: store, switchCtl: switchCtl, agents: agents}
}

//
// Extensions.
//

// Extensions lists every extension. Passwords are never returned.
func (s *Service) Extensions(ctx context.Context) ([]Extension, error) {
	return s.store.ListExtensions(ctx)
}

// CreateExtension adds an extension. It becomes usable on the phone's next
// registration, because the switch reads the directory per lookup.
func (s *Service) CreateExtension(ctx context.Context, e Extension) (Extension, error) {
	if err := e.validate(true); err != nil {
		return Extension{}, err
	}
	e.ID = uuid.Must(uuid.NewV7())
	return s.store.CreateExtension(ctx, e)
}

// UpdateExtension changes an extension. An empty password leaves it alone, so
// editing a phone never silently clears its credentials.
func (s *Service) UpdateExtension(ctx context.Context, e Extension) (Extension, error) {
	if err := e.validate(false); err != nil {
		return Extension{}, err
	}
	updated, err := s.store.UpdateExtension(ctx, e)
	if err != nil {
		return Extension{}, err
	}
	if e.Password != "" {
		if err := s.store.SetExtensionPassword(ctx, e.ID, e.Password); err != nil {
			return Extension{}, err
		}
	}
	return updated, nil
}

// DeleteExtension removes an extension.
func (s *Service) DeleteExtension(ctx context.Context, id uuid.UUID) error {
	return s.store.DeleteExtension(ctx, id)
}

//
// Queues.
//

// Queues lists every queue.
func (s *Service) Queues(ctx context.Context) ([]Queue, error) { return s.store.ListQueues(ctx) }

// CreateQueue adds a queue and tells the switch to read it.
func (s *Service) CreateQueue(ctx context.Context, q Queue) (Queue, error) {
	if err := q.validate(); err != nil {
		return Queue{}, err
	}
	q.ID = uuid.Must(uuid.NewV7())
	created, err := s.store.CreateQueue(ctx, q)
	if err != nil {
		return Queue{}, err
	}
	s.refreshQueue(ctx, created.Name)
	return created, nil
}

// UpdateQueue changes a queue and refreshes the switch's copy.
func (s *Service) UpdateQueue(ctx context.Context, q Queue) (Queue, error) {
	if err := q.validate(); err != nil {
		return Queue{}, err
	}
	updated, err := s.store.UpdateQueue(ctx, q)
	if err != nil {
		return Queue{}, err
	}
	s.refreshQueue(ctx, updated.Name)
	return updated, nil
}

// DeleteQueue removes a queue.
func (s *Service) DeleteQueue(ctx context.Context, id uuid.UUID) error {
	return s.store.DeleteQueue(ctx, id)
}

// refreshQueue makes the switch re-read one queue from the database.
//
// The switch caches queue configuration at load time, so a change that is only
// written to the database would take effect at the next restart. A switch that
// is down is not an error here: the whole configuration is re-read when it
// comes back.
func (s *Service) refreshQueue(ctx context.Context, name string) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	if err := s.switchCtl.ReloadQueue(name); err != nil {
		slog.WarnContext(ctx, "queue not refreshed on the switch", "queue", name, "error", err)
	}
}

//
// Staffing.
//

// QueueAgents lists who staffs a queue.
func (s *Service) QueueAgents(ctx context.Context, queueID uuid.UUID) ([]QueueAgent, error) {
	return s.store.ListQueueAgents(ctx, queueID)
}

// StaffQueue puts an agent on a queue, in the database and on the switch.
func (s *Service) StaffQueue(ctx context.Context, queueID, agentID uuid.UUID, level, position int) error {
	if level < 1 || position < 1 {
		return fmt.Errorf("%w: level and position start at 1", ErrValidation)
	}
	queue, err := s.store.QueueByID(ctx, queueID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err := s.store.SetQueueAgent(ctx, queueID, agentID, level, position); err != nil {
		return err
	}

	name, err := s.agents.CallcenterName(ctx, agentID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if s.switchCtl != nil && s.switchCtl.IsUp() {
		if err := s.switchCtl.AddCallcenterTier(queue.Name, name, level, position); err != nil {
			slog.WarnContext(ctx, "tier not applied on the switch", "queue", queue.Name, "error", err)
		}
	}
	return nil
}

// UnstaffQueue takes an agent off a queue.
func (s *Service) UnstaffQueue(ctx context.Context, queueID, agentID uuid.UUID) error {
	queue, err := s.store.QueueByID(ctx, queueID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err := s.store.RemoveQueueAgent(ctx, queueID, agentID); err != nil {
		return err
	}
	name, err := s.agents.CallcenterName(ctx, agentID)
	if err != nil {
		return nil // the agent is gone; the switch tier goes with it
	}
	if s.switchCtl != nil && s.switchCtl.IsUp() {
		if err := s.switchCtl.DeleteCallcenterTier(queue.Name, name); err != nil {
			slog.WarnContext(ctx, "tier not removed on the switch", "queue", queue.Name, "error", err)
		}
	}
	return nil
}

//
// Numbers.
//

// DIDs lists every external number.
func (s *Service) DIDs(ctx context.Context) ([]DID, error) { return s.store.ListDIDs(ctx) }

// CreateDID adds an external number.
func (s *Service) CreateDID(ctx context.Context, d DID) (DID, error) {
	if err := d.validate(); err != nil {
		return DID{}, err
	}
	d.ID = uuid.Must(uuid.NewV7())
	return s.store.CreateDID(ctx, d)
}

// UpdateDID changes an external number.
func (s *Service) UpdateDID(ctx context.Context, d DID) (DID, error) {
	if err := d.validate(); err != nil {
		return DID{}, err
	}
	return s.store.UpdateDID(ctx, d)
}

// DeleteDID removes an external number.
func (s *Service) DeleteDID(ctx context.Context, id uuid.UUID) error {
	return s.store.DeleteDID(ctx, id)
}

// digitsOnly reports whether a string is a usable dialable number.
func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func trim(s string) string { return strings.TrimSpace(s) }
