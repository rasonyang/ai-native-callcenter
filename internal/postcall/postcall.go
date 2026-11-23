// SPDX-License-Identifier: Apache-2.0

// Package postcall transcribes a finished call's recording (issue #27, design
// 08 section 18).
//
// It is a work queue over two tables, not a hook in the call path. A finished
// recording is offered here after it is booked; the offer is judged (is it
// worth the money?), and an accepted one becomes a PENDING row that a small
// worker pool drains. Nothing here runs while a call is live and nothing here
// can fail a call: every error ends in a row's FAILED status or a log line.
//
// The package is off unless configured. A nil *Service is a valid, inert one.
package postcall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
)

// Config is the policy of one deployment.
type Config struct {
	// MinDurationSec and MaxDurationSec bound the recordings worth sending. A
	// recording outside them is skipped, and a manual request over the maximum
	// is refused.
	MinDurationSec int
	MaxDurationSec int
	// DailyMaxAudioSec caps the audio put to the vendor in any rolling 24
	// hours; 0 is no cap.
	DailyMaxAudioSec int
	// Concurrency is the number of recordings in flight at once.
	Concurrency int
	// MaxAttempts bounds how many times one run is tried before it is FAILED.
	MaxAttempts int
	// LanguageHints narrows recognition; empty lets the model decide.
	LanguageHints []string
}

// Ledger is the part of the store this package uses; *store.LedgerStore
// satisfies it.
type Ledger interface {
	EnqueueTranscription(ctx context.Context, callID, recordingID uuid.UUID, audioSec int) (bool, error)
	RequeueTranscription(ctx context.Context, callID, recordingID uuid.UUID, audioSec int) (bool, error)
	GetTranscription(ctx context.Context, callID uuid.UUID) (store.CallTranscription, error)
	ClaimTranscription(ctx context.Context, model string) (store.CallTranscription, bool, error)
	ResetRunningTranscriptions(ctx context.Context) (int64, error)
	CompleteTranscription(ctx context.Context, callID uuid.UUID, run int, segs []store.TranscriptionSegment) (bool, error)
	RetryTranscription(ctx context.Context, callID uuid.UUID, run int, reason string, retryAt time.Time) error
	FailTranscription(ctx context.Context, callID uuid.UUID, run int, reason string) error
	TranscribedAudioSecSince(ctx context.Context, since time.Time) (int, error)
	LiveTranscriptSpeakers(ctx context.Context, callID uuid.UUID) ([]string, error)
	CallsAwaitingTranscription(ctx context.Context, since time.Time, limit int) ([]store.AwaitingTranscription, error)
	GetRecording(ctx context.Context, id uuid.UUID) (store.Recording, error)
	RecordingsByCall(ctx context.Context, callID uuid.UUID) ([]store.Recording, error)
	GetCDR(ctx context.Context, callID uuid.UUID) (store.CDR, error)
}

// Audio opens stored recordings.
type Audio interface {
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
}

// Refusals a manual request can meet. The handler maps each to its own code.
var (
	ErrNoRecording = errors.New("the call has no recording")
	ErrTooLong     = errors.New("the recording is longer than the configured maximum")
	ErrCapReached  = errors.New("the daily audio cap is reached")
)

// Service is the queue, its policy and its workers.
type Service struct {
	cfg    Config
	db     Ledger
	audio  Audio
	rec    transcribe.BatchRecognizer
	log    *slog.Logger
	now    func() time.Time
	settle time.Duration
	poll   time.Duration
	// backoff is the wait before attempt n+1 after attempt n failed.
	backoff func(attempt int) time.Duration

	mu      sync.Mutex
	tracked []uuid.UUID // runs the backfill queued, so Drain knows when they settle
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a service. Nothing runs until Start.
func New(cfg Config, db Ledger, audio Audio, rec transcribe.BatchRecognizer, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return &Service{
		cfg: cfg, db: db, audio: audio, rec: rec, log: log,
		now:    time.Now,
		settle: 20 * time.Second,
		poll:   5 * time.Second,
		backoff: func(attempt int) time.Duration {
			d := 30 * time.Second
			for i := 1; i < attempt; i++ {
				d *= 4
			}
			return d
		},
	}
}

// Model is the model every run names.
func (s *Service) Model() string { return s.rec.Model() }

// Start returns stranded runs to the queue and launches the workers. It is
// safe on a nil service.
func (s *Service) Start(ctx context.Context) error {
	if s == nil {
		return nil
	}
	n, err := s.db.ResetRunningTranscriptions(ctx)
	if err != nil {
		return fmt.Errorf("post-call transcription: %w", err)
	}
	if n > 0 {
		s.log.Info("post-call transcription: runs interrupted by a restart go back in the queue", "count", n)
	}
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(ctx)
	runCtx := s.ctx
	s.mu.Unlock()
	for i := 0; i < s.cfg.Concurrency; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.work(runCtx)
		}()
	}
	return nil
}

