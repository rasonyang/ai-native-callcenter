// SPDX-License-Identifier: Apache-2.0

// Package telephony is the call-control layer: it normalizes FreeSWITCH
// events, drives the call and agent state machines, and issues switch
// commands. Raw FreeSWITCH header names exist only inside this package.
package telephony

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// SwitchEventKind is the normalized event vocabulary. FreeSWITCH names never
// leave this package; everything above speaks these.
type SwitchEventKind string

// Channel lifecycle.
const (
	KindChannelCreate   SwitchEventKind = "CHANNEL_CREATE"
	KindChannelAnswer   SwitchEventKind = "CHANNEL_ANSWER"
	KindChannelPark     SwitchEventKind = "CHANNEL_PARK"
	KindChannelBridge   SwitchEventKind = "CHANNEL_BRIDGE"
	KindChannelUnbridge SwitchEventKind = "CHANNEL_UNBRIDGE"
	KindChannelHold     SwitchEventKind = "CHANNEL_HOLD"
	KindChannelUnhold   SwitchEventKind = "CHANNEL_UNHOLD"
	KindChannelHangup   SwitchEventKind = "CHANNEL_HANGUP"
	KindDTMF            SwitchEventKind = "DTMF"
	KindRecordStart     SwitchEventKind = "RECORD_START"
	KindRecordStop      SwitchEventKind = "RECORD_STOP"
)

// Endpoint registration, observed from sofia.
const (
	KindDeviceRegistered   SwitchEventKind = "DEVICE_REGISTERED"
	KindDeviceUnregistered SwitchEventKind = "DEVICE_UNREGISTERED"
	KindDeviceState        SwitchEventKind = "DEVICE_STATE"
)

// The media tap's own account of itself, from mod_audio_stream.
const (
	KindAudioStreamConnected    SwitchEventKind = "AUDIO_STREAM_CONNECTED"
	KindAudioStreamDisconnected SwitchEventKind = "AUDIO_STREAM_DISCONNECTED"
	KindAudioStreamError        SwitchEventKind = "AUDIO_STREAM_ERROR"
)

// Queue activity, observed from mod_callcenter.
const (
	KindQueueMemberJoined SwitchEventKind = "QUEUE_MEMBER_JOINED"
	KindQueueMemberLeft   SwitchEventKind = "QUEUE_MEMBER_LEFT"
	KindQueueAgentOffered SwitchEventKind = "QUEUE_AGENT_OFFERED"
	KindQueueBridgeStart  SwitchEventKind = "QUEUE_BRIDGE_START"
	KindQueueBridgeEnd    SwitchEventKind = "QUEUE_BRIDGE_END"
	KindQueueBridgeFailed SwitchEventKind = "QUEUE_BRIDGE_FAILED"
	KindQueueAgentState   SwitchEventKind = "QUEUE_AGENT_STATE"
	KindQueueAgentStatus  SwitchEventKind = "QUEUE_AGENT_STATUS"
	KindQueueMembersCount SwitchEventKind = "QUEUE_MEMBERS_COUNT"
)

// BotShare is what the AI leg stamped onto the caller's channel before
// handing the call to a person: its part of the eventual CDR. The channel
// variable names stop at this boundary.
type BotShare struct {
	Sec     int
	FlowID  *uuid.UUID
	DID     string
	Queue   string
	Summary string
	Reason  string
}

// IsZero reports whether the call ever met a bot.
func (b BotShare) IsZero() bool {
	return b.Sec == 0 && b.FlowID == nil && b.DID == "" && b.Queue == ""
}

// Merge fills in what this share does not know yet from another leg's copy.
//
// The legs of one call know different parts. The dialplan exports the DID to
// the leg it dials towards the bot, so that leg can answer for the DID and
// nothing else — and on a transfer it hangs up first, the moment the caller
// moves on. The bot's own tally, how long it spoke and which flow it ran and
// what it learned, is stamped on the caller's channel and only arrives a
// conversation later, when that leg finally hangs up.
//
// Taking the first non-empty share whole let the bot leg's DID shut the
// caller's leg out, and every transferred call reached the ledger with
// bot_sec 0, no flow and no summary.
func (b *BotShare) Merge(other BotShare) {
	if b.Sec == 0 {
		b.Sec = other.Sec
	}
	if b.FlowID == nil {
		b.FlowID = other.FlowID
	}
	if b.DID == "" {
		b.DID = other.DID
	}
	if b.Queue == "" {
		b.Queue = other.Queue
	}
	if b.Summary == "" {
		b.Summary = other.Summary
	}
	if b.Reason == "" {
		b.Reason = other.Reason
	}
}

