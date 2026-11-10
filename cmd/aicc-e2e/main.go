// SPDX-License-Identifier: Apache-2.0

// Command aicc-e2e drives the end-to-end environment of deploy/e2e.
//
//	aicc-e2e guard -compose-config resolved.json
//	aicc-e2e smoke -api http://10.1.2.10:8080 -compose-config /run/compose.json ...
//
// guard checks a resolved compose configuration (docker compose config
// --format json) and refuses one that is not the isolated environment. smoke
// runs inside the environment's network: it repeats the guard, checks the
// switch's gateways, places one AI outbound call through the public API and
// checks what the callee simulator and the call ledger saw. See
// deploy/e2e/README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/e2e"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "guard":
		err = guard(os.Args[2:])
	case "smoke":
		err = smoke(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aicc-e2e:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: aicc-e2e guard|smoke [flags]")
	os.Exit(2)
}

func guard(args []string) error {
	fs := flag.NewFlagSet("guard", flag.ExitOnError)
	path := fs.String("compose-config", "", "resolved compose configuration (JSON)")
	_ = fs.Parse(args)
	if *path == "" {
		return fmt.Errorf("-compose-config is required")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	if err := e2e.CheckComposeConfig(data); err != nil {
		return err
	}
	fmt.Println("compose configuration is the isolated environment")
	return nil
}

func smoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	var cfg e2e.SmokeConfig
	fs.StringVar(&cfg.API, "api", "", "application REST base, e.g. http://10.1.2.10:8080")
	fs.StringVar(&cfg.ReadyURL, "ready", "", "application readiness URL")
	fs.StringVar(&cfg.CalleeSim, "calleesim", "", "callee simulator report base, e.g. http://10.1.2.13:8771")
	fs.StringVar(&cfg.ESLAddr, "esl", "", "switch event socket address")
	fs.StringVar(&cfg.ESLPassword, "esl-password", os.Getenv("ESL_PASSWORD"), "event socket password")
	fs.StringVar(&cfg.ComposeConfig, "compose-config", "", "resolved compose configuration (JSON)")
	fs.StringVar(&cfg.Username, "username", "", "administrator login")
	fs.StringVar(&cfg.Password, "password", os.Getenv("E2E_PASSWORD"), "administrator password")
	fs.StringVar(&cfg.FlowFile, "flow", "", "flow spec file")
	fs.StringVar(&cfg.DID, "did", "", "the DID the call goes out from (default: a random one)")
	fs.StringVar(&cfg.To, "to", "", "the number the simulator answers")
	fs.StringVar(&cfg.CalledPrefix, "called-prefix", os.Getenv("E2E_CALLED_PREFIX"), "the gateway's destination-prefix, which must reach the wire and no record")
	fs.StringVar(&cfg.OutDir, "out", "", "run directory")
	fs.DurationVar(&cfg.Wait, "wait", 90*time.Second, "bound for each wait")
	_ = fs.Parse(args)
	for name, v := range map[string]string{"api": cfg.API, "ready": cfg.ReadyURL, "calleesim": cfg.CalleeSim,
		"esl": cfg.ESLAddr, "compose-config": cfg.ComposeConfig, "username": cfg.Username, "password": cfg.Password,
		"flow": cfg.FlowFile, "to": cfg.To, "called-prefix": cfg.CalledPrefix, "out": cfg.OutDir} {
		if v == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	return e2e.Smoke(ctx, cfg)
}
