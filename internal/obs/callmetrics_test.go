// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The global meter provider can be set once; every test reads the same reader
// and compares counts before and after.
var (
	testReader     = sdkmetric.NewManualReader()
	setTestReaders sync.Once
)

func useTestReader() sdkmetric.Reader {
	setTestReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	})
	return testReader
}

func delta(after, before map[string]uint64) map[string]uint64 {
	out := map[string]uint64{}
	for k, v := range after {
		out[k] = v - before[k]
	}
	return out
}

// stageCounts reads aicc_outbound_first_audio_seconds: observations per stage.
func stageCounts(t *testing.T, reader sdkmetric.Reader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_outbound_first_audio_seconds" {
				continue
			}
			h := m.Data.(metricdata.Histogram[float64])
			for _, dp := range h.DataPoints {
				stage, _ := dp.Attributes.Value("stage")
				out[stage.AsString()] += dp.Count
			}
		}
	}
	return out
}

func TestAnOutboundCallRecordsEachStageOfItsFirstWordOnce(t *testing.T) {
	reader := useTestReader()
	before := stageCounts(t, reader)

	id, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	t0 := time.Now()
	OutboundAnswered(id, t0)
	// A call that was not entered at the answer records nothing.
	OutboundInvited(other, t0)
	OutboundFirstAudio(other, t0)

	OutboundInvited(id, t0.Add(80*time.Millisecond))
	OutboundInvited(id, t0.Add(90*time.Millisecond)) // a repeat counts once
	OutboundSessionReady(id, t0.Add(500*time.Millisecond))
	OutboundFirstAudio(id, t0.Add(900*time.Millisecond))
	OutboundFirstAudio(id, t0.Add(1*time.Second)) // the call is no longer tracked

	got := delta(stageCounts(t, reader), before)
	for _, stage := range []string{OutboundStageAnswerToInvite, OutboundStageInviteToReady, OutboundStageReadyToFirstAudio} {
		if got[stage] != 1 {
			t.Errorf("stage %s observed %d times, want 1 (all: %v)", stage, got[stage], got)
		}
	}
	outboundStages.Lock()
	n := len(outboundStages.calls)
	outboundStages.Unlock()
	if n != 0 {
		t.Errorf("%d calls still tracked", n)
	}
}

func TestAProviderThatSpeaksBeforeItsSessionIsReadyStillRecordsAllStages(t *testing.T) {
	reader := useTestReader()
	before := stageCounts(t, reader)

	id := uuid.Must(uuid.NewV7())
	t0 := time.Now()
	OutboundAnswered(id, t0)
	OutboundInvited(id, t0.Add(50*time.Millisecond))
	OutboundFirstAudio(id, t0.Add(400*time.Millisecond))
	OutboundSessionReady(id, t0.Add(450*time.Millisecond)) // too late to count

	got := delta(stageCounts(t, reader), before)
	if got[OutboundStageInviteToReady] != 1 || got[OutboundStageReadyToFirstAudio] != 1 {
		t.Errorf("stages = %v", got)
	}
}

func TestACallThatEndsBeforeItsFirstWordIsForgotten(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	OutboundAnswered(id, time.Now())
	OutboundForget(id)
	outboundStages.Lock()
	_, tracked := outboundStages.calls[id]
	outboundStages.Unlock()
	if tracked {
		t.Error("an ended call is still tracked")
	}
}

// uplinkTotals reads the aicc_uplink_* counters.
func uplinkTotals(t *testing.T, reader sdkmetric.Reader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "aicc_uplink_") {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				out[m.Name] += uint64(dp.Value)
			}
		}
	}
	return out
}

func TestTheUplinkClockPublishesItsCountersWhenTheLegEnds(t *testing.T) {
	reader := useTestReader()
	before := uplinkTotals(t, reader)

	RecordUplinkClock(103, 123, 115, 95, 7)
	RecordUplinkClock(10, 12, 3, 0, 0) // a second call; zero counts publish nothing

	got := delta(uplinkTotals(t, reader), before)
	want := map[string]uint64{
		"aicc_uplink_frames_received_total":        113,
		"aicc_uplink_frames_sent_total":            135,
		"aicc_uplink_frames_filled_total":          118,
		"aicc_uplink_frames_dropped_backlog_total": 95,
		"aicc_uplink_frames_dropped_rx_total":      7,
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %d, want %d", name, got[name], value)
		}
	}
}

