// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #626 step 0: every cut an engine makes by itself lands in
// aicc_bot_turn_cuts_total once, named by what its final transcript made of
// it (WORDS, BLANK, MISSING) and the line it fell on (OPENING, LATER,
// CLOSING). The counts are compared before and after: the provider's meter
// instruments are bound through the global meter, so one manual reader
// watches them.

// The shared reader of turn_unanswered_test.go watches the same global meter
// (it can be set once per test binary, so counts are compared before and
// after).

func turnCutCounts(t *testing.T) map[string]int64 {
	t.Helper()
	setTurnReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(turnReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := turnReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_bot_turn_cuts_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				var words, line string
				for _, kv := range dp.Attributes.ToSlice() {
					switch kv.Key {
					case "words":
						words = string(kv.Value.AsString())
					case "line":
						line = string(kv.Value.AsString())
					}
				}
				out[fmt.Sprintf("%s/%s", words, line)] += dp.Value
			}
		}
	}
	return out
}

func TestEveryCutAnEngineMakesIsCountedOnce(t *testing.T) {
	before := turnCutCounts(t)
	cfg := func(armed bool) Config {
		c := Config{Logger: slog.New(slog.NewJSONHandler(&lockedBuffer{}, nil))}
		if armed {
			c.IsEndingArmed = func() bool { return true }
		}
		return c
	}
	newSession := func(c Config) *Session {
		session, err := New(newFakeLeg(media.LawMu), nopModel{}, provider.QwenProfile(), c)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}

	// A cut of the opening, judged by a final with words.
	opening := newSession(Config{
		Session: provider.SessionConfig{OpeningText: "Hi there."},
		Logger:  slog.New(slog.NewJSONHandler(&lockedBuffer{}, nil)),
	})
	opening.openCut(Event{Turn: 1, Played: 500 * 1e6})
	opening.judgeCut(provider.Event{Text: "不需要"})

	// Later lines: a blank final, a cut another cut replaces before its
	// final ever arrived, and one with words.
	later := newSession(cfg(false))
	later.openCut(Event{Turn: 2})
	later.judgeCut(provider.Event{})
	later.openCut(Event{Turn: 3})
	later.openCut(Event{Turn: 4})
	later.judgeCut(provider.Event{Text: "嗯"})

	// A cut over the closing line.
	closing := newSession(cfg(true))
	closing.openCut(Event{Turn: 5})
	closing.judgeCut(provider.Event{Text: "好"})

	after := turnCutCounts(t)
	want := map[string]int64{
		"WORDS/OPENING": 1,
		"BLANK/LATER":   1,
		"MISSING/LATER": 1,
		"WORDS/LATER":   1,
		"WORDS/CLOSING": 1,
	}
	for key, wantCount := range want {
		if got := after[key] - before[key]; got != wantCount {
			t.Errorf("%s = %d, want %d", key, got, wantCount)
		}
	}
}
