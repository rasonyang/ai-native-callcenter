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
	ExtensionPassword(ctx context.Context, id uuid.UUID) (string, error)
	AllocateExtension(ctx context.Context, e Extension, rangeLow, rangeHigh int) (Extension, error)
	DeleteExtension(ctx context.Context, id uuid.UUID) error

	ListQueues(ctx context.Context) ([]Queue, error)
	CreateQueue(ctx context.Context, q Queue, rangeLow, rangeHigh int) (Queue, error)
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
	// CallcenterTiers reports what the switch currently believes, as agent name
	// to the queues they staff, named as this system names them. Staffing is
	// reconciled rather than applied, and a tier the switch still holds cannot
	// be discovered any other way.
	//
	// Whole picture rather than one agent's: reconciling everything needs the
	// agents the switch knows about that this system does not, and those cannot
	// be asked for by name.
	//
	// Names only: level and position are always the database's answer, never
	// the switch's, so there is nothing to learn from the switch's copy of them
	// and no shared type either side has to know about.
	CallcenterTiers() (map[string][]string, error)
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
	// ErrPoolExhausted means every number in the configured range is taken.
	// Distinct from a conflict: nothing the operator asked for collided, the
	// deployment has simply run out of numbers and needs a wider range.
	ErrPoolExhausted = errors.New("no free extension number in the configured range")
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

// AllocateExtension creates a phone on the lowest free number in the pool.
//
// This is how an account gets a phone without anybody choosing a number: an
// operator creating an agent is not deciding on 1042, they are asking for a
// desk. The caller supplies the range because it is deployment configuration
// (AICC_EXTENSION_RANGE), and configuration does not belong to this service.
//
// Everything checkable is checked before the store opens its transaction —
// holding the pool lock while discovering that a password is six characters
// short would make one bad request everybody else's problem.
func (s *Service) AllocateExtension(ctx context.Context, e Extension,
	rangeLow, rangeHigh int) (Extension, error) {
	if rangeHigh < rangeLow {
		return Extension{}, fmt.Errorf("%w: extension range ends before it starts", ErrValidation)
	}
	if err := e.validateApartFromNumber(true); err != nil {
		return Extension{}, err
	}
	e.ID = uuid.Must(uuid.NewV7())
	return s.store.AllocateExtension(ctx, e, rangeLow, rangeHigh)
}

// ExtensionPassword reads a phone's SIP credential in clear.
//
// Deliberately not part of Extension: a credential should have to be asked for
// by name, so it cannot ride along in a list, a snapshot or a form that only
// meant to show a number.
func (s *Service) ExtensionPassword(ctx context.Context, id uuid.UUID) (string, error) {
	return s.store.ExtensionPassword(ctx, id)
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

// QueueByName resolves a queue name to its configuration: the switch speaks
// in names, everything above it in ids, display names and targets.
func (s *Service) QueueByName(ctx context.Context, name string) (Queue, bool) {
	queues, err := s.store.ListQueues(ctx)
	if err != nil {
		return Queue{}, false
	}
	for _, q := range queues {
		if q.Name == name {
			return q, true
		}
	}
	return Queue{}, false
}

// QueueIDByName resolves a queue name to its identity, for the ledger: the
// switch speaks in names, the ledger in ids.
func (s *Service) QueueIDByName(ctx context.Context, name string) (uuid.UUID, bool) {
	q, ok := s.QueueByName(ctx, name)
	if !ok {
		return uuid.Nil, false
	}
	return q.ID, true
}

// CreateQueue adds a queue and tells the switch to read it.
// CreateQueue adds a queue, allocating its number when none was named.
//
// The caller supplies the pool because it is deployment configuration
// (AICC_QUEUE_RANGE), and configuration is not this service's to know.
func (s *Service) CreateQueue(ctx context.Context, q Queue, rangeLow, rangeHigh int) (Queue, error) {
	if err := q.validate(); err != nil {
		return Queue{}, err
	}
	q.ID = uuid.Must(uuid.NewV7())
	created, err := s.store.CreateQueue(ctx, q, rangeLow, rangeHigh)
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
		slog.WarnContext(ctx, "could not read desired staffing", "agent", name, "error", err)
		return
	}
	onSwitch, err := s.switchCtl.CallcenterTiers()
	if err != nil {
		slog.WarnContext(ctx, "could not read the switch's staffing", "agent", name, "error", err)
		return
	}
	s.converge(ctx, name, desired, setOf(onSwitch[name]))
}

