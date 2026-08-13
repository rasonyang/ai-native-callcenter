import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError, agentApi, type Availability, type NotReadyReason } from './api'

export const PRESENCE_KEY = ['agent', 'presence'] as const
export const ROSTER_KEY = ['agents', 'roster'] as const

/** Semantic colour token for a state. Colour appears only as a dot or pill. */
export const AVAILABILITY_COLOR: Record<Availability, string> = {
  READY: 'var(--state-available)',
  ON_CALL: 'var(--state-oncall)',
  WRAP_UP: 'var(--state-acw)',
  NOT_READY: 'var(--state-aux)',
  DEVICE_UNREACHABLE: 'var(--state-breach)',
  LOGGED_OUT: 'var(--state-offline)',
}

/** The reasons an agent may choose. LOGIN, SYSTEM and SUPERVISOR are set for
 *  them, never by them, so they are not offered here. */
export const SELECTABLE_REASONS: NotReadyReason[] = ['BREAK', 'LUNCH', 'TRAINING']

/** The signed-in agent's own presence. Returns null for non-agent accounts. */
export function usePresence(enabled: boolean) {
  return useQuery({
    queryKey: PRESENCE_KEY,
    enabled,
    retry: false,
    staleTime: 5_000,
    queryFn: async () => {
      try {
        return await agentApi.presence()
      } catch (error) {
        // A supervisor or administrator has no agent identity; that is not an
        // error, it just means no softphone bar.
        if (error instanceof ApiError && error.status === 403) return null
        throw error
      }
    },
  })
}

export function usePresenceActions() {
  const queryClient = useQueryClient()
  const onSuccess = (presence: unknown) => {
    queryClient.setQueryData(PRESENCE_KEY, presence)
    void queryClient.invalidateQueries({ queryKey: ROSTER_KEY })
  }

  return {
    signIn: useMutation({ mutationFn: agentApi.login, onSuccess }),
    signOut: useMutation({ mutationFn: agentApi.logout, onSuccess }),
    ready: useMutation({ mutationFn: agentApi.ready, onSuccess }),
    notReady: useMutation({ mutationFn: agentApi.notReady, onSuccess }),
  }
}

export function useRoster(enabled: boolean) {
  return useQuery({
    queryKey: ROSTER_KEY,
    enabled,
    queryFn: agentApi.roster,
    staleTime: 5_000,
  })
}

export function useForceLogout() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: agentApi.forceLogout,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ROSTER_KEY }),
  })
}

/**
 * Seconds elapsed since a timestamp, ticking once a second.
 *
 * Durations are derived in the browser from the server's timestamp rather than
 * counted by the server, so a slow or reconnecting stream never makes a timer
 * drift or jump.
 */
export function useElapsedSec(since: string | undefined): number {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1_000)
    return () => clearInterval(id)
  }, [])

  if (!since) return 0
  return Math.max(0, Math.floor((now - new Date(since).getTime()) / 1000))
}