// Stop ends the workers and waits for them. A run cut short stays RUNNING and
// is requeued by the next Start.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Verdict is the judgement on one recording.
type Verdict struct {
	// Skip is empty when the recording should be transcribed, else the reason.
	Skip string
}

// Reasons a recording is not transcribed automatically.
const (
	SkipTooShort    = "TOO_SHORT"
	SkipTooLong     = "TOO_LONG"
	SkipLive        = "LIVE_TRANSCRIPT_COVERS_BOTH_CHANNELS"
	SkipCap         = "DAILY_CAP"
	SkipHasRun      = "ALREADY_REQUESTED"
	SkipNoRecording = "NO_RECORDING"
)

// Judge decides whether a recording is worth sending. The live-transcript rule
// is the one the owner chose to save money, and it is stated exactly in design
// 08 section 18.4:
//
//   - the left (CUSTOMER) channel is covered when the call has at least one
//     CUSTOMER line in its live transcript, written by either source;
//   - the right (FAR_END) channel is covered when every kind of party that
//     answered on it has a line: a BOT line if a bot answered, a HUMAN_AGENT
//     line if an agent did, and trivially when nobody did;
//   - the recording is skipped when both channels are covered.
//
// Anything short of that, including a bot call that was transferred to an
// agent nobody transcribed, is transcribed whole.
func (s *Service) Judge(ctx context.Context, callID uuid.UUID, durationSec int, hadBot, hadHuman bool) (Verdict, error) {
	if durationSec < s.cfg.MinDurationSec {
		return Verdict{Skip: SkipTooShort}, nil
	}
	if s.cfg.MaxDurationSec > 0 && durationSec > s.cfg.MaxDurationSec {
		return Verdict{Skip: SkipTooLong}, nil
	}
	speakers, err := s.db.LiveTranscriptSpeakers(ctx, callID)
	if err != nil {
		return Verdict{}, err
	}
	if liveCovers(speakers, hadBot, hadHuman) {
		return Verdict{Skip: SkipLive}, nil
	}
	capped, err := s.overCap(ctx, durationSec)
	if err != nil {
		return Verdict{}, err
	}
	if capped {
		return Verdict{Skip: SkipCap}, nil
	}
	return Verdict{}, nil
}

func liveCovers(speakers []string, hadBot, hadHuman bool) bool {
	has := map[string]bool{}
	for _, sp := range speakers {
		has[sp] = true
	}
	if !has["CUSTOMER"] {
		return false
	}
	if hadBot && !has["BOT"] {
		return false
	}
	if hadHuman && !has["HUMAN_AGENT"] {
		return false
	}
	return true
}

func (s *Service) overCap(ctx context.Context, extraSec int) (bool, error) {
	if s.cfg.DailyMaxAudioSec <= 0 {
		return false, nil
	}
	used, err := s.db.TranscribedAudioSecSince(ctx, s.now().Add(-24*time.Hour))
	if err != nil {
		return false, err
	}
	return used+extraSec > s.cfg.DailyMaxAudioSec, nil
}

// OfferRecording is called by the CDR path once a recording is booked. It
// returns at once: the judgement waits a few seconds so the bot's transcript,
// written when its session tears down, is in the database before the live
// rule reads it. An offer lost to a restart is not lost for good; the backfill
// finds the call. Safe on a nil service.
func (s *Service) OfferRecording(rec store.Recording, hadBot, hadHuman bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.settle):
		}
		s.offer(ctx, rec.CallID, rec.ID, rec.DurationSec, hadBot, hadHuman)
	}()
}

// OfferBookedRecording is OfferRecording for a booking made without a call
// snapshot — the spool sweep has only the key and the call id. Who answered
// is derived from the CDR row, the way Plan derives it, through the one
// helper they share so the two cannot drift (#523).
func (s *Service) OfferBookedRecording(rec store.Recording) {
	if s == nil {
		return
	}
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.settle):
		}
		hadBot, hadHuman := s.sidesFromCDR(ctx, rec.CallID)
		s.offer(ctx, rec.CallID, rec.ID, rec.DurationSec, hadBot, hadHuman)
	}()
}

