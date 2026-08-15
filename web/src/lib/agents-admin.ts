import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'
import { ROSTER_KEY } from './agent'

/**
 * Agent configuration, as administration edits it: who is an agent, and which
 * phone they are bound to. The binding is static, so it lives here rather than
 * being chosen at sign-in.
 */
export type Agent = components['schemas']['Agent']
export type AgentWrite = components['schemas']['AgentWrite']
/** Form state: a partially filled write, plus the id when editing. */
export type AgentDraft = Partial<Agent & AgentWrite>

export type User = components['schemas']['User']

export const USERS_KEY = ['users'] as const

export const agentAdminApi = {
  users: () => request<components['schemas']['UserList']>('/users'),

  create: (body: Partial<AgentWrite>) =>
    request<Agent>('/agents', { method: 'POST', body: JSON.stringify(body) }),

  update: (agentId: string, body: Partial<AgentWrite>) =>
    request<Agent>(`/agents/${agentId}`, { method: 'PUT', body: JSON.stringify(body) }),

  remove: (agentId: string) => request<void>(`/agents/${agentId}`, { method: 'DELETE' }),
}

export function useUsers(enabled = true) {
  return useQuery({ queryKey: USERS_KEY, queryFn: agentAdminApi.users, enabled })
}

/** Every write refreshes the roster, which is the list these edits appear in. */
export function useAgentAdminMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ROSTER_KEY })

  return {
    save: useMutation({
      mutationFn: ({ agentId, ...body }: AgentDraft) =>
        agentId ? agentAdminApi.update(agentId, body) : agentAdminApi.create(body),
      onSuccess: invalidate,
    }),
    remove: useMutation({ mutationFn: agentAdminApi.remove, onSuccess: invalidate }),
  }
}
