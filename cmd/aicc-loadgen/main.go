// SPDX-License-Identifier: Apache-2.0

// Command aicc-loadgen places SIP calls at the application's voice leg and
// reports what came back.
//
//	aicc-loadgen -target 127.0.0.1:6060 -calls 200 -ramp 60s -duration 30m
//
// It is the switch's side of the bot leg: the same headers, the same PCMU
// offer, the same twenty-millisecond frames. See docs/load-tests.md for the
// stages this serves and what each one has to show.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/loadgen"
)

func main() {
	target := flag.String("target", "127.0.0.1:6060", "the application's SIP address")
	did := flag.String("did", "95001", "the number to call")
	language := flag.String("language", "en", "the number's language")
	calls := flag.Int("calls", 1, "concurrent calls")
	ramp := flag.Duration("ramp", 0, "how long establishing all of them takes")
	duration := flag.Duration("duration", 30*time.Second, "how long one call stays up")
	total := flag.Duration("total", 0,
		"how long the run lasts; longer than -duration replaces each call as it ends")
	lateAfter := flag.Duration("late-after", 40*time.Millisecond,
		"a downlink gap this long or longer counts as late")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := loadgen.RunConfig{
		Call: loadgen.CallConfig{
			Target:    *target,
			DID:       *did,
			Language:  *language,
			Duration:  *duration,
			LateAfter: *lateAfter,
		},
		Calls:    *calls,
		Ramp:     *ramp,
		Repeat:   *total > *duration,
		Total:    *total,
		Progress: func(r loadgen.Report) { fmt.Println(r) },
	}

	fmt.Printf("calling %s: %d concurrent, %s each, ramp %s, run %s\n",
		*target, *calls, *duration, *ramp, maxDuration(*total, *duration))

	report := loadgen.Run(ctx, cfg)
	fmt.Println()
	fmt.Println(report)

	// A run that could not place its calls is a failed run, whatever the
	// pacing of the ones that did get through says.
	if report.Answered == 0 || report.Failed > 0 {
		os.Exit(1)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