func TestSwitchRTPExhaustionIsCountedByMedia(t *testing.T) {
	reader := useTestReader()
	read := func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "aicc_switch_rtp_ports_exhausted_total" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					media, _ := dp.Attributes.Value("media")
					out[media.AsString()] += dp.Value
				}
			}
		}
		return out
	}
	before := read()
	RecordSwitchRTPExhausted("audio")
	RecordSwitchRTPExhausted("audio")
	RecordSwitchRTPExhausted("video")
	after := read()
	if after["audio"]-before["audio"] != 2 || after["video"]-before["video"] != 1 {
		t.Errorf("before %v after %v: want audio +2, video +1", before, after)
	}
}

// echoCounts reads the echo measurement's instruments: observations of each
// counter series by its label, and the histograms' sample counts.
func echoCounts(t *testing.T, reader sdkmetric.Reader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					switch m.Name {
					case "aicc_bot_echo_calls_total":
						v, _ := dp.Attributes.Value("result")
						out["calls."+v.AsString()] += dp.Value
					case "aicc_bot_speech_interruptions_total":
						v, _ := dp.Attributes.Value("verdict")
						out["interruptions."+v.AsString()] += dp.Value
					case "aicc_bot_echo_suppression_calls_total":
						v, _ := dp.Attributes.Value("state")
						out["suppression."+v.AsString()] += dp.Value
					case "aicc_bot_echo_suppressed_frames_total":
						out["suppressedFrames"] += dp.Value
					case "aicc_bot_echo_gate_openings_total":
						out["gateOpenings"] += dp.Value
					}
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					if m.Name == "aicc_bot_echo_delay_ms" {
						out["delay"] += int64(dp.Count)
					}
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					if m.Name == "aicc_bot_echo_return_loss_db" {
						out["returnLoss"] += int64(dp.Count)
					}
				}
			}
		}
	}
	return out
}

func TestTheEchoMeasurementRecordsDelayAndLossOnlyForAFoundEcho(t *testing.T) {
	reader := useTestReader()
	before := echoCounts(t, reader)

	RecordEchoCall(EchoResultFound, 240, 12.5)
	RecordEchoCall(EchoResultNotFound, 0, 0)
	RecordEchoCall(EchoResultUnmeasured, 0, 0)
	RecordSpeechInterruption(EchoVerdictEcho)
	RecordSpeechInterruption(EchoVerdictNotEcho)
	RecordSpeechInterruption(EchoVerdictNotEcho)
	RecordSpeechInterruption(EchoVerdictUnknown)
	RecordSpeechInterruption("MAYBE") // not a verdict: kept inside the label's values

	got := echoCounts(t, reader)
	for key, want := range map[string]int64{
		"calls.FOUND": 1, "calls.NOT_FOUND": 1, "calls.UNMEASURED": 1,
		"delay": 1, "returnLoss": 1,
		"interruptions.ECHO": 1, "interruptions.NOT_ECHO": 2, "interruptions.UNKNOWN": 2,
		"interruptions.MAYBE": 0,
	} {
		if d := got[key] - before[key]; d != want {
			t.Errorf("%s moved by %d, want %d (all: %v)", key, d, want, got)
		}
	}
}

