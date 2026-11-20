// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// The hangup of a leg the switch ended at the deployment's maximum, as the dev
// switch reported it on 2026-11-20 (#637 premise probe): the extension's own
// hangup carries NORMAL_CLEARING (Q.850 16), and the two stamps beside it.
func maxDurationHangup(channelID, side string) map[string]string {
	h := map[string]string{
		"Event-Name":                 "CHANNEL_HANGUP_COMPLETE",
		"Unique-ID":                  channelID,
		"Call-Direction":             "outbound",
		"Caller-Destination-Number":  "aicc_max_call_duration_" + strings.ToLower(side),
		"Hangup-Cause":               "NORMAL_CLEARING",
		"variable_hangup_cause_q850": "16",
		"variable_aicc_hangup_cause": "MAX_CALL_DURATION",
	}
	if side != "" {
		h["variable_aicc_max_call_duration_side"] = side
	}
	return h
}

func TestTheSwitchsMaximumIsReadWithItsSide(t *testing.T) {
	t.Parallel()
	const leg = "019ff973-6b3b-7481-922b-cb52677a6cae"
	plain := func(cause string, extra map[string]string) map[string]string {
		h := map[string]string{"Event-Name": "CHANNEL_HANGUP_COMPLETE", "Unique-ID": leg, "Hangup-Cause": cause}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	for _, tc := range []struct {
		name      string
		headers   map[string]string
		wantCause string
		wantSide  string
	}{
		{"the bot side", maxDurationHangup(leg, "BOT"), "MAX_CALL_DURATION", "BOT"},
		{"the agent side", maxDurationHangup(leg, "AGENT"), "MAX_CALL_DURATION", "AGENT"},
		{"a side this build does not know is counted, not lost", maxDurationHangup(leg, "QUEUE"), "MAX_CALL_DURATION", "UNKNOWN"},
		{"no side at all", maxDurationHangup(leg, ""), "MAX_CALL_DURATION", "UNKNOWN"},
		{"a late answer's ALLOTTED_TIMEOUT is not the mark", plain("ALLOTTED_TIMEOUT", nil), "ALLOTTED_TIMEOUT", ""},
		{"a customer hanging up is not the mark", plain("NORMAL_CLEARING", map[string]string{
			"variable_sip_hangup_disposition": "recv_bye"}), "NORMAL_CLEARING", ""},
		// The leg bridged to the stamped one ends with the switch's own cause.
		{"the partner of a stamped leg is not the mark", plain("NORMAL_CLEARING", nil), "NORMAL_CLEARING", ""},
		{"another platform cause is not the mark", plain("NORMAL_CLEARING", map[string]string{
			"variable_aicc_hangup_cause": "OVERFLOW"}), "OVERFLOW", ""},
		{"a side without the cause is not the mark", plain("NORMAL_CLEARING", map[string]string{
			"variable_aicc_max_call_duration_side": "BOT"}), "NORMAL_CLEARING", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Normalize(esl.NewEvent(tc.headers, ""))
			if !ok || got.Kind != KindChannelHangup {
				t.Fatalf("kind %q ok %v, want a hangup", got.Kind, ok)
			}
			if got.HangupCause != tc.wantCause || got.MaxCallDurationSide != tc.wantSide {
				t.Errorf("cause %q side %q, want %q %q", got.HangupCause, got.MaxCallDurationSide, tc.wantCause, tc.wantSide)
			}
		})
	}
	// Only the hangup carries the mark: the stamp is already on the channel
	// while the extension runs, and those events say nothing about it.
	h := maxDurationHangup(leg, "BOT")
	h["Event-Name"] = "CHANNEL_ANSWER"
	if got, _ := Normalize(esl.NewEvent(h, "")); got.MaxCallDurationSide != "" {
		t.Errorf("a %s carries side %q", got.Kind, got.MaxCallDurationSide)
	}
}

func TestTheKillOnAnswerKeepsItsPlusAndRoundsUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		got       func() (string, string)
		wantValue string
	}{
		{"the bot cap plus the grace", func() (string, string) { return BotKillOnAnswer(300 * time.Second) },
			"sched_transfer +360 aicc_max_call_duration_bot XML aicc"},
		{"a cap with a fraction is not shortened", func() (string, string) { return BotKillOnAnswer(299200 * time.Millisecond) },
			"sched_transfer +360 aicc_max_call_duration_bot XML aicc"},
		{"the agent cap as it is", func() (string, string) { return AgentKillOnAnswer(time.Hour) },
			"sched_transfer +3600 aicc_max_call_duration_agent XML aicc"},
	} {
		name, value := tc.got()
		if name != "execute_on_answer_aicc_max_call_duration" || value != tc.wantValue {
			t.Errorf("%s: %s=%s, want execute_on_answer_aicc_max_call_duration=%s", tc.name, name, value, tc.wantValue)
		}
	}
}

// commandRecorder is the switch's command socket: what it was sent, and an
// answer per command prefix.
type commandRecorder struct {
	mu    sync.Mutex
	sent  []string
	fails map[string]error
}

func (r *commandRecorder) IsUp() bool                   { return true }
func (r *commandRecorder) BgAPI(string) (string, error) { return "job", nil }
func (r *commandRecorder) API(cmd string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, cmd)
	for prefix, err := range r.fails {
		if strings.HasPrefix(cmd, prefix) {
			return "", err
		}
	}
	return "+OK", nil
}

