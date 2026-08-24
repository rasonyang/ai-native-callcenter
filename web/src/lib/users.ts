import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/**
 * Accounts, and the ACD identity and phone that belong to the ones that take
 * calls. Wire types come from the generated contract, as everywhere else.
 */

export type User = components['schemas']['User']
export type UserStatus = components['schemas']['UserStatus']
export type UserCreate = components['schemas']['UserCreate']
export type UserUpdate = components['schemas']['UserUpdate']

/** Form state: a new account being typed, or an existing one being edited. */
export type UserDraft = Partial<User & UserCreate>

export const USERS_KEY = ['users'] as const

export const usersApi = {
  list: () => request<{ items: User[] }>('/users'),

  create: (body: UserCreate) =>
    request<User>('/users', { method: 'POST', body: JSON.stringify(body) }),

  update: (userId: string, body: UserUpdate) =>
    request<User>(`/users/${userId}`, { method: 'PUT', body: JSON.stringify(body) }),

  /** Named `password` so the audit trail redacts it by field name (C58). */
  resetPassword: (userId: string, password: string) =>
    request<void>(`/users/${userId}/password`, {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
}

export function useUsers() {
  return useQuery({ queryKey: USERS_KEY, queryFn: usersApi.list })
}

/**
 * The three writes, sharing one invalidation.
 *
 * Creating an account can also allocate a phone, and promoting one can too, so
 * the extension list is stale after either — it is invalidated alongside.
 */
export function useUserMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: USERS_KEY })
    void queryClient.invalidateQueries({ queryKey: ['catalog', 'extensions'] })
    void queryClient.invalidateQueries({ queryKey: ['agents'] })
  }

  return {
    create: useMutation({
      mutationFn: (body: UserCreate) => usersApi.create(body),
      onSuccess: invalidate,
    }),
    update: useMutation({
      mutationFn: ({ userId, body }: { userId: string; body: UserUpdate }) =>
        usersApi.update(userId, body),
      onSuccess: invalidate,
    }),
    resetPassword: useMutation({
      mutationFn: ({ userId, password }: { userId: string; password: string }) =>
        usersApi.resetPassword(userId, password),
      onSuccess: invalidate,
    }),
  }
}
