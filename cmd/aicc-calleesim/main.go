// SPDX-License-Identifier: Apache-2.0

// Command aicc-calleesim plays the called party of outbound calls: a SIP UAS
// that answers as a scenario file says, and records every dialog.
//
//	aicc-calleesim -listen 192.168.31.55:5080 -scenario callees.json -out report.json
//
// The record is served as JSON at /report on -http while it runs, and written
// to -out on exit. A run with an unexpected dialog (a number the scenario does
// not know) exits 1. Like aicc-loadgen it is built from the repository and is
// not shipped in a release.
//
// With -called-prefix the numbers are the dialled numbers without it: the
// prefix a trunk's gateway adds to the Request-URI user is removed before the
// lookup, the record keeps the dialled number, and an INVITE that lacks the
// prefix gets 404 like an unknown number.
//
// A scenario keys a list of behaviours by the dialled number. Attempt n on a
// number uses entry n; attempts past the end use the last entry. A number not
// listed gets 404 and fails the run.
//
//	{"numbers": {
//	  "13800000001": [
//	    {"finalStatus": 486},
//	    {"provisionals": [{"status": 180}, {"status": 183, "atMs": 200, "earlyMedia": true}],
//	     "finalAfterMs": 3000, "byeAfterMs": 5000, "uplink": {"kind": "TONE"}}
//	  ]
//	}}
//
// Uplink kinds are SILENCE (the default), TONE and WAV (8 kHz mono 16-bit PCM,
// played once).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:5080", "SIP UDP address to bind")
	advertise := flag.String("advertise", "", "IP to put in Contact and SDP; default is the bound host or the route to the caller")
	scenarioPath := flag.String("scenario", "", "scenario file (JSON)")
	httpAddr := flag.String("http", "127.0.0.1:8771", "address of the JSON report endpoint; empty disables it")
	out := flag.String("out", "", "file to write the report to on exit")
	rtpMin := flag.Int("rtp-min", 0, "lowest RTP port; 0 lets the system choose")
	rtpMax := flag.Int("rtp-max", 0, "highest RTP port")
	calledPrefix := flag.String("called-prefix", "", "string the gateway puts before the number in the Request-URI user (destination-prefix); it is removed before the scenario lookup, and an INVITE without it is unexpected; empty expects none")
	flag.Parse()

	if *scenarioPath == "" {
		fmt.Fprintln(os.Stderr, "aicc-calleesim: -scenario is required")
		os.Exit(2)
	}
	scenario, err := calleesim.LoadScenario(*scenarioPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aicc-calleesim:", err)
		os.Exit(2)
	}
	if *rtpMin > 0 && *rtpMax < *rtpMin {
		fmt.Fprintln(os.Stderr, "aicc-calleesim: -rtp-max must be at least -rtp-min")
		os.Exit(2)
	}

	sim, err := calleesim.New(calleesim.Config{
		Addr: *listen, Advertise: *advertise, Scenario: scenario,
		RTPPortMin: *rtpMin, RTPPortMax: *rtpMax, CalledPrefix: *calledPrefix, Log: slog.Default(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "aicc-calleesim:", err)
		os.Exit(2)
	}
	fmt.Printf("callee simulator on %s, %d numbers\n", sim.Addr(), len(scenario.Numbers))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *httpAddr != "" {
		listener, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "aicc-calleesim:", err)
			os.Exit(2)
		}
		server := &http.Server{Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "aicc-calleesim: report endpoint:", err)
			}
		}()
		defer server.Close()
		fmt.Printf("report at http://%s/report\n", listener.Addr())
	}

	if err := sim.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "aicc-calleesim:", err)
	}

	report := sim.Report()
	if *out != "" {
		if err := report.WriteFile(*out); err != nil {
			fmt.Fprintln(os.Stderr, "aicc-calleesim: write report:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("%d dialogs, %d unexpected\n", len(report.Dialogs), report.UnexpectedCount)
	if report.Failed {
		os.Exit(1)
	}
}
