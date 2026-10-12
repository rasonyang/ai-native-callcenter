// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// An originate is a bgapi command, and a command that fails before the switch
// has made a channel leaves nothing behind: no channel, no hangup, no CDR. The
// only account of it is the BACKGROUND_JOB event carrying the job's result.
// The service keeps the Job-UUID of every originate it issues so that result
// can be matched to the call it belongs to.
//
// One source finishes a call. If the switch created the channel, the CDR
// assembled from its events is the record and the job result is only logged.
// If it never did, this is the only record there will be, and it writes the
// CDR itself.

const (
	// jobMargin is how long past the answer window a job may stay silent
	// before it is given up on: events were lost, an ESL disconnect say.
	// Reconciliation of such a call is a separate piece of work; here it is
	// logged and forgotten.
	jobMargin = 30 * time.Second

	// A result can be dispatched before the bgapi reply that names its job
	// has been handled, because they travel on different goroutines. An
	// unclaimed failure is kept for this long, up to this many, so the
	// originate that follows can still claim it.
	earlyResultTTL = 10 * time.Second
	earlyResultMax = 256
)

// FailureLedger is where a call that never made a channel is recorded. It is
// the same single write path every other CDR goes through.
type FailureLedger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
}

// SetFailureLedger says where originate failures are written. Without one the
// failure is logged and the call is still dropped cleanly. Set before the
// service is used.
func (s *Service) SetFailureLedger(l FailureLedger) { s.failures = l }

// trackedJob is one originate awaiting its result.
type trackedJob struct {
	jobID    string
	callID   uuid.UUID
	legID    string
	placedAt time.Time

	// What the CDR of a failed placement says about the call.
	callType string
	from, to string
	did      string
	language string
	flowID   *uuid.UUID
	legKind  string
	legLabel string

	// hadChannel is set when the switch created the leg.
	hadChannel bool
	timer      *time.Timer
}

type earlyResult struct {
	cause string
	at    time.Time
}

// trackJob remembers an originate's Job-UUID. A failure that was already
// dispatched is applied at once.
func (s *Service) trackJob(job *trackedJob) {
	if job.jobID == "" {
		return
	}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*trackedJob{}
		s.legJobs = map[string]string{}
	}
	if early, ok := s.early[job.jobID]; ok {
		delete(s.early, job.jobID)
		s.mu.Unlock()
		s.log.Info("originate job failed before it was tracked",
			"callId", job.callID, "jobId", job.jobID, "cause", early.cause)
		s.failPlacement(job, early.cause)
		return
	}
	s.jobs[job.jobID] = job
	s.legJobs[job.legID] = job.jobID
	margin := s.jobMargin
	if margin <= 0 {
		margin = jobMargin
	}
	job.timer = time.AfterFunc(s.cfg.AnswerWindow+margin, func() { s.jobExpired(job) })
	s.mu.Unlock()
}

// JobCount reports how many originates still await their result.
func (s *Service) JobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// forgetJobLocked removes a job from both indexes.
func (s *Service) forgetJobLocked(job *trackedJob) {
	delete(s.jobs, job.jobID)
	if s.legJobs[job.legID] == job.jobID {
		delete(s.legJobs, job.legID)
	}
	if job.timer != nil {
		job.timer.Stop()
	}
}

// noteChannel records that the switch created a leg of ours.
func (s *Service) noteChannel(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.legJobs[channelID]; ok {
		if job := s.jobs[id]; job != nil {
			job.hadChannel = true
		}
	}
}

