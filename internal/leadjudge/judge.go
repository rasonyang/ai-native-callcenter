// SPDX-License-Identifier: Apache-2.0

// Package leadjudge reads a finished campaign call and decides its lead
// (docs/design/10-campaigns.md section 11.1).
//
// The lead a campaign call records is the realtime model's hearing of one
// moment of audio. After a call the bot handled alone ends with a HIGH or
// MEDIUM lead, the CDR's transaction marks a judgement owed (PENDING, with a
// deadline) and holds the call's webhook deliveries; this package then makes
// one text-model request over the transcript and writes the verdict, which
// decides the lead and releases the deliveries. It never runs during a call.
//
// Every way a job can fail ends in NOT_JUDGED, the declared lead standing: the
// worker writes it when it gives up in time, and a sweep writes it for every
// judgement whose deadline passed with nothing written (a crash, a restart, a
// model slower than the deadline). The attempt rows are the queue, so a job
// lost with the process is found again by the scan at start and on a slow
// ticker.
//
// A nil *Service is a valid, inert one. A Service with no text model only
// sweeps: judgements left owed by an earlier run still reach NOT_JUDGED.
package leadjudge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// The job's constants. The deadline and the webhook hold are
// campaign.LeadJudgeDeadline and campaign.LeadJudgeHold, which the store
// writes.
const (
	// Settle is how long a job waits after the call's CDR is written before
	// it reads the transcript: the bot's last lines land after the CDR.
	Settle = 3 * time.Second
	// DefaultTimeout and DefaultBudgetChars apply when Config leaves them
	// zero; the deployment's are AICC_TEXT_LLM_TIMEOUT and
	// AICC_TEXT_LLM_CONTEXT_CHARS.
	DefaultTimeout     = 20 * time.Second
	DefaultBudgetChars = 24000
	maxConcurrent      = 8
	// scanEvery is the crash-safety tick: owed judgements nobody handed the
	// service are picked up, and those past their deadline settled.
	scanEvery = 5 * time.Second
	scanLimit = 100
	// writeMargin is kept free before the deadline for writing the verdict,
	// so an answer that arrives just in time is not lost to its own write.
	writeMargin = time.Second
	// retryFloor is the least time before the deadline a retry is worth.
	retryFloor = time.Second
	// writeTimeout bounds the write of a verdict, which runs on a context of
	// its own because the job's may be spent; the deadline is the database's
	// to judge.
	writeTimeout = 5 * time.Second
)

// Store is what a job reads and writes; *store.LeadJudgeStore satisfies it.
type Store interface {
	LeadJudgement(ctx context.Context, callID uuid.UUID) (store.LeadJudgement, error)
	ListTranscript(ctx context.Context, callID uuid.UUID) ([]store.TranscriptLine, error)
	ApplyLeadVerdict(ctx context.Context, callID uuid.UUID, verdict campaign.LeadVerdict, reason string) (campaign.LeadIntent, bool, error)
	ExpireLeadJudgements(ctx context.Context, limit int) ([]uuid.UUID, error)
	PendingLeadJudgements(ctx context.Context, limit int) ([]store.PendingLeadJudgement, error)
}

// Config tunes the job.
type Config struct {
	// Timeout bounds one job end to end (AICC_TEXT_LLM_TIMEOUT); the
	// judgement's deadline bounds it too, whichever comes first.
	Timeout time.Duration
	// BudgetChars is the most runes the prompt may total
	// (AICC_TEXT_LLM_CONTEXT_CHARS).
	BudgetChars int
}

// Service runs the judgements.
type Service struct {
	store Store
	llm   textllm.Client
	cfg   Config
	log   *slog.Logger

	// The clock and the intervals, replaced by tests.
	now       func() time.Time
	settle    time.Duration
	margin    time.Duration
	scanEvery time.Duration

	slots chan struct{}
	ctx   context.Context
	stop  context.CancelFunc
	loop  sync.WaitGroup
	jobs  sync.WaitGroup

	mu       sync.Mutex
	stopped  bool
	started  bool
	inflight map[uuid.UUID]struct{}
}

