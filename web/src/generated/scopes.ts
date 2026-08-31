/**
 * This file was auto-generated from docs/openapi.json (root x-scopes)
 * by scripts/gen-scopes.mjs.
 * Do not make direct changes to the file.
 */

/** A capability an API key or a session may hold. */
export type Scope =
    | 'agent:act'
    | 'agent:manage'
    | 'agent:read'
    | 'audit:read'
    | 'calls:control'
    | 'calls:create'
    | 'calls:create:ai'
    | 'calls:monitor'
    | 'calls:read:all'
    | 'calls:read:own'
    | 'config:read'
    | 'config:write'
    | 'contacts:read'
    | 'contacts:write'
    | 'history:read:all'
    | 'history:read:own'
    | 'keys:manage'
    | 'quality:review'
    | 'reports:read'
    | 'users:write'

/** The complete vocabulary, sorted by name. */
export const SCOPES: readonly Scope[] = [
    'agent:act',
    'agent:manage',
    'agent:read',
    'audit:read',
    'calls:control',
    'calls:create',
    'calls:create:ai',
    'calls:monitor',
    'calls:read:all',
    'calls:read:own',
    'config:read',
    'config:write',
    'contacts:read',
    'contacts:write',
    'history:read:all',
    'history:read:own',
    'keys:manage',
    'quality:review',
    'reports:read',
    'users:write',
] as const

/** What each scope means, as the contract states it. */
export const SCOPE_DESCRIPTIONS: Record<Scope, string> = {
    'agent:act': "Drive this subject's own agent presence: sign in and out, ready, not ready, wrap up.",
    'agent:manage': "Supervise other agents: read the roster and force one out. Separate from agent:act, which only ever reaches the subject's own agent identity.",
    'agent:read': "Read this subject's own agent presence and the wrap-up vocabulary.",
    'audit:read': "Read the audit trail.",
    'calls:control': "Drive a call: answer, hold, retrieve, mute, transfer, DTMF, business data, hang up, and the callbacks that promise a call.",
    'calls:create': "Place a call.",
    'calls:create:ai': "Start the bot on a number: originate the customer leg and hand whoever answers to the flow published behind the DID. Required in addition to calls:create for kind=AI_OUTBOUND, because one operation carries one scope and this route serves two kinds of call with two different answers — click-to-dial is an agent's own work, starting a bot on a number is an operations decision.",
    'calls:monitor': "Listen in on, whisper to or barge into a call in progress. Separate from calls:control because it reaches a conversation the subject is not a party to.",
    'calls:read:all': "Read every live call on the floor, and receive every call's events on the stream. Widens calls:read:own rather than replacing it.",
    'calls:read:own': "Read the live calls this subject is a party to. For a key, that is the calls of the agent named in X-AICC-Agent-ID.",
    'config:read': "Read the platform's configuration: extensions, queues, numbers, flows, webhook subscriptions, accounts and system health.",
    'config:write': "Change the platform's configuration, and reveal an extension's SIP password.",
    'contacts:read': "Read the customer record book.",
    'contacts:write': "Add, edit and remove customer records.",
    'history:read:all': "Read what happened on every call, whoever took it. Widens history:read:own rather than replacing it.",
    'history:read:own': "Read what happened on this subject's own calls: CDRs, recordings, transcripts and their own day's numbers.",
    'keys:manage': "Create, disable and revoke API keys.",
    'quality:review': "Read and write quality reviews of recorded calls.",
    'reports:read': "Read the floor's aggregates: overview, per-queue and daily.",
    'users:write': "Create and edit accounts, set roles and reset passwords. Separate from config:write because it is the one path to privilege.",
}
