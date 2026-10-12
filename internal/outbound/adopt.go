// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// What a restart, or an ESL link that dropped and came back, does to calls in
// flight.
//
// Everything this package knows about a leg waiting for its answer is in
// memory, and the bot is in-process. The switch outlives both, so on every
// connect the channels it holds are read (telephony.AdoptLiveChannels) and
// each one of ours is given the only ending that is true to it:
//
//   - still ringing, placed by us, window not spent: arm again, for what is left
//     of the window counted from the channel's creation, not from now;
//   - still ringing, window spent: hang it up;
//   - answered while nobody was watching and still parked: hang it up, because
//     the person has been talking into silence and a bot arriving late is worse
//     than a clean goodbye;
//   - bridged to the bot gateway with no session behind it: hang it up, and the
//     bot's leg with it;
//   - anything else: not ours to decide, left alone.
//
// Where something needed to re-arm cannot be recovered from the channel and
// the DID store, the leg is hung up rather than guessed at.

// ReconcileCause is the hangup cause on a leg that recovery ends because the
// application lost track of it: answered into silence, or bridged to a bot
// that no longer exists. It reaches the CDR's hangupCause (a free string), so
// the ledger tells these endings from a person hanging up. A leg whose window
// ran out while the application was away is ended with LateAnswerCause, the
// same word as for an answer that arrives too late.
const ReconcileCause = "RECOVERY_ON_TIMER_EXPIRE"

const (
	// reconcileWait is how long a request waits for the first reconcile before
	// being refused. Long enough to cover a normal start; short enough that an
	// HTTP client gets an answer rather than a hang.
	reconcileWait = 10 * time.Second

	// botSessionGrace is how long a bot leg may exist before the absence of a
	// session behind it means anything: the session is opened when the bot
	// leg's INVITE arrives, a moment before the switch bridges to it.
	botSessionGrace = 5 * time.Second

	// recentFor is how long a decision about a channel is remembered so that a
	// second pass does not make it again.
	recentFor = 30 * time.Second
)

// ErrNotReady means the first reconciliation with the switch has not finished,
// so the service does not yet know which calls are already in flight and will
// not start another.
var ErrNotReady = errors.New("outbound: the switch is still being reconciled")

// HoldUntilReconciled closes the admission gate: Dial and DialAI wait for
// MarkReconciled. Called once, before the service is used.
//
// Only the first reconcile gates. A reconnect keeps the process's memory, so
// the legs it placed are still known and are recognised by reconcile (armed or
// still being placed), and refusing calls for the length of a reconnect would
// turn a blip in the link into an outage of the API. The switch commands of a
// dial fail on their own while the link is down.
func (s *Service) HoldUntilReconciled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
}

// MarkReconciled opens the gate. Idempotent.
func (s *Service) MarkReconciled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil || s.isReady {
		return
	}
	s.isReady = true
	close(s.ready)
}

// awaitReconciled blocks until the gate is open, the request is cancelled, or
// the wait is spent.
func (s *Service) awaitReconciled(ctx context.Context) error {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready == nil {
		return nil
	}
	select {
	case <-ready:
		return nil
	default:
	}
	wait := s.readyWait
	if wait <= 0 {
		wait = reconcileWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrNotReady
	}
}

// RecoveryReport counts what Recover did, by class.
type RecoveryReport struct {
	// Held: legs this process already holds; left exactly as they are.
	Held int
	// Rearmed: ringing legs armed again.
	Rearmed int
	// WindowSpent: ringing legs whose window ran out while we were away.
	WindowSpent int
	// AnsweredParked: legs answered into silence.
	AnsweredParked int
	// OrphanedBot: customer legs bridged to a bot with no session.
	OrphanedBot int
	// Unrecoverable: legs of ours that could not be re-armed.
	Unrecoverable int
	// Left: channels that were not for recovery to decide about.
	Left int
}

// Recover applies the classes above to the channels the switch holds.
// hasBotSession says whether an in-process bot conversation exists for a call.
//
// It is idempotent: a leg it armed is held on the next pass, a leg it ended is
// remembered for a while in case the switch still lists it, and a channel the
// live process already tracks is never touched.
func (s *Service) Recover(ctx context.Context, live []telephony.LiveChannel,
	hasBotSession func(uuid.UUID) bool) RecoveryReport {

	var rep RecoveryReport
	for _, ch := range live {
		if ctx.Err() != nil {
			return rep
		}
		if ch.IsBotLeg {
			rep.Left++ // ended with its customer leg, if it comes to that
			continue
		}

		// A customer leg on a bot with nobody behind it.
		if ch.PeerChannelID != "" && ch.PeerIsBotLeg {
			if hasBotSession != nil && hasBotSession(ch.CallID) {
				rep.Left++
				continue
			}
			if s.now().Sub(ch.PeerCreatedAt) < botSessionGrace {
				rep.Held++
				continue
			}
			s.log.Warn("recovery: customer leg is bridged to a bot that no longer exists; hanging up",
				"channelId", ch.ChannelID, "callId", ch.CallID, "botChannelId", ch.PeerChannelID)
			s.reap(ch.ChannelID, ReconcileCause)
			s.reap(ch.PeerChannelID, ReconcileCause)
			rep.OrphanedBot++
			continue
		}
		if ch.PeerChannelID != "" {
			rep.Left++ // bridged to a person or another leg of ours
			continue
		}

		if !ch.IsAnswered {
			s.recoverRinging(ctx, ch, &rep)
			continue
		}

		// Answered and not bridged. Only a leg we originated waits in park.
		if ch.IsParked && !ch.IsInbound {
			if s.wasJustAnswered(ch.ChannelID) {
				rep.Held++
				continue
			}
			s.dropPending(ch.ChannelID)
			s.log.Warn("recovery: leg was answered while nobody was watching and is parked; hanging up",
				"channelId", ch.ChannelID, "callId", ch.CallID)
			s.reap(ch.ChannelID, ReconcileCause)
			rep.AnsweredParked++
			continue
		}
		rep.Left++
	}
	return rep
}

