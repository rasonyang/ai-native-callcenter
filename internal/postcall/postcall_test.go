// SPDX-License-Identifier: Apache-2.0

package postcall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
)

// fakeLedger is the queue's store in memory. It keeps the semantics the real
// queries have: enqueue is idempotent, requeue only touches finished runs,
// settling is guarded by run and RUNNING.
type fakeLedger struct {
	mu       sync.Mutex
	runs     map[uuid.UUID]*store.CallTranscription
	segs     map[uuid.UUID][]store.TranscriptionSegment
	speakers map[uuid.UUID][]string
	recs     map[uuid.UUID][]store.Recording
	usedSec  int
	cdrs     map[uuid.UUID]store.CDR
	retries  []time.Time
	cdrErr   error
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		runs: map[uuid.UUID]*store.CallTranscription{}, segs: map[uuid.UUID][]store.TranscriptionSegment{},
		speakers: map[uuid.UUID][]string{}, recs: map[uuid.UUID][]store.Recording{}, cdrs: map[uuid.UUID]store.CDR{},
	}
}

func (f *fakeLedger) EnqueueTranscription(_ context.Context, callID, recID uuid.UUID, sec int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[callID]; ok {
		return false, nil
	}
	f.runs[callID] = &store.CallTranscription{CallID: callID, RecordingID: recID, Run: 1,
		Status: store.TranscriptionPending, AudioSec: sec, RequestedAt: time.Now()}
	return true, nil
}

func (f *fakeLedger) RequeueTranscription(_ context.Context, callID, recID uuid.UUID, sec int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[callID]
	if !ok || (r.Status != store.TranscriptionDone && r.Status != store.TranscriptionFailed) {
		return false, nil
	}
	r.Status, r.Run, r.Attempts, r.Error, r.RecordingID, r.AudioSec = store.TranscriptionPending, r.Run+1, 0, "", recID, sec
	r.NextAttemptAt = time.Time{}
	return true, nil
}

func (f *fakeLedger) GetTranscription(_ context.Context, callID uuid.UUID) (store.CallTranscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[callID]; ok {
		return *r, nil
	}
	return store.CallTranscription{}, pgx.ErrNoRows
}

func (f *fakeLedger) ClaimTranscription(_ context.Context, model string) (store.CallTranscription, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.Status == store.TranscriptionPending && !r.NextAttemptAt.After(time.Now()) {
			r.Status, r.Model = store.TranscriptionRunning, model
			r.Attempts++
			now := time.Now()
			r.StartedAt = &now
			f.usedSec += r.AudioSec
			return *r, true, nil
		}
	}
	return store.CallTranscription{}, false, nil
}

func (f *fakeLedger) ResetRunningTranscriptions(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.runs {
		if r.Status == store.TranscriptionRunning {
			r.Status = store.TranscriptionPending
			n++
		}
	}
	return n, nil
}

func (f *fakeLedger) settle(callID uuid.UUID, run int, to string, errText string, at time.Time) bool {
	r, ok := f.runs[callID]
	if !ok || r.Run != run || r.Status != store.TranscriptionRunning {
		return false
	}
	r.Status, r.Error, r.NextAttemptAt = to, errText, at
	return true
}

func (f *fakeLedger) CompleteTranscription(_ context.Context, callID uuid.UUID, run int, segs []store.TranscriptionSegment) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.settle(callID, run, store.TranscriptionDone, "", time.Time{}) {
		return false, nil
	}
	f.segs[callID] = segs
	return true, nil
}

func (f *fakeLedger) RetryTranscription(_ context.Context, callID uuid.UUID, run int, reason string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries = append(f.retries, at)
	f.settle(callID, run, store.TranscriptionPending, reason, at)
	return nil
}

func (f *fakeLedger) FailTranscription(_ context.Context, callID uuid.UUID, run int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settle(callID, run, store.TranscriptionFailed, reason, time.Time{})
	return nil
}

func (f *fakeLedger) TranscribedAudioSecSince(context.Context, time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usedSec, nil
}

func (f *fakeLedger) LiveTranscriptSpeakers(_ context.Context, id uuid.UUID) ([]string, error) {
	return f.speakers[id], nil
}

