import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'
import { SCOPES, SCOPE_DESCRIPTIONS, type Scope } from '@/generated/scopes'
import { request } from './api'

/** One integration's credential, as it can be read back. Never its secret. */
export type APIKey = components['schemas']['APIKey']

/**
 * The created key, plus the secret — the only time the secret exists outside
 * the server's response. It is stored as a digest, so nothing can produce it
 * again: a lost key is revoked and reissued.
 */
export type APIKeyCreated = components['schemas']['APIKeyCreated']

export type APIKeyStatus = components['schemas']['APIKeyStatus']

/**
 * The scope vocabulary, generated from the contract's `x-scopes`.
 *
 * Re-exported here so the form imports one module, but it is the contract's
 * list either way — writing the options out by hand would be a second
 * vocabulary, and the day a scope is added or renamed the form would go on
 * offering yesterday's.
 */
export { SCOPES, SCOPE_DESCRIPTIONS }
export type { Scope }

export const KEYS_KEY = ['api-keys'] as const

export const keyApi = {
  list: () => request<components['schemas']['APIKeyList']>('/api-keys'),
  create: (body: components['schemas']['APIKeyWrite']) =>
    request<APIKeyCreated>('/api-keys', { method: 'POST', body: JSON.stringify(body) }),
  update: (id: string, body: components['schemas']['APIKeyUpdate']) =>
    request<APIKey>(`/api-keys/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  // Terminal, and its own operation for that reason: a PATCH that could also
  // switch a key back on would make "revoked" a state rather than an ending.
  revoke: (id: string) => request<APIKey>(`/api-keys/${id}/revoke`, { method: 'POST' }),
}

export function useAPIKeys() {
  return useQuery({ queryKey: KEYS_KEY, queryFn: keyApi.list })
}

export function useAPIKeyMutations() {
  const queryClient = useQueryClient()
  const after = () => void queryClient.invalidateQueries({ queryKey: KEYS_KEY })

  return {
    createKey: useMutation({ mutationFn: keyApi.create, onSuccess: after }),
    updateKey: useMutation({
      mutationFn: ({ id, ...body }: { id: string; name?: string; scopes?: string[] }) =>
        keyApi.update(id, body),
      onSuccess: after,
    }),
    revokeKey: useMutation({ mutationFn: keyApi.revoke, onSuccess: after }),
  }
}