// New builds a service. llm nil means no text model: the service only
// settles deadlines. Nothing runs until Start, except jobs AttemptFinished
// hands it.
func New(st Store, llm textllm.Client, cfg Config, log *slog.Logger) *Service {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.BudgetChars <= 0 {
		cfg.BudgetChars = DefaultBudgetChars
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Service{
		store: st, llm: llm, cfg: cfg, log: log,
		now: time.Now, settle: Settle, margin: writeMargin, scanEvery: scanEvery,
		slots: make(chan struct{}, maxConcurrent), ctx: ctx, stop: stop,
		inflight: map[uuid.UUID]struct{}{},
	}
}

// IsJudging reports whether the service has a text model to judge with. The
// store marks judgements owed only when it does (store.Store.IsLeadJudgeEnabled).
func (s *Service) IsJudging() bool { return s != nil && s.llm != nil }

// Start sweeps once (settling expired judgements and picking up owed ones),
// then keeps sweeping on a slow ticker until Stop. Safe on a nil service.
func (s *Service) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped || s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.loop.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.loop.Done()
		s.sweep()
		t := time.NewTicker(s.scanEvery)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				s.sweep()
			}
		}
	}()
}

// Stop ends the ticker and every job, and waits for them. A judgement cut
// short stays PENDING: the next start picks it up, or settles it as
// NOT_JUDGED once its deadline has passed. Safe on a nil service.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stop()
	s.loop.Wait()
	s.jobs.Wait()
}

// Wait blocks until every started job has finished. For tests.
func (s *Service) Wait() { s.jobs.Wait() }

// AttemptFinished is the store's attempt hook: an attempt whose judgement the
// finish just made owed starts its job, reading the transcript Settle from now.
// It returns at once. Safe on a nil service.
func (s *Service) AttemptFinished(f campaign.Finished) {
	if s == nil || !f.IsLeadJudgementOwed {
		return
	}
	s.judge(f.CallID, s.now().Add(s.settle))
}

// sweep settles every judgement past its deadline as NOT_JUDGED and starts a
// job for every one still owed that has none.
func (s *Service) sweep() {
	ctx := s.ctx
	expired, err := s.store.ExpireLeadJudgements(ctx, scanLimit)
	if err != nil && ctx.Err() == nil {
		s.log.Error("campaign lead judgements could not be settled at their deadline", "error", err)
	}
	for _, callID := range expired {
		obs.RecordLeadVerdict(string(campaign.VerdictNotJudged))
		s.log.Info("campaign lead not judged before its deadline; the declared lead stands",
			"callId", callID, "verdict", campaign.VerdictNotJudged)
	}
	if s.llm == nil {
		return
	}
	pending, err := s.store.PendingLeadJudgements(ctx, scanLimit)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("owed campaign lead judgements could not be read", "error", err)
		}
		return
	}
	for _, p := range pending {
		// Settle from the CDR's write, which the deadline dates: at start, long
		// past for a judgement an earlier run left owed.
		s.judge(p.CallID, p.DeadlineAt.Add(s.settle-campaign.LeadJudgeDeadline))
	}
}

// judge starts the job for one call unless one is running.
func (s *Service) judge(callID uuid.UUID, readAt time.Time) {
	if s.llm == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if _, running := s.inflight[callID]; running {
		s.mu.Unlock()
		return
	}
	s.inflight[callID] = struct{}{}
	s.jobs.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.jobs.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, callID)
			s.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("campaign lead judgement panicked", "callId", callID, "panic", r)
			}
		}()
		s.run(callID, readAt)
	}()
}

// run is one job: read the judgement, wait for the transcript to settle,
// decide, write.
func (s *Service) run(callID uuid.UUID, readAt time.Time) {
	log := s.log.With("callId", callID)
	j, err := s.store.LeadJudgement(s.ctx, callID)
	if err != nil {
		if s.ctx.Err() == nil {
			log.Error("campaign lead judgement failed", "stage", "read the judgement", "error", err)
		}
		return
	}
	if j.Verdict != campaign.VerdictPending {
		return
	}

	ctx, cancel := context.WithDeadline(s.ctx, j.DeadlineAt.Add(-s.margin))
	defer cancel()
	ctx, cancelJob := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancelJob()

	verdict, reason, requests := campaign.VerdictNotJudged, "", 0
	if wait := readAt.Sub(s.now()); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	if ctx.Err() == nil {
		verdict, reason, requests = s.decide(ctx, log, j)
	}
	if s.ctx.Err() != nil {
		// Stopping: leave it owed for the next start or the deadline.
		return
	}

	wctx, cancelWrite := context.WithTimeout(s.ctx, writeTimeout)
	defer cancelWrite()
	lead, applied, err := s.store.ApplyLeadVerdict(wctx, callID, verdict, reason)
	if err != nil {
		log.Error("campaign lead judgement failed", "stage", "write the verdict", "verdict", verdict, "error", err)
		return
	}
	if !applied {
		log.Info("campaign lead verdict discarded: the judgement was settled or its deadline passed",
			"verdict", verdict, "requests", requests)
		return
	}
	obs.RecordLeadVerdict(string(verdict))
	log.Info("campaign lead judged", "verdict", verdict, "declaredLeadIntent", j.DeclaredLeadIntent,
		"leadIntent", lead, "requests", requests)
}

// decide reads the transcript and asks the model, at most twice. A call with
// no callee line, or only greetings and acknowledgement sounds from the
// callee, is NO_EVIDENCE without a request; every failure is
// NOT_JUDGED.
func (s *Service) decide(ctx context.Context, log *slog.Logger, j store.LeadJudgement) (campaign.LeadVerdict, string, int) {
	lines, err := s.store.ListTranscript(ctx, j.CallID)
	if err != nil {
		log.Error("campaign lead judgement failed", "stage", "read the transcript", "error", err)
		return campaign.VerdictNotJudged, "", 0
	}
	in := inputOf(lines)
	lang := languageOf(j.Language, in)
	if !hasCallee(in) {
		return campaign.VerdictNoEvidence, noCalleeReason(lang), 0
	}
	if saysOnlySounds(in) {
		return campaign.VerdictNoEvidence, onlySoundsReason(lang), 0
	}
	system, user, ok := render(in, j.DeclaredLeadIntent, j.LeadNotes, lang, s.cfg.BudgetChars)
	if !ok {
		log.Error("campaign lead judgement failed", "stage", "build the prompt",
			"error", "the transcript does not fit the prompt budget", "budgetChars", s.cfg.BudgetChars)
		return campaign.VerdictNotJudged, "", 0
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		log.Error("campaign lead judgement failed", "stage", "wait for a slot", "error", ctx.Err())
		return campaign.VerdictNotJudged, "", 0
	}

	requests := 0
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && !s.hasTimeForRetry(ctx) {
			break
		}
		requests++
		answer, err := s.llm.Complete(ctx, textllm.Request{System: system, User: user, JSON: true})
		if err == nil {
			verdict, reason, perr := parseAnswer(answer)
			if perr == nil {
				return verdict, reason, requests
			}
			err = perr
			log.Warn("campaign lead judgement: invalid answer", "attempt", requests, "error", err)
		} else {
			log.Warn("campaign lead judgement: the text model failed", "attempt", requests, "error", err)
		}
	}
	return campaign.VerdictNotJudged, "", requests
}

// hasTimeForRetry reports whether the job's context leaves room for a second
// request.
func (s *Service) hasTimeForRetry(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || deadline.Sub(s.now()) >= retryFloor
}
