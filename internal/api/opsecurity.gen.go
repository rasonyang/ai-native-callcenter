// SPDX-License-Identifier: Apache-2.0

// Code generated from docs/openapi.json (per-operation security) by scripts/gen-opsecurity.mjs. DO NOT EDIT.

package api

// OperationSecurity is one operation's `security` block, as the contract
// states it: which credentials reach the operation, and what each must carry.
type OperationSecurity struct {
	// OperationID names the operation in the contract.
	OperationID string

	// IsAnonymous is a contract `security: []` — the operation takes no
	// credential at all.
	IsAnonymous bool

	// SessionScopes and KeyScopes are what a browser session, respectively an
	// API key, must hold to reach this operation.
	//
	// nil and empty are different answers. nil means that credential has no
	// alternative here and cannot reach the operation whatever it holds — the
	// contract's two deliberate asymmetries, POST /auth/logout (a key has no
	// session to end) and POST /recordings/{recordingId}/reviews (a human
	// judgement is attributed to a person). Empty means it reaches the
	// operation while holding nothing in particular: authenticated is the
	// whole requirement.
	SessionScopes []string
	KeyScopes     []string

	// NeedsCSRF is the csrfHeader scheme on the session alternative. It rides
	// with the cookie because a cookie travels by itself; a key is presented
	// deliberately on every request and is never asked for it.
	NeedsCSRF bool
}

