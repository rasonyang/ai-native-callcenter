import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { act, render, type RenderResult } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { I18nextProvider } from 'react-i18next'

import { EventStreamProvider } from '@/lib/use-event-stream'
import type { ComponentProps, ReactElement } from 'react'
import { vi } from 'vitest'

import i18n from '@/lib/i18n'
import type { CallSnapshot, CurrentWrapUp, Presence, WaitingCall } from '@/lib/api'
import type { Contact } from '@/lib/contacts'
import type { CDR, RecordingRow } from '@/lib/ledger'
import type { AgentToday, Disposition } from '@/lib/ledger'

/**
 * Component-test harness for the agent cockpit.
 *
 * The cockpit's whole job is turning a click into one REST call against a live
 * switch, so the tests assert on the request that leaves the browser rather
 * than on a mocked module: if a handler is deleted or rewired, the recorded
 * request changes and the test fails.
 */

export interface RecordedRequest {
  method: string
  path: string
  body: unknown
}

export interface Backend {
  requests: RecordedRequest[]
  /** Requests that changed something, i.e. everything but GET. */
  commands: RecordedRequest[]
  presence: Presence
  calls: CallSnapshot[]
  /** Callers queued in the agent's own queues. */
  waiting: WaitingCall[]
  /** The wrap-up vocabulary the server offers. */
  dispositions: Disposition[]
  /** The agent's own numbers for the day. */
  today: AgentToday
  /** The after-call record waiting to be confirmed, if any. */
  currentWrapUp: CurrentWrapUp | null
  /** The contact book, matched by exact number the way the server does. */
  contacts: Contact[]
  /** The agent's own finished calls. */
  myCDRs: CDR[]
  /** The audio artifacts behind any call, served for every callId asked. */
  recordings: RecordingRow[]
  /** Rows the transcript snapshot serves, and the state it reports. */
  transcript: unknown[]
  transcriptState?: string
  /** Delays the transcript snapshot, so a test can land it after an event. */
  slowSnapshotMs?: number
}

const BASE = '/api/v1'

const json = (value: unknown) =>
  new Response(JSON.stringify(value), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  })

export const AGENT_ID = '00000000-0000-4000-8000-0000000000a1'
export const CALL_ID = '00000000-0000-4000-8000-0000000000c1'
export const CALLER = '+861083550341'

export function presenceFixture(overrides: Partial<Presence> = {}): Presence {
  return {
    agentId: AGENT_ID,
    state: 'READY',
    availability: 'READY',
    extensionNumber: '1001',
    enteredAt: new Date().toISOString(),
    ...overrides,
  }
}

/** A two-leg inbound call with the agent's own leg in the given state. */
/** A caller queued in support-zh, waiting since `agoSec` ago. */
export function waitingFixture(overrides: Partial<WaitingCall> = {}): WaitingCall {
  return {
    callId: '00000000-0000-4000-8000-0000000000w1',
    callType: 'INBOUND',
    fromNumber: '+8613700990011',
    queueId: '00000000-0000-4000-8000-0000000000q1',
    queueName: 'support-zh',
    queueDisplayName: 'Billing',
    slaThresholdSec: 20,
    joinedAt: new Date(Date.now() - 12_000).toISOString(),
    ...overrides,
  }
}

/** The vocabulary an installation ships with. */
export function dispositionsFixture(): Disposition[] {
  return [
    { code: 'RESOLVED', label: 'Resolved' },
    { code: 'FOLLOW_UP_REQUIRED', label: 'Follow-up Required' },
    { code: 'NO_ANSWER', label: 'No Answer' },
    { code: 'OTHER', label: 'Other' },
  ]
}

/**
 * The record the platform opened when the call ended: a default disposition,
 * no note, nobody's confirmation.
 */
export function openWrapUpFixture(overrides: Partial<CurrentWrapUp> = {}): CurrentWrapUp {
  return {
    callId: CALL_ID,
    dispositionCode: 'RESOLVED',
    dispositionLabel: 'Resolved',
    note: '',
    isConfirmed: false,
    createdAt: new Date().toISOString(),
    ...overrides,
  }
}