func (f *fakeLedger) CallsAwaitingTranscription(_ context.Context, _ time.Time, limit int) ([]store.AwaitingTranscription, error) {
	var out []store.AwaitingTranscription
	for id, recs := range f.recs {
		if _, has := f.runs[id]; !has {
			out = append(out, store.AwaitingTranscription{CallID: id, RecordingID: recs[0].ID, DurationSec: recs[0].DurationSec})
		}
	}
	return out, nil
}

func (f *fakeLedger) GetRecording(_ context.Context, id uuid.UUID) (store.Recording, error) {
	for _, recs := range f.recs {
		for _, r := range recs {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return store.Recording{}, pgx.ErrNoRows
}

func (f *fakeLedger) RecordingsByCall(_ context.Context, id uuid.UUID) ([]store.Recording, error) {
	return f.recs[id], nil
}

func (f *fakeLedger) GetCDR(_ context.Context, id uuid.UUID) (store.CDR, error) {
	if f.cdrErr != nil {
		return store.CDR{}, f.cdrErr
	}
	if c, ok := f.cdrs[id]; ok {
		return c, nil
	}
	return store.CDR{}, pgx.ErrNoRows
}

func (f *fakeLedger) addRecording(sec int) store.Recording {
	r := store.Recording{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), ObjectKey: "k", DurationSec: sec}
	f.recs[r.CallID] = []store.Recording{r}
	return r
}

type fakeAudio struct {
	b   []byte
	err error
}

type nopSeeker struct{ *bytes.Reader }

func (nopSeeker) Close() error { return nil }

func (a fakeAudio) Open(context.Context, string) (io.ReadSeekCloser, int64, error) {
	if a.err != nil {
		return nil, 0, a.err
	}
	return nopSeeker{bytes.NewReader(a.b)}, int64(len(a.b)), nil
}

// fakeRecognizer answers per call to Recognize from a script and remembers
// every clip it was given.
type fakeRecognizer struct {
	mu    sync.Mutex
	reqs  []transcribe.BatchRequest
	reply func(n int, req transcribe.BatchRequest) (transcribe.BatchResult, error)
}

func (r *fakeRecognizer) Model() string { return "fake-asr" }
func (r *fakeRecognizer) Recognize(_ context.Context, req transcribe.BatchRequest) (transcribe.BatchResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	n := len(r.reqs)
	r.mu.Unlock()
	return r.reply(n, req)
}

func (r *fakeRecognizer) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func tone(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// stereo builds a canonical stereo WAV from two channels.
func stereo(l, r []int16, rate int) []byte {
	// Interleave through the package's own mono encoder: the header of the
	// first file is rewritten for two channels.
	n := len(l)
	mono := transcribe.EncodeWAV(make([]int16, n*2), rate)
	for i := 0; i < n; i++ {
		mono[44+i*4], mono[45+i*4] = byte(uint16(l[i])), byte(uint16(l[i])>>8)
		mono[46+i*4], mono[47+i*4] = byte(uint16(r[i])), byte(uint16(r[i])>>8)
	}
	mono[22] = 2
	mono[28], mono[29], mono[30], mono[31] = byte(rate*4), byte(rate*4>>8), byte(rate*4>>16), byte(rate*4>>24)
	mono[32] = 4
	return mono
}

func newService(db *fakeLedger, aud Audio, rec transcribe.BatchRecognizer, cfg Config) *Service {
	s := New(cfg, db, aud, rec, nil)
	s.poll = 5 * time.Millisecond
	s.settle = time.Millisecond
	s.backoff = func(int) time.Duration { return 0 }
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeLedger) status(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		return r.Status
	}
	return ""
}

var defaultCfg = Config{MinDurationSec: 10, MaxDurationSec: 3600, Concurrency: 2, MaxAttempts: 3}

// The live-transcript rule, exactly as design 08 section 18.4 states it.
func TestTheLiveTranscriptRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		speakers         []string
		hadBot, hadHuman bool
		skip             bool
	}{
		{"a bot call with both sides live", []string{"CUSTOMER", "BOT"}, true, false, true},
		{"a bot call with only the bot live", []string{"BOT"}, true, false, false},
		{"a bot call with no live rows", nil, true, false, false},
		{"an agent call transcribed live", []string{"CUSTOMER", "HUMAN_AGENT"}, false, true, true},
		{"an agent call with only the customer live", []string{"CUSTOMER"}, false, true, false},
		{"bot then agent, agent never transcribed", []string{"CUSTOMER", "BOT"}, true, true, false},
		{"bot then agent, all three live", []string{"CUSTOMER", "BOT", "HUMAN_AGENT"}, true, true, true},
		{"nobody answered, customer live", []string{"CUSTOMER"}, false, false, true},
		{"nobody answered, nothing live", nil, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeLedger()
			rec := db.addRecording(120)
			db.speakers[rec.CallID] = tc.speakers
			s := newService(db, fakeAudio{}, &fakeRecognizer{}, defaultCfg)
			v, err := s.Judge(context.Background(), rec.CallID, 120, tc.hadBot, tc.hadHuman)
			if err != nil {
				t.Fatal(err)
			}
			if (v.Skip == SkipLive) != tc.skip {
				t.Errorf("skip = %q, want skip=%v", v.Skip, tc.skip)
			}
		})
	}
}

