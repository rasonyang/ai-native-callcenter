// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// goodConfig is the shape `docker compose config --format json` gives for
// deploy/e2e: one internal network, nothing published, the mock provider and
// the simulator gateway.
func goodConfig() map[string]any {
	svc := func(env map[string]any) map[string]any {
		return map[string]any{"networks": map[string]any{"default": map[string]any{}}, "environment": env}
	}
	return map[string]any{
		"networks": map[string]any{"default": map[string]any{"internal": true}},
		"services": map[string]any{
			"postgres":   svc(map[string]any{}),
			"freeswitch": svc(map[string]any{}),
			"aicc": svc(map[string]any{
				"AICC_OUTBOUND_ENDPOINT": OutboundEndpoint,
				"AICC_PROVIDER":          "openai",
				"AICC_PROVIDER_ENDPOINT": "ws://10.77.1.12:9099/v1/realtime",
				"OPENAI_API_KEY":         "not-a-key",
			}),
		},
	}
}

func check(t *testing.T, mutate func(c map[string]any)) error {
	t.Helper()
	c := goodConfig()
	if mutate != nil {
		mutate(c)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return CheckComposeConfig(data)
}

func aiccEnv(c map[string]any) map[string]any {
	return c["services"].(map[string]any)["aicc"].(map[string]any)["environment"].(map[string]any)
}

func TestTheIsolatedConfigurationIsAccepted(t *testing.T) {
	if err := check(t, nil); err != nil {
		t.Fatalf("refused the isolated environment: %v", err)
	}
}

// Setting the outbound endpoint to any other gateway makes the harness refuse
// the run (issue #244 acceptance).
func TestAnotherOutboundGatewayIsRefused(t *testing.T) {
	for _, endpoint := range []string{
		"sofia/gateway/pstn_gateway/%s",
		"sofia/gateway/aicc_bot/%s",
		"sofia/gateway/e2e_calleesim/%s ",
		"sofia/external/%s@203.0.113.7",
		"",
	} {
		err := check(t, func(c map[string]any) { aiccEnv(c)["AICC_OUTBOUND_ENDPOINT"] = endpoint })
		if err == nil || !strings.Contains(err.Error(), "AICC_OUTBOUND_ENDPOINT") {
			t.Errorf("endpoint %q: want a refusal naming AICC_OUTBOUND_ENDPOINT, got %v", endpoint, err)
		}
	}
}

func TestAnythingThatCouldReachOutIsRefused(t *testing.T) {
	cases := map[string]func(c map[string]any){
		"network not internal": func(c map[string]any) {
			c["networks"].(map[string]any)["default"] = map[string]any{"internal": false}
		},
		"second network": func(c map[string]any) {
			c["networks"].(map[string]any)["wan"] = map[string]any{}
		},
		"service on a second network": func(c map[string]any) {
			c["services"].(map[string]any)["freeswitch"].(map[string]any)["networks"] = map[string]any{"default": map[string]any{}, "wan": map[string]any{}}
		},
		"host networking": func(c map[string]any) {
			c["services"].(map[string]any)["freeswitch"].(map[string]any)["network_mode"] = "host"
		},
		"published port": func(c map[string]any) {
			c["services"].(map[string]any)["aicc"].(map[string]any)["ports"] = []any{map[string]any{"target": 8080, "published": "8080"}}
		},
		"real provider": func(c map[string]any) { aiccEnv(c)["OPENAI_API_KEY"] = "sk-live" },
		"provider key beside the mock": func(c map[string]any) {
			aiccEnv(c)["XAI_API_KEY"] = "xai-live"
		},
		"public provider endpoint": func(c map[string]any) {
			aiccEnv(c)["AICC_PROVIDER_ENDPOINT"] = "wss://api.openai.com/v1/realtime"
		},
		"no services": func(c map[string]any) { c["services"] = map[string]any{} },
	}
	for name, mutate := range cases {
		if err := check(t, mutate); err == nil {
			t.Errorf("%s: want a refusal, got none", name)
		}
	}
	if err := CheckComposeConfig([]byte("not json")); err == nil {
		t.Error("garbage: want a refusal")
	}
}

const sofiaStatus = `                     Name	   Type	                                      Data	State
=================================================================================================
            external::aicc_bot	gateway	                                sip:aicc_bot@10.77.1.10:6060	NOREG
    external::e2e_calleesim	gateway	                       sip:e2e_calleesim@10.77.1.13:5080	NOREG
                  external	profile	                         sip:mod_sofia@10.77.1.11:5080	RUNNING (0)
=================================================================================================
2 profiles 2 aliases
`

func TestOnlyTheTwoExpectedGatewaysAreAccepted(t *testing.T) {
	if got := Gateways(sofiaStatus); len(got) != 2 {
		t.Fatalf("parsed %v, want two gateways", got)
	}
	if err := CheckGateways(sofiaStatus); err != nil {
		t.Fatalf("refused the expected switch: %v", err)
	}

	extra := strings.Replace(sofiaStatus, "                  external\tprofile",
		"        external::pstn_gateway\tgateway\tsip:x@192.0.2.1:5080\tNOREG\n                  external\tprofile", 1)
	if err := CheckGateways(extra); err == nil {
		t.Error("a third gateway: want a refusal")
	}
	missing := strings.Replace(sofiaStatus, "    external::e2e_calleesim\tgateway\t                       sip:e2e_calleesim@10.77.1.13:5080\tNOREG\n", "", 1)
	if err := CheckGateways(missing); err == nil {
		t.Error("no simulator gateway: want a refusal")
	}
	if err := CheckGateways(""); err == nil {
		t.Error("an empty answer: want a refusal")
	}
}

// The greeting-cut run puts the application on the gateway profile, whose
// server cancels by itself; the mock stands in for it as for openai, with the
// same placeholder-key rule (#596).
func TestTheGatewayProfileIsAcceptedWithThePlaceholder(t *testing.T) {
	err := check(t, func(c map[string]any) {
		aiccEnv(c)["AICC_PROVIDER"] = "gateway"
		aiccEnv(c)["REALTIME_API_KEY"] = "not-a-key"
	})
	if err != nil {
		t.Fatalf("refused the gateway profile on the placeholder: %v", err)
	}
	if err := check(t, func(c map[string]any) {
		aiccEnv(c)["AICC_PROVIDER"] = "gateway"
		aiccEnv(c)["REALTIME_API_KEY"] = "rt-live"
	}); err == nil {
		t.Error("a real gateway key: want a refusal")
	}
	if err := check(t, func(c map[string]any) {
		aiccEnv(c)["AICC_PROVIDER"] = "qwen"
	}); err == nil {
		t.Error("a profile the mock does not stand in for: want a refusal")
	}
}
