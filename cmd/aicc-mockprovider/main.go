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
// With -upstream it is a fault-injecting relay instead: it forwards every
// session to a real provider and, on a seeded share of sessions, breaks one
// turn the way the provider has been seen to (see -fault). The application
// keeps its real profile and only its endpoint changes:
//
//	aicc-mockprovider -addr 127.0.0.1:9099 \
//	    -upstream wss://dashscope.aliyuncs.com/api-ws/v1/realtime \
//	    -fault stall -fault-ratio 0.5 -fault-turns 2-4 -seed 1 &
//	AICC_PROVIDER=qwen \
//	AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9099/api-ws/v1/realtime \
//	ALIYUN_API_KEY=... ./aicc
//
// Every injection is logged at WARN with the relay's session number, so a
// call can be matched against the application's log afterwards.
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
	scenarioFile := flag.String("scenario", "",
		"scenario file: sessions play scripted steps (speak, call a tool, await its result, end) instead of timed turns; see docs/load-tests.md")
	upstream := flag.String("upstream", "",
		"relay mode: forward sessions to this real provider WebSocket URL (ws:// or wss://) instead of playing the mock")
	faultName := flag.String("fault", "",
		"relay mode: stall | stall-hold | unanswered | vad-refused; empty relays untouched")
	faultRatio := flag.Float64("fault-ratio", 1, "relay mode: fraction of sessions that are faulted, 0 to 1")
	faultTurns := flag.String("fault-turns", "2-4",
		"relay mode: the fault hits one turn chosen uniformly in this inclusive range (N or LO-HI), counted over the caller turns the provider's VAD answered")
	seed := flag.Int64("seed", 1, "relay mode: with the session number, decides which sessions are faulted and on which turn")
	hold := flag.Duration("hold", 20*time.Second,
		"relay mode, stall-hold: how long a held response.done is kept when no barge-in releases it")
	quiet := flag.Bool("quiet", false, "log warnings only")
	flag.Parse()

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *upstream != "" {
		fault, err := ParseFault(*faultName)
		turns, err2 := ParseTurnRange(*faultTurns)
		if err != nil || err2 != nil || *faultRatio < 0 || *faultRatio > 1 {
			if err == nil {
				err = err2
			}
			if err == nil {
				err = fmt.Errorf("fault-ratio %v: want 0 to 1", *faultRatio)
			}
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		relay := NewRelay(RelayConfig{
			Upstream: *upstream, Fault: fault, Ratio: *faultRatio, Turns: turns,
			Seed: *seed, Hold: *hold, Log: log,
		})
		log.Info("mockprovider relay listening", "addr", *addr, "upstream", *upstream,
			"fault", fault, "ratio", *faultRatio, "turns", *faultTurns, "seed", *seed)
		if err := http.ListenAndServe(*addr, relay.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	var scenario *mockprovider.Scenario
	if *scenarioFile != "" {
		var err error
		if scenario, err = mockprovider.LoadScenario(*scenarioFile); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
	}

	server := mockprovider.New(mockprovider.Config{
		FirstAudio:   *firstAudio,
		TurnAudio:    *turnAudio,
		TurnEvery:    *turnEvery,
		SpeechBefore: *speechBefore,
		DeltaAudio:   *deltaAudio,
		DeltaPace:    *deltaPace,
		Scenario:     scenario,
		Log:          log,
	})

	go func() {
		for range time.Tick(10 * time.Second) {
			stats := server.Stats()
			log.Info("mockprovider", "live", stats.Live, "peak", stats.Peak,
				"total", stats.Total, "turns", stats.Turns, "cut", stats.Cut,
				"audioSec", stats.AudioSec)
			if scenario != nil {
				log.Info("mockprovider scenario", "toolCalls", stats.ToolCalls,
					"toolResults", stats.ToolResults, "scriptsDone", stats.ScriptsDone,
					"scriptsFailed", stats.ScriptsFailed, "unmatched", stats.Unmatched)
			}
		}
	}()

	log.Info("mockprovider listening", "addr", *addr,
		"firstAudio", *firstAudio, "turnAudio", *turnAudio, "turnEvery", *turnEvery)

	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
