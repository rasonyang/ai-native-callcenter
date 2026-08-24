import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

export type Extension = components['schemas']['Extension']
/** The write shape: what create and update accept (password lives only here). */
export type ExtensionWrite = components['schemas']['ExtensionWrite']
/** Form state: a partially filled write, plus the id when editing. */
export type ExtensionDraft = Partial<Extension & ExtensionWrite>

export type Strategy = components['schemas']['Strategy']

export type OverflowType = components['schemas']['OverflowType']

export type Queue = components['schemas']['Queue']
export type QueueWrite = components['schemas']['QueueWrite']
export type QueueDraft = Partial<Queue & QueueWrite>

export type DID = components['schemas']['DID']
export type DIDWrite = components['schemas']['DIDWrite']
export type DIDDraft = Partial<DID & DIDWrite>

export type QueueAgent = components['schemas']['QueueAgent']

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
  createExtension: (body: Partial<ExtensionWrite>) =>
    request<Extension>('/extensions', { method: 'POST', body: JSON.stringify(body) }),
  updateExtension: (id: string, body: Partial<ExtensionWrite>) =>
    request<Extension>(`/extensions/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteExtension: (id: string) => request<void>(`/extensions/${id}`, { method: 'DELETE' }),

  /**
   * A phone's SIP password, in clear. Asked for by name and never carried by
   * the list, because reading it is recorded and a credential that arrives
   * unasked cannot be.
   */
  extensionPassword: (id: string) =>
    request<{ password: string }>(`/extensions/${id}/password`),

  queues: () => request<{ items: Queue[] }>('/queues'),
  createQueue: (body: Partial<QueueWrite>) =>
    request<Queue>('/queues', { method: 'POST', body: JSON.stringify(body) }),
  updateQueue: (id: string, body: Partial<QueueWrite>) =>
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
  createDID: (body: Partial<DIDWrite>) =>
    request<DID>('/dids', { method: 'POST', body: JSON.stringify(body) }),
  updateDID: (id: string, body: Partial<DIDWrite>) =>
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
      mutationFn: ({ id, ...body }: ExtensionDraft) =>
        id ? catalogApi.updateExtension(id, body) : catalogApi.createExtension(body),
      onSuccess: after(EXTENSIONS_KEY),
    }),
    deleteExtension: useMutation({
      mutationFn: catalogApi.deleteExtension,
      onSuccess: after(EXTENSIONS_KEY),
    }),
    saveQueue: useMutation({
      mutationFn: ({ id, ...body }: QueueDraft) =>
        id ? catalogApi.updateQueue(id, body) : catalogApi.createQueue(body),
      onSuccess: after(QUEUES_KEY),
    }),
    deleteQueue: useMutation({
      mutationFn: catalogApi.deleteQueue,
      onSuccess: after(QUEUES_KEY),
    }),
    saveDID: useMutation({
      mutationFn: ({ id, ...body }: DIDDraft) =>
        id ? catalogApi.updateDID(id, body) : catalogApi.createDID(body),
      onSuccess: after(DIDS_KEY),
    }),
    deleteDID: useMutation({
      mutationFn: catalogApi.deleteDID,
      onSuccess: after(DIDS_KEY),
    }),
  }
}

/**
 * A phone credential nobody has to invent, minted in the browser for a new
 * extension. The server mints its own for a phone it allocates; this is for
 * the one an operator adds by hand, so the field is never left to "1234".
 */
export function generateSIPPassword(): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join('')
}