func TestDurationBoundsAndTheDailyCap(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	cfg := defaultCfg
	cfg.DailyMaxAudioSec = 600
	s := newService(db, fakeAudio{}, &fakeRecognizer{}, cfg)
	judge := func(sec int) string {
		v, err := s.Judge(context.Background(), uuid.Must(uuid.NewV7()), sec, true, false)
		if err != nil {
			t.Fatal(err)
		}
		return v.Skip
	}
	if got := judge(5); got != SkipTooShort {
		t.Errorf("5 s = %q", got)
	}
	if got := judge(4000); got != SkipTooLong {
		t.Errorf("4000 s = %q", got)
	}
	if got := judge(300); got != "" {
		t.Errorf("300 s = %q, want accepted", got)
	}
	db.usedSec = 400
	if got := judge(300); got != SkipCap {
		t.Errorf("300 s with 400 used of 600 = %q, want the cap", got)
	}
	if got := judge(200); got != "" {
		t.Errorf("200 s with 400 used of 600 = %q, want accepted (exactly at the cap)", got)
	}
}

func TestAnOfferedRecordingIsQueuedOnceAndASkippedOneIsNot(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	s := newService(db, fakeAudio{}, &fakeRecognizer{}, defaultCfg)
	ctx := context.Background()

	rec := db.addRecording(90)
	if got := s.offer(ctx, rec.CallID, rec.ID, 90, true, false); got != "" {
		t.Fatalf("offer skipped: %q", got)
	}
	if got := s.offer(ctx, rec.CallID, rec.ID, 90, true, false); got != SkipHasRun {
		t.Errorf("second offer = %q, want it recognised as already queued", got)
	}
	if len(db.runs) != 1 {
		t.Errorf("%d runs, want 1", len(db.runs))
	}

	covered := db.addRecording(90)
	db.speakers[covered.CallID] = []string{"CUSTOMER", "BOT"}
	if got := s.offer(ctx, covered.CallID, covered.ID, 90, true, false); got != SkipLive {
		t.Errorf("covered call = %q", got)
	}
	if _, ok := db.runs[covered.CallID]; ok {
		t.Error("a call the live transcript covers was queued")
	}
}

func TestANilServiceIsInert(t *testing.T) {
	t.Parallel()
	var s *Service
	s.OfferRecording(store.Recording{}, true, true)
	if err := s.Start(context.Background()); err != nil {
		t.Error(err)
	}
	s.Stop()
}

// A recording is split into its two channels, each sent as a mono 8 kHz WAV,
// and the sentences come back merged by time with their channel.
func TestARecordingIsSplitSentAndMergedByTime(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(30)
	left, right := tone(8000*5, 1000), tone(8000*5, -1000)
	aud := fakeAudio{b: stereo(left, right, 8000)}

	asr := &fakeRecognizer{reply: func(_ int, req transcribe.BatchRequest) (transcribe.BatchResult, error) {
		pcm, err := transcribe.ParseWAV(req.WAV)
		if err != nil || len(pcm.Channels) != 1 || pcm.Rate != 8000 || req.SampleRate != 8000 {
			t.Errorf("clip is not mono 8 kHz: %v rate %d", err, req.SampleRate)
			return transcribe.BatchResult{}, errors.New("bad clip")
		}
		if pcm.Channels[0][0] > 0 { // the left channel
			return transcribe.BatchResult{Language: "en", Sentences: []transcribe.BatchSentence{
				{Text: "hi, I need help", StartMs: 500, EndMs: 2000},
				{Text: "my bill", StartMs: 4000, EndMs: 4500}}}, nil
		}
		return transcribe.BatchResult{Language: "en", Sentences: []transcribe.BatchSentence{
			{Text: "how can I help", StartMs: 2100, EndMs: 3500}}}, nil
	}}

	s := newService(db, aud, asr, defaultCfg)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the run to finish", func() bool { return db.status(rec.CallID) == store.TranscriptionDone })

	got := db.segs[rec.CallID]
	var order []string
	for _, g := range got {
		order = append(order, g.Channel+":"+g.Text)
	}
	want := []string{"CUSTOMER:hi, I need help", "FAR_END:how can I help", "CUSTOMER:my bill"}
	if !slices.Equal(order, want) {
		t.Fatalf("segments = %v, want %v", order, want)
	}
	for i, g := range got {
		if g.Seq != i+1 || g.Language != "en" {
			t.Errorf("segment %d = %+v", i, g)
		}
	}
	if db.runs[rec.CallID].Model != "fake-asr" {
		t.Errorf("model = %q", db.runs[rec.CallID].Model)
	}
}

