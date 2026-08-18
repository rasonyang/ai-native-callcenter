import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { render, type RenderResult } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { I18nextProvider } from 'react-i18next'

import { EventStreamProvider } from '@/lib/use-event-stream'
import type { ComponentProps, ReactElement } from 'react'
import { vi } from 'vitest'

import i18n from '@/lib/i18n'
import type { CallSnapshot, Presence } from '@/lib/api'

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
    state: 'READY',
    availability: 'READY',
    extensionNumber: '1001',
    enteredAt: new Date().toISOString(),
    ...overrides,
  }
}

/** A two-leg inbound call with the agent's own leg in the given state. */
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
      if (path === '/calls/mine') return json({ items: backend.calls })
      if (path === '/callbacks') return json({ items: [] })
      if (path === '/calls/dial') return json({ callId: CALL_ID })
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
} {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const result = render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>
    </I18nextProvider>,
  )
  return { ...result, user: userEvent.setup() }
}