func TestTheSwitchIsToldBothMaximumsInWholeSeconds(t *testing.T) {
	t.Parallel()
	rec := &commandRecorder{}
	a := NewAdapter(rec, "aicc.test")
	if err := a.SetMaxCallDurations(BotKillAfter(300*time.Second), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMaxCallDurations(BotKillAfter(90500*time.Millisecond), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"global_setvar aicc_max_call_duration_bot_sec=360",
		"global_setvar aicc_max_call_duration_agent_sec=3600",
		"global_setvar aicc_max_call_duration_bot_sec=151",
		"global_setvar aicc_max_call_duration_agent_sec=86400",
	}
	if strings.Join(rec.sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(rec.sent, "\n"), strings.Join(want, "\n"))
	}

	// A refusal is reported, and the command after it is not sent.
	refusing := &commandRecorder{fails: map[string]error{"global_setvar aicc_max_call_duration_bot_sec": esl.ErrTimeout}}
	if err := NewAdapter(refusing, "aicc.test").SetMaxCallDurations(time.Minute, time.Minute); !errors.Is(err, esl.ErrTimeout) {
		t.Errorf("err = %v, want the timeout", err)
	}
	if len(refusing.sent) != 1 {
		t.Errorf("sent %q after a refusal, want the first command only", refusing.sent)
	}
}

// The kill instrument, read through an OTel ManualReader. The global meter
// provider can be set once per test binary, so the test compares counts
// before and after.
var (
	killReader     = sdkmetric.NewManualReader()
	setKillReaders sync.Once
)

func readKills(t *testing.T) map[string]int64 {
	t.Helper()
	setKillReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(killReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := killReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_call_max_duration_kills_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				side, _ := dp.Attributes.Value("side")
				out[side.AsString()] += dp.Value
			}
		}
	}
	return out
}

// Each leg the switch ended at the maximum is counted once by its side and
// logged once with its call; the legs around it, and every other hangup,
// count nothing (#637). From the raw events, through Normalize, as the
// switch sends them.
//
// Not parallel: it replaces the process's default logger.
func TestAKillAtTheMaximumIsCountedAndLoggedOncePerLeg(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	out := &capture{}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})
	ctx := context.Background()
	call := func(legs ...string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := registry.CreateCall(ctx, id, events.CallTypeInbound, "en", false, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, leg := range legs {
			if err := registry.BindChannel(leg, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	handle := func(h map[string]string) {
		t.Helper()
		ev, ok := Normalize(esl.NewEvent(h, ""))
		if !ok {
			t.Fatalf("not normalized: %v", h)
		}
		c.Handle(ctx, ev)
	}
	plainHangup := func(leg, cause string) map[string]string {
		return map[string]string{"Event-Name": "CHANNEL_HANGUP_COMPLETE", "Unique-ID": leg, "Hangup-Cause": cause}
	}

	before := readKills(t)

	// A bot call the switch ended: the customer's leg is stamped, the bot leg
	// bridged to it is hung up by the switch with its own cause.
	botCustomer, botLeg := uuid.NewString(), uuid.NewString()
	botCall := call(botCustomer, botLeg)
	handle(plainHangup(botLeg, "NORMAL_CLEARING"))
	handle(maxDurationHangup(botCustomer, "BOT"))

	// A call with an agent, ended at the agent cap; the agent's leg is the
	// partner.
	agentCustomer, agentLeg := uuid.NewString(), uuid.NewString()
	agentCall := call(agentCustomer, agentLeg)
	handle(maxDurationHangup(agentCustomer, "AGENT"))
	handle(plainHangup(agentLeg, "NORMAL_CLEARING"))

	// Calls that ended any other way: a customer's hangup, a late answer.
	other := uuid.NewString()
	call(other)
	handle(plainHangup(other, "NORMAL_CLEARING"))
	late := uuid.NewString()
	call(late)
	handle(plainHangup(late, "ALLOTTED_TIMEOUT"))

	after := readKills(t)
	if d := after["BOT"] - before["BOT"]; d != 1 {
		t.Errorf("BOT counted %d, want 1", d)
	}
	if d := after["AGENT"] - before["AGENT"]; d != 1 {
		t.Errorf("AGENT counted %d, want 1", d)
	}
	for side, n := range after {
		if side != "BOT" && side != "AGENT" && n != before[side] {
			t.Errorf("side %q counted %d", side, n-before[side])
		}
	}

	logs := out.String()
	lines := strings.Count(logs, "at the deployment's maximum call duration")
	if lines != 2 {
		t.Errorf("%d kill lines, want 2:\n%s", lines, logs)
	}
	for _, want := range []string{
		"callId=" + botCall.String(), "channelId=" + botCustomer, "side=BOT",
		"the application's own ending at the flow's time limit did not run",
		"callId=" + agentCall.String(), "channelId=" + agentCustomer, "side=AGENT",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "maximum call duration") && !strings.Contains(line, "level=WARN") {
			t.Errorf("not a warning: %s", line)
		}
		if strings.Contains(line, "maximum call duration") && strings.Contains(line, "side=AGENT") &&
			strings.Contains(line, "did not run") {
			t.Errorf("an agent-side kill blames the application's ending: %s", line)
		}
	}
}

// A leg no call owns any more (a restart in between) is still counted, and
// named by the call id the dialplan stamped on it.
//
// Not parallel: it replaces the process's default logger.
func TestAKillOnALegNoCallOwnsIsNamedByItsStampedCall(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	out := &capture{}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	before := readKills(t)
	callID := uuid.Must(uuid.NewV7())
	h := maxDurationHangup(uuid.NewString(), "AGENT")
	h["variable_aicc_call_id"] = callID.String()
	ev, _ := Normalize(esl.NewEvent(h, ""))
	c.Handle(context.Background(), ev)

	if d := readKills(t)["AGENT"] - before["AGENT"]; d != 1 {
		t.Errorf("AGENT counted %d, want 1", d)
	}
	if !strings.Contains(out.String(), "callId="+callID.String()) {
		t.Errorf("log lacks the stamped call: %s", out)
	}
}
