// SPDX-License-Identifier: Apache-2.0

// Command aicc-mockprovider serves the Realtime protocol without a vendor
// behind it, so a load test can run two hundred conversations for free.
//
// The application reaches it the way it reaches anything speaking that
// protocol — by endpoint:
//
//	aicc-mockprovider -addr 127.0.0.1:9099 &
//	AICC_PROVIDER=openai \
//	AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9099/v1/realtime \
//	OPENAI_API_KEY=not-a-key ./aicc
//
// See docs/load-tests.md.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "where to listen")
	firstAudio := flag.Duration("first-audio", 400*time.Millisecond,
		"wait between accepting a turn and the first audio of it")
	turnAudio := flag.Duration("turn-audio", 4*time.Second, "speech per turn")
	turnEvery := flag.Duration("turn-every", 6*time.Second, "gap between turns")
	speechBefore := flag.Duration("speech-before", 0,
		"how long the caller is heard speaking before a turn; 0 emits no speech events")
	deltaAudio := flag.Duration("delta-audio", 100*time.Millisecond, "audio per delta")
	deltaPace := flag.Duration("delta-pace", 0,
		"wait between deltas; 0 delivers a turn as fast as the socket takes it, as real providers do")
	quiet := flag.Bool("quiet", false, "log warnings only")
	flag.Parse()

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	server := mockprovider.New(mockprovider.Config{
		FirstAudio:   *firstAudio,
		TurnAudio:    *turnAudio,
		TurnEvery:    *turnEvery,
		SpeechBefore: *speechBefore,
		DeltaAudio:   *deltaAudio,
		DeltaPace:    *deltaPace,
		Log:          log,
	})

	go func() {
		for range time.Tick(10 * time.Second) {
			stats := server.Stats()
			log.Info("mockprovider", "live", stats.Live, "peak", stats.Peak,
				"total", stats.Total, "turns", stats.Turns, "cut", stats.Cut,
				"audioSec", stats.AudioSec)
		}
	}()

	log.Info("mockprovider listening", "addr", *addr,
		"firstAudio", *firstAudio, "turnAudio", *turnAudio, "turnEvery", *turnEvery)

	if err := http.ListenAndServe(*addr, server); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