func botShare(ev *esl.Event) BotShare {
	var out BotShare
	if sec, ok := ev.GetInt("variable_aicc_bot_sec"); ok {
		out.Sec = int(sec)
	}
	if raw := ev.Get("variable_aicc_flow_id"); raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil {
			out.FlowID = &parsed
		}
	}
	out.DID = ev.Get("variable_aicc_did")
	out.Queue = ev.Get("variable_aicc_queue")
	out.Summary = ev.Get("variable_aicc_bot_summary")
	out.Reason = ev.Get("variable_aicc_bot_reason")
	return out
}

// CallDirection is the switch's view of which side started the channel.
type CallDirection string

// Channel directions as reported by FreeSWITCH.
const (
	DirectionInbound  CallDirection = "INBOUND"
	DirectionOutbound CallDirection = "OUTBOUND"
)

// SwitchEvent is a normalized FreeSWITCH event.
//
// Only fields relevant to a given Kind are populated; the raw event stays
// attached for diagnostics and for the few places that need an unmapped
// channel variable.
type SwitchEvent struct {
	Kind       SwitchEventKind
	OccurredAt time.Time

	// Channel identity.
	ChannelID      string
	OtherChannelID string
	ChannelName    string
	Direction      CallDirection

	// Caller profile.
	ANI               string // calling number
	DestinationNumber string // dialed number
	CallerIDName      string
	Context           string
	// CallTypeHint is the type stamped by whoever placed the call, for the
	// cases the switch's own view cannot decide: every leg we originate is
	// "outbound" to the switch, whether it reaches an extension down the hall
	// or a carrier. Empty leaves the classification to the channel.
	CallTypeHint string

	// Hangup detail.
	HangupCause     string
	HangupCauseQ850 int
	// BilledSec is the switch's own count of the seconds this leg was
	// answered, read from its billsec at hangup. It is not what the ledger
	// records — that is derived from the leg's own timestamps — but a second,
	// independent account of the same fact, which is what makes it worth
	// carrying: a billing figure with nothing to check it against is a figure
	// nobody can dispute or defend.
	BilledSec int
	// Bot carries the AI leg's share of the story, read from channel
	// variables when the caller's leg hangs up. Zero when the call never
	// met a bot.
	Bot BotShare
	// TransferredAway reports that this leg ended because the call moved on
	// (blind transfer or REFER), not because the caller dropped.
	TransferredAway bool

	// DTMF.
	Digit      string
	DurationMs int

	// Recording.
	RecordingPath string

	// Device registration.
	Extension  string
	SIPCallID  string
	NetworkIP  string
	UserAgent  string
	Registered bool

	// Queue activity (mod_callcenter).
	Queue       string
	AgentName   string
	AgentState  string
	AgentStatus string
	// MemberChannelID is the waiting caller's channel: on a queue event the
	// member the event is about, on a channel event the caller the leg was
	// dialled to serve. Empty on a leg that is nobody's delivery.
	MemberChannelID string
	MemberCount     int
	Cause           string
	CancelReason    string
	JoinedAt        time.Time
	LeftAt          time.Time

	// Raw is the underlying event, for logging and rare lookups.
	Raw *esl.Event
}

// Subscriptions is the exact event set the application asks FreeSWITCH for.
var Subscriptions = []string{
	"CHANNEL_CREATE", "CHANNEL_ANSWER", "CHANNEL_PARK", "CHANNEL_BRIDGE",
	"CHANNEL_UNBRIDGE", "CHANNEL_HOLD", "CHANNEL_UNHOLD",
	"CHANNEL_HANGUP_COMPLETE", "DTMF", "RECORD_START", "RECORD_STOP",
	"CUSTOM", "sofia::register", "sofia::unregister", "sofia::sip_user_state",
	"callcenter::info",
	// mod_audio_stream reports on the media tap it runs for us. Without these
	// the module is a thing we command and never hear from: a stream that
	// never connected, one the far end dropped, and one that errored all look
	// identical from here, which is how a whole call's audio can go nowhere
	// with nothing anywhere reporting a fault.
	"mod_audio_stream::connect", "mod_audio_stream::disconnect",
	"mod_audio_stream::error", "mod_audio_stream::json",
	"mod_audio_stream::play",
}

// Normalize converts a raw event, reporting false for events this application
// does not consume.
func Normalize(ev *esl.Event) (SwitchEvent, bool) {
	if ev == nil {
		return SwitchEvent{}, false
	}

	out := SwitchEvent{
		OccurredAt:  ev.Timestamp(),
		ChannelID:   ev.UniqueID(),
		ChannelName: ev.Get("Channel-Name"),
		Raw:         ev,
	}
	if out.OccurredAt.IsZero() {
		out.OccurredAt = time.Now().UTC()
	}

	if ev.Name() == "CUSTOM" {
		return normalizeCustom(ev, out)
	}
	return normalizeChannel(ev, out)
}

