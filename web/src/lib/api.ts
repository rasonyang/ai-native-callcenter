/**
 * REST client. All wire types come from the generated contract
 * (web/src/generated/api.ts, from docs/openapi.json); this module only
 * re-exports them under their established names.
 */
import type { components } from '@/generated/api'

/** Machine-readable error codes; the UI renders errors.<CODE>. */
export type ErrorCode = components['schemas']['ErrorCode']

export type Role = components['schemas']['Role']

export type Identity = components['schemas']['Identity']

export type ApiErrorBody = components['schemas']['Error']

/** Error carrying the translatable code and its interpolation params. */
export class ApiError extends Error {
  readonly code: ErrorCode
  readonly params: Record<string, unknown>
  readonly status: number

  constructor(status: number, body: ApiErrorBody) {
    super(body.message)
    this.name = 'ApiError'
    this.status = status
    this.code = body.code
    this.params = body.params ?? {}
  }
}

const BASE = '/api/v1'

/** Mutations must carry this header; see docs/design/04-api-sse.md §2. */
const CSRF_HEADER = 'X-AICC-Csrf'

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = init.method ?? 'GET'
  const headers = new Headers(init.headers)
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
  if (method !== 'GET' && method !== 'HEAD') headers.set(CSRF_HEADER, '1')

  const response = await fetch(`${BASE}${path}`, {
    ...init,
    method,
    headers,
    credentials: 'same-origin',
  })

  if (response.status === 204) return undefined as T

  const text = await response.text()
  const payload: unknown = text ? JSON.parse(text) : null

  if (!response.ok) {
    const body = (payload as { error?: ApiErrorBody } | null)?.error
    throw new ApiError(
      response.status,
      body ?? { code: 'INTERNAL', message: response.statusText },
    )
  }
  return payload as T
}

export const api = {
  login: (username: string, password: string) =>
    request<{ user: Identity }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  logout: () => request<void>('/auth/logout', { method: 'POST' }),

  me: () => request<{ user: Identity }>('/auth/me'),

  /** Stream counters and the trunks the switch holds, read never written. */
  health: () => request<components['schemas']['SystemHealth']>('/system/health'),
}

// --- Agents ---------------------------------------------------------------

export type AgentState = components['schemas']['AgentState']

export type NotReadyReason = components['schemas']['NotReadyReason']

/** The single word that answers "could this agent take a call, and if not, why". */
export type Availability = components['schemas']['Availability']

export type Presence = components['schemas']['Presence']

/**
 * The credentials one agent's phone registers with. The platform mints them
 * and the browser hands them straight to the extension — nobody types a SIP
 * password into a handset, and nothing here keeps a copy.
 */
export type SipSession = components['schemas']['SipSession']

export type RosterEntry = components['schemas']['RosterEntry']
export type MonitorMode = components['schemas']['MonitorMode']
export type MonitorRequest = components['schemas']['MonitorRequest']

/** What an agent confirms for the call they just finished. */
export type WrapUpRequest = components['schemas']['WrapUpRequest']

/**
 * The after-call record waiting on this agent. The platform opened it when the
 * call ended, so it exists before the agent has touched anything — and it
 * survives a page reload, which is how a reopened cockpit knows there is still
 * work to confirm.
 */
export type CurrentWrapUp = components['schemas']['CurrentWrapUp']

