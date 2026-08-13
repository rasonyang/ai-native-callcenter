import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { request } from './api'

export type ExtensionKind = 'AGENT' | 'BOT' | 'PLAIN'

export interface Extension {
  id: string
  number: string
  kind: ExtensionKind
  displayName: string
  isEnabled: boolean
  /** Write-only: accepted on save, never returned. */
  password?: string
}

export type Strategy =
  | 'LONGEST_IDLE_AGENT' | 'ROUND_ROBIN' | 'TOP_DOWN'
  | 'AGENT_WITH_LEAST_TALK_TIME' | 'AGENT_WITH_FEWEST_CALLS' | 'RANDOM'

export type OverflowType = 'ANNOUNCE_HANGUP' | 'BOT_FLOW' | 'FORWARD'

export interface Queue {
  id: string
  name: string
  extNumber: string
  displayName: string
  strategy: Strategy
  mohSound: string
  maxWaitSec: number
  maxWaitNoAgentSec: number
  announceSound?: string
  announceFrequencySec: number
  tierRules: { isApplied: boolean; waitSec: number }
  discardAbandonedAfterSec: number
  isAbandonedResumeAllowed: boolean
  ronaDelaySec: number
  slaThresholdSec: number
  isRecordingEnabled: boolean
  hours: Array<{ weekday: number; open: string; close: string }>
  overflow: { type: OverflowType; target?: string; sound?: string }
  isEnabled: boolean
}

export interface DID {
  id: string
  number: string
  language: string
  flowId?: string
  fallbackQueueId?: string
  isRecordingEnabled: boolean
  description: string
  isEnabled: boolean
}

export interface QueueAgent {
  queueId: string
  agentId: string
  displayName: string
  level: number
  position: number
}

export const EXTENSIONS_KEY = ['catalog', 'extensions'] as const
export const QUEUES_KEY = ['catalog', 'queues'] as const
export const DIDS_KEY = ['catalog', 'dids'] as const

export const STRATEGIES: Strategy[] = [
  'LONGEST_IDLE_AGENT', 'ROUND_ROBIN', 'TOP_DOWN',
  'AGENT_WITH_LEAST_TALK_TIME', 'AGENT_WITH_FEWEST_CALLS', 'RANDOM',
]

export const OVERFLOW_TYPES: OverflowType[] = ['ANNOUNCE_HANGUP', 'BOT_FLOW', 'FORWARD']

export const catalogApi = {
  extensions: () => request<{ items: Extension[] }>('/extensions'),
  createExtension: (body: Partial<Extension>) =>
    request<Extension>('/extensions', { method: 'POST', body: JSON.stringify(body) }),
  updateExtension: (id: string, body: Partial<Extension>) =>
    request<Extension>(`/extensions/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteExtension: (id: string) => request<void>(`/extensions/${id}`, { method: 'DELETE' }),

  queues: () => request<{ items: Queue[] }>('/queues'),
  createQueue: (body: Partial<Queue>) =>
    request<Queue>('/queues', { method: 'POST', body: JSON.stringify(body) }),
  updateQueue: (id: string, body: Partial<Queue>) =>
    request<Queue>(`/queues/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteQueue: (id: string) => request<void>(`/queues/${id}`, { method: 'DELETE' }),
  queueAgents: (id: string) => request<{ items: QueueAgent[] }>(`/queues/${id}/agents`),
  staffQueue: (id: string, agentId: string, level = 1, position = 1) =>
    request<void>(`/queues/${id}/agents`, {
      method: 'PUT',
      body: JSON.stringify({ agentId, level, position }),
    }),
  unstaffQueue: (id: string, agentId: string) =>
    request<void>(`/queues/${id}/agents/${agentId}`, { method: 'DELETE' }),

  dids: () => request<{ items: DID[] }>('/dids'),
  createDID: (body: Partial<DID>) =>
    request<DID>('/dids', { method: 'POST', body: JSON.stringify(body) }),
  updateDID: (id: string, body: Partial<DID>) =>
    request<DID>(`/dids/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteDID: (id: string) => request<void>(`/dids/${id}`, { method: 'DELETE' }),
}

export function useExtensions() {
  return useQuery({ queryKey: EXTENSIONS_KEY, queryFn: catalogApi.extensions })
}

export function useQueues(enabled = true) {
  return useQuery({ queryKey: QUEUES_KEY, queryFn: catalogApi.queues, enabled })
}

export function useDIDs() {
  return useQuery({ queryKey: DIDS_KEY, queryFn: catalogApi.dids })
}

/** Mutations that refresh their own list, since the server may normalize. */
export function useCatalogMutations() {
  const queryClient = useQueryClient()
  const after = (key: readonly unknown[]) => () =>
    void queryClient.invalidateQueries({ queryKey: key })

  return {
    saveExtension: useMutation({
      mutationFn: ({ id, ...body }: Partial<Extension> & { id?: string }) =>
        id ? catalogApi.updateExtension(id, body) : catalogApi.createExtension(body),
      onSuccess: after(EXTENSIONS_KEY),
    }),
    deleteExtension: useMutation({
      mutationFn: catalogApi.deleteExtension,
      onSuccess: after(EXTENSIONS_KEY),
    }),
    saveQueue: useMutation({
      mutationFn: ({ id, ...body }: Partial<Queue> & { id?: string }) =>
        id ? catalogApi.updateQueue(id, body) : catalogApi.createQueue(body),
      onSuccess: after(QUEUES_KEY),
    }),
    deleteQueue: useMutation({
      mutationFn: catalogApi.deleteQueue,
      onSuccess: after(QUEUES_KEY),
    }),
    saveDID: useMutation({
      mutationFn: ({ id, ...body }: Partial<DID> & { id?: string }) =>
        id ? catalogApi.updateDID(id, body) : catalogApi.createDID(body),
      onSuccess: after(DIDS_KEY),
    }),
    deleteDID: useMutation({
      mutationFn: catalogApi.deleteDID,
      onSuccess: after(DIDS_KEY),
    }),
  }
}
