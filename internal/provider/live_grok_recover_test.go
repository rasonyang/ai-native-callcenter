// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// TestLiveGrokRecoversADroppedLine reproduces issue #231 against the real
// grok engine, through the repo's own client, and checks the fix.
//
// grok drops a response.create that carries only a per-response override once
// the conversation has a turn in it (grok-findings 5.2: 0/5, no
// response.created, no error). The grok profile avoids that frame with
// IgnoresOverrideOnceTalking; this test turns the trait off on a copy of the
// profile, so that SpeakText sends exactly the frame that was dropped, and
// watches what the client does when the provider never creates the response:
// after responseCreatedDeadline it logs a WARN, counts
// aicc_provider_response_unanswered_total and retries once with the direction
// put in the conversation (the form 5.2 measured 3/3).
//
// The state matches TestLiveGrokResponseOverride's "after-turn-override": the
// greeting, then one caller turn spoken as audio (testdata/q_en_hours.ulaw,
// "What are your opening hours?", fed as pcmu at real-time pace with room tone
// around it) and its reply, a one-second pause, then the line. grok has no
// NeedsDirectedLineInConversation, so a closing line goes override-only.
//
// Each run is classified. The test fails only if the floor stayed stuck (no
// response started and no DROPPED outcome within 2×deadline + 2 s), or if a
// retry's turn was made and did not say the line. grok not dropping is a
// finding, logged, not a failure.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run TestLiveGrokRecoversADroppedLine -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 5) is the number of sessions.
func TestLiveGrokRecoversADroppedLine(t *testing.T) {
	profile := liveProfile(GrokProfile())
	profile.IgnoresOverrideOnceTalking = false
	requireLive(t, profile)
	runs := 5
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	clip := grokClip(t, "en_hours")
	// Install the metric reader before any session records into it.
	_ = unansweredCounts(t, profile.Name)

	tally := map[string]int{}
	for run := 1; run <= runs; run++ {
		class := recoverDroppedLineRun(t, profile, clip, run)
		tally[class]++
	}
	t.Logf("TALLY (%d runs, deadline %v): %v", runs, responseCreatedTimeout, tally)
}

const (
	recoverLine         = "Thank you for calling NovaNet. Goodbye."
	recoverInstructions = "You answer the phone for NovaNet, an internet provider. " +
		"Speak English. Reply in one short sentence."
)