/** One agent's day, as the Today card receives it. */
export function todayFixture(overrides: Partial<AgentToday> = {}): AgentToday {
  return {
    callsHandled: 23,
    talkSec: 5980,
    wrapUpSec: 966,
    signedInSec: 8900,
    avgHandleSec: 276,
    avgWrapUpSec: 42,
    occupancyPct: 78,
    wrapUpsOpened: 23,
    wrapUpsConfirmed: 21,
    confirmedPct: 91,
    ...overrides,
  }
}

/** One finished call as the agent's own list receives it. */
export function cdrFixture(overrides: Partial<CDR> = {}): CDR {
  const startedAt = new Date(Date.now() - 600_000)
  return {
    callId: '00000000-0000-4000-8000-0000000000d1',
    startedAt: startedAt.toISOString(),
    endedAt: new Date(startedAt.getTime() + 300_000).toISOString(),
    callType: 'INBOUND',
    fromNumber: CALLER,
    toNumber: '95001',
    ringSec: 3,
    botSec: 40,
    queueWaitSec: 12,
    talkSec: 245,
    billSec: 300,
    totalSec: 300,
    status: 'ANSWERED',
    isContained: false,
    hasRecording: false,
    legs: [{ kind: 'QUEUE', label: 'support-zh', durationSec: 12 }],
    ...overrides,
  }
}

export function contactFixture(overrides: Partial<Contact> = {}): Contact {
  const at = new Date().toISOString()
  return {
    id: '00000000-0000-4000-8000-0000000000e1',
    phoneNumber: CALLER,
    name: 'Zhang Wei',
    company: 'NovaNet',
    email: '',
    tags: ['VIP'],
    notes: 'Prefers callbacks after 16:00.',
    createdAt: at,
    updatedAt: at,
    ...overrides,
  }
}

export function callFixture(state: CallSnapshot['parties'][number]['state']): CallSnapshot {
  const at = new Date().toISOString()
  const answered = state === 'TALKING' || state === 'HELD'
  return {
    callId: CALL_ID,
    callType: 'INBOUND',
    state: 'RUNNING',
    language: 'zh',
    createdAt: at,
    userData: { ticketId: 'ORD-10391' },
    queue: { name: 'support-zh', joinedAt: at },
    parties: [
      {
        partyId: '00000000-0000-4000-8000-0000000000p1',
        channelId: 'caller',
        role: 'ORIGINATOR',
        state,
        number: CALLER,
        createdAt: at,
        answeredAt: answered ? at : undefined,
      },
      {
        partyId: '00000000-0000-4000-8000-0000000000p2',
        channelId: 'agent',
        role: 'TARGET',
        state,
        number: '1001',
        otherNumber: CALLER,
        agentId: AGENT_ID,
        createdAt: at,
        answeredAt: answered ? at : undefined,
      },
    ],
  }
}

/**
 * Installs a fetch stub over the real `request()` path and records everything
 * that goes through it. Unknown paths resolve to an empty list, so a component
 * that grows a new query does not fail with a network error.
 */
