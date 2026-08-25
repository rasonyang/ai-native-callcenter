// SPDX-License-Identifier: Apache-2.0

// Package events defines the server-sent event contract and the fan-out hub.
//
// The wire contract is documented in docs/design/04-api-sse.md: one ordered
// stream per browser session, a global monotonic seq, resume via
// Last-Event-ID, and identity-derived scoping.
package events

import (
	"time"

	"github.com/google/uuid"
)

// Version is the envelope schema version. It changes only on a breaking
// change; new event types and new fields are additive.
const Version = 1

// Type is an SSE event type. Party-scoped types describe one call leg;
// call-scoped types describe the aggregate.
type Type string

// Party lifecycle: one event per leg, partyId always set.
const (
	TypePartyDialing     Type = "PARTY_DIALING"
	TypePartyRinging     Type = "PARTY_RINGING"
	TypePartyEstablished Type = "PARTY_ESTABLISHED"
	TypePartyHeld        Type = "PARTY_HELD"
	TypePartyRetrieved   Type = "PARTY_RETRIEVED"
	TypePartyReleased    Type = "PARTY_RELEASED"
	TypePartyChanged     Type = "PARTY_CHANGED"
	TypePartyDTMF        Type = "PARTY_DTMF"
)

// Call-scoped facts about the aggregate.
const (
	TypeCallUserData           Type = "CALL_USER_DATA"
	TypeCallRecordingStarted   Type = "CALL_RECORDING_STARTED"
	TypeCallRecordingStopped   Type = "CALL_RECORDING_STOPPED"
	TypeCallCDR                Type = "CALL_CDR"
	TypeCallTranscript         Type = "CALL_TRANSCRIPT"
	TypeCallTranscriptionState Type = "CALL_TRANSCRIPTION_STATE"
)

// Queue, agent, device, bot and callback scopes.
const (
	TypeQueueJoined       Type = "QUEUE_JOINED"
	TypeQueueLeft         Type = "QUEUE_LEFT"
	TypeQueueCount        Type = "QUEUE_COUNT"
	TypeQueueAgentOffered Type = "QUEUE_AGENT_OFFERED"
	TypeAgentLoggedIn     Type = "AGENT_LOGGED_IN"
	TypeAgentLoggedOut    Type = "AGENT_LOGGED_OUT"
	TypeAgentReady        Type = "AGENT_READY"
	TypeAgentNotReady     Type = "AGENT_NOT_READY"
	TypeAgentAvailability Type = "AGENT_AVAILABILITY"
	// A phone lives on two independent axes and each transition has a name of
	// its own. REGISTERED/UNREGISTERED say whether a SIP registration exists;
	// REACHABLE/UNREACHABLE say whether the registered phone still answers the
	// switch's OPTIONS ping.
	//
	// IN_SERVICE used to stand where REACHABLE stands, and it had no opposite:
	// a phone that stopped answering was announced as UNREGISTERED, which was
	// a claim about the other axis and not true. That is the failure that
	// matters most here — a dead browser tab is still registered and looks
	// exactly like a working one, so its agent sits in ready while every call
	// rings out — and it now says what it is.
	TypeDeviceRegistered   Type = "DEVICE_REGISTERED"
	TypeDeviceUnregistered Type = "DEVICE_UNREGISTERED"
	TypeDeviceReachable    Type = "DEVICE_REACHABLE"
	TypeDeviceUnreachable  Type = "DEVICE_UNREACHABLE"
	// The bot's half of a call has one thing to say that nothing else says:
	// that the conversation actually started. PARTY_ESTABLISHED on the bot leg
	// means the SIP leg answered, and the model session is opened after that
	// and can fail — the caller is rescued to a queue, and from the stream
	// alone that looked exactly like a bot which talked and handed over.
	//
	// There is no ENDED to match it and no INTERRUPTED beside it, deliberately.
	// The bot leg is a party, so its end is already PARTY_RELEASED to the same
	// audience, and the CDR carries botSec, isContained and the bot's own
	// reason; a second word for it would be a second announcement of one fact.
	// Barge-in is a rate to watch rather than a thing to watch happen, so it is
	// a metric (aicc_bot_interruptions_total) — one event per interruption
	// answers "is it happening now", which nothing asks, and not "how often",
	// which is the question tuning the barge guard depends on.
	TypeBotSessionStarted Type = "BOT_SESSION_STARTED"
	TypeCallbackCreated   Type = "CALLBACK_CREATED"
	TypeCallbackUpdated   Type = "CALLBACK_UPDATED"
)

// System events describe the server itself.
const (
	TypeSystemLink  Type = "SYSTEM_LINK"
	TypeSystemReset Type = "SYSTEM_RESET"
)

// CallType is the caller-perspective classification of a call. It is stamped
// at creation and never changes, including across transfers.
type CallType string

// Call types.
const (
	CallTypeInbound  CallType = "INBOUND"
	CallTypeOutbound CallType = "OUTBOUND"
	CallTypeConsult  CallType = "CONSULT"
	CallTypeInternal CallType = "INTERNAL"
)

// Event is one envelope on the stream. Call events repeat enough context
// (callType, numbers, userData) for a screen-pop without further requests.
type Event struct {
	Version    int            `json:"version"`
	Seq        int64          `json:"seq"`
	Type       Type           `json:"type"`
	OccurredAt time.Time      `json:"occurredAt"`
	CallID     *uuid.UUID     `json:"callId,omitempty"`
	CallType   CallType       `json:"callType,omitempty"`
	PartyID    *uuid.UUID     `json:"partyId,omitempty"`
	AgentID    *uuid.UUID     `json:"agentId,omitempty"`
	QueueID    *uuid.UUID     `json:"queueId,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	UserData   map[string]any `json:"userData,omitempty"`
}

// Scope classifies who should receive an event. The hub applies it against a
// subscriber's identity; clients cannot widen their own scope.
//
// The zero value delivers to supervisors and administrators only. That is the
// deliberate default: an event whose scope nobody set is an event nobody
// decided the audience of, and the safe reading of an undecided audience is
// the one that already sees everything. It used to be the opposite — an empty
// scope fell through to every subscriber — which meant "nobody is on this call
// yet" and "everyone may see this" were the same value.
type Scope struct {
	// AgentIDs receive the event because they are a party to it or it is
	// about them. Empty means no agent in particular, not every agent.
	AgentIDs []uuid.UUID
	// QueueID also admits agents staffing that queue. It widens AgentIDs
	// rather than restricting it, so an event that must reach only the agents
	// on a call leaves it nil.
	QueueID *uuid.UUID
	// IsBroadcast marks the few events that are genuinely everybody's — a
	// callback appearing on every screen, a system notice. Stated rather than
	// inferred from an empty scope, because those two used to be the same
	// value and one of them was a mistake.
	IsBroadcast bool
}