// recoverDroppedLineRun runs one session and returns its classification.
func recoverDroppedLineRun(t *testing.T, profile Profile, clip []byte, run int) string {
	t.Helper()
	label := fmt.Sprintf("recover#%d", run)

	logs := &capturedLogs{}
	session, err := New(profile, slog.New(&captureHandler{logs: logs, level: slog.LevelDebug}))
	if err != nil {
		t.Fatalf("[%s] new session: %v", label, err)
	}
	defer func() { _ = session.Close(context.Background()) }()

	input, output := profile.FormatsFor(media.LawMu)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, SessionConfig{
		Instructions: recoverInstructions,
		Language:     "en",
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
	}); err != nil {
		t.Errorf("[%s] start: %v", label, err)
		return "OTHER"
	}
	feeder := &clipFeeder{}
	stop := make(chan struct{})
	defer close(stop)
	go feeder.run(session, stop)

	// read reads one event or reports false on timeout or a closed session.
	read := func(wait time.Duration) (Event, bool) {
		select {
		case ev, ok := <-session.Events():
			return ev, ok
		case <-time.After(wait):
			return Event{}, false
		}
	}
	// turnEnd reads until a response ends.
	turnEnd := func(wait time.Duration) bool {
		end := time.Now().Add(wait)
		for time.Now().Before(end) {
			ev, ok := read(time.Until(end))
			if !ok {
				return false
			}
			switch ev.Type {
			case EventTypeResponseDone, EventTypeInterrupted:
				return true
			case EventTypeError:
				t.Logf("[%s] provider error (fatal=%v): %v", label, ev.IsFatal, ev.Err)
				if ev.IsFatal {
					return false
				}
			}
		}
		return false
	}
	drain := func(d time.Duration) {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if _, ok := read(time.Until(end)); !ok {
				return
			}
		}
	}

	if !turnEnd(30 * time.Second) {
		t.Errorf("[%s] the greeting never finished", label)
		return "OTHER"
	}
	drain(500 * time.Millisecond)
	feeder.say(clip)
	if !turnEnd(30 * time.Second) {
		t.Errorf("[%s] the caller turn got no reply", label)
		return "OTHER"
	}
	drain(1 * time.Second)

	before := unansweredCounts(t, profile.Name)
	logMark := logs.len()
	t0 := time.Now()
	if err := session.SpeakText(recoverLine, true); err != nil {
		t.Errorf("[%s] SpeakText: %v", label, err)
		return "OTHER"
	}

	// Collect for up to 20 s: every response started after the line was asked
	// for, with its transcript; stop 1.5 s after the first one ends.
	type turn struct {
		startedAt, doneAt time.Duration
		final, deltas     string
		isDone            bool
	}
	var turns []*turn
	type entry struct {
		at   time.Duration
		text string
	}
	var timeline []entry
	mark := func(format string, args ...any) {
		timeline = append(timeline, entry{time.Since(t0), fmt.Sprintf(format, args...)})
	}
	stopAt := t0.Add(20 * time.Second)
	for time.Now().Before(stopAt) {
		ev, ok := read(time.Until(stopAt))
		if !ok {
			break
		}
		var cur *turn
		if len(turns) > 0 {
			cur = turns[len(turns)-1]
		}
		switch ev.Type {
		case EventTypeResponseStarted:
			turns = append(turns, &turn{startedAt: time.Since(t0)})
			mark("RESPONSE_STARTED")
		case EventTypeOutputTranscript:
			if cur == nil {
				break
			}
			if ev.IsFinal {
				cur.final = ev.Text
				mark("OUTPUT_TRANSCRIPT final %q", ev.Text)
			} else {
				cur.deltas += ev.Text
			}
		case EventTypeResponseDone, EventTypeInterrupted:
			mark("%s", ev.Type)
			if cur != nil && !cur.isDone {
				cur.isDone, cur.doneAt = true, time.Since(t0)
				if next := time.Now().Add(1500 * time.Millisecond); next.Before(stopAt) {
					stopAt = next
				}
			}
		case EventTypeError:
			mark("ERROR fatal=%v %v", ev.IsFatal, ev.Err)
		case EventTypeSpeechStarted, EventTypeSpeechStopped, EventTypeInputTranscript:
			mark("%s %s", ev.Type, ev.Text)
		}
	}
	after := unansweredCounts(t, profile.Name)
	delta := map[string]int64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			delta[k] = d
		}
	}

	// The WARNs the fix logs, by outcome.
	type warn struct {
		at      time.Duration
		outcome string
		kind    string
		waited  string
	}
	var warns []warn
	for _, rec := range logs.since(logMark) {
		if rec.level < slog.LevelWarn {
			continue
		}
		outcome := "OTHER"
		switch {
		case strings.Contains(rec.msg, "nor the retry"):
			outcome = "DROPPED"
		case strings.Contains(rec.msg, "asking once more"):
			outcome = "RETRIED"
		case strings.Contains(rec.msg, "waiting for the floor instead"):
			outcome = "SUPERSEDED"
		}
		w := warn{at: rec.at.Sub(t0), outcome: outcome, kind: rec.attrs["kind"], waited: rec.attrs["waitedMs"]}
		warns = append(warns, w)
		timeline = append(timeline, entry{w.at, fmt.Sprintf("WARN %s kind=%s waitedMs=%s isDirectionAdded=%s msg=%q",
			outcome, w.kind, w.waited, rec.attrs["isDirectionAdded"], rec.msg)})
	}

	said := func(tr *turn) string {
		if tr.final != "" {
			return tr.final
		}
		return tr.deltas
	}
	var retriedAt, droppedAt time.Duration = -1, -1
	for _, w := range warns {
		if w.outcome == "RETRIED" && retriedAt < 0 {
			retriedAt = w.at
		}
		if w.outcome == "DROPPED" && droppedAt < 0 {
			droppedAt = w.at
		}
	}
	if droppedAt < 0 && delta[string(responseKindLine)+"/DROPPED"] > 0 {
		droppedAt = 0 // counted, though no WARN was matched
	}

	stuckBound := 2*responseCreatedTimeout + 2*time.Second
	var firstStart time.Duration = -1
	if len(turns) > 0 {
		firstStart = turns[0].startedAt
	}
	if (firstStart < 0 || firstStart > stuckBound) && (droppedAt < 0 || droppedAt > stuckBound) {
		t.Errorf("[%s] the floor stayed stuck: no response started and no DROPPED outcome within %v", label, stuckBound)
	}

	// The turn the retry made: the first one started after the RETRIED WARN.
	var retryTurn *turn
	if retriedAt >= 0 {
		for _, tr := range turns {
			if tr.startedAt >= retriedAt {
				retryTurn = tr
				break
			}
		}
		if retryTurn != nil && !saysLine(said(retryTurn), recoverLine) {
			t.Errorf("[%s] the retry's turn did not say the line: %q", label, said(retryTurn))
		}
	}

	class := "OTHER"
	switch {
	case retriedAt >= 0 && retryTurn != nil && saysLine(said(retryTurn), recoverLine):
		class = "DROPPED_THEN_RECOVERED"
	case droppedAt >= 0:
		class = "DROPPED_UNRECOVERED"
	case len(warns) == 0 && firstStart >= 0 && firstStart <= responseCreatedTimeout:
		class = "NOT_DROPPED"
	}

	ms := func(d time.Duration) string {
		if d < 0 {
			return "-"
		}
		return strconv.FormatInt(d.Milliseconds(), 10)
	}
	var startsMs, saids []string
	for _, tr := range turns {
		startsMs = append(startsMs, ms(tr.startedAt))
		saids = append(saids, fmt.Sprintf("%q", said(tr)))
	}
	slices.SortStableFunc(timeline, func(a, b entry) int { return cmp.Compare(a.at, b.at) })
	lines := make([]string, len(timeline))
	for i, e := range timeline {
		lines[i] = fmt.Sprintf("+%dms %s", e.at.Milliseconds(), e.text)
	}
	t.Logf("[%s] TIMELINE\n    %s", label, strings.Join(lines, "\n    "))
	t.Logf("[%s] SUMMARY %s: SpeakText->WARN RETRIED %sms, ->DROPPED %sms, ->RESPONSE_STARTED %v ms, metric %v, said %v",
		label, class, ms(retriedAt), ms(droppedAt), startsMs, delta, saids)
	return class
}

