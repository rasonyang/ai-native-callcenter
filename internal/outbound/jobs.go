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
// assembled from its events is the record and a failed job result is only
// logged. If it never did, this is the only record there will be, and it
// writes the CDR itself.
//
// A successful result has a second job: the originate owns its leg until it
// writes +OK, so that is the moment the leg's armed continuation may run (the
// answer alone is not enough, #333). Every +OK ends in settleJob, or in
// releaseLeg for a leg whose job is no longer tracked.

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
	// isUnconfirmed: the originate timed out, so the Job-UUID is unknown and
	// the placement is tracked by its leg alone (unconfirmed.go).
	isUnconfirmed bool
	// hasReturned is set once the originate has returned and its outcome
	// was taken in hand (attachJob or holdUnconfirmed). Until then a result
	// matched by the leg is kept in result, and applied by whichever of the
	// two runs: it cannot be applied before, because the caller is about to
	// arm the answer or hold the placement, and would undo it.
	hasReturned bool
	result      *jobResult
	timer       stopper
}

// jobResult is a BACKGROUND_JOB result kept for a placement whose originate
// has not returned yet.
type jobResult struct {
	jobID string
	isOK  bool
	cause string
}

// stopper is a timer that can be stopped: a *time.Timer, or a test's own.
type stopper interface{ Stop() bool }

// afterFunc starts the backstop timers of placements. Tests replace it
// (Service.afterFunc) to fire a window at a moment of their choosing.
func (s *Service) afterFunc(d time.Duration, f func()) stopper {
	if s.timers != nil {
		return s.timers(d, f)
	}
	return time.AfterFunc(d, f)
}

type earlyResult struct {
	cause string
	at    time.Time
}

// expectLeg registers a placement before its originate is sent. The leg id is
// minted by us, so it is known long before the bgapi reply names the job; the
// switch's CHANNEL_CREATE for it can be dispatched before that reply is, and
// must find the placement waiting or a call whose channel did exist would be
// taken for one that never did.
func (s *Service) expectLeg(job *trackedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]*trackedJob{}
		s.legJobs = map[string]*trackedJob{}
	}
	s.legJobs[job.legID] = job
}

// abandonLeg undoes expectLeg when the originate was never sent, and gives the
// callId back.
func (s *Service) abandonLeg(job *trackedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetJobLocked(job)
	delete(s.placing, job.callID)
}

// attachJob gives an expected leg the Job-UUID bgapi returned. A result that
// was already dispatched is applied at once: one matched by the leg and kept
// on the job (handleJob), or a failure kept by its Job-UUID.
func (s *Service) attachJob(job *trackedJob, jobID string) {
	s.mu.Lock()
	job.hasReturned = true
	if res := job.result; res != nil {
		job.result = nil
		hadChannel := job.hadChannel
		job.jobID = res.jobID
		s.forgetJobLocked(job)
		s.mu.Unlock()
		s.log.Info("originate job finished before its reply was handled",
			"callId", job.callID, "jobId", res.jobID, "isOk", res.isOK, "cause", res.cause)
		s.settleJob(job, hadChannel, res.isOK, res.cause)
		return
	}
	if jobID == "" {
		// Nothing to correlate with; the CDR alone will tell.
		s.forgetJobLocked(job)
		delete(s.placing, job.callID)
		s.mu.Unlock()
		return
	}
	job.jobID = jobID
	if early, ok := s.early[jobID]; ok {
		delete(s.early, jobID)
		hadChannel := job.hadChannel
		s.forgetJobLocked(job)
		s.mu.Unlock()
		s.log.Info("originate job failed before it was tracked",
			"callId", job.callID, "jobId", jobID, "cause", early.cause)
		s.finishJob(job, hadChannel, early.cause)
		return
	}
	s.jobs[jobID] = job
	job.timer = s.afterFunc(s.cfg.AnswerWindow+s.margin(), func() { s.jobExpired(job) })
	s.mu.Unlock()
}

// JobCount reports how many originates still await their result.
func (s *Service) JobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// forgetJobLocked removes a job from both indexes.
//
// The claim on its callId (claimCall) is not given back here but where the
// placement's ending is settled: at once when the call exists or nothing was
// sent, and only after the placement-failure CDR is written when it never
// existed, so a retry in between still finds it placed.
func (s *Service) forgetJobLocked(job *trackedJob) {
	if job.jobID != "" && s.jobs[job.jobID] == job {
		delete(s.jobs, job.jobID)
	}
	if s.legJobs[job.legID] == job {
		delete(s.legJobs, job.legID)
	}
	if job.timer != nil {
		job.timer.Stop()
	}
}

// noteChannel records that the switch created a leg of ours.
//
// The leg of an unconfirmed placement is the call arriving after all: its
// answer window starts now, because the switch rings it from now and the
// window armed when the originate timed out may be mostly spent. The leg of a
// placement already given up on is ended at once (unconfirmed.go).
func (s *Service) noteChannel(channelID string) {
	s.mu.Lock()
	if job := s.legJobs[channelID]; job != nil {
		if job.isUnconfirmed && !job.hadChannel {
			if leg := s.pending[channelID]; leg != nil {
				leg.expiresAt = s.now().Add(s.cfg.AnswerWindow)
			}
			s.log.Info("unconfirmed placement made its channel; adopting it",
				"callId", job.callID, "channelId", channelID)
		}
		job.hadChannel = true
	}
	late, isLate := s.takeLateLegLocked(channelID)
	s.mu.Unlock()
	if isLate {
		s.log.Warn("the switch created a leg after its placement was recorded as failed; hanging it up",
			"callId", late.callID, "channelId", channelID, "cause", LateAnswerCause)
		go s.reap(channelID, LateAnswerCause)
	}
}

