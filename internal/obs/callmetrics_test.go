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
