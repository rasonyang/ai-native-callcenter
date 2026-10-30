// SPDX-License-Identifier: Apache-2.0

package obs

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
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
// prefix.
var (
	turnLatency        metric.Int64Histogram
	callsActive        metric.Int64UpDownCounter
	rtpLateTicks       metric.Int64Counter
	rtpFramesSent      metric.Int64Counter
	jitterLost         metric.Int64Counter
	jitterDropped      metric.Int64Counter
	jitterFilled       metric.Int64Counter
	uplinkReceived     metric.Int64Counter
	uplinkSent         metric.Int64Counter
	uplinkFilled       metric.Int64Counter
	uplinkDropBacklog  metric.Int64Counter
	uplinkDropRx       metric.Int64Counter
	providerFirstAud   metric.Int64Histogram
	providerErrors     metric.Int64Counter
	providerSessions   metric.Int64Counter
	providerUnanswered metric.Int64Counter
	botInterruptions   metric.Int64Counter
	botSessionLimits   metric.Int64Counter
	botUnbacked        metric.Int64Counter
	botFirstMedia      metric.Int64Histogram
	botFirstAudio      metric.Int64Histogram
	botSlots           metric.Int64UpDownCounter
	botOverflow        metric.Int64Counter
	botOverflowHold    metric.Int64Histogram

	transcribeFramesDropped metric.Int64Counter
	transcribeFramesSent    metric.Int64Counter

	webhookDeliveries metric.Int64Counter

	campaignUnmappedCause metric.Int64Counter
	campaignAttempts      metric.Int64Counter
	dialerInFlight        metric.Int64UpDownCounter
	dialerBlocked         metric.Int64Counter
	didOutboundInUse      metric.Int64UpDownCounter
	campaignAnswerUCB     metric.Float64Gauge
	campaignOverflow      metric.Int64Counter
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
	//
	// The latency budget lives or dies on one number: how long the caller
	// waits between finishing their sentence and hearing the answer begin. It
	// is measured per turn, from the provider reporting the caller stopped to
	// the first frame of the reply being handed to the wire. The window starts
	// at speech_stopped, after the VAD's silence hold: the hold is a
	// configuration constant (500 ms by default), so the budget's
	// caller-stop-to-caller-hears p50 <= 1.2 s corresponds to <= ~700 ms here.
	turnLatency, _ = meter.Int64Histogram("aicc_turn_latency_ms",
		metric.WithDescription("Per turn: provider speech_stopped to the first "+
			"reply frame handed to the RTP queue, in milliseconds. The VAD "+
			"silence hold happens before this window."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(100, 200, 300, 400, 500, 700, 900,
			1200, 1600, 2000, 3000, 5000),
	)
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
	uplinkReceived, _ = meter.Int64Counter("aicc_uplink_frames_received_total",
		metric.WithDescription("Caller frames that reached the AI leg's 20 ms uplink clock."))
	uplinkSent, _ = meter.Int64Counter("aicc_uplink_frames_sent_total",
		metric.WithDescription("Frames the uplink clock fed the provider, real and filled."))
	uplinkFilled, _ = meter.Int64Counter("aicc_uplink_frames_filled_total",
		metric.WithDescription("Silence frames the uplink clock sent because the caller's "+
			"phone sent nothing for that 20 ms slot."))
	uplinkDropBacklog, _ = meter.Int64Counter("aicc_uplink_frames_dropped_backlog_total",
		metric.WithDescription("Caller frames dropped because the uplink backlog passed 100 ms."))
	uplinkDropRx, _ = meter.Int64Counter("aicc_uplink_frames_dropped_rx_total",
		metric.WithDescription("Caller frames the RTP session dropped because the consumer "+
			"fell behind."))
	providerFirstAud, _ = meter.Int64Histogram("aicc_provider_first_audio_ms",
		metric.WithDescription("Provider share of turn latency: caller stopped speaking "+
			"to the first byte of the reply."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(100, 200, 300, 400, 500, 700, 900, 1200, 1600, 2000, 3000))
	providerErrors, _ = meter.Int64Counter("aicc_provider_ws_errors_total",
		metric.WithDescription("Provider sessions that ended on an error rather than a hangup."))
	providerSessions, _ = meter.Int64Counter("aicc_provider_sessions_started_total",
		metric.WithDescription("Provider sessions this process opened, by provider. Vendors "+
			"rate-limit session creation rather than concurrency — doubao allows 60 a "+
			"minute per application id — so the rate of opening them is a quota a busy "+
			"minute can exhaust while every other number here looks healthy."))
	providerUnanswered, _ = meter.Int64Counter("aicc_provider_response_unanswered_total",
		metric.WithDescription("Requests for a response (response.create) the provider never "+
			"created within the client's bound, by provider, kind (OPENING, LINE, TOOL, CUE, "+
			"OWED) and outcome: SUPERSEDED (a line waiting for the floor was asked for "+
			"instead), RETRIED (asked once more) or DROPPED (the retry was not created "+
			"either and the request was given up). Each one is dead air the caller heard."))
	botSessionLimits, _ = meter.Int64Counter("aicc_bot_session_limits_total",
		metric.WithDescription("AI calls the platform ended because they reached the "+
			"flow's time limit (global.maxDurationSec), by how: TRANSFER, counted "+
			"when the transfer to the number's queue is armed (not when it completes), "+
			"or HANGUP when no queue was open."))
	botUnbacked, _ = meter.Int64Counter("aicc_bot_unbacked_claims_total",
		metric.WithDescription("Bot turns that told the caller of an action no tool call "+
			"backed, by claim (TRANSFER, LOOKUP, MESSAGE, FAREWELL) and provider. Read "+
			"from the bot's own transcript, so a signal rather than proof; counted before "+
			"anything acts on it, and labelled by provider to say whether one engine "+
			"does it and the others do not."))
	botInterruptions, _ = meter.Int64Counter("aicc_bot_interruptions_total",
		metric.WithDescription("Times a caller took the floor back from the bot, by what "+
			"took it: SPEECH or DTMF. Speech inside the barge-in guard is not counted — "+
			"it is line echo and was ignored — which makes this the only measurement of "+
			"whether that guard is set right."))
	botFirstMedia, _ = meter.Int64Histogram("aicc_bot_first_media_ms",
		metric.WithDescription("Per bot call: the answer to the first inbound RTP packet, "+
			"in milliseconds. A carrier may open its media path seconds after the "+
			"answer; this is how long, from the real answer."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(20, 50, 100, 250, 500, 1000, 2000, 4000, 6000, 9000, 15000, 30000))
	botFirstAudio, _ = meter.Int64Histogram("aicc_bot_first_audio_ms",
		metric.WithDescription("Per bot call: the answer to the first speech frame queued "+
			"for the caller, in milliseconds, by provider. before_media=true says it was "+
			"queued while no inbound media had arrived, so the caller could not "+
			"have heard it: the greeting went into a path that did not exist yet."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(250, 500, 750, 1000, 1500, 2000, 3000, 5000, 9000, 15000, 30000))
	botSlots, _ = meter.Int64UpDownCounter("aicc_bot_slots",
		metric.WithDescription("Bot capacity in use, by state: IN_USE is an accepted SIP "+
			"dialog, RESERVED is an answered outbound leg holding a slot for the INVITE "+
			"that has not arrived yet. Both count against AICC_BOT_MAX_CALLS."))
	botOverflow, _ = meter.Int64Counter("aicc_bot_overflow_total",
		metric.WithDescription("Answered outbound calls hung up because no bot slot freed "+
			"within the overflow hold (AICC_OVERFLOW_HOLD). Each is a customer who "+
			"answered and met no bot."))
	botOverflowHold, _ = meter.Int64Histogram("aicc_bot_overflow_hold_ms",
		metric.WithDescription("How long an answered outbound leg was parked waiting for a "+
			"bot slot, in milliseconds, by outcome: RESERVED (a slot freed in time), "+
			"OVERFLOW (none did) or ABANDONED (the customer hung up first). Calls that "+
			"found a slot at once are not recorded."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(50, 100, 250, 500, 750, 1000, 1500, 2500))
	webhookDeliveries, _ = meter.Int64Counter("aicc_webhook_deliveries_total",
		metric.WithDescription("CDR deliveries that settled, by outcome. FAILED means the "+
			"retry schedule ran out and that call was never told to that subscriber."))
	campaignUnmappedCause, _ = meter.Int64Counter("aicc_campaign_unmapped_cause_total",
		metric.WithDescription("Campaign attempts that ended with a hangup cause the "+
			"outcome table has no row for. Each was treated as TRANSIENT (retried within "+
			"maxSystemRetries); a rising count means the table needs a row."))
	campaignAttempts, _ = meter.Int64Counter("aicc_campaign_attempts_total",
		metric.WithDescription("Campaign attempts that finished, by outcome. A revision of "+
			"an attempt that was already finished is not counted again."))
	dialerInFlight, _ = meter.Int64UpDownCounter("aicc_dialer_in_flight",
		metric.WithDescription("Campaign calls in flight, by kind: RINGING until the "+
			"customer answers, TALKING from the answer to the end of the call."))
	dialerBlocked, _ = meter.Int64Counter("aicc_dialer_admission_blocked_total",
		metric.WithDescription("Times the dialer had a contact to dial and did not, by "+
			"reason: CAMPAIGN_CONCURRENCY, OUTBOUND_RINGING, BOT_CAPACITY (the expected "+
			"answers would not fit in the bot's free capacity) or DID_LINES (the campaign's "+
			"DID is at its outbound line limit)."))
	didOutboundInUse, _ = meter.Int64UpDownCounter("aicc_did_outbound_in_use",
		metric.WithDescription("Outbound lines held on a DID: calls the platform placed "+
			"or is placing from it, plus hand-dialled legs it observed. Compare with "+
			"the DID's maxOutboundChannels."))
	campaignAnswerUCB, _ = meter.Float64Gauge("aicc_campaign_answer_rate_ucb",
		metric.WithDescription("The answer rate the dialer plans with for a campaign: the "+
			"upper confidence bound over its recent rung attempts, 1 while samples are "+
			"few or overflow control has fallen back."))
	campaignOverflow, _ = meter.Int64Counter("aicc_campaign_overflow_total",
		metric.WithDescription("Campaign calls the customer answered and no bot slot "+
			"could take (outcome OVERFLOW), by campaign."))
	outboundFirstAudio, _ = meter.Float64Histogram("aicc_outbound_first_audio_seconds",
		metric.WithDescription("Time to the first word of an AI outbound call, by stage "+
			"(design 10 section 13.1): ANSWER_TO_INVITE is the customer leg answering to the "+
			"bridge INVITE reaching the SIP server, INVITE_TO_SESSION_READY the INVITE to the "+
			"provider session being ready, SESSION_READY_TO_FIRST_AUDIO that to the first "+
			"bot audio frame queued. Each stage is recorded once per call."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10))
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

// RecordUplinkClock publishes one AI leg's uplink clock counters when it ends.
func RecordUplinkClock(received, sent, filled, droppedBacklog, droppedRx int64) {
	ctx := context.Background()
	for instrument, value := range map[metric.Int64Counter]int64{
		uplinkReceived:    received,
		uplinkSent:        sent,
		uplinkFilled:      filled,
		uplinkDropBacklog: droppedBacklog,
		uplinkDropRx:      droppedRx,
	} {
		if instrument != nil && value > 0 {
			instrument.Add(ctx, value)
		}
	}
}

// RecordTurnLatency publishes one turn's whole wait: the caller falling silent
// to the first reply frame reaching the RTP queue.
func RecordTurnLatency(provider string, ms int64) {
	if turnLatency == nil {
		return
	}
	turnLatency.Record(context.Background(), ms,
		metric.WithAttributes(attribute.String("provider", provider)))
}

// RecordProviderFirstAudio publishes the provider's share of one turn's wait.
func RecordProviderFirstAudio(provider string, ms int64) {
	if providerFirstAud == nil || ms <= 0 {
		return
	}
	providerFirstAud.Record(context.Background(), ms,
		metric.WithAttributes(attribute.String("provider", provider)))
}

// RecordBotFirstMedia publishes how long after the answer the first inbound
// packet of a bot call arrived.
func RecordBotFirstMedia(ms int64) {
	if botFirstMedia == nil {
		return
	}
	botFirstMedia.Record(context.Background(), ms)
}

// RecordBotFirstAudio publishes how long after the answer the bot's first
// speech frame was queued, and whether that was before any inbound media.
func RecordBotFirstAudio(provider string, ms int64, isBeforeMedia bool) {
	if botFirstAudio == nil {
		return
	}
	botFirstAudio.Record(context.Background(), ms, metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.Bool("before_media", isBeforeMedia)))
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

// SessionLimitOutcome labels how a call that reached its time limit ended.
const (
	SessionLimitOutcomeTransfer = "TRANSFER"
	SessionLimitOutcomeHangup   = "HANGUP"
)

// RecordSessionLimit counts one AI call the platform ended because it reached
// the flow's time limit, by outcome (SessionLimitOutcome*).
//
// It is the platform's own cap: a rise here says callers are being kept on the
// line longer than the flow allows, a loop or a conversation the flow never
// closes, and the flow is what answers it.
func RecordSessionLimit(outcome string) {
	if botSessionLimits == nil {
		return
	}
	botSessionLimits.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordUnbackedClaim counts one bot turn that claimed an action — a
// transfer, a lookup, a saved message, a goodbye — with no tool call behind
// it (#44).
//
// The flow cannot see such a turn: it moves on tool results and silence, and
// a claim is neither. What an operator asks is how often it happens and on
// which engine, and the answer decides whether the prompt is enough or the
// platform must step in; one occurrence answers neither.
func RecordUnbackedClaim(claim, provider string) {
	if botUnbacked == nil {
		return
	}
	botUnbacked.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("claim", claim), attribute.String("provider", provider)))
}

// RecordProviderSessionStarted counts one conversation session being opened.
//
// It is a rate, not a population: aicc_calls_active already says how many are
// up. What this answers is the question that gauge cannot — how fast they are
// being created — because the vendor's limit is on creations per minute (60 per
// application id on doubao) and not on how many run at once. A deployment can
// sit well inside its concurrency and still be refused at the door.
func RecordProviderSessionStarted(provider string) {
	if providerSessions == nil {
		return
	}
	providerSessions.Add(context.Background(), 1,
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

// The outcomes of aicc_provider_response_unanswered_total.
const (
	ResponseUnansweredSuperseded = "SUPERSEDED"
	ResponseUnansweredRetried    = "RETRIED"
	ResponseUnansweredDropped    = "DROPPED"
)

// RecordResponseUnanswered counts one request for a response the provider
// never created, by what asked for it and what was done instead
// (ResponseUnanswered*).
//
// An engine that drops a request says nothing, so this is the only trace of
// it beyond a log line: a rate that climbs after a model or vendor update says
// the engine has started dropping a frame shape it used to answer.
func RecordResponseUnanswered(provider, kind, outcome string) {
	if providerUnanswered == nil {
		return
	}
	providerUnanswered.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("kind", kind),
		attribute.String("outcome", outcome)))
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

// The states of aicc_bot_slots.
const (
	BotSlotInUse    = "IN_USE"
	BotSlotReserved = "RESERVED"
)

// The outcomes of a hold for a bot slot.
const (
	BotHoldReserved  = "RESERVED"
	BotHoldOverflow  = "OVERFLOW"
	BotHoldAbandoned = "ABANDONED"
)

// BotSlotsChanged moves the bot-capacity gauge for one state.
func BotSlotsChanged(state string, delta int64) {
	if botSlots == nil {
		return
	}
	botSlots.Add(context.Background(), delta,
		metric.WithAttributes(attribute.String("state", state)))
}

// RecordBotHold records an answered outbound leg that had to wait for a bot
// slot. Only an overflow moves the overflow counter.
func RecordBotHold(outcome string, ms int64) {
	if botOverflowHold != nil {
		botOverflowHold.Record(context.Background(), ms,
			metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	if outcome == BotHoldOverflow && botOverflow != nil {
		botOverflow.Add(context.Background(), 1)
	}
}

// The kinds of aicc_dialer_in_flight and the reasons of
// aicc_dialer_admission_blocked_total.
const (
	DialerRinging = "RINGING"
	DialerTalking = "TALKING"

	DialerBlockedCampaign = "CAMPAIGN_CONCURRENCY"
	DialerBlockedRinging  = "OUTBOUND_RINGING"
	DialerBlockedBot      = "BOT_CAPACITY"
	DialerBlockedDIDLines = "DID_LINES"
)

// DIDOutboundInUseChanged moves the outbound-lines gauge of a DID.
func DIDOutboundInUseChanged(did string, delta int64) {
	if didOutboundInUse != nil {
		didOutboundInUse.Add(context.Background(), delta,
			metric.WithAttributes(attribute.String("did", did)))
	}
}

// SetCampaignAnswerRateUCB publishes the answer rate a campaign is admitted by.
func SetCampaignAnswerRateUCB(campaignID string, p float64) {
	if campaignAnswerUCB != nil {
		campaignAnswerUCB.Record(context.Background(), p,
			metric.WithAttributes(attribute.String("campaign", campaignID)))
	}
}

// RecordCampaignOverflow counts an answered campaign call that met no bot.
func RecordCampaignOverflow(campaignID string) {
	if campaignOverflow != nil {
		campaignOverflow.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("campaign", campaignID)))
	}
}

// RecordCampaignAttempt counts a finished campaign attempt by outcome.
func RecordCampaignAttempt(outcome string) {
	if campaignAttempts != nil {
		campaignAttempts.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// DialerInFlightChanged moves the in-flight gauge for one kind.
func DialerInFlightChanged(kind string, delta int64) {
	if dialerInFlight != nil {
		dialerInFlight.Add(context.Background(), delta,
			metric.WithAttributes(attribute.String("kind", kind)))
	}
}

// RecordDialerBlocked counts an admission refused for a reason.
func RecordDialerBlocked(reason string) {
	if dialerBlocked != nil {
		dialerBlocked.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// RecordCampaignUnmappedCause counts an attempt whose hangup cause the
// campaign outcome table does not know. The cause is a label, so it is cut to
// a bounded alphabet and length: a free string must not mint unbounded series.
func RecordCampaignUnmappedCause(cause string) {
	if campaignUnmappedCause == nil {
		return
	}
	label := make([]byte, 0, 40)
	for i := 0; i < len(cause) && len(label) < 40; i++ {
		c := cause[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			label = append(label, c)
		}
	}
	if len(label) == 0 {
		label = append(label, "NONE"...)
	}
	campaignUnmappedCause.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("cause", string(label))))
}

// The stages of the time to the first word of an AI outbound call, the label
// values of aicc_outbound_first_audio_seconds.
const (
	OutboundStageAnswerToInvite    = "ANSWER_TO_INVITE"
	OutboundStageInviteToReady     = "INVITE_TO_SESSION_READY"
	OutboundStageReadyToFirstAudio = "SESSION_READY_TO_FIRST_AUDIO"
	outboundStageTrackedCalls      = 4096
	outboundStageStaleAfter        = 5 * time.Minute
	outboundStageSweepThreshold    = 256
)

var outboundFirstAudio metric.Float64Histogram

// outboundStages remembers, per AI outbound call, how far toward the first
// word it has got. Only calls the application placed are entered (at the
// answer), so every other bot call costs a map miss once per stage and never
// a frame: the first-audio hook runs once per call, not per frame.
var outboundStages = struct {
	sync.Mutex
	calls map[uuid.UUID]*outboundStage
}{calls: map[uuid.UUID]*outboundStage{}}

type outboundStage struct {
	answeredAt time.Time
	invitedAt  time.Time
	readyAt    time.Time
}

// OutboundAnswered starts the clock: the customer leg of an AI outbound call
// answered and is being bridged to the bot.
func OutboundAnswered(callID uuid.UUID, at time.Time) {
	outboundStages.Lock()
	defer outboundStages.Unlock()
	if len(outboundStages.calls) >= outboundStageSweepThreshold {
		for id, st := range outboundStages.calls {
			if at.Sub(st.answeredAt) > outboundStageStaleAfter {
				delete(outboundStages.calls, id)
			}
		}
	}
	if len(outboundStages.calls) >= outboundStageTrackedCalls {
		return
	}
	outboundStages.calls[callID] = &outboundStage{answeredAt: at}
}

// OutboundInvited records the bridge INVITE reaching the SIP server (stage 1).
// Calls that were not entered at the answer are ignored.
func OutboundInvited(callID uuid.UUID, at time.Time) {
	outboundStages.Lock()
	defer outboundStages.Unlock()
	st := outboundStages.calls[callID]
	if st == nil || !st.invitedAt.IsZero() {
		return
	}
	st.invitedAt = at
	recordOutboundStage(OutboundStageAnswerToInvite, at.Sub(st.answeredAt))
}

// OutboundSessionReady records the provider session being ready (stage 2).
func OutboundSessionReady(callID uuid.UUID, at time.Time) {
	outboundStages.Lock()
	defer outboundStages.Unlock()
	st := outboundStages.calls[callID]
	if st == nil || st.invitedAt.IsZero() || !st.readyAt.IsZero() {
		return
	}
	st.readyAt = at
	recordOutboundStage(OutboundStageInviteToReady, at.Sub(st.invitedAt))
}

// OutboundFirstAudio records the first bot audio frame (stage 3) and ends the
// call's tracking. A provider may speak before its session start returns; then
// the session counts as ready at that frame and stage 3 is zero.
func OutboundFirstAudio(callID uuid.UUID, at time.Time) {
	outboundStages.Lock()
	defer outboundStages.Unlock()
	st := outboundStages.calls[callID]
	if st == nil || st.invitedAt.IsZero() {
		return
	}
	if st.readyAt.IsZero() {
		st.readyAt = at
		recordOutboundStage(OutboundStageInviteToReady, at.Sub(st.invitedAt))
	}
	recordOutboundStage(OutboundStageReadyToFirstAudio, at.Sub(st.readyAt))
	delete(outboundStages.calls, callID)
}

// OutboundForget drops a call that ended before its first word.
func OutboundForget(callID uuid.UUID) {
	outboundStages.Lock()
	delete(outboundStages.calls, callID)
	outboundStages.Unlock()
}

func recordOutboundStage(stage string, d time.Duration) {
	if outboundFirstAudio == nil {
		return
	}
	outboundFirstAudio.Record(context.Background(), max(d, 0).Seconds(),
		metric.WithAttributes(attribute.String("stage", stage)))
}