// handleJob consumes one BACKGROUND_JOB result.
//
// A placement that has no Job-UUID yet is recognised by the leg the command
// asked for (JobChannelID), registered by expectLeg before the originate was
// sent: an unconfirmed placement, which never learns its Job-UUID, and one
// whose originate has not returned yet, because the result can be dispatched
// before the bgapi reply is handled or before an originate that timed out is
// held (#302). The second kind keeps the result for attachJob or
// holdUnconfirmed to apply, so the switch's cause is never lost to a window.
func (s *Service) handleJob(ev telephony.SwitchEvent) {
	s.mu.Lock()
	job := s.jobs[ev.JobID]
	if job == nil && ev.JobChannelID != "" {
		if j := s.legJobs[ev.JobChannelID]; j != nil && j.jobID == "" {
			if !j.hasReturned {
				if ev.IsJobOK || ev.JobCause != "" {
					j.result = &jobResult{jobID: ev.JobID, isOK: ev.IsJobOK, cause: ev.JobCause}
				}
				s.mu.Unlock()
				return
			}
			j.jobID = ev.JobID
			job = j
		}
	}
	if job == nil {
		// Not ours, or not ours yet. Only a failure is worth keeping.
		if !ev.IsJobOK && ev.JobCause != "" {
			s.stashEarlyLocked(ev.JobID, ev.JobCause)
		}
		s.mu.Unlock()
		// A +OK for a leg still armed but whose job is forgotten releases it:
		// an unconfirmed placement adopted by its window, or a ringing leg
		// re-armed after a restart whose originate the previous process sent
		// (the switch tells every listener).
		if ev.IsJobOK && ev.JobChannelID != "" {
			s.releaseLeg(ev.JobChannelID)
		}
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
	s.settleJob(job, hadChannel, ev.IsJobOK, ev.JobCause)
}

// settleJob acts on a job's result once the job has left the maps, which is
// what makes it run once.
func (s *Service) settleJob(job *trackedJob, hadChannel, isOK bool, cause string) {
	if isOK {
		s.unclaim(job.callID)
		s.log.Debug("originate job ok", "callId", job.callID, "jobId", job.jobID)
		s.releaseLeg(job.legID)
		return
	}
	s.finishJob(job, hadChannel, cause)
}

// finishJob acts on a failed job that has already left the map, so it runs
// once.
func (s *Service) finishJob(job *trackedJob, hadChannel bool, cause string) {
	if hadChannel || (s.isLive != nil && s.isLive(job.callID)) {
		// The CDR is the single source for how a call that existed ended.
		// No +OK will release the leg, and a transfer onto a dying leg helps
		// nobody, so what is armed for it goes.
		s.mu.Lock()
		delete(s.pending, job.legID)
		s.mu.Unlock()
		s.unclaim(job.callID)
		s.log.Info("originate job failed after the channel existed; the cdr is the record",
			"callId", job.callID, "jobId", job.jobID, "cause", cause)
		return
	}
	s.log.Warn("originate failed before any channel existed",
		"callId", job.callID, "jobId", job.jobID, "cause", cause)
	s.failPlacement(job, cause)
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
	// The claim goes last, after the line: a retry with this callId may claim
	// it the moment it is free, and must not have its line released under it.
	defer s.unclaim(job.callID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if done, err := s.hasCDR(ctx, job.callID); err == nil && done {
		return
	}
	if s.isLive != nil && s.isLive(job.callID) {
		return
	}
	s.lines.Release(job.callID)
	s.log.Warn("originate job never reported and the call has no cdr",
		"callId", job.callID, "jobId", job.jobID, "placedAt", job.placedAt)
}

// failPlacement finishes a call whose originate never made a channel: its
// armed continuation and business data go, and a failed CDR carries the
// cause. The caller has already removed the job from the map, which is what
// makes this run once. The callId is given back once the CDR is written, so
// a retry never finds the call neither placing nor recorded.
func (s *Service) failPlacement(job *trackedJob, cause string) {
	// No channel was ever made, so no call will end to give the line back.
	s.lines.Release(job.callID)
	s.mu.Lock()
	delete(s.pending, job.legID)
	s.mu.Unlock()

	var userData map[string]any
	if s.callData != nil {
		userData = s.callData.CallData(job.callID)
		s.callData.Drop(job.callID)
	}
	if s.failures == nil {
		s.unclaim(job.callID)
		return
	}

	cdr := placementFailureCDR(job, cause, userData, time.Now().UTC())

	// Off the dispatch loop: it must not wait on a database.
	go func() {
		defer s.unclaim(job.callID)
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

// placementFailureCDR is the ledger row for a call whose originate never made
// a channel. Tech.isPlacementFailure marks it as the row that knows least: the
// store never lets it replace another row, and any other row replaces it.
func placementFailureCDR(job *trackedJob, cause string, userData map[string]any, now time.Time) store.CDR {
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
			"isPlacementFailure":  true,
			"originateJobId":      job.jobID,
			"originatedChannelId": job.legID,
		},
		Legs: []store.Leg{{Kind: job.legKind, Label: job.legLabel, Note: cause}},
	}
	cdr.TotalSec = max(0, int(now.Sub(job.placedAt).Seconds()))
	return cdr
}
