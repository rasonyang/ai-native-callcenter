// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The capacity budget's runtime enforcement (design 06 §6). Everything here is
// a number the budget makes a claim about, so that the claim can be checked on
// a running system rather than argued about: how many calls are up, how well
// their media kept time, and how quickly the provider started speaking.
//
// Instruments live here rather than in the packages that feed them because
// their names are a contract with whatever scrapes them, and a contract spread
// across five packages is one nobody can read. Every name carries the aicc_
// prefix, as aicc_turn_latency_ms already did.
var (
	callsActive      metric.Int64UpDownCounter
	rtpLateTicks     metric.Int64Counter
	rtpFramesSent    metric.Int64Counter
	jitterLost       metric.Int64Counter
	jitterDropped    metric.Int64Counter
	jitterFilled     metric.Int64Counter
	providerFirstAud metric.Int64Histogram
	providerErrors   metric.Int64Counter
)

// CallKind labels a live call. The two populations overlap rather than sum:
// a call the bot answered is on the switch as well, so it is counted under
// both. SWITCH is what the machine is carrying; BOT is what the model is.
const (
	CallKindBot    = "BOT"
	CallKindSwitch = "SWITCH"
)

func init() {
	meter := otel.Meter("aicc")

	// Errors are ignored deliberately: an instrument that cannot be created
	// leaves a nil that every call site already tolerates, and no telephony
	// path should fail because a counter did not.
	callsActive, _ = meter.Int64UpDownCounter("aicc_calls_active",
		metric.WithDescription("Calls currently up. SWITCH counts every call the switch "+
			"is carrying, BOT the subset the model is answering; they overlap."))
	rtpFramesSent, _ = meter.Int64Counter("aicc_rtp_frames_sent_total",
		metric.WithDescription("Audio frames handed to the wire on AI legs."))
	rtpLateTicks, _ = meter.Int64Counter("aicc_rtp_late_ticks_total",
		metric.WithDescription("Send ticks that ran a whole frame late. The caller hears these."))
	jitterLost, _ = meter.Int64Counter("aicc_jitter_lost_total",
		metric.WithDescription("Inbound frames that never arrived."))
	jitterDropped, _ = meter.Int64Counter("aicc_jitter_dropped_total",
		metric.WithDescription("Inbound frames that arrived too late to be of use."))
	jitterFilled, _ = meter.Int64Counter("aicc_jitter_filled_total",
		metric.WithDescription("Silence frames substituted for missing inbound audio."))
	providerFirstAud, _ = meter.Int64Histogram("aicc_provider_first_audio_ms",
		metric.WithDescription("Provider share of turn latency: caller stopped speaking "+
			"to the first byte of the reply."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(100, 200, 300, 400, 500, 700, 900, 1200, 1600, 2000, 3000))
	providerErrors, _ = meter.Int64Counter("aicc_provider_ws_errors_total",
		metric.WithDescription("Provider sessions that ended on an error rather than a hangup."))
}

// CallStarted and CallEnded move the live-call gauge. They must be paired:
// the gauge is the first thing anyone looks at, and a leak in it reads as a
// leak in the process.
func CallStarted(kind string) { addCall(kind, 1) }
func CallEnded(kind string)   { addCall(kind, -1) }

func addCall(kind string, delta int64) {
	if callsActive == nil {
		return
	}
	callsActive.Add(context.Background(), delta,
		metric.WithAttributes(attribute.String("kind", kind)))
}

// RecordRTPHealth publishes one AI leg's media counters when it ends.
//
// They are reported per call rather than per frame on purpose: these are
// atomics the RTP session already maintains, and reading them once at teardown
// costs nothing on a path that runs fifty times a second per call.
func RecordRTPHealth(sent, lateTicks, lost, dropped, filled int64) {
	ctx := context.Background()
	for instrument, value := range map[metric.Int64Counter]int64{
		rtpFramesSent: sent,
		rtpLateTicks:  lateTicks,
		jitterLost:    lost,
		jitterDropped: dropped,
		jitterFilled:  filled,
	} {
		if instrument != nil && value > 0 {
			instrument.Add(ctx, value)
		}
	}
}

// RecordProviderFirstAudio publishes the provider's share of one turn's wait.
func RecordProviderFirstAudio(provider string, ms int64) {
	if providerFirstAud == nil || ms <= 0 {
		return
	}
	providerFirstAud.Record(context.Background(), ms,
		metric.WithAttributes(attribute.String("provider", provider)))
}

// RecordProviderError counts a session that ended badly. A rise here is the
// difference between callers hanging up and a vendor having a bad afternoon.
func RecordProviderError(provider string) {
	if providerErrors == nil {
		return
	}
	providerErrors.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("provider", provider)))
}
