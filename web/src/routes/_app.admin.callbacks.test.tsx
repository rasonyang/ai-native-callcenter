import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import type { Callback } from '@/lib/ledger'
import { Route } from '@/routes/_app.admin.callbacks'
import { identityFixture, installBackend, renderWithProviders } from '@/test/harness'

/**
 * The team's callback queue: read-only, filtered by status, paged, and a claim
 * is shown by the claimant's name, never by an id.
 */

const TeamCallbacks = Route.options.component as () => ReactElement

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const at = '2026-10-20T09:12:00Z'

function callback(n: number, over: Partial<Callback> = {}): Callback {
  return {
    id: id(n), phoneNumber: `+86138000000${String(n).padStart(2, '0')}`, message: `message ${n}`,
    status: 'OPEN', createdAt: at, ...over,
  }
}

const KNOWN = id(900)
const GONE = id(901)

afterEach(() => vi.unstubAllGlobals())

function render(handle: (path: string) => unknown) {
  const api = installBackend({
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/agents') {
        return { items: [{ agentId: id(800), userId: KNOWN, username: 'mina', displayName: 'Mina Chen' }] }
      }
      return path.startsWith('/callbacks') ? handle(path) : undefined
    },
  })
  return { api, ...renderWithProviders(<TeamCallbacks />) }
}

const callbackRequests = (api: { requests: Array<{ path: string }> }) =>
  api.requests.filter((r) => r.path.startsWith('/callbacks')).map((r) => r.path)

describe('the team callback queue', () => {
  it('asks for the open callbacks, the first page, by default', async () => {
    const { api } = render(() => ({ items: [callback(1)], total: 1 }))
    await screen.findByText('message 1')
    expect(callbackRequests(api)[0]).toBe('/callbacks?status=OPEN&limit=50&offset=0')
  })

  it('changes the query with the filter and returns to the first page', async () => {
    const { api, user } = render((path) => {
      const offset = new URL(path, 'http://x').searchParams.get('offset')
      return { items: [callback(offset === '50' ? 2 : 1)], total: 120 }
    })
    await screen.findByText('message 1')
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await screen.findByText('message 2')
    expect(callbackRequests(api).at(-1)).toBe('/callbacks?status=OPEN&limit=50&offset=50')

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'CLAIMED' } })
    await waitFor(() =>
      expect(callbackRequests(api).at(-1)).toBe('/callbacks?status=CLAIMED&limit=50&offset=0'),
    )
    // "All" sends no status at all.
    fireEvent.change(screen.getByRole('combobox'), { target: { value: '' } })
    await waitFor(() => expect(callbackRequests(api).at(-1)).toBe('/callbacks?limit=50&offset=0'))
  })

  it('names a claimant from the roster and shows a neutral word for one it does not know', async () => {
    render(() => ({
      items: [
        callback(1, { status: 'CLAIMED', handledBy: KNOWN }),
        callback(2, { status: 'CLAIMED', handledBy: GONE }),
      ],
      total: 2,
    }))
    expect(await screen.findByText('Mina Chen')).toBeInTheDocument()
    expect(screen.getByText('Unknown user')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain(GONE)
  })

  it('offers no way to claim, release or close a callback', async () => {
    render(() => ({ items: [callback(1), callback(2, { status: 'CLAIMED', handledBy: KNOWN })], total: 2 }))
    await screen.findByText('message 1')
    expect(screen.queryByRole('button', { name: /claim|release|done|dismiss/i })).toBeNull()
  })

  it('says when the last attempt was and how it went', async () => {
    render(() => ({
      items: [callback(1, { status: 'CLAIMED', handledBy: KNOWN, lastAttemptAt: at, lastAttemptStatus: 'NO_ANSWER' })],
      total: 1,
    }))
    expect(await screen.findByText(/no answer/)).toBeInTheDocument()
  })
})

const bounce = (role: 'ADMIN' | 'SUPERVISOR' | 'AGENT') => {
  try {
    Route.options.beforeLoad?.({ context: { user: identityFixture({ role }) } } as never)
  } catch (thrown) {
    return (thrown as { options?: { to?: string } }).options?.to
  }
  return undefined
}

describe('who may open the page', () => {
  it('lets an administrator and a supervisor in', () => {
    expect(bounce('ADMIN')).toBeUndefined()
    expect(bounce('SUPERVISOR')).toBeUndefined()
  })

  it('turns an agent away to their own home', () => {
    expect(bounce('AGENT')).toBe('/agent')
  })
})
