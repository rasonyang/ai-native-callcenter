// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// RunConfig is a whole load run.
type RunConfig struct {
	Call CallConfig

	// Calls is how many run at once.
	Calls int
	// Ramp is how long establishing all of them takes. Arriving at once tests
	// the accept path; arriving over a minute tests the steady state, which is
	// what the capacity budget is about.
	Ramp time.Duration
	// Repeat keeps a slot busy: as each call ends, another takes its place
	// until Total elapses. Zero runs one call per slot.
	Repeat bool
	// Total bounds the run when repeating.
	Total time.Duration

	// Progress, if set, is called once a second with the results so far.
	Progress func(Report)
}

// Report aggregates a run.
type Report struct {
	Elapsed time.Duration

	Placed    int
	Answered  int
	Failed    int
	Failures  map[string]int
	LiveCalls int

	SetupP50Ms      float64
	SetupP95Ms      float64
	FirstAudioP50Ms float64
	FirstAudioP95Ms float64

	FramesSent     int
	FramesReceived int
	LateFrames     int
	LatePercent    float64
	MaxGapMs       float64
}

// Run places the load and reports it.
func Run(ctx context.Context, cfg RunConfig) Report {
	if cfg.Calls <= 0 {
		cfg.Calls = 1
	}
	if cfg.Total <= 0 {
		cfg.Total = cfg.Call.Duration
	}

	collector := &collector{failures: map[string]int{}}
	startedAt := time.Now()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if cfg.Progress != nil {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					cfg.Progress(collector.report(time.Since(startedAt)))
				}
			}
		}()
	}

	// One goroutine per slot. Two hundred of them is nothing next to what the
	// application under test is carrying, and it keeps each call's timing
	// independent of every other's.
	var wg sync.WaitGroup
	stagger := time.Duration(0)
	if cfg.Ramp > 0 {
		stagger = cfg.Ramp / time.Duration(cfg.Calls)
	}
	deadline := startedAt.Add(cfg.Total)

	for slot := range cfg.Calls {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			select {
			case <-time.After(time.Duration(slot) * stagger):
			case <-runCtx.Done():
				return
			}
			for {
				collector.starting()
				result := PlaceCall(runCtx, cfg.Call)
				collector.finished(result)
				if !cfg.Repeat || time.Now().After(deadline) || runCtx.Err() != nil {
					return
				}
			}
		}(slot)
	}
	wg.Wait()

	return collector.report(time.Since(startedAt))
}

// collector accumulates results while the run is still going.
type collector struct {
	mu sync.Mutex

	live     int
	placed   int
	answered int
	failed   int
	failures map[string]int

	setups      []float64
	firstAudios []float64

	framesSent     int
	framesReceived int
	lateFrames     int
	maxGapMs       float64
}

func (c *collector) starting() {
	c.mu.Lock()
	c.live++
	c.placed++
	c.mu.Unlock()
}

func (c *collector) finished(result CallResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.live--
	if result.Err != nil {
		c.failed++
		c.failures[classify(result.Err)]++
		return
	}
	c.answered++
	c.setups = append(c.setups, result.SetupMs)
	if result.FirstAudioMs >= 0 {
		c.firstAudios = append(c.firstAudios, result.FirstAudioMs)
	}
	c.framesSent += result.FramesSent
	c.framesReceived += result.FramesReceived
	c.lateFrames += result.LateFrames
	if result.MaxGapMs > c.maxGapMs {
		c.maxGapMs = result.MaxGapMs
	}
}

func (c *collector) report(elapsed time.Duration) Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	report := Report{
		Elapsed:        elapsed,
		Placed:         c.placed,
		Answered:       c.answered,
		Failed:         c.failed,
		Failures:       map[string]int{},
		LiveCalls:      c.live,
		FramesSent:     c.framesSent,
		FramesReceived: c.framesReceived,
		LateFrames:     c.lateFrames,
		MaxGapMs:       c.maxGapMs,
	}
	maps.Copy(report.Failures, c.failures)
	report.SetupP50Ms = percentile(c.setups, 0.50)
	report.SetupP95Ms = percentile(c.setups, 0.95)
	report.FirstAudioP50Ms = percentile(c.firstAudios, 0.50)
	report.FirstAudioP95Ms = percentile(c.firstAudios, 0.95)
	if c.framesReceived > 0 {
		report.LatePercent = 100 * float64(c.lateFrames) / float64(c.framesReceived)
	}
	return report
}

// String renders a report the way a run wants to read at a glance.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%5.0fs  live %3d  placed %4d  answered %4d  failed %3d",
		r.Elapsed.Seconds(), r.LiveCalls, r.Placed, r.Answered, r.Failed)
	fmt.Fprintf(&b, "  setup p50/p95 %.0f/%.0fms  first audio p50/p95 %.0f/%.0fms",
		r.SetupP50Ms, r.SetupP95Ms, r.FirstAudioP50Ms, r.FirstAudioP95Ms)
	fmt.Fprintf(&b, "  frames %d↑ %d↓  late %.3f%% (max gap %.0fms)",
		r.FramesSent, r.FramesReceived, r.LatePercent, r.MaxGapMs)
	for reason, count := range r.Failures {
		fmt.Fprintf(&b, "\n         %d × %s", count, reason)
	}
	return b.String()
}

func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := int(q * float64(len(sorted)-1))
	return sorted[index]
}

// classify collapses an error to something countable: two hundred calls
// failing the same way should read as one line, not two hundred.
// Errors nest as "what we were doing: what went wrong: detail". The first
// clause is the stable part; the detail carries addresses and ports, which
// would make every call its own category.
func classify(err error) string {
	if head, _, found := strings.Cut(err.Error(), ": "); found {
		return head
	}
	return err.Error()
}
