// SPDX-License-Identifier: Apache-2.0

// Code generated from docs/openapi.json (root x-scopes) by scripts/gen-scopes.mjs. DO NOT EDIT.

package api

// The scope vocabulary. A scope is a capability, never a role: the names are
// `resource:action[:range]` and no single one of them is a role's alias
// (docs/design/07-naming.md §7).
//
// The values are the wire strings. They are untyped string constants so they
// drop straight into the []string the contract's request bodies use.
const (
	// ScopeAgentAct — Drive this subject's own agent presence: sign in and out, ready, not ready, wrap up.
	ScopeAgentAct = "agent:act"

	// ScopeAgentManage — Supervise other agents: read the roster and force one out. Separate from agent:act, which only ever reaches the subject's own agent identity.
	ScopeAgentManage = "agent:manage"

	// ScopeAgentRead — Read this subject's own agent presence and the wrap-up vocabulary.
	ScopeAgentRead = "agent:read"

	// ScopeAuditRead — Read the audit trail.
	ScopeAuditRead = "audit:read"

	// ScopeCallsControl — Drive a call: answer, hold, retrieve, mute, transfer, DTMF, business data, hang up, and the callbacks that promise a call.
	ScopeCallsControl = "calls:control"

	// ScopeCallsCreate — Place a call.
	ScopeCallsCreate = "calls:create"

	// ScopeCallsCreateAI — Start the bot on a number: originate the customer leg and hand whoever answers to the flow published behind the DID. Required in addition to calls:create for kind=AI_OUTBOUND, because one operation carries one scope and this route serves two kinds of call with two different answers — click-to-dial is an agent's own work, starting a bot on a number is an operations decision.
	ScopeCallsCreateAI = "calls:create:ai"

	// ScopeCallsMonitor — Listen in on, whisper to or barge into a call in progress. Separate from calls:control because it reaches a conversation the subject is not a party to.
	ScopeCallsMonitor = "calls:monitor"

	// ScopeCallsReadAll — Read every live call on the floor, and receive every call's events on the stream. Widens calls:read:own rather than replacing it.
	ScopeCallsReadAll = "calls:read:all"

	// ScopeCallsReadOwn — Read the live calls this subject is a party to. For a key, that is the calls of the agent named in X-AICC-Agent-ID.
	ScopeCallsReadOwn = "calls:read:own"

	// ScopeConfigRead — Read the platform's configuration: extensions, queues, numbers, flows, webhook subscriptions, accounts and system health.
	ScopeConfigRead = "config:read"

	// ScopeConfigWrite — Change the platform's configuration, and reveal an extension's SIP password.
	ScopeConfigWrite = "config:write"

	// ScopeContactsRead — Read the customer record book.
	ScopeContactsRead = "contacts:read"

	// ScopeContactsWrite — Add, edit and remove customer records.
	ScopeContactsWrite = "contacts:write"

	// ScopeHistoryReadAll — Read what happened on every call, whoever took it. Widens history:read:own rather than replacing it.
	ScopeHistoryReadAll = "history:read:all"

	// ScopeHistoryReadOwn — Read what happened on this subject's own calls: CDRs, recordings, transcripts and their own day's numbers.
	ScopeHistoryReadOwn = "history:read:own"

	// ScopeKeysManage — Create, disable and revoke API keys.
	ScopeKeysManage = "keys:manage"

	// ScopeQualityReview — Read and write quality reviews of recorded calls.
	ScopeQualityReview = "quality:review"

	// ScopeReportsRead — Read the floor's aggregates: overview, per-queue and daily.
	ScopeReportsRead = "reports:read"

	// ScopeUsersWrite — Create and edit accounts, set roles and reset passwords. Separate from config:write because it is the one path to privilege.
	ScopeUsersWrite = "users:write"
)

// AllScopes is the complete vocabulary, sorted by name.
var AllScopes = []string{
	ScopeAgentAct,
	ScopeAgentManage,
	ScopeAgentRead,
	ScopeAuditRead,
	ScopeCallsControl,
	ScopeCallsCreate,
	ScopeCallsCreateAI,
	ScopeCallsMonitor,
	ScopeCallsReadAll,
	ScopeCallsReadOwn,
	ScopeConfigRead,
	ScopeConfigWrite,
	ScopeContactsRead,
	ScopeContactsWrite,
	ScopeHistoryReadAll,
	ScopeHistoryReadOwn,
	ScopeKeysManage,
	ScopeQualityReview,
	ScopeReportsRead,
	ScopeUsersWrite,
}

// ScopeDescriptions is what each scope means, as the contract states it.
var ScopeDescriptions = map[string]string{
	ScopeAgentAct:       "Drive this subject's own agent presence: sign in and out, ready, not ready, wrap up.",
	ScopeAgentManage:    "Supervise other agents: read the roster and force one out. Separate from agent:act, which only ever reaches the subject's own agent identity.",
	ScopeAgentRead:      "Read this subject's own agent presence and the wrap-up vocabulary.",
	ScopeAuditRead:      "Read the audit trail.",
	ScopeCallsControl:   "Drive a call: answer, hold, retrieve, mute, transfer, DTMF, business data, hang up, and the callbacks that promise a call.",
	ScopeCallsCreate:    "Place a call.",
	ScopeCallsCreateAI:  "Start the bot on a number: originate the customer leg and hand whoever answers to the flow published behind the DID. Required in addition to calls:create for kind=AI_OUTBOUND, because one operation carries one scope and this route serves two kinds of call with two different answers — click-to-dial is an agent's own work, starting a bot on a number is an operations decision.",
	ScopeCallsMonitor:   "Listen in on, whisper to or barge into a call in progress. Separate from calls:control because it reaches a conversation the subject is not a party to.",
	ScopeCallsReadAll:   "Read every live call on the floor, and receive every call's events on the stream. Widens calls:read:own rather than replacing it.",
	ScopeCallsReadOwn:   "Read the live calls this subject is a party to. For a key, that is the calls of the agent named in X-AICC-Agent-ID.",
	ScopeConfigRead:     "Read the platform's configuration: extensions, queues, numbers, flows, webhook subscriptions, accounts and system health.",
	ScopeConfigWrite:    "Change the platform's configuration, and reveal an extension's SIP password.",
	ScopeContactsRead:   "Read the customer record book.",
	ScopeContactsWrite:  "Add, edit and remove customer records.",
	ScopeHistoryReadAll: "Read what happened on every call, whoever took it. Widens history:read:own rather than replacing it.",
	ScopeHistoryReadOwn: "Read what happened on this subject's own calls: CDRs, recordings, transcripts and their own day's numbers.",
	ScopeKeysManage:     "Create, disable and revoke API keys.",
	ScopeQualityReview:  "Read and write quality reviews of recorded calls.",
	ScopeReportsRead:    "Read the floor's aggregates: overview, per-queue and daily.",
	ScopeUsersWrite:     "Create and edit accounts, set roles and reset passwords. Separate from config:write because it is the one path to privilege.",
}

// IsScope reports whether name is in the vocabulary. An unknown name is
// refused rather than ignored: a key silently missing a capability fails
// later, somewhere else, for a reason nobody can see.
func IsScope(name string) bool {
	_, ok := ScopeDescriptions[name]
	return ok
}