export const agentApi = {
  presence: () => request<Presence>('/agent/presence'),

  /**
   * Signs in at the extension bound to this agent in configuration. The
   * binding is static, so a number is only ever passed to override it.
   */
  login: (extensionNumber?: string) =>
    request<Presence>('/agent/login', {
      method: 'POST',
      body: JSON.stringify(extensionNumber ? { extensionNumber } : {}),
    }),

  logout: () => request<Presence>('/agent/logout', { method: 'POST' }),

  ready: () => request<Presence>('/agent/ready', { method: 'POST' }),

  notReady: (reason: NotReadyReason) =>
    request<Presence>('/agent/not-ready', {
      method: 'POST',
      body: JSON.stringify({ reason }),
    }),

  /**
   * The record waiting to be confirmed, or undefined when there is none (the
   * server answers 204).
   */
  currentWrapUp: () => request<CurrentWrapUp | undefined>('/agent/wrap-up'),

  /**
   * Confirms after-call work, applying whatever the agent changed against the
   * call the server says it was for. The call is never named here: the
   * platform knows which one the agent just finished.
   */
  wrapUp: (body: WrapUpRequest) =>
    request<Presence>('/agent/wrap-up', { method: 'POST', body: JSON.stringify(body) }),

  /**
   * Mints the credentials this agent's phone registers with.
   *
   * One session per agent: a second call replaces the first and flushes the
   * registration the previous one held. The expiry follows the web session's,
   * so there is nothing to refresh on a timer — asking again before the person
   * signs out returns the same expiry.
   *
   * There is no revoke here on purpose. `DELETE /agent/sip-session` exists in
   * the contract, but signing out is what ends a phone's session and the
   * server does that itself, after signing the agent out of presence —
   * flushing the registration from the browser first reported a phone lost by
   * an agent who was still READY.
   */
  createSipSession: () => request<SipSession>('/agent/sip-session', { method: 'POST' }),

  roster: () => request<{ items: RosterEntry[] }>('/agents'),

  forceLogout: (agentId: string) =>
    request<Presence>(`/agents/${agentId}/force-logout`, { method: 'POST' }),
}

// --- Calls ----------------------------------------------------------------

export type PartyState = components['schemas']['PartyState']
export type CallType = components['schemas']['CallType']

export type PartySnapshot = components['schemas']['PartySnapshot']

export type CallSnapshot = components['schemas']['CallSnapshot']

/** A caller waiting in a queue this agent staffs. */
export type WaitingCall = components['schemas']['WaitingCall']

/** A queue the reader works, listed whether or not anybody is waiting in it. */
export type StaffedQueue = components['schemas']['StaffedQueue']

export const callApi = {
  mine: () => request<components['schemas']['CallList']>('/calls/mine'),
  waiting: () => request<components['schemas']['WaitingCallList']>('/calls/waiting'),
  /**
   * Click-to-dial. The agent's own phone is raised first, so no extension is
   * named: the server uses the one they signed in at, and an agent may name
   * no other.
   */
  dial: (destination: string) =>
    request<components['schemas']['CreateCallResponse']>('/calls', {
      method: 'POST',
      body: JSON.stringify({ kind: 'AGENT_OUTBOUND', to: destination }),
    }),
  /**
   * The same dial, made to keep a callback: the call is noted on the callback
   * before the phone rings and its outcome is copied back when it ends.
   */
  dialForCallback: (callbackId: string, destination: string) =>
    request<components['schemas']['CreateCallResponse']>('/calls', {
      method: 'POST',
      body: JSON.stringify({ kind: 'AGENT_OUTBOUND', to: destination, callbackId }),
    }),
  answer: (callId: string) => request<void>(`/calls/${callId}/answer`, { method: 'POST' }),
  hold: (callId: string) => request<void>(`/calls/${callId}/hold`, { method: 'POST' }),
  retrieve: (callId: string) => request<void>(`/calls/${callId}/retrieve`, { method: 'POST' }),
  hangup: (callId: string) => request<void>(`/calls/${callId}/hangup`, { method: 'POST' }),
  transfer: (callId: string, destination: string) =>
    request<void>(`/calls/${callId}/transfer`, {
      method: 'POST',
      body: JSON.stringify({ destination }),
    }),
  mute: (callId: string) => request<void>(`/calls/${callId}/mute`, { method: 'POST' }),
  unmute: (callId: string) => request<void>(`/calls/${callId}/unmute`, { method: 'POST' }),
  sendDtmf: (callId: string, digits: string) =>
    request<void>(`/calls/${callId}/dtmf`, {
      method: 'POST',
      body: JSON.stringify({ digits }),
    }),
  /**
   * Supervision: raise the supervisor's phone against an agent's leg. The
   * agent is named because an internal call carries two; the phone is named
   * unless the supervisor is signed in as an agent.
   */
  monitor: (callId: string, body: MonitorRequest) =>
    request<void>(`/calls/${callId}/monitor`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),
}