// A recording longer than one request allows is cut, each clip is offset, and
// a channel with nothing on it costs no request.
func TestALongRecordingIsChunkedAndOffset(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(600)
	const rate = 8000
	n := rate * 600
	aud := fakeAudio{b: stereo(tone(n, 1000), make([]int16, n), rate)} // the right channel is silent

	asr := &fakeRecognizer{reply: func(_ int, req transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{Text: "x", Sentences: []transcribe.BatchSentence{{Text: "x", StartMs: 100, EndMs: 200}}}, nil
	}}
	s := newService(db, aud, asr, defaultCfg)
	segs, err := s.transcribe(context.Background(), store.CallTranscription{RecordingID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	if asr.calls() < 3 {
		t.Fatalf("%d requests for 10 minutes, want at least 3", asr.calls())
	}
	for _, r := range asr.reqs {
		if len(r.WAV) > 44+transcribe.MaxClipSec*rate*2 {
			t.Errorf("a clip of %d bytes is over the limit", len(r.WAV))
		}
	}
	if len(segs) != asr.calls() {
		t.Fatalf("%d segments for %d requests", len(segs), asr.calls())
	}
	for i := 1; i < len(segs); i++ {
		if segs[i].OffsetMs <= segs[i-1].OffsetMs+100 {
			t.Errorf("segment %d at %d ms does not follow segment %d at %d ms", i, segs[i].OffsetMs, i-1, segs[i-1].OffsetMs)
		}
		if segs[i].Channel != store.ChannelCustomer {
			t.Errorf("segment %d is on %s; the right channel was silent", i, segs[i].Channel)
		}
	}
	if segs[1].OffsetMs < 200*1000 {
		t.Errorf("second clip starts at %d ms; it should start near the 4:30 limit", segs[1].OffsetMs)
	}
}

func TestATransientFailureIsRetriedWithBackoffThenSucceeds(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(30)
	aud := fakeAudio{b: stereo(tone(8000*3, 1000), make([]int16, 8000*3), 8000)}
	asr := &fakeRecognizer{reply: func(n int, _ transcribe.BatchRequest) (transcribe.BatchResult, error) {
		if n < 3 {
			return transcribe.BatchResult{}, &transcribe.BatchError{Status: 429, Code: "Throttling", Retryable: true}
		}
		return transcribe.BatchResult{Text: "ok", Sentences: []transcribe.BatchSentence{{Text: "ok", EndMs: 100}}}, nil
	}}
	s := newService(db, aud, asr, defaultCfg)
	s.backoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Millisecond }
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the third attempt to succeed", func() bool { return db.status(rec.CallID) == store.TranscriptionDone })

	if r := db.runs[rec.CallID]; r.Attempts != 3 || r.Error != "" {
		t.Errorf("attempts = %d, error = %q; want 3 attempts and the error cleared", r.Attempts, r.Error)
	}
	if len(db.retries) != 2 {
		t.Errorf("%d retries scheduled, want 2", len(db.retries))
	}
}

func TestRetriesAreBoundedThenTheRunFails(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(30)
	aud := fakeAudio{b: stereo(tone(8000*3, 1000), make([]int16, 8000*3), 8000)}
	asr := &fakeRecognizer{reply: func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{}, &transcribe.BatchError{Status: 503, Message: "down", Retryable: true}
	}}
	s := newService(db, aud, asr, defaultCfg)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the run to fail", func() bool { return db.status(rec.CallID) == store.TranscriptionFailed })

	r := db.runs[rec.CallID]
	if r.Attempts != defaultCfg.MaxAttempts {
		t.Errorf("attempts = %d, want %d", r.Attempts, defaultCfg.MaxAttempts)
	}
	if r.Error == "" {
		t.Error("a failed run carries no error")
	}
	if len(db.segs[rec.CallID]) != 0 {
		t.Error("a failed run left segments")
	}
}

func TestAPermanentFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		audio Audio
		reply func(int, transcribe.BatchRequest) (transcribe.BatchResult, error)
	}{
		"the vendor rejects the clip": {
			fakeAudio{b: stereo(tone(8000, 1000), tone(8000, 1000), 8000)},
			func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
				return transcribe.BatchResult{}, &transcribe.BatchError{Status: 400, Message: "bad audio"}
			}},
		"the audio is gone":     {fakeAudio{err: errors.New("no such key")}, nil},
		"the file is not a wav": {fakeAudio{b: []byte("garbage")}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := newFakeLedger()
			rec := db.addRecording(30)
			asr := &fakeRecognizer{reply: tc.reply}
			s := newService(db, tc.audio, asr, defaultCfg)
			db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			waitFor(t, "the run to fail", func() bool { return db.status(rec.CallID) == store.TranscriptionFailed })
			if r := db.runs[rec.CallID]; r.Attempts != 1 {
				t.Errorf("attempts = %d, want 1: retrying cannot help", r.Attempts)
			}
		})
	}
}

func TestARunInterruptedByARestartGoesBackInTheQueue(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(30)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	db.runs[rec.CallID].Status = store.TranscriptionRunning
	db.runs[rec.CallID].Attempts = 1

	aud := fakeAudio{b: stereo(tone(8000*3, 1000), make([]int16, 8000*3), 8000)}
	asr := &fakeRecognizer{reply: func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{Text: "back", Sentences: []transcribe.BatchSentence{{Text: "back", EndMs: 100}}}, nil
	}}
	s := newService(db, aud, asr, defaultCfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the stranded run to finish", func() bool { return db.status(rec.CallID) == store.TranscriptionDone })
	if db.runs[rec.CallID].Attempts != 2 {
		t.Errorf("attempts = %d; the interrupted attempt must stay counted", db.runs[rec.CallID].Attempts)
	}
}

func TestTheWorkersHoldBackWhileTheCapIsReached(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	rec := db.addRecording(30)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	db.usedSec = 600
	cfg := defaultCfg
	cfg.DailyMaxAudioSec = 600
	asr := &fakeRecognizer{reply: func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{}, nil
	}}
	s := newService(db, fakeAudio{b: stereo(tone(8000, 1000), tone(8000, 1000), 8000)}, asr, cfg)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	time.Sleep(100 * time.Millisecond)
	if st := db.status(rec.CallID); st != store.TranscriptionPending || asr.calls() != 0 {
		t.Errorf("status %s with %d requests: the cap did not hold the queue", st, asr.calls())
	}
}

func TestAManualRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newFakeLedger()
	cfg := defaultCfg
	cfg.DailyMaxAudioSec = 600
	s := newService(db, fakeAudio{}, &fakeRecognizer{}, cfg)

	// No recording.
	if _, err := s.Request(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, ErrNoRecording) {
		t.Errorf("no recording: %v", err)
	}

	// A short recording and a covered call are both accepted: a person asked.
	rec := db.addRecording(4)
	db.speakers[rec.CallID] = []string{"CUSTOMER", "BOT"}
	run, err := s.Request(ctx, rec.CallID)
	if err != nil || run.Status != store.TranscriptionPending {
		t.Fatalf("manual request: %v %+v", err, run)
	}
	// Asking again while it is in flight changes nothing.
	again, err := s.Request(ctx, rec.CallID)
	if err != nil || again.Run != run.Run || again.Status != store.TranscriptionPending {
		t.Errorf("repeat while pending: %v %+v", err, again)
	}

	// A finished run starts over, with the next run number.
	db.runs[rec.CallID].Status = store.TranscriptionFailed
	redo, err := s.Request(ctx, rec.CallID)
	if err != nil || redo.Run != 2 || redo.Status != store.TranscriptionPending || redo.Attempts != 0 {
		t.Errorf("re-request after failure: %v %+v", err, redo)
	}

	// The maximum and the cap still apply.
	long := db.addRecording(4000)
	if _, err := s.Request(ctx, long.CallID); !errors.Is(err, ErrTooLong) {
		t.Errorf("over the maximum: %v", err)
	}
	db.usedSec = 500
	big := db.addRecording(300)
	if _, err := s.Request(ctx, big.CallID); !errors.Is(err, ErrCapReached) {
		t.Errorf("over the cap: %v", err)
	}
}