// OperationSecurityByRoute is every operation the contract declares, keyed
// "METHOD /path" with the path exactly as the contract spells it — which is
// chi's route pattern with the server's /api/v1 prefix removed.
var OperationSecurityByRoute = map[string]OperationSecurity{
	"DELETE /agent/sip-session": {
		OperationID:   "deleteAgentSipSession",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"DELETE /agents/{agentId}": {
		OperationID:   "deleteAgent",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"DELETE /contacts/{contactId}": {
		OperationID:   "deleteContact",
		SessionScopes: []string{"contacts:write"},
		KeyScopes:     []string{"contacts:write"},
		NeedsCSRF:     true,
	},
	"DELETE /dids/{didId}": {
		OperationID:   "deleteDID",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"DELETE /extensions/{extensionId}": {
		OperationID:   "deleteExtension",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"DELETE /queues/{queueId}": {
		OperationID:   "deleteQueue",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"DELETE /queues/{queueId}/agents/{agentId}": {
		OperationID:   "unstaffQueue",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"DELETE /webhook-subscriptions/{subscriptionId}": {
		OperationID:   "deleteWebhookSubscription",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"GET /agent/presence": {
		OperationID:   "getAgentPresence",
		SessionScopes: []string{"agent:read"},
		KeyScopes:     []string{"agent:read"},
	},
	"GET /agent/wrap-up": {
		OperationID:   "getAgentWrapUp",
		SessionScopes: []string{"agent:read"},
		KeyScopes:     []string{"agent:read"},
	},
	"GET /agents": {
		OperationID:   "listAgents",
		SessionScopes: []string{"agent:manage"},
		KeyScopes:     []string{"agent:manage"},
	},
	"GET /api-keys": {
		OperationID:   "listAPIKeys",
		SessionScopes: []string{"keys:manage"},
		KeyScopes:     []string{"keys:manage"},
	},
	"GET /api-keys/{keyId}": {
		OperationID:   "getAPIKey",
		SessionScopes: []string{"keys:manage"},
		KeyScopes:     []string{"keys:manage"},
	},
	"GET /audit-logs": {
		OperationID:   "listAuditLogs",
		SessionScopes: []string{"audit:read"},
		KeyScopes:     []string{"audit:read"},
	},
	"GET /auth/me": {
		OperationID:   "getMe",
		SessionScopes: []string{},
		KeyScopes:     []string{},
	},
	"GET /callbacks": {
		OperationID:   "listCallbacks",
		SessionScopes: []string{"calls:read:own"},
		KeyScopes:     []string{"calls:read:own"},
	},
	"GET /calls": {
		OperationID:   "listCalls",
		SessionScopes: []string{"calls:read:all"},
		KeyScopes:     []string{"calls:read:all"},
	},
	"GET /calls/mine": {
		OperationID:   "listMyCalls",
		SessionScopes: []string{"calls:read:own"},
		KeyScopes:     []string{"calls:read:own"},
	},
	"GET /calls/waiting": {
		OperationID:   "listWaitingCalls",
		SessionScopes: []string{"calls:read:own"},
		KeyScopes:     []string{"calls:read:own"},
	},
	"GET /calls/{callId}/queue-events": {
		OperationID:   "listCallQueueEvents",
		SessionScopes: []string{"history:read:all"},
		KeyScopes:     []string{"history:read:all"},
	},
	"GET /calls/{callId}/recordings": {
		OperationID:   "listCallRecordings",
		SessionScopes: []string{"history:read:own"},
		KeyScopes:     []string{"history:read:own"},
	},
	"GET /calls/{callId}/reviews": {
		OperationID:   "listCallReviews",
		SessionScopes: []string{"quality:review"},
		KeyScopes:     []string{"quality:review"},
	},
	"GET /calls/{callId}/transcript": {
		OperationID:   "getCallTranscript",
		SessionScopes: []string{"history:read:own"},
		KeyScopes:     []string{"history:read:own"},
	},
	"GET /cdrs": {
		OperationID:   "listCDRs",
		SessionScopes: []string{"history:read:all"},
		KeyScopes:     []string{"history:read:all"},
	},
	"GET /cdrs/mine": {
		OperationID:   "listMyCDRs",
		SessionScopes: []string{"history:read:own"},
		KeyScopes:     []string{"history:read:own"},
	},
	"GET /cdrs/{callId}": {
		OperationID:   "getCDR",
		SessionScopes: []string{"history:read:all"},
		KeyScopes:     []string{"history:read:all"},
	},
	"GET /contacts": {
		OperationID:   "listContacts",
		SessionScopes: []string{"contacts:read"},
		KeyScopes:     []string{"contacts:read"},
	},
	"GET /dids": {
		OperationID:   "listDIDs",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /dispositions": {
		OperationID:   "listDispositions",
		SessionScopes: []string{"agent:read"},
		KeyScopes:     []string{"agent:read"},
	},
	"GET /events": {
		OperationID:   "streamEvents",
		SessionScopes: []string{"calls:read:own"},
		KeyScopes:     []string{"calls:read:own"},
	},
	"GET /extensions": {
		OperationID:   "listExtensions",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /extensions/{extensionId}/password": {
		OperationID:   "revealExtensionPassword",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
	},
	"GET /flows": {
		OperationID:   "listFlows",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /flows/{flowId}": {
		OperationID:   "getFlow",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /openapi.json": {
		OperationID:   "getOpenAPI",
		IsAnonymous:   true,
		SessionScopes: nil,
		KeyScopes:     nil,
	},
	"GET /queues": {
		OperationID:   "listQueues",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /queues/{queueId}/agents": {
		OperationID:   "listQueueAgents",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /recordings/{recordingId}/audio": {
		OperationID:   "getRecordingAudio",
		SessionScopes: []string{"history:read:own"},
		KeyScopes:     []string{"history:read:own"},
	},
	"GET /reports/daily": {
		OperationID:   "getReportDaily",
		SessionScopes: []string{"reports:read"},
		KeyScopes:     []string{"reports:read"},
	},
	"GET /reports/me": {
		OperationID:   "getMyDay",
		SessionScopes: []string{"history:read:own"},
		KeyScopes:     []string{"history:read:own"},
	},
	"GET /reports/overview": {
		OperationID:   "getReportOverview",
		SessionScopes: []string{"reports:read"},
		KeyScopes:     []string{"reports:read"},
	},
	"GET /reports/queues": {
		OperationID:   "getReportQueues",
		SessionScopes: []string{"reports:read"},
		KeyScopes:     []string{"reports:read"},
	},
	"GET /system/health": {
		OperationID:   "getSystemHealth",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /users": {
		OperationID:   "listUsers",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /webhook-subscriptions": {
		OperationID:   "listWebhookSubscriptions",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /webhook-subscriptions/{subscriptionId}": {
		OperationID:   "getWebhookSubscription",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"GET /webhook-subscriptions/{subscriptionId}/deliveries": {
		OperationID:   "listWebhookDeliveries",
		SessionScopes: []string{"config:read"},
		KeyScopes:     []string{"config:read"},
	},
	"PATCH /api-keys/{keyId}": {
		OperationID:   "updateAPIKey",
		SessionScopes: []string{"keys:manage"},
		KeyScopes:     []string{"keys:manage"},
		NeedsCSRF:     true,
	},
	"PATCH /calls/{callId}/user-data": {
		OperationID:   "patchUserData",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /agent/login": {
		OperationID:   "agentLogin",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agent/logout": {
		OperationID:   "agentLogout",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agent/not-ready": {
		OperationID:   "agentNotReady",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agent/ready": {
		OperationID:   "agentReady",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agent/sip-session": {
		OperationID:   "createAgentSipSession",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agent/wrap-up": {
		OperationID:   "agentWrapUp",
		SessionScopes: []string{"agent:act"},
		KeyScopes:     []string{"agent:act"},
		NeedsCSRF:     true,
	},
	"POST /agents": {
		OperationID:   "createAgent",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /agents/{agentId}/force-logout": {
		OperationID:   "forceLogoutAgent",
		SessionScopes: []string{"agent:manage"},
		KeyScopes:     []string{"agent:manage"},
		NeedsCSRF:     true,
	},
	"POST /api-keys": {
		OperationID:   "createAPIKey",
		SessionScopes: []string{"keys:manage"},
		KeyScopes:     []string{"keys:manage"},
		NeedsCSRF:     true,
	},
	"POST /api-keys/{keyId}/revoke": {
		OperationID:   "revokeAPIKey",
		SessionScopes: []string{"keys:manage"},
		KeyScopes:     []string{"keys:manage"},
		NeedsCSRF:     true,
	},
	"POST /auth/login": {
		OperationID:   "login",
		IsAnonymous:   true,
		SessionScopes: nil,
		KeyScopes:     nil,
	},
	"POST /auth/logout": {
		OperationID:   "logout",
		SessionScopes: []string{},
		KeyScopes:     nil,
		NeedsCSRF:     true,
	},
	"POST /callbacks/{callbackId}/claim": {
		OperationID:   "claimCallback",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /callbacks/{callbackId}/complete": {
		OperationID:   "completeCallback",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /callbacks/{callbackId}/release": {
		OperationID:   "releaseCallback",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls": {
		OperationID:   "createCall",
		SessionScopes: []string{"calls:create"},
		KeyScopes:     []string{"calls:create"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/answer": {
		OperationID:   "answerCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/dtmf": {
		OperationID:   "sendCallDTMF",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/hangup": {
		OperationID:   "hangupCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/hold": {
		OperationID:   "holdCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/monitor": {
		OperationID:   "monitorCall",
		SessionScopes: []string{"calls:monitor"},
		KeyScopes:     []string{"calls:monitor"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/mute": {
		OperationID:   "muteCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/retrieve": {
		OperationID:   "retrieveCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/transfer": {
		OperationID:   "transferCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /calls/{callId}/unmute": {
		OperationID:   "unmuteCall",
		SessionScopes: []string{"calls:control"},
		KeyScopes:     []string{"calls:control"},
		NeedsCSRF:     true,
	},
	"POST /contacts": {
		OperationID:   "createContact",
		SessionScopes: []string{"contacts:write"},
		KeyScopes:     []string{"contacts:write"},
		NeedsCSRF:     true,
	},
	"POST /dids": {
		OperationID:   "createDID",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /extensions": {
		OperationID:   "createExtension",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /flows": {
		OperationID:   "createFlow",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /flows/{flowId}/publish": {
		OperationID:   "publishFlow",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /queues": {
		OperationID:   "createQueue",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"POST /recordings/{recordingId}/reviews": {
		OperationID:   "createRecordingReview",
		SessionScopes: []string{"quality:review"},
		KeyScopes:     nil,
		NeedsCSRF:     true,
	},
	"POST /users": {
		OperationID:   "createUser",
		SessionScopes: []string{"users:write"},
		KeyScopes:     []string{"users:write"},
		NeedsCSRF:     true,
	},
	"POST /users/{userId}/password": {
		OperationID:   "resetUserPassword",
		SessionScopes: []string{"users:write"},
		KeyScopes:     []string{"users:write"},
		NeedsCSRF:     true,
	},
	"POST /webhook-subscriptions": {
		OperationID:   "createWebhookSubscription",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /agents/{agentId}": {
		OperationID:   "updateAgent",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /contacts/{contactId}": {
		OperationID:   "updateContact",
		SessionScopes: []string{"contacts:write"},
		KeyScopes:     []string{"contacts:write"},
		NeedsCSRF:     true,
	},
	"PUT /dids/{didId}": {
		OperationID:   "updateDID",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /extensions/{extensionId}": {
		OperationID:   "updateExtension",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /flows/{flowId}": {
		OperationID:   "updateFlowDraft",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /queues/{queueId}": {
		OperationID:   "updateQueue",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /queues/{queueId}/agents": {
		OperationID:   "staffQueue",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
	"PUT /users/{userId}": {
		OperationID:   "updateUser",
		SessionScopes: []string{"users:write"},
		KeyScopes:     []string{"users:write"},
		NeedsCSRF:     true,
	},
	"PUT /webhook-subscriptions/{subscriptionId}": {
		OperationID:   "updateWebhookSubscription",
		SessionScopes: []string{"config:write"},
		KeyScopes:     []string{"config:write"},
		NeedsCSRF:     true,
	},
}

// SecurityForRoute answers what the operation mounted at this method and path
// requires. The path is the contract's, so a caller holding chi's route
// pattern strips the server's own prefix first.
//
// ok is false when the route is not in the contract at all — which is a
// routing table that has drifted, not a request to let through.
func SecurityForRoute(method, path string) (OperationSecurity, bool) {
	sec, ok := OperationSecurityByRoute[method+" "+path]
	return sec, ok
}
