import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import type { Campaign, CampaignContact } from '@/lib/campaigns'
import type { Callback } from '@/lib/ledger'

import { CampaignLeads } from './campaign-leads'

const CAMPAIGN_ID = '00000000-0000-4000-8000-000000000001'
const CALL_A = '00000000-0000-4000-8000-00000000ca11'
const CALL_B = '00000000-0000-4000-8000-00000000cb11'
const CALL_D = '00000000-0000-4000-8000-00000000cd11'

const campaign = { id: CAMPAIGN_ID, name: 'Spring renewals' } as Campaign

function lead(over: Partial<CampaignContact>): CampaignContact {
  return {
    id: crypto.randomUUID(),
    campaignId: CAMPAIGN_ID,
    number: '+8613800138000',
    numberRaw: '13800138000',
    slots: {},
    status: 'DONE',
    attempts: 1,
    systemFailures: 0,
    lastOutcome: 'ANSWERED',
    leadIntent: 'HIGH',
    leadNotes: 'wants the offer',
    lastCallId: null,
    lastAttemptId: null,
    createdAt: '2026-10-01T00:00:00Z',
    ...over,
  }
}

function callback(over: Partial<Callback>): Callback {
  return {
    id: crypto.randomUUID(),
    phoneNumber: '+8613800138000',
    message: '',
    status: 'OPEN',
    createdAt: '2026-10-01T00:00:00Z',
    ...over,
  } as Callback
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })

afterEach(() => {
  vi.unstubAllGlobals()
})

/**
 * Serves the leads and answers /callbacks?callId= from byCall. An unfiltered
 * /callbacks answers with a callback for every lead's number, which is what a
 * screen matching by phone would wrongly pick up.
 */
function stubBackend(leads: CampaignContact[], byCall: Record<string, Callback>) {
  const requests: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), 'http://test')
    const path = url.pathname.replace('/api/v1', '')
    requests.push(path + url.search)
    if (path === `/campaigns/${CAMPAIGN_ID}/contacts`) return json({ items: leads, total: leads.length })
    if (path === '/callbacks') {
      const callId = url.searchParams.get('callId')
      if (callId === null) return json({ items: leads.map((l) => callback({ phoneNumber: l.number })) })
      return json({ items: byCall[callId] ? [byCall[callId]] : [] })
    }
    return json({ items: [] })
  }))
  return requests
}

function renderLeads() {
  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({
      getParentRoute: () => root, path: '/admin/campaigns/$campaignId',
      component: () => <CampaignLeads campaign={campaign} intent="HIGH" page={0} onChange={() => {}} />,
    }),
    createRoute({ getParentRoute: () => root, path: '/admin/cdr/$callId', component: () => null }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: [`/admin/campaigns/${CAMPAIGN_ID}`] }),
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />
      </QueryClientProvider>
    </I18nextProvider>,
  )
}

const rowOf = (number: string) => screen.getByText(number).closest('tr') as HTMLElement

describe('the leads tab', () => {
  it('links a HIGH lead to its call and its callback by call id, never by number', async () => {
    const requests = stubBackend(
      [
        lead({ number: '+8613800138000', lastCallId: CALL_A, lastAttemptId: crypto.randomUUID() }),
        // Its number has callbacks in the queue, but its own call filed none.
        lead({ number: '+8613800138001', lastCallId: CALL_B, lastAttemptId: crypto.randomUUID() }),
        // No call named yet.
        lead({ number: '+8613800138002', lastOutcome: undefined }),
        // A call reconciled UNKNOWN: its callback is shown, no call page promised.
        lead({ number: '+8613800138003', lastOutcome: 'UNKNOWN', lastCallId: CALL_D, lastAttemptId: crypto.randomUUID() }),
      ],
      {
        [CALL_A]: callback({ callId: CALL_A, status: 'OPEN' }),
        [CALL_D]: callback({ callId: CALL_D, status: 'CLAIMED', phoneNumber: '+8613800138003' }),
      },
    )
    renderLeads()

    await screen.findByText('+8613800138000')
    await waitFor(() => expect(within(rowOf('+8613800138000')).getByText('Open')).toBeInTheDocument())
    const link = within(rowOf('+8613800138000')).getByRole('link', { name: 'Open call' })
    expect(link).toHaveAttribute('href', `/admin/cdr/${CALL_A}`)

    await waitFor(() => expect(within(rowOf('+8613800138003')).getByText('Claimed')).toBeInTheDocument())
    expect(within(rowOf('+8613800138003')).queryByRole('link')).toBeNull()

    // A lead whose call filed no callback shows a dash, not its number's.
    const noCallback = rowOf('+8613800138001')
    expect(within(noCallback).queryByText('Open')).toBeNull()
    expect(within(noCallback).getByRole('link', { name: 'Open call' })).toHaveAttribute('href', `/admin/cdr/${CALL_B}`)

    const unnamed = rowOf('+8613800138002')
    expect(within(unnamed).queryByRole('link')).toBeNull()
    expect(within(unnamed).queryByText('Open')).toBeNull()

    const callbackRequests = requests.filter((r) => r.startsWith('/callbacks'))
    expect(callbackRequests.toSorted()).toEqual(
      [CALL_A, CALL_B, CALL_D].map((id) => `/callbacks?callId=${id}`).toSorted(),
    )
  })

  it('asks for no callback for a lead that is not HIGH, and still links its call', async () => {
    const requests = stubBackend(
      [lead({ leadIntent: 'MEDIUM', lastCallId: CALL_A, lastAttemptId: crypto.randomUUID() })],
      { [CALL_A]: callback({ callId: CALL_A }) },
    )
    renderLeads()
    await screen.findByText('+8613800138000')
    const row = rowOf('+8613800138000')
    expect(within(row).getByRole('link', { name: 'Open call' })).toHaveAttribute('href', `/admin/cdr/${CALL_A}`)
    expect(requests.some((r) => r.startsWith('/callbacks'))).toBe(false)
  })
})