export function installBackend(initial: Partial<Backend> = {}): Backend {
  const backend: Backend = {
    requests: [],
    get commands() {
      return backend.requests.filter((r) => r.method !== 'GET')
    },
    presence: initial.presence ?? presenceFixture(),
    calls: initial.calls ?? [],
    waiting: initial.waiting ?? [],
    dispositions: initial.dispositions ?? [],
    today: initial.today ?? todayFixture(),
    currentWrapUp: initial.currentWrapUp ?? null,
    contacts: initial.contacts ?? [],
    myCDRs: initial.myCDRs ?? [],
    recordings: initial.recordings ?? [],
    transcript: initial.transcript ?? [],
    transcriptState: initial.transcriptState,
    slowSnapshotMs: initial.slowSnapshotMs,
  } as Backend

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input)
      const path = url.startsWith(BASE) ? url.slice(BASE.length) : url
      const method = init.method ?? 'GET'
      backend.requests.push({
        method,
        path,
        body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined,
      })

      if (path.includes('/transcript')) {
        if (backend.slowSnapshotMs) {
          await new Promise((resolve) => setTimeout(resolve, backend.slowSnapshotMs))
        }
        return json({ items: backend.transcript ?? [], nextSinceSeq: 0, isLive: true,
          state: backend.transcriptState ?? 'LIVE' })
      }
      if (path === '/agent/presence') return json(backend.presence)
      if (path === '/agent/wrap-up' && method === 'GET') {
        // 204 is how the server says there is nothing to confirm.
        return backend.currentWrapUp
          ? json(backend.currentWrapUp)
          : new Response(null, { status: 204 })
      }
      if (path === '/agent/wrap-up' && method === 'POST') {
        // Confirming applies what was sent and marks the record looked at,
        // which is what the next read returns — the screen has no other
        // source for it.
        const body = (typeof init.body === 'string' ? JSON.parse(init.body) : {}) as {
          dispositionCode?: string
          note?: string
        }
        if (backend.currentWrapUp) {
          backend.currentWrapUp = {
            ...backend.currentWrapUp,
            dispositionCode: body.dispositionCode || backend.currentWrapUp.dispositionCode,
            dispositionLabel:
              (backend.dispositions.find((d) => d.code === body.dispositionCode) ?? {}).label ??
              backend.currentWrapUp.dispositionLabel,
            note: body.note ?? backend.currentWrapUp.note,
            isConfirmed: true,
          }
        }
        // Confirming ends after-call work, exactly as the server does.
        backend.presence = {
          ...backend.presence,
          state: 'READY',
          availability: 'READY',
          reason: undefined,
          wrapUpCallId: undefined,
        }
        return json(backend.presence)
      }
      if (/^\/calls\/[^/]+\/recordings$/.test(path)) {
        return json({ items: backend.recordings })
      }
      if (path === '/calls/mine') return json({ items: backend.calls })
      if (path === '/calls/waiting') return json({ items: backend.waiting })
      if (path.startsWith('/cdrs/mine')) {
        return json({ items: backend.myCDRs, total: backend.myCDRs.length })
      }
      if (path === '/dispositions') return json({ items: backend.dispositions })
      if (path === '/reports/me') return json(backend.today)
      if (path.startsWith('/contacts')) {
        const number = new URLSearchParams(path.split('?')[1] ?? '').get('phoneNumber')
        const items = number
          ? backend.contacts.filter((c) => c.phoneNumber === number)
          : backend.contacts
        return json({ items, total: items.length })
      }
      if (path === '/callbacks') return json({ items: [] })
      if (path === '/calls') return json({ callId: CALL_ID })
      if (path.startsWith('/agent/')) return json(backend.presence)
      return json({ items: [] })
    }),
  )

  return backend
}

/**
 * Renders a page component under a memory router.
 *
 * The cockpit links to `/agent/callbacks`, and a `Link` needs a router that
 * knows the target, so the test tree registers it as a stub.
 */
/** The listener registry the app shell would provide, exposed so a test can
 * push an event the way the EventSource does. */
type EventStreamListeners = NonNullable<
  ComponentProps<typeof EventStreamProvider>['value']
>['listeners']

export const testListeners = new Map<string, ((event: unknown) => void)[]>()

export function emitEvent(type: string, event: unknown) {
  for (const listener of testListeners.get(type) ?? []) listener(event)
}

export function renderPage(Component: () => ReactElement | null) {
  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({ getParentRoute: () => root, path: '/agent', component: Component }),
    createRoute({ getParentRoute: () => root, path: '/agent/callbacks', component: () => null }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: ['/agent'] }),
  })
  testListeners.clear()
  return renderWithProviders(
    <EventStreamProvider
      value={{
        status: 'connected',
        listeners: testListeners as unknown as EventStreamListeners,
      }}
    >
      <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />
    </EventStreamProvider>,
  )
}

export function renderWithProviders(ui: ReactElement): RenderResult & {
  user: ReturnType<typeof userEvent.setup>
  /**
   * Refetches every query, which is what the event stream does in production
   * when the switch reports a change (`applyToCache`). A test that moves the
   * backend on — a call ending, presence changing — asks for this rather than
   * waiting for a poll that does not exist.
   */
  refetch: () => Promise<void>
} {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const result = render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>
    </I18nextProvider>,
  )
  return {
    ...result,
    user: userEvent.setup(),
    refetch: () => act(async () => void (await queryClient.invalidateQueries())),
  }
}
