import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { CDRS_KEY } from './ledger'
import {
  ApiError,
  agentApi,
  callApi,
  type Availability,
  type CallSnapshot,
  type NotReadyReason,
  type WrapUpRequest,
} from './api'

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
    /**
     * Completing after-call work also writes to the ledger, so the agent's own
     * call list shows the disposition they just filed.
     */
    wrapUp: useMutation({
      mutationFn: (body: WrapUpRequest) => agentApi.wrapUp(body),
      onSuccess: (presence) => {
        onSuccess(presence)
        void queryClient.invalidateQueries({ queryKey: CDRS_KEY })
      },
    }),
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

export const CALLS_KEY = ['calls', 'mine'] as const

/** The calls this agent is currently a party to. */
export function useMyCalls(enabled: boolean) {
  return useQuery({
    queryKey: CALLS_KEY,
    enabled,
    retry: false,
    staleTime: 2_000,
    queryFn: async () => {
      try {
        return await callApi.mine()
      } catch (error) {
        if (error instanceof ApiError && error.status === 403) return { items: [] }
        throw error
      }
    },
  })
}

export const WAITING_KEY = ['calls', 'waiting'] as const

/**
 * The callers waiting in the queues this agent staffs.
 *
 * Refetched on the queue events rather than polled: the list moves whenever
 * somebody joins, is answered or gives up, and nothing else moves it.
 */
export function useWaitingCalls(enabled: boolean) {
  return useQuery({
    queryKey: WAITING_KEY,
    enabled,
    retry: false,
    staleTime: 2_000,
    queryFn: async () => {
      try {
        return await callApi.waiting()
      } catch (error) {
        if (error instanceof ApiError && error.status === 403) return { items: [] }
        throw error
      }
    },
  })
}

export function useCallActions() {
  const queryClient = useQueryClient()
  // Call state is authoritative on the switch, so an action refetches rather
  // than guessing what the switch will do next.
  const settle = () => void queryClient.invalidateQueries({ queryKey: CALLS_KEY })

  return {
    answer: useMutation({ mutationFn: callApi.answer, onSettled: settle }),
    hold: useMutation({ mutationFn: callApi.hold, onSettled: settle }),
    retrieve: useMutation({ mutationFn: callApi.retrieve, onSettled: settle }),
    hangup: useMutation({ mutationFn: callApi.hangup, onSettled: settle }),
    transfer: useMutation({
      mutationFn: ({ callId, destination }: { callId: string; destination: string }) =>
        callApi.transfer(callId, destination),
      onSettled: settle,
    }),
    mute: useMutation({ mutationFn: callApi.mute, onSettled: settle }),
    unmute: useMutation({ mutationFn: callApi.unmute, onSettled: settle }),
    // Tones change nothing about the call, so this one does not resettle the
    // snapshot — a refetch per keypress would be noise.
    sendDtmf: useMutation({
      mutationFn: ({ callId, digits }: { callId: string; digits: string }) =>
        callApi.sendDtmf(callId, digits),
    }),
  }
}

/** The agent's own leg of a call, which is the one they can control. */
export function myParty(call: CallSnapshot, agentId: string | undefined) {
  return call.parties.find((p) => p.agentId && p.agentId === agentId && p.state !== 'RELEASED')
}

/** The other side of the conversation, for display. */
export function otherParty(call: CallSnapshot, agentId: string | undefined) {
  return call.parties.find((p) => !p.agentId || p.agentId !== agentId)
}
