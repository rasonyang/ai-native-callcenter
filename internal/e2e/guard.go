// SPDX-License-Identifier: Apache-2.0

// Package e2e is the end-to-end harness's own logic: the guard that refuses a
// run which could reach a real trunk, and the smoke test that places one AI
// outbound call through the public API and checks what the callee simulator
// and the call ledger saw.
//
// The guard is defence in depth. The real guarantee is the compose network:
// every service of deploy/e2e sits on one network with internal: true, so no
// container has a route out. The guard exists so that a change which breaks
// that, or points the dialler at another gateway, fails loudly instead of
// quietly dialling.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// SimulatorGateway is the one gateway the application may dial through.
const SimulatorGateway = "e2e_calleesim"

// BotGateway is the switch's gateway to the application's own SIP server. It
// is configuration the image ships and never leaves the compose network.
const BotGateway = "aicc_bot"

// OutboundEndpoint is the only value AICC_OUTBOUND_ENDPOINT may have.
const OutboundEndpoint = "sofia/gateway/" + SimulatorGateway + "/%s"

// composeConfig is the part of `docker compose config --format json` the
// guard reads.
type composeConfig struct {
	Services map[string]struct {
		Environment map[string]*string         `json:"environment"`
		Networks    map[string]json.RawMessage `json:"networks"`
		NetworkMode string                     `json:"network_mode"`
		Ports       []json.RawMessage          `json:"ports"`
	} `json:"services"`
	Networks map[string]struct {
		Internal bool `json:"internal"`
	} `json:"networks"`
}

// providerKeys are the environment variables a real provider is reached with.
var providerKeys = []string{
	"OPENAI_API_KEY", "XAI_API_KEY", "ALIYUN_API_KEY", "REALTIME_API_KEY", "DOUBAO_API_KEY",
}

// CheckComposeConfig refuses a resolved compose configuration that is not the
// isolated environment: a service outside the one internal network, a
// published port, a dialler pointed anywhere but the simulator, or a provider
// that is not the mock.
func CheckComposeConfig(data []byte) error {
	var cfg composeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("compose config is not JSON: %w", err)
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if len(cfg.Services) == 0 {
		add("no services in the compose configuration")
	}
	if len(cfg.Networks) != 1 {
		add("want exactly one network, have %d", len(cfg.Networks))
	}
	if n, ok := cfg.Networks["default"]; !ok || !n.Internal {
		add("the default network is not internal: true")
	}

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := cfg.Services[name]
		if svc.NetworkMode != "" {
			add("service %s sets network_mode %q", name, svc.NetworkMode)
		}
		if len(svc.Ports) > 0 {
			add("service %s publishes %d port(s)", name, len(svc.Ports))
		}
		if len(svc.Networks) != 1 {
			add("service %s is on %d networks, want only default", name, len(svc.Networks))
		}
		if _, ok := svc.Networks["default"]; !ok {
			add("service %s is not on the default network", name)
		}
	}

	app, ok := cfg.Services["aicc"]
	if !ok {
		add("no aicc service")
	} else {
		env := func(key string) string {
			if v := app.Environment[key]; v != nil {
				return *v
			}
			return ""
		}
		if got := env("AICC_OUTBOUND_ENDPOINT"); got != OutboundEndpoint {
			add("AICC_OUTBOUND_ENDPOINT is %q, want %q", got, OutboundEndpoint)
		}
		// The profiles the mock can stand in for: openai (the default) and,
		// for the server-cancels runs (e2e.sh greeting-cut), gateway. Whichever
		// runs, every provider key that is set must be the placeholder: nothing
		// real may be present.
		provider := env("AICC_PROVIDER")
		if provider != "openai" && provider != "gateway" {
			add("AICC_PROVIDER is %q, want openai or gateway pointed at the mock", provider)
		}
		if err := privateWebSocket(env("AICC_PROVIDER_ENDPOINT")); err != nil {
			add("AICC_PROVIDER_ENDPOINT: %v", err)
		}
		for _, key := range providerKeys {
			if got := env(key); got != "" && got != "not-a-key" {
				add("%s is set; no real provider key may be present", key)
			}
		}
	}
	if len(problems) > 0 {
		return errors.New("refusing to run: " + strings.Join(problems, "; "))
	}
	return nil
}

// privateWebSocket accepts ws://<private IP>:port/... and nothing else.
func privateWebSocket(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if u.Scheme != "ws" {
		return fmt.Errorf("scheme %q, want ws", u.Scheme)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() {
		return fmt.Errorf("host %q is not a private IP address", u.Hostname())
	}
	return nil
}

// Gateways returns the names of the gateways in `sofia status` output, sorted.
// A gateway line reads `external::name   gateway   sip:...   NOREG`.
func Gateways(sofiaStatus string) []string {
	var names []string
	for _, line := range strings.Split(sofiaStatus, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "gateway" {
			continue
		}
		name := fields[0]
		if i := strings.LastIndex(name, "::"); i >= 0 {
			name = name[i+2:]
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckGateways refuses a switch that holds any gateway besides the bot's and
// the simulator's, and one that lacks either.
func CheckGateways(sofiaStatus string) error {
	got := Gateways(sofiaStatus)
	want := []string{BotGateway, SimulatorGateway}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Errorf("refusing to run: the switch holds gateways %v, want exactly %v", got, want)
	}
	return nil
}