func (s *Service) recoverRinging(ctx context.Context, ch telephony.LiveChannel, rep *RecoveryReport) {
	if ch.IsInbound || ch.PlacedBy == "" {
		rep.Left++ // not an originate of ours, or one an older build did not stamp
		return
	}
	if s.holdsLeg(ch.ChannelID) {
		rep.Held++
		return
	}
	if ch.CreatedAt.IsZero() {
		s.log.Warn("recovery: ringing leg has no creation time, so its window cannot be measured; hanging up",
			"channelId", ch.ChannelID, "callId", ch.CallID)
		s.reap(ch.ChannelID, ReconcileCause)
		rep.Unrecoverable++
		return
	}
	expiresAt := ch.CreatedAt.Add(s.cfg.AnswerWindow)
	if !s.now().Before(expiresAt) {
		s.log.Warn("recovery: ringing leg outlived its answer window; hanging up",
			"channelId", ch.ChannelID, "callId", ch.CallID, "cause", LateAnswerCause)
		s.reap(ch.ChannelID, LateAnswerCause)
		rep.WindowSpent++
		return
	}

	var onAnswer func()
	switch ch.PlacedBy {
	case telephony.PlacedByDialAI:
		did, err := s.findDID(ctx, ch.DID)
		if err != nil || ch.PlacedTo == "" || !isDialable(ch.PlacedTo) {
			s.log.Warn("recovery: cannot rebuild the bridge to the bot for a ringing leg; hanging up",
				"channelId", ch.ChannelID, "callId", ch.CallID, "did", ch.DID, "error", err)
			s.reap(ch.ChannelID, ReconcileCause)
			rep.Unrecoverable++
			return
		}
		language := ch.Language
		if language == "" {
			language = did.Language
		}
		onAnswer = s.bridgeToBot(ch.CallID, ch.ChannelID, did.Number, language, ch.PlacedTo)
	case telephony.PlacedByDial:
		if !isDialable(ch.Destination) {
			s.log.Warn("recovery: click-to-dial leg has no destination to go on to; hanging up",
				"channelId", ch.ChannelID, "callId", ch.CallID)
			s.reap(ch.ChannelID, ReconcileCause)
			rep.Unrecoverable++
			return
		}
		onAnswer = s.transferOnAnswer(ch.CallID, ch.ChannelID, ch.Destination)
	default:
		rep.Left++
		return
	}

	s.armUntil(ch.ChannelID, expiresAt, onAnswer)
	s.log.Info("recovery: ringing leg re-armed for what is left of its answer window",
		"channelId", ch.ChannelID, "callId", ch.CallID, "placedBy", ch.PlacedBy,
		"remainingMs", expiresAt.Sub(s.now()).Milliseconds())
	rep.Rearmed++
}

// holdsLeg reports whether this process is already waiting on the channel or
// is in the middle of placing it.
func (s *Service) holdsLeg(channelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[channelID] != nil || s.legJobs[channelID] != nil
}

func (s *Service) dropPending(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, channelID)
}

// wasJustAnswered reports an answer this process fired a continuation for a
// moment ago: the leg is parked only because the bridge command is on its way.
func (s *Service) wasJustAnswered(channelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.answered[channelID]
	return ok && s.now().Sub(at) < recentFor
}

func (s *Service) noteAnswered(channelID string) {
	if s.answered == nil {
		s.answered = map[string]time.Time{}
	}
	now := s.now()
	for id, at := range s.answered {
		if now.Sub(at) >= recentFor {
			delete(s.answered, id)
		}
	}
	s.answered[channelID] = now
}

// reap hangs a channel up once: a second ask inside the memory window is a
// pass seeing a channel the switch has not finished clearing.
func (s *Service) reap(channelID, cause string) {
	s.mu.Lock()
	now := s.now()
	if at, ok := s.reaped[channelID]; ok && now.Sub(at) < recentFor {
		s.mu.Unlock()
		return
	}
	if s.reaped == nil {
		s.reaped = map[string]time.Time{}
	}
	for id, at := range s.reaped {
		if now.Sub(at) >= recentFor {
			delete(s.reaped, id)
		}
	}
	s.reaped[channelID] = now
	s.mu.Unlock()

	if err := s.sw.Hangup(channelID, cause); err != nil {
		s.log.Error("recovery: hangup failed", "channelId", channelID, "cause", cause, "error", err)
	}
}
