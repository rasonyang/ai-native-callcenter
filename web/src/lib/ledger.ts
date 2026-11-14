import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components, operations } from '@/generated/api'

import { ApiError, request } from './api'

/**
 * The finished-call ledger: CDRs, transcripts, recordings, callbacks and the
 * report aggregates. All wire types come from the generated contract; this
 * module re-exports them under their established names.
 */

export type Leg = components['schemas']['Leg']

export type CDRStatus = components['schemas']['CDRStatus']

export type MissedReason = components['schemas']['MissedReason']

export type CDR = components['schemas']['CDR']

export type TranscriptLine = components['schemas']['TranscriptLine']

export type RecordingRow = components['schemas']['Recording']

export type CallbackStatus = components['schemas']['CallbackStatus']

export type Callback = components['schemas']['Callback']

export type CallbackList = components['schemas']['CallbackList']

/** The after-call work filed against a call. */
export type WrapUp = components['schemas']['WrapUp']

export type Disposition = components['schemas']['Disposition']

/** One agent's own day, for the cockpit's Today card. */
export type AgentToday = components['schemas']['AgentToday']

export type Overview = components['schemas']['Overview']

export type QueueReport = components['schemas']['QueueReport']

export type DailyReport = components['schemas']['DailyReport']

export type AuditEntry = components['schemas']['AuditEntry']

/** Who changed what. A zero field is no constraint. */
export interface AuditFilter {
  actorId?: string
  /**
   * A literal prefix over "METHOD /route/template", which is what the stored
   * action is: "DELETE " selects every deletion, "PUT /api/v1/queues" every
   * queue edit. No category is invented on top of the value.
   */
  actionPrefix?: string
  from?: string
  to?: string
  limit?: number
  offset?: number
}

/** The query of GET /cdrs, as the contract declares it (status comes from a free-form select). */
export type CDRFilter = Omit<NonNullable<operations['listCDRs']['parameters']['query']>, 'status'> & {
  status?: string
}

/** One page of a list: how many rows, and how many to skip. */
export interface PageParams {
  limit?: number
  offset?: number
}