func TestTheEchoSuppressorCountsCallsByStateAndItsFrames(t *testing.T) {
	reader := useTestReader()
	before := echoCounts(t, reader)

	RecordEchoSuppression(EchoSuppressionOff, 0, 0)
	RecordEchoSuppression(EchoSuppressionIdle, 0, 0)
	RecordEchoSuppression(EchoSuppressionEngaged, 120, 3)
	RecordEchoSuppression(EchoSuppressionFailed, 7, 1)
	RecordEchoSuppression("HALF", 0, 0) // not a state: kept inside the label's values

	got := echoCounts(t, reader)
	for key, want := range map[string]int64{
		"suppression.OFF": 2, "suppression.IDLE": 1, "suppression.ENGAGED": 1, "suppression.FAILED": 1,
		"suppression.HALF": 0, "suppressedFrames": 127, "gateOpenings": 4,
	} {
		if d := got[key] - before[key]; d != want {
			t.Errorf("%s moved by %d, want %d (all: %v)", key, d, want, got)
		}
	}
}

func TestAWordlessHangupIsCountedByReasonCallerTurnAndProvider(t *testing.T) {
	reader := useTestReader()
	read := func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "aicc_bot_wordless_hangups_total" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					var key []string
					for _, name := range []string{"reason", "caller_turn", "provider"} {
						v, _ := dp.Attributes.Value(attribute.Key(name))
						key = append(key, v.AsString())
					}
					out[strings.Join(key, "/")] += dp.Value
				}
			}
		}
		return out
	}
	before := read()
	RecordWordlessHangup("FLOW_INSTRUCTED", CallerTurnQuestion, "qwen")
	RecordWordlessHangup("CALLER_GOODBYE", CallerTurnQuestion, "qwen")
	RecordWordlessHangup("CALLER_GOODBYE", CallerTurnQuestion, "qwen")
	RecordWordlessHangup("CALLER_GOODBYE", CallerTurnUnknown, "doubao")
	after := read()
	for key, want := range map[string]int64{
		"FLOW_INSTRUCTED/QUESTION/qwen": 1,
		"CALLER_GOODBYE/QUESTION/qwen":  2,
		"CALLER_GOODBYE/UNKNOWN/doubao": 1,
		"CALLER_GOODBYE/OTHER/qwen":     0,
	} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d (before %v, after %v)", key, got, want, before, after)
		}
	}
}

func TestALeadGuardVerdictIsCountedByReasonAndProvider(t *testing.T) {
	reader := useTestReader()
	read := func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "aicc_bot_lead_guard_total" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					reason, _ := dp.Attributes.Value("reason")
					provider, _ := dp.Attributes.Value("provider")
					out[reason.AsString()+"/"+provider.AsString()] += dp.Value
				}
			}
		}
		return out
	}
	before := read()
	RecordLeadGuard(LeadGuardDeclined, "qwen")
	RecordLeadGuard(LeadGuardDeclined, "qwen")
	RecordLeadGuard(LeadGuardOpeningUnheard, "qwen")
	RecordLeadGuard(LeadGuardWords, "openai")
	RecordLeadGuard(LeadGuardMachineWording, "qwen")
	after := read()
	for key, want := range map[string]int64{
		"DECLINED/qwen":        2,
		"OPENING_UNHEARD/qwen": 1,
		"WORDS/openai":         1,
		"MACHINE_WORDING/qwen": 1,
		"NO_FINAL/qwen":        0,
	} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d (before %v, after %v)", key, got, want, before, after)
		}
	}
}

func TestAMachineAnsweredCallIsCountedByCategoryAndProvider(t *testing.T) {
	reader := useTestReader()
	before := sumByLabels(t, reader, "aicc_bot_machine_answered_calls_total", "category", "provider")
	RecordMachineAnswered(MachineCallAssistant, "qwen")
	RecordMachineAnswered(MachineCallAssistant, "qwen")
	RecordMachineAnswered(MachineVoicemail, "openai")
	RecordMachineAnswered(MachineCarrierAnnouncement, "qwen")
	after := sumByLabels(t, reader, "aicc_bot_machine_answered_calls_total", "category", "provider")
	for key, want := range map[string]int64{
		"CALL_ASSISTANT/qwen":       2,
		"VOICEMAIL/openai":          1,
		"CARRIER_ANNOUNCEMENT/qwen": 1,
		"VOICEMAIL/qwen":            0,
	} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d (before %v, after %v)", key, got, want, before, after)
		}
	}
}

