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
	botInterruptions metric.Int64Counter

	transcribeFramesDropped metric.Int64Counter
	transcribeFramesSent    metric.Int64Counter

	webhookDeliveries metric.Int64Counter
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
	botInterruptions, _ = meter.Int64Counter("aicc_bot_interruptions_total",
		metric.WithDescription("Times a caller took the floor back from the bot, by what "+
			"took it: SPEECH or DTMF. Speech inside the barge-in guard is not counted — "+
			"it is line echo and was ignored — which makes this the only measurement of "+
			"whether that guard is set right."))
	webhookDeliveries, _ = meter.Int64Counter("aicc_webhook_deliveries_total",
		metric.WithDescription("CDR deliveries that settled, by outcome. FAILED means the "+
			"retry schedule ran out and that call was never told to that subscriber."))
	transcribeFramesSent, _ = meter.Int64Counter("aicc_transcribe_frames_sent_total",
		metric.WithDescription("Audio frames handed to a recognition session, by speaker."))
	transcribeFramesDropped, _ = meter.Int64Counter("aicc_transcribe_frames_dropped_total",
		metric.WithDescription("Audio frames discarded because a recognition session could "+
			"not keep up. Dropped audio is otherwise invisible: a missing transcript line "+
			"looks the same whether the engine failed to hear it or we never sent it."))
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

// RecordBotInterruption counts a caller taking the floor back from the bot.
//
// A rate rather than an occurrence, which is why it is here and not on the
// event stream: what an operator asks is whether interruptions are climbing —
// a bot that has grown too talkative, a turn detector that has grown too
// eager — and no single interruption answers that. It is also the only
// evidence that exists for whether the barge-in guard is set right: speech
// inside the guard is line echo and is ignored before this is reached, so a
// guard set too wide shows up here as interruptions that stopped being
// counted, and one set too narrow as a bot interrupting itself.
func RecordBotInterruption(reason string) {
	if botInterruptions == nil {
		return
	}
	botInterruptions.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason", reason)))
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

// RecordTranscribeAudio publishes one recognition session's frame accounting
// when it ends, in the same shape and for the same reason as RecordRTPHealth:
// these are counters the session already keeps, and reading them once at
// teardown costs nothing on a path that runs fifty times a second.
//
// The dropped count is the one that matters. A bounded queue that discards the
// oldest frame is right for a media path — stalling the switch to keep audio
// is a worse failure — but a drop leaves no other trace, and a transcript with
// a hole in it looks identical whether the engine mis-heard the words or we
// never sent them. This is what tells those two apart afterwards.
func RecordTranscribeAudio(provider, speaker string, sent, dropped int64) {
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("speaker", speaker),
	)
	if transcribeFramesSent != nil && sent > 0 {
		transcribeFramesSent.Add(ctx, sent, attrs)
	}
	if transcribeFramesDropped != nil && dropped > 0 {
		transcribeFramesDropped.Add(ctx, dropped, attrs)
	}
}

// WebhookDelivered and WebhookFailed settle one CDR delivery.
//
// Labelled by subscription rather than counted in one total, because the
// question an operator has is never "how many failed" but "which subscriber is
// broken" — a single figure that rises when one customer's endpoint goes down
// tells them something happened and not where.
func WebhookDelivered(subscriptionID string) { countWebhook(subscriptionID, "DELIVERED") }
func WebhookFailed(subscriptionID string)    { countWebhook(subscriptionID, "FAILED") }

func countWebhook(subscriptionID, outcome string) {
	if webhookDeliveries == nil {
		return
	}
	webhookDeliveries.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("subscriptionId", subscriptionID),
		attribute.String("outcome", outcome),
	))
}