// sidesFromCDR derives who answered from the CDR row, as Plan does: a bot
// spoke when the call carries bot seconds, a person when an agent is on it.
// A row that cannot be read answers nobody — as it does for Plan.
func (s *Service) sidesFromCDR(ctx context.Context, callID uuid.UUID) (hadBot, hadHuman bool) {
	cdr, err := s.db.GetCDR(ctx, callID)
	if err != nil {
		return false, false
	}
	return cdr.BotSec > 0, len(cdr.AgentIDs) > 0
}

func (s *Service) offer(ctx context.Context, callID, recordingID uuid.UUID, durationSec int, hadBot, hadHuman bool) string {
	v, err := s.Judge(ctx, callID, durationSec, hadBot, hadHuman)
	if err != nil {
		s.log.Error("post-call transcription: cannot judge the recording", "callId", callID, "error", err)
		return ""
	}
	if v.Skip != "" {
		s.log.Info("post-call transcription: skipped", "callId", callID, "reason", v.Skip, "durationSec", durationSec)
		return v.Skip
	}
	inserted, err := s.db.EnqueueTranscription(ctx, callID, recordingID, durationSec)
	if err != nil {
		s.log.Error("post-call transcription: cannot queue the recording", "callId", callID, "error", err)
		return ""
	}
	if !inserted {
		return SkipHasRun
	}
	s.log.Info("post-call transcription: queued", "callId", callID, "durationSec", durationSec)
	return ""
}

// Request is the manual path (POST /calls/{callId}/transcription): it ignores
// the live-transcript rule and the minimum, because a person asked, but keeps
// the maximum and the cap, which exist for money and memory. A run already
// pending or running is returned as it is; a finished one starts over.
func (s *Service) Request(ctx context.Context, callID uuid.UUID) (store.CallTranscription, error) {
	recs, err := s.db.RecordingsByCall(ctx, callID)
	if err != nil {
		return store.CallTranscription{}, err
	}
	if len(recs) == 0 {
		return store.CallTranscription{}, ErrNoRecording
	}
	rec := recs[len(recs)-1]

	cur, gerr := s.db.GetTranscription(ctx, callID)
	exists := gerr == nil
	if gerr != nil && !errors.Is(gerr, pgx.ErrNoRows) {
		return store.CallTranscription{}, gerr
	}
	if exists && (cur.Status == store.TranscriptionPending || cur.Status == store.TranscriptionRunning) {
		return cur, nil
	}

	if s.cfg.MaxDurationSec > 0 && rec.DurationSec > s.cfg.MaxDurationSec {
		return store.CallTranscription{}, ErrTooLong
	}
	capped, err := s.overCap(ctx, rec.DurationSec)
	if err != nil {
		return store.CallTranscription{}, err
	}
	if capped {
		return store.CallTranscription{}, ErrCapReached
	}
	if !exists {
		if _, err := s.db.EnqueueTranscription(ctx, callID, rec.ID, rec.DurationSec); err != nil {
			return store.CallTranscription{}, err
		}
	} else if _, err := s.db.RequeueTranscription(ctx, callID, rec.ID, rec.DurationSec); err != nil {
		return store.CallTranscription{}, err
	}
	return s.db.GetTranscription(ctx, callID)
}

// Candidate is a call the backfill would transcribe.
type Candidate struct {
	CallID      uuid.UUID
	RecordingID uuid.UUID
	DurationSec int
}

// Plan lists the recorded calls since a moment that have no run and pass the
// automatic policy, newest first, at most limit of them. It spends nothing.
// The cap is reported in the second result rather than applied, so a dry run
// shows the whole backlog.
func (s *Service) Plan(ctx context.Context, since time.Time, limit int) ([]Candidate, error) {
	// Over-read: the policy drops some of what the query returns.
	rows, err := s.db.CallsAwaitingTranscription(ctx, since, limit*4+20)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		hadBot, hadHuman := s.sidesFromCDR(ctx, r.CallID)
		if r.DurationSec < s.cfg.MinDurationSec || (s.cfg.MaxDurationSec > 0 && r.DurationSec > s.cfg.MaxDurationSec) {
			continue
		}
		speakers, err := s.db.LiveTranscriptSpeakers(ctx, r.CallID)
		if err != nil {
			return nil, err
		}
		if liveCovers(speakers, hadBot, hadHuman) {
			continue
		}
		out = append(out, Candidate{CallID: r.CallID, RecordingID: r.RecordingID, DurationSec: r.DurationSec})
	}
	return out, nil
}

