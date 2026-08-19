import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/**
 * The finished-call ledger: CDRs, transcripts, recordings, callbacks and the
 * report aggregates. All wire types come from the generated contract; this
 * module re-exports them under their established names.
 */

export type Leg = components['schemas']['Leg']

export type CDRStatus = components['schemas']['CDRStatus']

export type CDR = components['schemas']['CDR']

export type TranscriptLine = components['schemas']['TranscriptLine']

export type RecordingRow = components['schemas']['Recording']

export type CallbackStatus = components['schemas']['CallbackStatus']

export type Callback = components['schemas']['Callback']

/** The after-call work filed against a call. */
export type WrapUp = components['schemas']['WrapUp']

export type Disposition = components['schemas']['Disposition']

export type DispositionCategory = components['schemas']['DispositionCategory']

export type Overview = components['schemas']['Overview']

export type QueueReport = components['schemas']['QueueReport']

export type DailyReport = components['schemas']['DailyReport']

export interface CDRFilter {
  status?: string
  did?: string
  fromNumber?: string
  from?: string
  to?: string
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

  dispositions: () =>
    request<{ categories: DispositionCategory[] }>('/dispositions'),

  cdr: (callId: string) =>
    request<{ cdr: CDR; transcript: TranscriptLine[]; recordings: RecordingRow[] }>(
      `/cdrs/${callId}`,
    ),

  recordingsByCall: (callId: string) =>
    request<{ items: RecordingRow[] }>(`/calls/${callId}/recordings`),

  callbacks: (status?: string) =>
    request<{ items: Callback[] }>(`/callbacks${query({ status })}`),

  claimCallback: (id: string) =>
    request<Callback>(`/callbacks/${id}/claim`, { method: 'POST' }),

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

export function useCDR(callId: string) {
  return useQuery({ queryKey: [...CDRS_KEY, callId], queryFn: () => ledgerApi.cdr(callId) })
}

export function useCallbacks(status?: string) {
  return useQuery({
    queryKey: [...CALLBACKS_KEY, status ?? 'all'],
    queryFn: () => ledgerApi.callbacks(status),
  })
}

export function useCallbackMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: CALLBACKS_KEY })
  return {
    claim: useMutation({ mutationFn: ledgerApi.claimCallback, onSuccess: invalidate, onError: invalidate }),
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