function query(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

export const ledgerApi = {
  cdrs: (filter: CDRFilter) =>
    request<{ items: CDR[]; total: number }>(`/cdrs${query({ ...filter })}`),

  /** The caller's own finished calls. The agent is the session, not a filter. */
  myCDRs: (filter: CDRFilter) =>
    request<{ items: CDR[]; total: number }>(`/cdrs/mine${query({ ...filter })}`),

  /** The audit trail. ADMIN only; the server draws that line. */
  auditLogs: (filter: AuditFilter) =>
    request<{ items: AuditEntry[]; total: number }>(`/audit-logs${query({ ...filter })}`),

  dispositions: () => request<{ items: Disposition[] }>('/dispositions'),

  /** The caller's own numbers for today; the agent is the session. */
  myDay: () => request<AgentToday>('/reports/me'),

  cdr: (callId: string) =>
    request<{ cdr: CDR; transcript: TranscriptLine[]; recordings: RecordingRow[] }>(
      `/cdrs/${callId}`,
    ),

  recordingsByCall: (callId: string) =>
    request<{ items: RecordingRow[] }>(`/calls/${callId}/recordings`),

  callbacks: (status?: string, page: PageParams = {}) =>
    request<CallbackList>(`/callbacks${query({ status, ...page })}`),

  callbacksOfCall: (callId: string) =>
    request<{ items: Callback[] }>(`/callbacks${query({ callId })}`),

  claimCallback: (id: string) =>
    request<Callback>(`/callbacks/${id}/claim`, { method: 'POST' }),

  releaseCallback: (id: string) =>
    request<Callback>(`/callbacks/${id}/release`, { method: 'POST' }),

  completeCallback: (id: string, status: 'DONE' | 'DISMISSED') =>
    request<Callback>(`/callbacks/${id}/complete`, {
      method: 'POST',
      body: JSON.stringify({ status }),
    }),

  overview: (from?: string, to?: string) =>
    request<Overview>(`/reports/overview${query({ from, to })}`),

  queues: (from?: string, to?: string) =>
    request<{ items: QueueReport[] }>(`/reports/queues${query({ from, to })}`),

  daily: (from?: string, to?: string) =>
    request<{ items: DailyReport[] }>(`/reports/daily${query({ from, to })}`),
}

/** The playback endpoint; used as an <audio> source, not fetched as JSON. */
export function recordingAudioUrl(recordingId: string): string {
  return `/api/v1/recordings/${recordingId}/audio`
}

export const CDRS_KEY = ['cdrs']
export const CALLBACKS_KEY = ['callbacks']
export const REPORTS_KEY = ['reports']

export function useCDRs(filter: CDRFilter) {
  return useQuery({
    queryKey: [...CDRS_KEY, filter],
    queryFn: () => ledgerApi.cdrs(filter),
    placeholderData: (previous) => previous,
  })
}

const AUDIT_KEY = ['audit-logs']

/** Who changed what, newest first. */
export function useAuditLogs(filter: AuditFilter) {
  return useQuery({
    queryKey: [...AUDIT_KEY, filter],
    queryFn: () => ledgerApi.auditLogs(filter),
    placeholderData: (previous) => previous,
  })
}

/** The signed-in agent's own calls. */
export function useMyCDRs(filter: CDRFilter) {
  return useQuery({
    queryKey: [...CDRS_KEY, 'mine', filter],
    queryFn: () => ledgerApi.myCDRs(filter),
    placeholderData: (previous) => previous,
  })
}

/**
 * The wrap-up vocabulary. It is configuration, not call state, so it is held
 * for the session rather than refetched behind every call.
 */
export function useDispositions(enabled = true) {
  return useQuery({
    queryKey: ['dispositions'],
    enabled,
    staleTime: 5 * 60_000,
    queryFn: ledgerApi.dispositions,
  })
}

/**
 * The agent's own day, refreshed on the events that move it: a finished call
 * changes what they handled, a presence change changes what the time went on.
 */
export function useMyDay(enabled: boolean) {
  return useQuery({
    queryKey: [...REPORTS_KEY, 'me'],
    enabled,
    staleTime: 10_000,
    queryFn: ledgerApi.myDay,
  })
}

export function useCDR(callId: string) {
  return useQuery({
    queryKey: [...CDRS_KEY, callId],
    queryFn: () => ledgerApi.cdr(callId),
    // A call with no record answers 404 at once and will go on doing so; the
    // default retry would hold the page on "Loading" for seconds first.
    retry: (count, error) => !(error instanceof ApiError && error.status === 404) && count < 1,
  })
}

/**
 * One page of the callback queue. Every key sits under CALLBACKS_KEY, so the
 * event stream's invalidation reaches each page and filter on screen. Without
 * a page the server's default applies.
 */
export function useCallbacks(status?: string, page: PageParams = {}) {
  return useQuery({
    queryKey: [...CALLBACKS_KEY, status ?? 'all', { limit: page.limit, offset: page.offset }],
    queryFn: () => ledgerApi.callbacks(status, page),
  })
}

/**
 * The callback a call filed, if any: a campaign call files at most one (its
 * HIGH lead's), so the first row is the one. Resolved by call id, never by
 * phone number, which one person's several calls share.
 */
export function useCallbackOfCall({ callId }: { callId: string | undefined }) {
  return useQuery({
    queryKey: [...CALLBACKS_KEY, 'call', callId],
    enabled: callId !== undefined,
    queryFn: () => ledgerApi.callbacksOfCall(callId as string),
    select: (page): Callback | null => page.items[0] ?? null,
  })
}

export function useCallbackMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: CALLBACKS_KEY })
  return {
    claim: useMutation({ mutationFn: ledgerApi.claimCallback, onSuccess: invalidate, onError: invalidate }),
    release: useMutation({ mutationFn: ledgerApi.releaseCallback, onSuccess: invalidate, onError: invalidate }),
    complete: useMutation({
      mutationFn: ({ id, status }: { id: string; status: 'DONE' | 'DISMISSED' }) =>
        ledgerApi.completeCallback(id, status),
      onSuccess: invalidate,
      onError: invalidate,
    }),
  }
}

export function useOverview(from?: string, to?: string) {
  return useQuery({
    queryKey: [...REPORTS_KEY, 'overview', from, to],
    queryFn: () => ledgerApi.overview(from, to),
  })
}

export function useQueueReport(from?: string, to?: string) {
  return useQuery({
    queryKey: [...REPORTS_KEY, 'queues', from, to],
    queryFn: () => ledgerApi.queues(from, to),
  })
}

export function useDailyReport(from?: string, to?: string) {
  return useQuery({
    queryKey: [...REPORTS_KEY, 'daily', from, to],
    queryFn: () => ledgerApi.daily(from, to),
  })
}

/** mm:ss for tables; hours only appear when a call actually lasted that long. */
export function formatDuration(totalSec: number): string {
  const sec = Math.max(0, Math.round(totalSec))
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = sec % 60
  const mm = String(m).padStart(2, '0')
  const ss = String(s).padStart(2, '0')
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`
}
