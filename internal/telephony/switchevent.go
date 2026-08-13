// SPDX-License-Identifier: Apache-2.0

// Package telephony is the call-control layer: it normalizes FreeSWITCH
// events, drives the call and agent state machines, and issues switch
// commands. Raw FreeSWITCH header names exist only inside this package.
package telephony

import (
	"strconv"
	"strings"
	"time"

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

	// Hangup detail.
	HangupCause     string
	HangupCauseQ850 int
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
	Queue           string
	AgentName       string
	AgentState      string
	AgentStatus     string
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

	case "callcenter::info":
		return normalizeCallcenter(ev, out)
	}
	return SwitchEvent{}, false
}

// normalizeCallcenter maps mod_callcenter's CC-* headers. Its own vocabulary
// ("Available", "member-queue-start") stops here.
func normalizeCallcenter(ev *esl.Event, out SwitchEvent) (SwitchEvent, bool) {
	out.Queue = ev.Get("CC-Queue")
	out.AgentName = ev.Get("CC-Agent")
	out.MemberChannelID = ev.Get("CC-Member-Session-UUID")
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