func TestALeadVerdictIsCountedByVerdict(t *testing.T) {
	reader := useTestReader()
	before := sumByLabels(t, reader, "aicc_campaign_lead_verdicts_total", "verdict")
	RecordLeadVerdict("CONFIRMED")
	RecordLeadVerdict("NOT_JUDGED")
	RecordLeadVerdict("NOT_JUDGED")
	after := sumByLabels(t, reader, "aicc_campaign_lead_verdicts_total", "verdict")
	for key, want := range map[string]int64{"CONFIRMED": 1, "NOT_JUDGED": 2, "REFUSED": 0} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d (before %v, after %v)", key, got, want, before, after)
		}
	}
}

// sumByLabels reads one counter or gauge: its value per combination of the
// named labels, joined with "/".
func sumByLabels(t *testing.T, reader sdkmetric.Reader, name string, labels ...string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	key := func(set attribute.Set) string {
		values := make([]string, 0, len(labels))
		for _, label := range labels {
			v, _ := set.Value(attribute.Key(label))
			values = append(values, v.AsString())
		}
		return strings.Join(values, "/")
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[key(dp.Attributes)] += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[key(dp.Attributes)] = dp.Value
				}
			}
		}
	}
	return out
}

func TestAProviderDialFailureIsCountedByStatusAndReason(t *testing.T) {
	reader := useTestReader()
	const name = "aicc_provider_dial_failures_total"
	before := sumByLabels(t, reader, name, "provider", "status", "reason")
	RecordProviderDialFailure("qwen", "CLOSE_1007", "QUOTA_EXCEEDED")
	RecordProviderDialFailure("qwen", "CLOSE_1007", "QUOTA_EXCEEDED")
	RecordProviderDialFailure("openai", "401", "AUTH_REJECTED")
	after := sumByLabels(t, reader, name, "provider", "status", "reason")
	for key, want := range map[string]int64{
		"qwen/CLOSE_1007/QUOTA_EXCEEDED": 2,
		"openai/401/AUTH_REJECTED":       1,
		"qwen/429/QUOTA_EXCEEDED":        0,
	} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d", key, got, want)
		}
	}
}

func TestARateLimitHeaderSetsOnlyTheValuesItCarried(t *testing.T) {
	reader := useTestReader()
	SetProviderRateLimit("gateway", "TOKENS", 100000, true, 0, false)
	SetProviderRateLimit("gateway", "TOKENS", 0, false, 99000, true)
	limits := sumByLabels(t, reader, "aicc_provider_ratelimit_limit", "provider", "kind")
	left := sumByLabels(t, reader, "aicc_provider_ratelimit_remaining", "provider", "kind")
	if limits["gateway/TOKENS"] != 100000 || left["gateway/TOKENS"] != 99000 {
		t.Errorf("limit %d remaining %d, want 100000 and 99000", limits["gateway/TOKENS"], left["gateway/TOKENS"])
	}
}

func TestProviderTokensAreCountedByDirection(t *testing.T) {
	reader := useTestReader()
	const name = "aicc_provider_tokens_total"
	before := sumByLabels(t, reader, name, "provider", "direction")
	RecordProviderTokens("qwen", 1288, 255, 0)
	RecordProviderTokens("qwen", 0, 0, 7)
	after := sumByLabels(t, reader, name, "provider", "direction")
	for key, want := range map[string]int64{"qwen/INPUT": 1288, "qwen/OUTPUT": 255, "qwen/UNSPLIT": 7} {
		if got := after[key] - before[key]; got != want {
			t.Errorf("%s gained %d, want %d", key, got, want)
		}
	}
}

func TestTheProviderQuotaIsPublishedByKind(t *testing.T) {
	reader := useTestReader()
	SetProviderQuota("doubao", 60, 100000)
	got := sumByLabels(t, reader, "aicc_provider_quota_per_minute", "provider", "kind")
	if got["doubao/SESSIONS"] != 60 || got["doubao/TOKENS"] != 100000 {
		t.Errorf("quota = %v, want doubao 60 sessions and 100000 tokens", got)
	}
}