// handleJob consumes one BACKGROUND_JOB result.
func (s *Service) handleJob(ev telephony.SwitchEvent) {
	s.mu.Lock()
	job := s.jobs[ev.JobID]
	if job == nil {
		// Not ours, or not ours yet. Only a failure is worth keeping.
		if !ev.IsJobOK && ev.JobCause != "" {
			s.stashEarlyLocked(ev.JobID, ev.JobCause)
		}
		s.mu.Unlock()
		return
	}
	if !ev.IsJobOK && ev.JobCause == "" {
		// An empty or unreadable body says nothing. The job stays tracked so
		// the backstop still covers it.
		s.mu.Unlock()
		s.log.Warn("originate job result unreadable",
			"callId", job.callID, "jobId", ev.JobID)
		return
	}
	s.forgetJobLocked(job)
	hadChannel := job.hadChannel
	s.mu.Unlock()

	switch {
	case ev.IsJobOK:
		s.log.Debug("originate job ok", "callId", job.callID, "jobId", ev.JobID)
	case hadChannel || (s.isLive != nil && s.isLive(job.callID)):
		// The CDR is the single source for how a call that existed ended.
		s.log.Info("originate job failed after the channel existed; the cdr is the record",
			"callId", job.callID, "jobId", ev.JobID, "cause", ev.JobCause)
	default:
		s.log.Warn("originate failed before any channel existed",
			"callId", job.callID, "jobId", ev.JobID, "cause", ev.JobCause)
		s.failPlacement(job, ev.JobCause)
	}
}

func (s *Service) stashEarlyLocked(jobID, cause string) {
	now := time.Now()
	if s.early == nil {
		s.early = map[string]earlyResult{}
	}
	for id, e := range s.early {
		if now.Sub(e.at) > earlyResultTTL {
			delete(s.early, id)
		}
	}
	if len(s.early) >= earlyResultMax {
		return
	}
	s.early[jobID] = earlyResult{cause: cause, at: now}
}

// jobExpired is the backstop: no result and, it turns out, no CDR either.
func (s *Service) jobExpired(job *trackedJob) {
	s.mu.Lock()
	if s.jobs[job.jobID] != job {
		s.mu.Unlock()
		return
	}
	s.forgetJobLocked(job)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if done, err := s.hasCDR(ctx, job.callID); err == nil && done {
		return
	}
	if s.isLive != nil && s.isLive(job.callID) {
		return
	}
	s.log.Warn("originate job never reported and the call has no cdr",
		"callId", job.callID, "jobId", job.jobID, "placedAt", job.placedAt)
}

// failPlacement finishes a call whose originate never made a channel: its
// armed continuation and business data go, and a failed CDR carries the
// cause. The caller has already removed the job from the map, which is what
// makes this run once.
func (s *Service) failPlacement(job *trackedJob, cause string) {
	s.mu.Lock()
	delete(s.pending, job.legID)
	s.mu.Unlock()

	var userData map[string]any
	if s.callData != nil {
		userData = s.callData.CallData(job.callID)
		s.callData.Drop(job.callID)
	}
	if s.failures == nil {
		return
	}

	now := time.Now().UTC()
	cdr := store.CDR{
		CallID:      job.callID,
		StartedAt:   job.placedAt,
		EndedAt:     now,
		CallType:    job.callType,
		Language:    job.language,
		FromNumber:  job.from,
		ToNumber:    job.to,
		DID:         job.did,
		FlowID:      job.flowID,
		Status:      store.CDRStatusFailed,
		HangupCause: cause,
		UserData:    userData,
		Tech: map[string]any{
			"originateJobId":      job.jobID,
			"originatedChannelId": job.legID,
		},
		Legs: []store.Leg{{Kind: job.legKind, Label: job.legLabel, Note: cause}},
	}
	cdr.TotalSec = max(0, int(now.Sub(job.placedAt).Seconds()))

	// Off the dispatch loop: it must not wait on a database.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A ledger row that already exists saw more of the call than this
		// does; never write over it.
		if done, err := s.hasCDR(ctx, job.callID); err == nil && done {
			return
		}
		if err := s.failures.InsertCDR(ctx, cdr); err != nil {
			s.log.Error("could not write the cdr of a failed originate",
				"callId", job.callID, "error", err)
		}
	}()
}