func normalizeChannel(ev *esl.Event, out SwitchEvent) (SwitchEvent, bool) {
	// The caller profile is present on every channel event; ANI falls back to
	// the caller id number, as verified against live events.
	out.ANI = ev.GetFirst("Caller-ANI", "Caller-Caller-ID-Number")
	out.DestinationNumber = ev.Get("Caller-Destination-Number")
	out.CallerIDName = ev.Get("Caller-Caller-ID-Name")
	out.Context = ev.Get("Caller-Context")
	out.OtherChannelID = ev.Get("Other-Leg-Unique-ID")
	out.CallTypeHint = ev.Get("variable_aicc_call_type")

	// mod_callcenter dials the agent on a waiting caller's behalf and stamps
	// that caller's channel onto the leg it creates, which is the only thing
	// tying the two together before they bridge. cc_side names which half of a
	// delivery a leg is; the member's own leg carries its own id here, so the
	// comparison identifies the agent's half even where cc_side is absent.
	if member := ev.Get("variable_cc_member_session_uuid"); member != "" && member != out.ChannelID {
		out.MemberChannelID = member
	}

	switch strings.ToUpper(ev.Get("Call-Direction")) {
	case "INBOUND":
		out.Direction = DirectionInbound
	case "OUTBOUND":
		out.Direction = DirectionOutbound
	}

	switch ev.Name() {
	case "CHANNEL_CREATE":
		out.Kind = KindChannelCreate
	case "CHANNEL_ANSWER":
		out.Kind = KindChannelAnswer
	case "CHANNEL_PARK":
		out.Kind = KindChannelPark
	case "CHANNEL_BRIDGE":
		out.Kind = KindChannelBridge
		// Bridge events name both legs explicitly.
		if a, b := ev.Get("Bridge-A-Unique-ID"), ev.Get("Bridge-B-Unique-ID"); a != "" && b != "" {
			out.ChannelID, out.OtherChannelID = a, b
		}
	case "CHANNEL_UNBRIDGE":
		out.Kind = KindChannelUnbridge
	case "CHANNEL_HOLD":
		out.Kind = KindChannelHold
	case "CHANNEL_UNHOLD":
		out.Kind = KindChannelUnhold
	case "CHANNEL_HANGUP_COMPLETE":
		out.Kind = KindChannelHangup
		out.HangupCause = ev.GetFirst("Hangup-Cause", "variable_hangup_cause")
		if q850, ok := ev.GetInt("variable_hangup_cause_q850"); ok {
			out.HangupCauseQ850 = int(q850)
		}
		out.TransferredAway = transferredAway(ev)
		out.Bot = botShare(ev)
		if sec, ok := ev.GetInt("variable_billsec"); ok {
			out.BilledSec = int(sec)
		}
	case "DTMF":
		out.Kind = KindDTMF
		out.Digit = ev.Get("DTMF-Digit")
		// FreeSWITCH reports duration in 8 kHz RTP ticks.
		if ticks, ok := ev.GetInt("DTMF-Duration"); ok {
			out.DurationMs = int(ticks / 8)
		}
	case "RECORD_START":
		out.Kind = KindRecordStart
		out.RecordingPath = ev.Get("Record-File-Path")
	case "RECORD_STOP":
		out.Kind = KindRecordStop
		out.RecordingPath = ev.Get("Record-File-Path")
	default:
		return SwitchEvent{}, false
	}
	return out, true
}

// transferredAway reports whether a leg ended because the call was transferred
// rather than dropped. A successful handoff must not be reported as a lost
// call, so both the SIP disposition and the transfer history are consulted.
func transferredAway(ev *esl.Event) bool {
	switch ev.Variable("sip_hangup_disposition") {
	case "recv_refer", "send_refer":
		return true
	}
	switch strings.ToUpper(ev.GetFirst("Hangup-Cause", "variable_hangup_cause")) {
	case "BLIND_TRANSFER", "ATTENDED_TRANSFER":
		return true
	}
	return ev.Variable("transfer_history") != ""
}

