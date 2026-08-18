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
	// CallcenterQueuesForAgent reports the queues the switch currently believes
	// this agent staffs. Staffing is reconciled rather than applied, and a tier
	// the switch still holds cannot be discovered any other way.
	//
	// Names only: level and position are always the database's answer, never
	// the switch's, so there is nothing to learn from the switch's copy of them
	// and no shared type either side has to know about.
	CallcenterQueuesForAgent(agent string) ([]string, error)
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

// QueueIDByName resolves a queue name to its identity, for the ledger: the
// switch speaks in names, the ledger in ids.
func (s *Service) QueueIDByName(ctx context.Context, name string) (uuid.UUID, bool) {
	queues, err := s.store.ListQueues(ctx)
	if err != nil {
		return uuid.Nil, false
	}
	for _, q := range queues {
		if q.Name == name {
			return q.ID, true
		}
	}
	return uuid.Nil, false
}

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

// ReconcileAgentTiers converges the switch's view of one agent's staffing to
// this system's.
//
// It runs when an agent becomes addressable, because that is the first moment
// a tier for them can succeed: mod_callcenter refuses a tier for an agent it
// does not know, so staffing somebody who was signed out fails at the time and
// nothing retries it. The agent then signs in, is registered, shows Available,
// sits in a queue and is offered nothing.
//
// Bidirectional on purpose. Adding what is missing fixes the case above;
// removing what is stale fixes its mirror, where a queue was unstaffed while
// the agent was signed out and the switch kept the tier — an agent receiving
// calls for a queue they were taken off is the worse of the two failures.
//
// Zero queues is a legitimate answer, not a fault: plenty of agents staff
// nothing. The invariant is desired against actual, not desired above zero.
func (s *Service) ReconcileAgentTiers(ctx context.Context, agentID uuid.UUID) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	name, err := s.agents.CallcenterName(ctx, agentID)
	if err != nil {
		return // the agent is gone; its tiers go with it
	}

	desired, err := s.desiredTiers(ctx, agentID)
	if err != nil {
		slog.WarnContext(ctx, "could not read desired staffing",
			"agent", name, "error", err)
		return
	}
	actual, err := s.actualQueues(name)
	if err != nil {
		slog.WarnContext(ctx, "could not read the switch's staffing",
			"agent", name, "error", err)
		return
	}

	var added, removed, failed int
	for queue, tier := range desired {
		if _, ok := actual[queue]; ok {
			continue
		}
		if err := s.switchCtl.AddCallcenterTier(queue, name, tier.Level, tier.Position); err != nil {
			slog.WarnContext(ctx, "tier not added", "queue", queue, "agent", name, "error", err)
			failed++
			continue
		}
		added++
	}
	for queue := range actual {
		if _, ok := desired[queue]; ok {
			continue
		}
		if err := s.switchCtl.DeleteCallcenterTier(queue, name); err != nil {
			slog.WarnContext(ctx, "stale tier not removed", "queue", queue, "agent", name, "error", err)
			failed++
			continue
		}
		removed++
	}

	// Logged every time, zeros included. "This agent staffs nothing" and "this
	// agent staffs two queues the switch never heard about" are different
	// answers, and telling them apart afterwards must not require reproducing
	// the call that went nowhere.
	attrs := []any{"agent", name, "desired", len(desired), "actual", len(actual),
		"added", added, "removed", removed}
	if added+removed+failed > 0 {
		// The switch had drifted from this system. That is the condition this
		// exists to correct, so it is said at a level someone will see.
		slog.WarnContext(ctx, "agent staffing reconciled", append(attrs, "failed", failed)...)
		return
	}
	slog.InfoContext(ctx, "agent staffing already matched", attrs...)
}

// desiredTiers is what this system says the agent staffs, keyed by the queue's
// switch-side name. Assembled from the queries that already exist rather than
// a new one: sign-in is not a hot path and a handful of queues is a handful of
// reads.
func (s *Service) desiredTiers(ctx context.Context, agentID uuid.UUID) (map[string]QueueAgent, error) {
	queues, err := s.store.ListQueues(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]QueueAgent{}
	for _, queue := range queues {
		staffing, err := s.store.ListQueueAgents(ctx, queue.ID)
		if err != nil {
			return nil, err
		}
		for _, member := range staffing {
			if member.AgentID == agentID {
				out[queue.Name] = member
			}
		}
	}
	return out, nil
}

// actualQueues is what the switch holds for this agent.
func (s *Service) actualQueues(name string) (map[string]struct{}, error) {
	queues, err := s.switchCtl.CallcenterQueuesForAgent(name)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for _, q := range queues {
		// The switch reports a queue qualified by its domain while the commands
		// that change one take it bare, so both sides of this comparison are
		// held in the bare form the database uses.
		out[bareQueue(q)] = struct{}{}
	}
	return out, nil
}

// bareQueue strips the domain the switch qualifies queue names with. A queue
// name cannot contain an @, so the first one is always the separator.
func bareQueue(name string) string {
	if at := strings.IndexByte(name, '@'); at >= 0 {
		return name[:at]
	}
	return name
}

// SyncTiers re-applies every queue's staffing to the switch.
//
// mod_callcenter holds agents and tiers as runtime state, so a switch restart
// forgets both. Agent presence is already rebuilt on reconnect, but a tier is
// what actually makes an agent eligible for a queue's calls: without one the
// queue has no one to offer to, and callers wait out max_wait_time and abandon
// with the queue reporting calls_answered=0. That failure is silent from the
// application's side — our database still says the agent staffs the queue —
// which is why this runs on every reconnect rather than only when someone
// notices.
func (s *Service) SyncTiers(ctx context.Context) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	queues, err := s.store.ListQueues(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read queues to restore tiers", "error", err)
		return
	}

	restored := 0
	for _, queue := range queues {
		staffing, err := s.store.ListQueueAgents(ctx, queue.ID)
		if err != nil {
			slog.WarnContext(ctx, "could not read queue staffing", "queue", queue.Name, "error", err)
			continue
		}
		for _, member := range staffing {
			name, err := s.agents.CallcenterName(ctx, member.AgentID)
			if err != nil {
				continue // the agent is gone; its tier goes with it
			}
			if err := s.switchCtl.AddCallcenterTier(queue.Name, name, member.Level, member.Position); err != nil {
				slog.WarnContext(ctx, "tier not restored on the switch",
					"queue", queue.Name, "agent", name, "error", err)
				continue
			}
			restored++
		}
	}
	slog.InfoContext(ctx, "queue tiers mirrored to the switch", "tiers", restored)
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
