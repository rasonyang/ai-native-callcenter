// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// debugTap keeps the "unmapped provider event" debug lines, which is how the
// client reports server events it has no mapping for (for example the ack of
// conversation.item.truncate).
type debugTap struct {
	mu    sync.Mutex
	types []string
}

func (d *debugTap) Enabled(context.Context, slog.Level) bool { return true }
func (d *debugTap) WithAttrs([]slog.Attr) slog.Handler       { return d }
func (d *debugTap) WithGroup(string) slog.Handler            { return d }
func (d *debugTap) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message != "unmapped provider event" {
		return nil
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "type" {
			d.mu.Lock()
			d.types = append(d.types, a.Value.String())
			d.mu.Unlock()
		}
		return true
	})
	return nil
}

func (d *debugTap) drain() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.types
	d.types = nil
	return out
}

// collect reads events until a RESPONSE_DONE (when untilDone) or the window
// ends, and returns the final output transcript and any error messages.
func collect(s *Realtime, window time.Duration, untilDone bool) (said string, errs []string) {
	timer := time.After(window)
	for {
		select {
		case <-timer:
			return said, errs
		case ev, ok := <-s.Events():
			if !ok {
				return said, errs
			}
			switch ev.Type {
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					said += ev.Text
				}
			case EventTypeError:
				errs = append(errs, fmt.Sprint(ev.Err))
			case EventTypeResponseDone:
				if untilDone {
					return said, errs
				}
			}
		}
	}
}

// truncateVariant runs one session: a spoken fact, an optional truncate, then
// a question about what was said. playedMs < 0 means no truncate.
func truncateVariant(t *testing.T, playedMs int) (first, replay string, raw, errs []string) {
	t.Helper()
	profile := liveProfile(QwenProfile())
	tap := &debugTap{}
	s, err := New(profile, slog.New(tap))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	input, output := profile.FormatsFor(media.LawMu)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := s.Start(ctx, SessionConfig{
		Instructions: "你是电话客服。只用中文简短回答。",
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// qwen greets unprompted; let that turn end before asking for the fact.
	_, errs = collect(s, 25*time.Second, true)
	tap.drain()

	if err := s.SendUserText("请自己编一个四位数的验证码,只用一句话告诉我,格式:您的验证码是XXXX。"); err != nil {
		t.Fatal(err)
	}
	first, e0 := collect(s, 25*time.Second, true)
	errs = append(errs, e0...)
	tap.drain()

	s.mu.Lock()
	itemID := s.responseItemID
	s.mu.Unlock()
	t.Logf("item id to truncate: %q", itemID)
	if playedMs >= 0 {
		if err := s.Interrupt(InterruptReasonSpeech, playedMs); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
	}
	_, e := collect(s, 3*time.Second, false)
	errs = append(errs, e...)
	raw = tap.drain()

	if err := s.SendUserText("我刚才没听清,你上一句话完整说了什么?请原样重复。"); err != nil {
		t.Fatal(err)
	}
	replay, e = collect(s, 25*time.Second, true)
	errs = append(errs, e...)
	return first, replay, raw, errs
}

// TestLiveQwenTruncate checks that qwen accepts conversation.item.truncate at
// audio_end_ms 0 and at a positive value, and logs what the model then says it
// said. Model output is not asserted; only a provider error is a failure.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenTruncate -v ./internal/provider/
func TestLiveQwenTruncate(t *testing.T) {
	requireLive(t, liveProfile(QwenProfile()))
	for _, v := range []struct {
		name     string
		playedMs int
	}{{"A_truncate0", 0}, {"B_truncate300", 300}, {"C_none", -1}} {
		for run := 1; run <= 3; run++ {
			first, replay, raw, errs := truncateVariant(t, v.playedMs)
			t.Logf("%s run %d\n  said:   %q\n  raw after truncate: %s\n  errors: %v\n  replay: %q",
				v.name, run, first, strings.Join(raw, ","), errs, replay)
			if len(errs) > 0 {
				t.Errorf("%s run %d: provider error: %v", v.name, run, errs)
			}
		}
	}
}
