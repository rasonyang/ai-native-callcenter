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
	TypeCallUserData         Type = "CALL_USER_DATA"
	TypeCallRecordingStarted Type = "CALL_RECORDING_STARTED"
	TypeCallRecordingStopped Type = "CALL_RECORDING_STOPPED"
	TypeCallCDR              Type = "CALL_CDR"
)

// Queue, agent, device, bot and callback scopes.
const (
	TypeQueueJoined        Type = "QUEUE_JOINED"
	TypeQueueLeft          Type = "QUEUE_LEFT"
	TypeQueueCount         Type = "QUEUE_COUNT"
	TypeQueueAgentOffered  Type = "QUEUE_AGENT_OFFERED"
	TypeAgentLoggedIn      Type = "AGENT_LOGGED_IN"
	TypeAgentLoggedOut     Type = "AGENT_LOGGED_OUT"
	TypeAgentReady         Type = "AGENT_READY"
	TypeAgentNotReady      Type = "AGENT_NOT_READY"
	TypeAgentAvailability  Type = "AGENT_AVAILABILITY"
	TypeDeviceRegistered   Type = "DEVICE_REGISTERED"
	TypeDeviceUnregistered Type = "DEVICE_UNREGISTERED"
	TypeDeviceInService    Type = "DEVICE_IN_SERVICE"
	TypeBotSessionStarted  Type = "BOT_SESSION_STARTED"
	TypeBotTranscript      Type = "BOT_TRANSCRIPT"
	TypeBotInterrupted     Type = "BOT_INTERRUPTED"
	TypeBotSessionEnded    Type = "BOT_SESSION_ENDED"
	TypeCallbackCreated    Type = "CALLBACK_CREATED"
	TypeCallbackUpdated    Type = "CALLBACK_UPDATED"
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
type Scope struct {
	// AgentIDs receive the event because they are a party to it or it is
	// about them. Empty means "not agent-specific".
	AgentIDs []uuid.UUID
	// QueueID restricts delivery to agents staffing that queue.
	QueueID *uuid.UUID
	// SupervisorOnly marks events that agents never receive.
	SupervisorOnly bool
}