// saysLine reports whether a transcript contains the line, ignoring case,
// punctuation and spacing.
func saysLine(transcript, line string) bool {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	want := norm(line)
	return want != "" && strings.Contains(norm(transcript), want)
}

// clipFeeder sends room tone every 20 ms, and the queued clip in its place
// while there is one, so a caller turn is spoken at real-time pace on a line
// that is never silent. It sends G.711 mu-law as is: the session's input
// format on a profile that accepts G.711.
type clipFeeder struct {
	mu    sync.Mutex
	queue []byte
}

func (f *clipFeeder) say(clip []byte) {
	f.mu.Lock()
	f.queue = append(f.queue, clip...)
	f.mu.Unlock()
}

func (f *clipFeeder) run(session *Realtime, stop <-chan struct{}) {
	ticker := time.NewTicker(FrameInterval)
	defer ticker.Stop()
	samples := make([]int16, media.FrameSamples)
	tone := make([]byte, 0, media.FrameSamples)
	phase := 0
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		f.mu.Lock()
		var frame []byte
		if len(f.queue) >= media.FrameSamples {
			frame = append([]byte(nil), f.queue[:media.FrameSamples]...)
			f.queue = f.queue[media.FrameSamples:]
		} else {
			f.queue = nil
		}
		f.mu.Unlock()
		if frame == nil {
			for i := range samples {
				samples[i] = int16(30 * math.Sin(float64(phase+i)/7.0))
			}
			phase += media.FrameSamples
			tone = media.LawMu.Encode(tone[:0], samples)
			frame = tone
		}
		if err := session.SendAudio(frame); err != nil {
			return
		}
	}
}

// capturedLog is one record the session logged, with the time it was logged.
type capturedLog struct {
	at    time.Time
	level slog.Level
	msg   string
	attrs map[string]string
}

type capturedLogs struct {
	mu      sync.Mutex
	records []capturedLog
}

func (c *capturedLogs) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

func (c *capturedLogs) since(i int) []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedLog(nil), c.records[i:]...)
}

// captureHandler is a slog.Handler that keeps every record for the test to
// read, with its attributes flattened to strings (groups ignored).
type captureHandler struct {
	logs  *capturedLogs
	level slog.Level
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *captureHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.logs.mu.Lock()
	h.logs.records = append(h.logs.records, capturedLog{at: time.Now(), level: rec.Level, msg: rec.Message, attrs: attrs})
	h.logs.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }
