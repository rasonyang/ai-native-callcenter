import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { request } from './api'

/**
 * The finished-call ledger: CDRs, transcripts, recordings, callbacks and the
 * report aggregates. Property names match the JSON contract byte for byte.
 */

export interface Leg {
  kind: 'TRUNK' | 'DIALING' | 'BOT' | 'QUEUE' | 'AGENT'
  label: string
  durationSec: number
  note?: string
}

export type CDRStatus = 'ANSWERED' | 'MISSED' | 'FAILED' | 'NO_ANSWER'

export interface CDR {
  callId: string
  startedAt: string
  answeredAt?: string
  endedAt: string
  callType: 'INBOUND' | 'OUTBOUND' | 'CONSULT' | 'INTERNAL'
  language?: string
  fromNumber: string
  toNumber: string
  did?: string
  flowId?: string
  queueId?: string
  agentIds?: string[]
  primaryAgentId?: string
  ringSec: number
  botSec: number
  queueWaitSec: number
  talkSec: number
  totalSec: number
  status: CDRStatus
  hangupCause?: string
  missedReason?: string
  disposition?: string
  isContained: boolean
  hasRecording: boolean
  userData?: Record<string, unknown>
  legs: Leg[]
}

export interface TranscriptEntry {
  seq: number
  occurredAt: string
  role: 'CALLER' | 'BOT' | 'AGENT'
  kind: string
  content: Record<string, unknown>
}

export interface RecordingRow {
  id: string
  callId: string
  backend: string
  bucket?: string
  objectKey: string
  sizeBytes: number
  durationSec: number
  format: string
  createdAt: string
}

export type CallbackStatus = 'OPEN' | 'CLAIMED' | 'DONE' | 'DISMISSED'

export interface Callback {
  id: string
  callId?: string
  queueId?: string
  phoneNumber: string
  message: string
  status: CallbackStatus
  createdAt: string
  handledBy?: string
  handledAt?: string
}

export interface Overview {
  totalCalls: number
  answeredCalls: number
  abandonedCalls: number
  containedCalls: number
  queueCalls: number
  answeredWithinSla: number
  avgWaitSec: number
  avgTalkSec: number
  avgBotSec: number
}

export interface QueueReport {
  queueId: string | null
  totalCalls: number
  answeredCalls: number
  abandonedCalls: number
  answeredWithinSla: number
  avgWaitSec: number
  maxWaitSec: number
  avgTalkSec: number
}

export interface DailyReport {
  day: string
  totalCalls: number
  answeredCalls: number
  containedCalls: number
  abandonedCalls: number
}

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

  cdr: (callId: string) =>
    request<{ cdr: CDR; transcript: TranscriptEntry[]; recordings: RecordingRow[] }>(
      `/cdrs/${callId}`,
    ),

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