// CandidateFor names a call's latest recording as a candidate, whatever the
// policy would say: the operator chose it.
func (s *Service) CandidateFor(ctx context.Context, callID string) (Candidate, error) {
	id, err := uuid.Parse(callID)
	if err != nil {
		return Candidate{}, fmt.Errorf("%q is not a call id", callID)
	}
	recs, err := s.db.RecordingsByCall(ctx, id)
	if err != nil {
		return Candidate{}, err
	}
	if len(recs) == 0 {
		return Candidate{}, fmt.Errorf("call %s: %w", id, ErrNoRecording)
	}
	rec := recs[len(recs)-1]
	return Candidate{CallID: id, RecordingID: rec.ID, DurationSec: rec.DurationSec}, nil
}

// Enqueue queues candidates and reports how many started a run. With
// again set, a candidate whose run is DONE or FAILED starts over (the operator
// named the call); a run in flight is left alone either way.
func (s *Service) Enqueue(ctx context.Context, cands []Candidate, again bool) (int, error) {
	n := 0
	for _, c := range cands {
		ok, err := s.db.EnqueueTranscription(ctx, c.CallID, c.RecordingID, c.DurationSec)
		if err == nil && !ok && again {
			ok, err = s.db.RequeueTranscription(ctx, c.CallID, c.RecordingID, c.DurationSec)
		}
		if err != nil {
			return n, err
		}
		s.mu.Lock()
		s.tracked = append(s.tracked, c.CallID)
		s.mu.Unlock()
		if ok {
			n++
		}
	}
	return n, nil
}

// Drain runs the workers until the queue is empty, then stops them. It is the
// backfill command's engine.
func (s *Service) Drain(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	defer s.Stop()
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		busy, err := s.hasWork(ctx)
		if err != nil {
			return err
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// hasWork reports whether any run the backfill queued is still pending or
// running.
func (s *Service) hasWork(ctx context.Context) (bool, error) {
	s.mu.Lock()
	ids := append([]uuid.UUID(nil), s.tracked...)
	s.mu.Unlock()
	for _, id := range ids {
		t, err := s.db.GetTranscription(ctx, id)
		if err != nil {
			return false, err
		}
		if t.Status == store.TranscriptionPending || t.Status == store.TranscriptionRunning {
			return true, nil
		}
	}
	return false, nil
}

// work is one worker's loop.
func (s *Service) work(ctx context.Context) {
	for ctx.Err() == nil {
		if capped, err := s.overCap(ctx, 1); err == nil && capped {
			// The cap is soft by up to Concurrency recordings: the check is
			// before the claim, and a claimed run's length is not known until
			// it is claimed.
			s.sleep(ctx, s.poll)
			continue
		}
		job, ok, err := s.db.ClaimTranscription(ctx, s.rec.Model())
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("post-call transcription: cannot claim a run", "error", err)
			}
			s.sleep(ctx, s.poll)
			continue
		}
		if !ok {
			s.sleep(ctx, s.poll)
			continue
		}
		s.run(ctx, job)
	}
}

func (s *Service) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// run executes one claimed run and settles its row.
func (s *Service) run(ctx context.Context, job store.CallTranscription) {
	segs, err := s.transcribe(ctx, job)
	if ctx.Err() != nil {
		// Shutdown, not failure: the row stays RUNNING and Start requeues it.
		return
	}
	// The settling writes use their own deadline: the work may have used up
	// the context it was given.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err == nil {
		ok, werr := s.db.CompleteTranscription(wctx, job.CallID, job.Run, segs)
		if werr != nil {
			s.log.Error("post-call transcription: cannot store the result", "callId", job.CallID, "error", werr)
			err = werr
		} else {
			if ok {
				s.log.Info("post-call transcription: done", "callId", job.CallID,
					"segments", len(segs), "audioSec", job.AudioSec, "attempt", job.Attempts)
			}
			return
		}
	}

	reason := err.Error()
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if transcribe.IsRetryable(err) && job.Attempts < s.cfg.MaxAttempts {
		at := s.now().Add(s.backoff(job.Attempts))
		s.log.Warn("post-call transcription: attempt failed, will retry", "callId", job.CallID,
			"attempt", job.Attempts, "retryAt", at, "error", err)
		if rerr := s.db.RetryTranscription(wctx, job.CallID, job.Run, reason, at); rerr != nil {
			s.log.Error("post-call transcription: cannot requeue", "callId", job.CallID, "error", rerr)
		}
		return
	}
	s.log.Error("post-call transcription: failed", "callId", job.CallID, "attempt", job.Attempts, "error", err)
	if ferr := s.db.FailTranscription(wctx, job.CallID, job.Run, reason); ferr != nil {
		s.log.Error("post-call transcription: cannot record the failure", "callId", job.CallID, "error", ferr)
	}
}