// converge makes the switch agree with desired for one agent, and reports what
// it had to change.
//
// Zero queues is a legitimate answer, not a fault: plenty of agents staff
// nothing. The invariant is desired against actual, not desired above zero.
func (s *Service) converge(ctx context.Context, name string,
	desired map[string]QueueAgent, actual map[string]struct{}) reconcileResult {

	var attemptedAdd, attemptedRemove, failed, deferred int
	for queue, tier := range desired {
		if _, ok := actual[queue]; ok {
			continue
		}
		err := s.switchCtl.AddCallcenterTier(queue, name, tier.Level, tier.Position)
		switch {
		case err == nil:
			attemptedAdd++
		case isDeferred(err):
			// The switch does not know this agent yet, which is the ordinary
			// state of anyone staffed while signed out. Their sign-in applies
			// it; counting this as a failure would report drift on every
			// reconnect that nothing could have fixed.
			deferred++
		default:
			slog.WarnContext(ctx, "tier not added", "queue", queue, "agent", name, "error", err)
			failed++
		}
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
		attemptedRemove++
	}

	// What the switch accepted is not what the switch did. `tier del` answers
	// +OK for a tier that was never there — verbatim, from a live probe:
	//
	//     callcenter_config tier del does-not-exist agent-wei  → +OK
	//     callcenter_config tier del support-en agent-nobody   → +OK
	//
	// so counting a nil error as a removal made `removed=` a claim nothing
	// stood behind (C1). It said 1 for a tier that had not existed since the
	// machine changed address, which is a report of work done where none was.
	//
	// So ask the switch again and count the difference. Only when something
	// was attempted: the ordinary case is that nothing changed, and that case
	// still costs one read of the switch's staffing, not two.
	added, removed := attemptedAdd, attemptedRemove
	verified := true
	if attemptedAdd+attemptedRemove > 0 {
		added, removed, verified = s.recount(ctx, name, actual, attemptedAdd, attemptedRemove)
	}

	// Logged every time, zeros included. "This agent staffs nothing" and "this
	// agent staffs two queues the switch never heard about" are different
	// answers, and telling them apart afterwards must not require reproducing
	// the call that went nowhere.
	attrs := []any{"agent", name, "desired", len(desired), "actual", len(actual),
		"added", added, "removed", removed}
	if !verified {
		attrs = append(attrs, "isVerified", false)
	}
	if deferred > 0 {
		attrs = append(attrs, "deferredUntilSignIn", deferred)
	}
	if attemptedAdd+attemptedRemove+failed > 0 {
		// The switch had drifted from this system. That is the condition this
		// exists to correct, so it is said at a level someone will see.
		slog.WarnContext(ctx, "agent staffing reconciled", append(attrs, "failed", failed)...)
		return reconcileResult{added, removed, failed, deferred, verified}
	}
	slog.InfoContext(ctx, "agent staffing already matched", attrs...)
	return reconcileResult{added, removed, failed, deferred, verified}
}

// recount asks the switch what its staffing is now and reports how it actually
// moved, so `added`/`removed` describe the world rather than the commands we
// sent it.
//
// A failed re-read is reported as unverified rather than swallowed: falling
// back to the attempted counts silently would put us back where we started,
// with a number nothing stands behind. The counts are still the best available
// answer, so they are returned — labelled.
func (s *Service) recount(ctx context.Context, name string, before map[string]struct{},
	attemptedAdd, attemptedRemove int) (added, removed int, verified bool) {

	onSwitch, err := s.switchCtl.CallcenterTiers()
	if err != nil {
		slog.WarnContext(ctx, "could not verify what the switch did with the tiers",
			"agent", name, "error", err)
		return attemptedAdd, attemptedRemove, false
	}
	after := setOf(onSwitch[name])
	for queue := range after {
		if _, was := before[queue]; !was {
			added++
		}
	}
	for queue := range before {
		if _, still := after[queue]; !still {
			removed++
		}
	}
	return added, removed, true
}

// reconcileResult is what one agent's convergence did.
//
// Returned as well as logged because the difference between "deferred" and
// "failed" is the whole point of telling them apart, and an outcome that exists
// only in a log line is one no test can hold to account.
//
// added/removed are counted from the switch's own staffing before and after,
// not from the commands it accepted — see recount. isVerified is false when
// that second read failed, which is the one case the numbers are a claim
// rather than an observation.
type reconcileResult struct {
	added, removed, failed, deferred int
	isVerified                       bool
}

// isDeferred reports an error the switch will stop returning once the agent
// signs in. Recognised by behaviour so this package need not import the one
// that speaks to the switch.
func isDeferred(err error) bool {
	var d interface{ AgentNotOnSwitch() bool }
	return errors.As(err, &d) && d.AgentNotOnSwitch()
}

func setOf(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, i := range items {
		out[i] = struct{}{}
	}
	return out
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

// SyncTiers reconciles every agent's staffing after the switch reconnects.
//
// mod_callcenter holds agents and tiers as runtime state, so a restart forgets
// both. Presence is already rebuilt on reconnect; a tier is what actually makes
// an agent eligible for a queue's calls, and without one the queue has nobody
// to offer to while our database still insists the agent staffs it.
//
// It is the same convergence sign-in performs, over every agent either side
// knows about — including agents the switch holds tiers for and this system
// does not, which is the only place those can be found. Before this it added
// and never removed, so it could not correct that half at all.
func (s *Service) SyncTiers(ctx context.Context) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	desired, err := s.desiredByAgent(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read staffing to reconcile", "error", err)
		return
	}
	onSwitch, err := s.switchCtl.CallcenterTiers()
	if err != nil {
		slog.WarnContext(ctx, "could not read the switch's staffing", "error", err)
		return
	}

	// The union: an agent this system staffs, an agent only the switch still
	// believes in, or both.
	names := map[string]struct{}{}
	for name := range desired {
		names[name] = struct{}{}
	}
	for name := range onSwitch {
		names[name] = struct{}{}
	}
	for name := range names {
		s.converge(ctx, name, desired[name], setOf(onSwitch[name]))
	}
	slog.InfoContext(ctx, "queue staffing reconciled", "agents", len(names))
}

// desiredByAgent is what this system says everyone staffs, keyed by the agent's
// switch-side name.
func (s *Service) desiredByAgent(ctx context.Context) (map[string]map[string]QueueAgent, error) {
	queues, err := s.store.ListQueues(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]QueueAgent{}
	for _, queue := range queues {
		staffing, err := s.store.ListQueueAgents(ctx, queue.ID)
		if err != nil {
			return nil, err
		}
		for _, member := range staffing {
			name, err := s.agents.CallcenterName(ctx, member.AgentID)
			if err != nil {
				continue // the agent is gone; its tiers go with it
			}
			if out[name] == nil {
				out[name] = map[string]QueueAgent{}
			}
			out[name][queue.Name] = member
		}
	}
	return out, nil
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