func TestThePlanSpendsNothingAndAppliesThePolicy(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	short := db.addRecording(3)
	_ = short
	good := db.addRecording(120)
	covered := db.addRecording(120)
	db.speakers[covered.CallID] = []string{"CUSTOMER"} // no far end in the CDR: the customer line covers it
	queued := db.addRecording(120)
	db.EnqueueTranscription(context.Background(), queued.CallID, queued.ID, 120)

	s := newService(db, fakeAudio{}, &fakeRecognizer{}, defaultCfg)
	plan, err := s.Plan(context.Background(), time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].CallID != good.CallID {
		t.Fatalf("plan = %+v, want only the 120 s call with no live transcript", plan)
	}
	if len(db.runs) != 1 {
		t.Error("planning queued something")
	}
}

// A clip the vendor heard nothing in is an empty clip. The run keeps what the
// other clips said, and a recording with no speech at all is DONE with no
// segments rather than FAILED.
func TestAClipWithNoSpeechDoesNotFailTheRun(t *testing.T) {
	t.Parallel()
	const rate = 8000
	n := rate * 600
	left, right := tone(n, 1000), tone(n, -1000)
	aud := fakeAudio{b: stereo(left, right, rate)}

	run := func(t *testing.T, reply func(n int, req transcribe.BatchRequest) (transcribe.BatchResult, error)) (string, []store.TranscriptionSegment) {
		db := newFakeLedger()
		rec := db.addRecording(600)
		s := newService(db, aud, &fakeRecognizer{reply: reply}, defaultCfg)
		db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 600)
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.Stop()
		waitFor(t, "the run to settle", func() bool {
			st := db.status(rec.CallID)
			return st == store.TranscriptionDone || st == store.TranscriptionFailed
		})
		return db.status(rec.CallID), db.segs[rec.CallID]
	}

	t.Run("some clips empty", func(t *testing.T) {
		status, segs := run(t, func(n int, _ transcribe.BatchRequest) (transcribe.BatchResult, error) {
			if n%2 == 1 {
				return transcribe.BatchResult{NoSpeech: true}, nil
			}
			return transcribe.BatchResult{Text: "x", Sentences: []transcribe.BatchSentence{{Text: "x", EndMs: 100}}}, nil
		})
		if status != store.TranscriptionDone || len(segs) == 0 {
			t.Errorf("status %s, %d segments; want DONE with the clips that did have speech", status, len(segs))
		}
	})
	t.Run("every clip empty", func(t *testing.T) {
		status, segs := run(t, func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
			return transcribe.BatchResult{NoSpeech: true}, nil
		})
		if status != store.TranscriptionDone || len(segs) != 0 {
			t.Errorf("status %s, %d segments; want DONE with none", status, len(segs))
		}
	})
}

// The backfill names calls on purpose: a FAILED run for one of them starts
// over, a run in flight is left alone, and without that choice nothing is
// disturbed.
func TestTheBackfillCanRerunAFailedCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newFakeLedger()
	failed, running := db.addRecording(60), db.addRecording(60)
	for _, r := range []store.Recording{failed, running} {
		db.EnqueueTranscription(ctx, r.CallID, r.ID, 60)
	}
	db.runs[failed.CallID].Status = store.TranscriptionFailed
	db.runs[running.CallID].Status = store.TranscriptionRunning
	cands := []Candidate{
		{CallID: failed.CallID, RecordingID: failed.ID, DurationSec: 60},
		{CallID: running.CallID, RecordingID: running.ID, DurationSec: 60},
	}
	s := newService(db, fakeAudio{}, &fakeRecognizer{}, defaultCfg)

	if n, err := s.Enqueue(ctx, cands, false); err != nil || n != 0 {
		t.Fatalf("without again: %d, %v", n, err)
	}
	if db.status(failed.CallID) != store.TranscriptionFailed {
		t.Fatal("a failed run was disturbed without being asked")
	}
	if n, err := s.Enqueue(ctx, cands, true); err != nil || n != 1 {
		t.Fatalf("with again: %d, %v; want only the failed run restarted", n, err)
	}
	if r := db.runs[failed.CallID]; r.Status != store.TranscriptionPending || r.Run != 2 {
		t.Errorf("failed run = %+v", r)
	}
	if db.status(running.CallID) != store.TranscriptionRunning {
		t.Error("a run in flight was restarted")
	}
}

