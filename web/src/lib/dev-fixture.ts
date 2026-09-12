import type { QueryClient } from '@tanstack/react-query'

import { CALLS_KEY, PRESENCE_KEY } from './agent'
import { CALLBACKS_KEY } from './ledger'
import type { CallSnapshot, Presence } from './api'
import type { Callback } from './ledger'

/**
 * Development-only state fixtures for the agent cockpit.
 *
 * The cockpit's states depend on a live switch, so screenshotting or measuring
 * "on a call" would otherwise mean placing a real call. This seeds the query
 * cache instead, exactly as the reference design does with its own simulate
 * handle, so a given state can be reached deterministically.
 *
 * It is installed only under `import.meta.env.DEV` and never reaches a build.
 */
export type FixtureState = 'idle' | 'ringing' | 'onCall' | 'wrapUp'

const AGENT_ID = '00000000-0000-4000-8000-0000000000a1'
const CALL_ID = '00000000-0000-4000-8000-0000000000c1'
const CALLER = '+861083550341'

function presenceFor(state: FixtureState, now: number): Presence {
  const enteredAt = new Date(now).toISOString()
  if (state === 'wrapUp') {
    return {
      agentId: AGENT_ID,
      state: 'NOT_READY',
      reason: 'AFTER_CALL_WORK',
      availability: 'WRAP_UP',
      extensionNumber: '1005',
      isDeviceRegistered: true,
      deviceAccount: '1005',
      enteredAt,
      wrapUpCallId: CALL_ID,
    }
  }
  return {
    agentId: AGENT_ID,
    state: 'READY',
    reason: undefined,
    availability: state === 'onCall' || state === 'ringing' ? 'ON_CALL' : 'READY',
    extensionNumber: '1005',
    isDeviceRegistered: true,
    deviceAccount: '1005',
    enteredAt,
  }
}

function callFor(state: FixtureState, now: number): CallSnapshot | null {
  if (state === 'idle' || state === 'wrapUp') return null
  const createdAt = new Date(now).toISOString()
  const isTalking = state === 'onCall'
  return {
    callId: CALL_ID,
    callType: 'INBOUND',
    state: 'RUNNING',
    language: 'zh',
    parties: [
      {
        partyId: '00000000-0000-4000-8000-0000000000p1',
        channelId: 'fixture-caller',
        role: 'ORIGINATOR',
        state: isTalking ? 'TALKING' : 'RINGING',
        number: CALLER,
        createdAt,
        answeredAt: isTalking ? createdAt : undefined,
      },
      {
        partyId: '00000000-0000-4000-8000-0000000000p2',
        channelId: 'fixture-agent',
        role: 'TARGET',
        state: isTalking ? 'TALKING' : 'RINGING',
        number: '1005',
        otherNumber: CALLER,
        agentId: AGENT_ID,
        createdAt,
        answeredAt: isTalking ? createdAt : undefined,
      },
    ],
    userData: { ticketId: 'ORD-10391', tier: 'VIP' },
    queue: { name: 'support-zh', joinedAt: createdAt },
    createdAt,
  }
}

const CALLBACKS: Callback[] = [
  {
    id: '00000000-0000-4000-8000-0000000000b1',
    phoneNumber: '+8613700990011',
    message: 'Refund status',
    status: 'OPEN',
    createdAt: new Date(0).toISOString(),
  },
]

/** Seeds the cache for one state and stops the queries refetching over it. */
export function installDevFixture(queryClient: QueryClient) {
  if (!import.meta.env.DEV) return

  const apply = (state: FixtureState) => {
    const now = Date.now()
    queryClient.setQueryData(PRESENCE_KEY, presenceFor(state, now))
    const call = callFor(state, now)
    queryClient.setQueryData(CALLS_KEY, { items: call ? [call] : [] })
    queryClient.setQueryData([...CALLBACKS_KEY, 'OPEN'], { items: CALLBACKS })
    return state
  }

  // The seeded state would be overwritten by the next refetch, so refetching
  // is paused for as long as a fixture is in force.
  const freeze = () => {
    queryClient.setDefaultOptions({
      queries: { refetchOnMount: false, refetchOnWindowFocus: false, staleTime: Infinity },
    })
  }

  ;(window as unknown as { aiccSimulate?: (state: FixtureState) => FixtureState }).aiccSimulate =
    (state: FixtureState) => {
      freeze()
      return apply(state)
    }
}
