import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError, api, type Identity } from './api'

const SESSION_KEY = ['session'] as const

/**
 * The current session. An expired or absent session resolves to null rather
 * than throwing, so the router can redirect instead of showing an error.
 */
export function useSession() {
  return useQuery({
    queryKey: SESSION_KEY,
    queryFn: async (): Promise<Identity | null> => {
      try {
        const { user } = await api.me()
        return user
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) return null
        throw error
      }
    },
    staleTime: 30_000,
    retry: false,
  })
}

export function useLogin() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }: { username: string; password: string }) =>
      api.login(username, password),
    onSuccess: ({ user }) => {
      queryClient.setQueryData(SESSION_KEY, user)
    },
  })
}

export function useLogout() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => api.logout(),
    onSuccess: () => {
      queryClient.setQueryData(SESSION_KEY, null)
      // Session-scoped caches must not survive a user switch.
      void queryClient.invalidateQueries()
    },
  })
}