// channelsOf transcribes a stereo recording with speech on one side only and
// returns the channel labels of the segments, given the call's CDR (nil: none).
func channelsOf(t *testing.T, cdr *store.CDR, speechLeft bool) []string {
	t.Helper()
	db := newFakeLedger()
	rec := db.addRecording(30)
	if cdr != nil {
		db.cdrs[rec.CallID] = *cdr
	}
	speech, quiet := tone(8000*3, 1000), make([]int16, 8000*3)
	l, r := speech, quiet
	if !speechLeft {
		l, r = quiet, speech
	}
	asr := &fakeRecognizer{reply: func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{Text: "x", Sentences: []transcribe.BatchSentence{{Text: "x", EndMs: 100}}}, nil
	}}
	s := newService(db, fakeAudio{b: stereo(l, r, 8000)}, asr, defaultCfg)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the run to finish", func() bool { return db.status(rec.CallID) == store.TranscriptionDone })
	var out []string
	for _, g := range db.segs[rec.CallID] {
		out = append(out, g.Channel)
	}
	return out
}

// A call a phone places to the trunk is recorded on the phone's leg, so the
// left channel is the person on the phone, not the customer.
func TestTheChannelsFollowTheRecordedLeg(t *testing.T) {
	t.Parallel()
	flow := uuid.Must(uuid.NewV7())
	cases := []struct {
		name       string
		cdr        *store.CDR
		speechLeft bool
		want       string
	}{
		{"click-to-dial, left", &store.CDR{CallType: "OUTBOUND", AgentIDs: []uuid.UUID{uuid.Must(uuid.NewV7())}}, true, store.ChannelFarEnd},
		{"signed-out phone, left", &store.CDR{CallType: "OUTBOUND"}, true, store.ChannelFarEnd},
		{"click-to-dial, right", &store.CDR{CallType: "OUTBOUND", AgentIDs: []uuid.UUID{uuid.Must(uuid.NewV7())}}, false, store.ChannelCustomer},
		{"inbound", &store.CDR{CallType: "INBOUND"}, true, store.ChannelCustomer},
		{"AI outbound by bot seconds", &store.CDR{CallType: "OUTBOUND", BotSec: 12}, true, store.ChannelCustomer},
		{"AI outbound by flow", &store.CDR{CallType: "OUTBOUND", FlowID: &flow}, true, store.ChannelCustomer},
		{"no CDR", nil, true, store.ChannelCustomer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := channelsOf(t, tc.cdr, tc.speechLeft); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("channels = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// A CDR that cannot be read must not let a run settle with labels that may be
// the wrong way round.
func TestAnUnreadableCDRFailsTheAttemptInsteadOfGuessing(t *testing.T) {
	t.Parallel()
	db := newFakeLedger()
	db.cdrErr = errors.New("connection reset")
	rec := db.addRecording(30)
	asr := &fakeRecognizer{reply: func(int, transcribe.BatchRequest) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{Text: "x", Sentences: []transcribe.BatchSentence{{Text: "x", EndMs: 100}}}, nil
	}}
	s := newService(db, fakeAudio{b: stereo(tone(8000*3, 1000), make([]int16, 8000*3), 8000)}, asr, defaultCfg)
	db.EnqueueTranscription(context.Background(), rec.CallID, rec.ID, 30)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "the run to fail", func() bool { return db.status(rec.CallID) == store.TranscriptionFailed })
	if len(db.segs[rec.CallID]) != 0 {
		t.Errorf("segments written despite the CDR error: %v", db.segs[rec.CallID])
	}
	if len(db.retries) != defaultCfg.MaxAttempts-1 {
		t.Errorf("%d retries, want %d", len(db.retries), defaultCfg.MaxAttempts-1)
	}
	if asr.calls() != 0 {
		t.Errorf("recognizer called %d times before the mapping was known", asr.calls())
	}
}