func normalizeCustom(ev *esl.Event, out SwitchEvent) (SwitchEvent, bool) {
	switch ev.Subclass() {
	case "sofia::register", "sofia::unregister":
		out.Kind = KindDeviceRegistered
		out.Registered = true
		if ev.Subclass() == "sofia::unregister" {
			out.Kind = KindDeviceUnregistered
			out.Registered = false
		}
		out.Extension = ev.GetFirst("username", "from-user")
		out.SIPCallID = ev.Get("call-id")
		out.NetworkIP = ev.Get("network-ip")
		out.UserAgent = ev.Get("user-agent")
		out.ChannelID = ""
		return out, true

	case "sofia::sip_user_state":
		out.Kind = KindDeviceState
		out.Extension = ev.GetFirst("user", "username", "from-user")
		// FreeSWITCH reports reachability from its OPTIONS ping.
		out.Registered = !strings.EqualFold(ev.Get("ping-status"), "DOWN")
		out.ChannelID = ""
		return out, true

	case "mod_audio_stream::connect":
		out.Kind = KindAudioStreamConnected
		return out, true

	case "mod_audio_stream::disconnect":
		out.Kind = KindAudioStreamDisconnected
		return out, true

	case "mod_audio_stream::error":
		out.Kind = KindAudioStreamError
		// The module puts its complaint in the body; the header carries only
		// which channel it was about.
		out.Cause = ev.GetFirst("Error", "error", "Reply-Text")
		return out, true

	case "callcenter::info":
		return normalizeCallcenter(ev, out)
	}
	return SwitchEvent{}, false
}

// normalizeCallcenter maps mod_callcenter's CC-* headers. Its own vocabulary
// ("Available", "member-queue-start") stops here.
func normalizeCallcenter(ev *esl.Event, out SwitchEvent) (SwitchEvent, bool) {
	// mod_callcenter names queues with the switch domain appended; the domain
	// is upstream vocabulary and stops here.
	out.Queue, _, _ = strings.Cut(ev.Get("CC-Queue"), "@")
	out.AgentName = ev.Get("CC-Agent")
	out.MemberChannelID = ev.Get("CC-Member-Session-UUID")
	// The queue's own copy of who is waiting. It is the only account of the
	// caller a queue event carries: the channel-event caller profile is
	// absent here, and a member who has not been adopted as a call has no
	// other source of a number at all.
	out.ANI = ev.Get("CC-Member-CID-Number")
	out.CallerIDName = ev.Get("CC-Member-CID-Name")
	if out.ChannelID == "" {
		out.ChannelID = out.MemberChannelID
	}
	out.JoinedAt = epochSeconds(ev.Get("CC-Member-Joined-Time"))

	switch ev.Get("CC-Action") {
	case "member-queue-start":
		out.Kind = KindQueueMemberJoined
	case "member-queue-end":
		out.Kind = KindQueueMemberLeft
		out.Cause = ev.Get("CC-Cause")
		out.CancelReason = ev.Get("CC-Cancel-Reason")
		out.LeftAt = epochSeconds(ev.Get("CC-Member-Leaving-Time"))
	case "agent-offering":
		out.Kind = KindQueueAgentOffered
	case "bridge-agent-start":
		out.Kind = KindQueueBridgeStart
	case "bridge-agent-end":
		out.Kind = KindQueueBridgeEnd
	case "bridge-agent-fail":
		out.Kind = KindQueueBridgeFailed
		out.HangupCause = ev.Get("CC-Hangup-Cause")
	case "agent-state-change":
		out.Kind = KindQueueAgentState
		out.AgentState = ev.Get("CC-Agent-State")
	case "agent-status-change":
		out.Kind = KindQueueAgentStatus
		out.AgentStatus = ev.Get("CC-Agent-Status")
	case "members-count":
		out.Kind = KindQueueMembersCount
		if n, ok := ev.GetInt("CC-Count"); ok {
			out.MemberCount = int(n)
		}
	default:
		// agent-add, tier-update and similar administrative echoes of our own
		// commands carry no state we do not already know.
		return SwitchEvent{}, false
	}

	// A queue event about a waiting caller belongs to that caller's call, and
	// mod_callcenter does not always raise it on their channel: the bridge is
	// announced on the leg it dialled to reach the agent. Routing by whichever
	// channel the switch happened to use put the bridge time on the agent's
	// leg, where the caller's ledger never saw it, and queue_wait_sec fell
	// back to the moment the caller left the queue — counting the whole
	// conversation as time spent waiting.
	switch out.Kind {
	case KindQueueMemberJoined, KindQueueMemberLeft, KindQueueAgentOffered,
		KindQueueBridgeStart, KindQueueBridgeEnd, KindQueueBridgeFailed:
		if out.MemberChannelID != "" {
			out.ChannelID = out.MemberChannelID
		}
	}
	return out, true
}

func epochSeconds(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