// permanent wraps an error that sending the same recording again cannot cure.
func permanent(format string, a ...any) error {
	return &transcribe.BatchError{Message: fmt.Sprintf(format, a...)}
}

// transcribe turns one recording into segments.
func (s *Service) transcribe(ctx context.Context, job store.CallTranscription) (segs []store.TranscriptionSegment, err error) {
	defer func() {
		if r := recover(); r != nil {
			segs, err = nil, permanent("panic while transcribing: %v", r)
		}
	}()

	rec, err := s.db.GetRecording(ctx, job.RecordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, permanent("the recording is no longer stored")
	}
	if err != nil {
		return nil, err
	}
	r, size, err := s.audio.Open(ctx, rec.ObjectKey)
	if err != nil {
		return nil, permanent("cannot open the recording: %v", err)
	}
	defer r.Close()
	// 8 kHz stereo 16-bit is 32000 bytes a second; allow for other rates.
	if limit := int64(s.cfg.MaxDurationSec) * 192000; s.cfg.MaxDurationSec > 0 && size > limit+1<<20 {
		return nil, permanent("the recording is %d bytes, over the configured maximum", size)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading the recording: %w", err)
	}
	pcm, err := transcribe.ParseWAV(raw)
	if err != nil {
		return nil, permanent("%v", err)
	}
	raw = nil

	// The stereo record puts the recorded leg's read stream on the left.
	// Inbound calls and AI outbound are recorded on the customer's leg, so
	// left is the customer. A call a phone places to the trunk is recorded on
	// the phone's own leg: left is the person on the phone, right the customer.
	// No agent is required, because a dial from a signed-out phone carries
	// none and is still recorded on the phone's leg. Decided once per run.
	left, right := store.ChannelCustomer, store.ChannelFarEnd
	cdr, err := s.db.GetCDR(ctx, job.CallID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case cdr.CallType == string(events.CallTypeOutbound) && cdr.BotSec == 0 && cdr.FlowID == nil:
		left, right = store.ChannelFarEnd, store.ChannelCustomer
	}

	var pieces []store.TranscriptionSegment
	for ch, samples := range pcm.Channels {
		channel := left
		if ch == 1 {
			channel = right
		}
		for _, clip := range transcribe.Clips(samples, pcm.Rate) {
			res, err := s.rec.Recognize(ctx, transcribe.BatchRequest{
				WAV:           transcribe.EncodeWAV(clip.Samples, pcm.Rate),
				SampleRate:    pcm.Rate,
				LanguageHints: s.cfg.LanguageHints,
			})
			if err != nil {
				return nil, err
			}
			if res.NoSpeech {
				continue
			}
			clipEnd := clip.OffsetMs + len(clip.Samples)*1000/pcm.Rate
			if len(res.Sentences) == 0 && res.Text != "" {
				pieces = append(pieces, store.TranscriptionSegment{
					Channel: channel, OffsetMs: clip.OffsetMs, EndMs: clipEnd, Text: res.Text, Language: res.Language,
				})
				continue
			}
			for _, sn := range res.Sentences {
				end := clip.OffsetMs + sn.EndMs
				if sn.EndMs <= 0 || end > clipEnd {
					end = clipEnd
				}
				start := clip.OffsetMs + sn.StartMs
				if start > end {
					start = end
				}
				pieces = append(pieces, store.TranscriptionSegment{
					Channel: channel, OffsetMs: start, EndMs: end, Text: sn.Text, Language: res.Language,
				})
			}
		}
	}
	sort.SliceStable(pieces, func(i, j int) bool {
		if pieces[i].OffsetMs != pieces[j].OffsetMs {
			return pieces[i].OffsetMs < pieces[j].OffsetMs
		}
		return pieces[i].Channel < pieces[j].Channel
	})
	for i := range pieces {
		pieces[i].Seq = i + 1
	}
	return pieces, nil
}
