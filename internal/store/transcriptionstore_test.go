// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

func TestTheTranscriptionQueueLifecycle(t *testing.T) {
	st, ctx := liveStore(t)
	l := st.Ledger()
	callID, recID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	// Enqueue is idempotent.
	if ok, err := l.EnqueueTranscription(ctx, callID, recID, 120); err != nil || !ok {
		t.Fatalf("first enqueue: %v %v", ok, err)
	}
	if ok, err := l.EnqueueTranscription(ctx, callID, recID, 999); err != nil || ok {
		t.Fatalf("second enqueue: %v %v, want it refused quietly", ok, err)
	}
	run, err := l.GetTranscription(ctx, callID)
	if err != nil || run.Status != TranscriptionPending || run.AudioSec != 120 || run.Run != 1 {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// A pending run cannot be requeued.
	if ok, _ := l.RequeueTranscription(ctx, callID, recID, 120); ok {
		t.Error("a pending run was requeued")
	}

	// Claim counts the attempt and names the model; a second claim finds nothing.
	got, ok, err := l.ClaimTranscription(ctx, "m1")
	if err != nil || !ok || got.CallID != callID || got.Attempts != 1 || got.Status != TranscriptionRunning || got.Model != "m1" {
		t.Fatalf("claim = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := l.ClaimTranscription(ctx, "m1"); ok {
		t.Fatal("a running run was claimed twice")
	}

	// A retry is not due before its time.
	if err := l.RetryTranscription(ctx, callID, got.Run, "boom", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.ClaimTranscription(ctx, "m1"); ok {
		t.Fatal("a retry was claimed before its backoff")
	}
	run, _ = l.GetTranscription(ctx, callID)
	if run.Status != TranscriptionPending || run.Error != "boom" {
		t.Fatalf("after retry: %+v", run)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE call_transcriptions SET next_attempt_at = now() WHERE call_id = $1`, callID); err != nil {
		t.Fatal(err)
	}
	got, ok, _ = l.ClaimTranscription(ctx, "m1")
	if !ok || got.Attempts != 2 {
		t.Fatalf("second claim = %+v %v", got, ok)
	}

	// Completing writes the segments and clears the error, together.
	segs := []TranscriptionSegment{
		{Seq: 1, Channel: ChannelCustomer, OffsetMs: 0, EndMs: 900, Text: "hello", Language: "en"},
		{Seq: 2, Channel: ChannelFarEnd, OffsetMs: 1000, EndMs: 2000, Text: "hi"},
	}
	if ok, err := l.CompleteTranscription(ctx, callID, got.Run, segs); err != nil || !ok {
		t.Fatalf("complete: %v %v", ok, err)
	}
	run, _ = l.GetTranscription(ctx, callID)
	if run.Status != TranscriptionDone || run.Error != "" || run.FinishedAt == nil || run.StartedAt == nil {
		t.Fatalf("after complete: %+v", run)
	}
	gotSegs, err := l.TranscriptionSegments(ctx, callID)
	if err != nil || len(gotSegs) != 2 || gotSegs[1].Channel != ChannelFarEnd || gotSegs[0].Language != "en" {
		t.Fatalf("segments = %+v, %v", gotSegs, err)
	}
	// A finished run cannot be completed again.
	if ok, _ := l.CompleteTranscription(ctx, callID, got.Run, nil); ok {
		t.Error("a DONE run was completed twice")
	}

	// A request starts a new run, whose result replaces the old segments.
	if ok, err := l.RequeueTranscription(ctx, callID, recID, 130); err != nil || !ok {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	run, _ = l.GetTranscription(ctx, callID)
	if run.Run != 2 || run.Attempts != 0 || run.Status != TranscriptionPending || run.AudioSec != 130 || run.FinishedAt != nil {
		t.Fatalf("after requeue: %+v", run)
	}
	got, _, _ = l.ClaimTranscription(ctx, "m1")
	// The superseded run (1) can neither complete nor fail the new one.
	if ok, _ := l.CompleteTranscription(ctx, callID, 1, segs); ok {
		t.Error("a superseded run completed its successor")
	}
	if err := l.FailTranscription(ctx, callID, 1, "late"); err != nil {
		t.Fatal(err)
	}
	if run, _ = l.GetTranscription(ctx, callID); run.Status != TranscriptionRunning {
		t.Errorf("a superseded run's failure reached the new run: %+v", run)
	}
	if ok, err := l.CompleteTranscription(ctx, callID, got.Run, segs[:1]); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if gotSegs, _ = l.TranscriptionSegments(ctx, callID); len(gotSegs) != 1 {
		t.Errorf("%d segments after the second run, want the first run's replaced by 1", len(gotSegs))
	}
}

func TestAFailedRunCanBeRequeuedAndAStrandedOneIsReset(t *testing.T) {
	st, ctx := liveStore(t)
	l := st.Ledger()
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, id := range []uuid.UUID{a, b} {
		if _, err := l.EnqueueTranscription(ctx, id, uuid.Must(uuid.NewV7()), 60); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _ := l.ClaimTranscription(ctx, "m")
	second, _, _ := l.ClaimTranscription(ctx, "m")
	if first.CallID == second.CallID {
		t.Fatal("two claims took the same run")
	}
	if err := l.FailTranscription(ctx, first.CallID, first.Run, "gave up"); err != nil {
		t.Fatal(err)
	}
	if run, _ := l.GetTranscription(ctx, first.CallID); run.Status != TranscriptionFailed || run.Error != "gave up" {
		t.Fatalf("failed run = %+v", run)
	}
	if ok, _ := l.RequeueTranscription(ctx, first.CallID, uuid.Must(uuid.NewV7()), 60); !ok {
		t.Error("a FAILED run could not be requeued")
	}

	// The other is still RUNNING: a restart returns it, attempt counted.
	n, err := l.ResetRunningTranscriptions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v", n, err)
	}
	if run, _ := l.GetTranscription(ctx, second.CallID); run.Status != TranscriptionPending || run.Attempts != 1 {
		t.Errorf("stranded run = %+v", run)
	}
	if _, err := l.GetTranscription(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("an unknown call: %v", err)
	}
}

func TestTheDailyAudioCountsWhatWasStarted(t *testing.T) {
	st, ctx := liveStore(t)
	l := st.Ledger()
	for _, sec := range []int{60, 120} {
		if _, err := l.EnqueueTranscription(ctx, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), sec); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := l.TranscribedAudioSecSince(ctx, time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("queued but unstarted audio counted: %d", n)
	}
	l.ClaimTranscription(ctx, "m")
	l.ClaimTranscription(ctx, "m")
	if n, _ := l.TranscribedAudioSecSince(ctx, time.Now().Add(-time.Hour)); n != 180 {
		t.Errorf("counted %d s, want 180", n)
	}
	if n, _ := l.TranscribedAudioSecSince(ctx, time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("audio before the window counted: %d", n)
	}
}

func TestLiveSpeakersAndTheBackfillQuery(t *testing.T) {
	st, ctx := liveStore(t)
	l := st.Ledger()
	callID := uuid.Must(uuid.NewV7())
	for i, sp := range []string{"CUSTOMER", "BOT", "CUSTOMER"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO transcripts (call_id, seq, occurred_at, speaker, kind, content)
			VALUES ($1, $2, now(), $3, 'TEXT', '{}')`, callID, i+1, sp); err != nil {
			t.Fatal(err)
		}
	}
	// A tool call is not a line anyone said.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO transcripts (call_id, seq, occurred_at, speaker, kind, content)
		VALUES ($1, 9, now(), 'HUMAN_AGENT', 'TOOL_CALL', '{}')`, callID); err != nil {
		t.Fatal(err)
	}
	sp, err := l.LiveTranscriptSpeakers(ctx, callID)
	if err != nil || len(sp) != 2 {
		t.Fatalf("speakers = %v, %v", sp, err)
	}

	rec, err := l.InsertRecording(ctx, Recording{CallID: callID, Backend: RecordingBackendFS, ObjectKey: "a.wav", SizeBytes: 1000, DurationSec: 42})
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.Must(uuid.NewV7())
	if _, err := l.InsertRecording(ctx, Recording{CallID: other, Backend: RecordingBackendFS, ObjectKey: "b.wav"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.EnqueueTranscription(ctx, other, uuid.Must(uuid.NewV7()), 5); err != nil {
		t.Fatal(err)
	}
	got, err := l.CallsAwaitingTranscription(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(got) != 1 || got[0].CallID != callID || got[0].RecordingID != rec.ID || got[0].DurationSec != 42 {
		t.Fatalf("awaiting = %+v, %v", got, err)
	}
}

// 00035 only adds tables, but a deployment that already holds recordings and
// transcripts must come through it with those rows untouched, and the CHECKs
// must hold the vocabulary the API serves.
func TestMigrationAddsPostCallTranscriptionBesideExistingRows(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 34); err != nil {
		t.Fatalf("migrating to 34: %v", err)
	}
	callID := uuid.Must(uuid.NewV7())
	if _, err := db.ExecContext(ctx, `INSERT INTO recordings (id, call_id, backend, object_key, duration_sec)
		VALUES ($1, $2, 'FS', 'x.wav', 30)`, uuid.Must(uuid.NewV7()), callID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO transcripts (call_id, seq, occurred_at, speaker, kind, content)
		VALUES ($1, 1, now(), 'CUSTOMER', 'TEXT', '{}')`, callID); err != nil {
		t.Fatal(err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM recordings) + (SELECT count(*) FROM transcripts)`).Scan(&n); err != nil || n != 2 {
		t.Errorf("existing rows = %d, %v; the migration disturbed them", n, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO call_transcriptions (call_id, recording_id, status) VALUES ($1, $2, 'QUEUED')`, callID, uuid.Must(uuid.NewV7())); err == nil {
		t.Error("a status outside PENDING/RUNNING/DONE/FAILED was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO call_transcription_segments (call_id, seq, channel, offset_ms, end_ms, text) VALUES ($1, 1, 'BOT', 0, 1, 'x')`, callID); err == nil {
		t.Error("a channel outside CUSTOMER/FAR_END was accepted")
	}
	// And back down, which drops both tables and nothing else.
	if err := goose.DownToContext(ctx, db, "migrations", 34); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recordings`).Scan(&n); err != nil || n != 1 {
		t.Errorf("recordings after rollback = %d, %v", n, err)
	}
}
