// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// originateLog is the switch's command socket as the real adapter writes to
// it: every originate it is sent, verbatim.
type originateLog struct {
	mu   sync.Mutex
	sent []string
}

func (l *originateLog) IsUp() bool                 { return true }
func (l *originateLog) API(string) (string, error) { return "+OK", nil }
func (l *originateLog) BgAPI(cmd string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, cmd)
	return "job-1", nil
}

func (l *originateLog) last(t *testing.T) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.sent) == 0 {
		t.Fatal("nothing was originated")
	}
	return l.sent[len(l.sent)-1]
}

// renderingService is a service on the real adapter, so what a test reads is
// the originate line the switch would parse.
func renderingService(t *testing.T, cfg Config) (*Service, *originateLog) {
	t.Helper()
	log := &originateLog{}
	flowID := uuid.Must(uuid.NewV7())
	dids := fakeDIDs{
		{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowInbound: true, AllowOutbound: true},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	cfg.EndpointFormat = "sofia/gateway/pstn_gateway/%s"
	svc := New(cfg, telephony.NewAdapter(log, "aicc.test"), dids, provisioned("1007"),
		func(context.Context, uuid.UUID) (bool, error) { return false, nil },
		func(uuid.UUID) bool { return false },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, log
}

// Both ways an AI call goes out (a single call and a campaign attempt share
// PlaceAI) arm the switch's own end to it: a transfer to the bot-side
// extension at the bot cap plus the 60 s grace from the customer's answer,
// and a park that ends an answered leg the bot never got (#637). The value is
// quoted, because it holds spaces, and keeps its +.
func TestAnAIOutboundLegCarriesTheSwitchsMaximum(t *testing.T) {
	t.Parallel()
	const park = "park_timeout=30:RECOVERY_ON_TIMER_EXPIRE"
	for _, tc := range []struct {
		name   string
		cfg    Config
		want   string
		isCamp bool
	}{
		{"a single call at the default cap", Config{},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +360 aicc_max_call_duration_bot XML aicc'", false},
		{"a campaign attempt at the default cap", Config{},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +360 aicc_max_call_duration_bot XML aicc'", true},
		{"a single call at a deployment's cap", Config{BotMaxCallDuration: 20 * time.Minute},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +1260 aicc_max_call_duration_bot XML aicc'", false},
		{"a campaign attempt at a deployment's cap", Config{BotMaxCallDuration: time.Hour},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +3660 aicc_max_call_duration_bot XML aicc'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, log := renderingService(t, tc.cfg)
			req := AIDialRequest{To: "13912345678", DIDNumber: "95012"}
			if tc.isCamp {
				req.CampaignID, req.AttemptID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
				if _, err := svc.PlaceAI(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			} else if _, err := svc.DialAI(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			line := log.last(t)
			t.Logf("rendered: %s", line)
			block, rest, ok := strings.Cut(strings.TrimPrefix(line, "originate {"), "}")
			if !ok || !strings.HasPrefix(line, "originate {") {
				t.Fatalf("not an originate with a block: %s", line)
			}
			vars := "," + block + ","
			for _, want := range []string{tc.want, park} {
				if !strings.Contains(vars, ","+want+",") {
					t.Errorf("block lacks %s:\n%s", want, block)
				}
			}
			if strings.Contains(block, "aicc_max_call_duration_agent") {
				t.Errorf("an AI leg carries the agent cap: %s", block)
			}
			if !strings.HasSuffix(rest, " &park()") {
				t.Errorf("the leg does not wait in park: %s", rest)
			}
		})
	}
}

// The agent leg of a click-to-dial carries the agent cap, counted from the
// agent's answer, beside the unset of the auto-answer header that has always
// run there: two execute_on_answer variables, both run by the switch.
func TestAClickToDialAgentLegCarriesTheAgentCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"the default cap", Config{},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +3600 aicc_max_call_duration_agent XML aicc'"},
		{"a deployment's cap", Config{AgentMaxCallDuration: 24 * time.Hour},
			"execute_on_answer_aicc_max_call_duration='sched_transfer +86400 aicc_max_call_duration_agent XML aicc'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, log := renderingService(t, tc.cfg)
			if _, err := svc.Dial(context.Background(), AgentDialRequest{AgentExtension: "1007", To: "13912345678"}); err != nil {
				t.Fatal(err)
			}
			line := log.last(t)
			t.Logf("rendered: %s", line)
			block, _, _ := strings.Cut(strings.TrimPrefix(line, "originate {"), "}")
			vars := "," + block + ","
			for _, want := range []string{tc.want, "execute_on_answer='unset sip_h_Call-Info'"} {
				if !strings.Contains(vars, ","+want+",") {
					t.Errorf("block lacks %s:\n%s", want, block)
				}
			}
			if strings.Contains(block, "aicc_max_call_duration_bot") || strings.Contains(block, "park_timeout") {
				t.Errorf("the agent leg carries the bot side's backstop: %s", block)
			}
		})
	}
}
