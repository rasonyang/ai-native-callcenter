import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Campaign, CampaignAttempt } from '@/lib/campaigns'
import i18n from '@/lib/i18n'
import { installBackend, renderWithProviders } from '@/test/harness'

import { AttemptsTab } from './_app.admin.campaigns.$campaignId'

const CAMPAIGN_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const campaign = { id: CAMPAIGN_ID, name: 'Renewal wave 1' } as Campaign

const attempt = (n: number, over: Partial<CampaignAttempt>): CampaignAttempt => ({
  id: `0199f3a2-7c41-7d22-9b1e-${String(4000 + n).padStart(12, '0')}`,
  campaignId: CAMPAIGN_ID,
  contactId: `0199f3a2-7c41-7d22-9b1e-${String(1000 + n).padStart(12, '0')}`,
  number: `+861380000000${n}`,
  callId: `0199f3a2-7c41-7d22-9b1e-${String(2000 + n).padStart(12, '0')}`,
  legId: `0199f3a2-7c41-7d22-9b1e-${String(5000 + n).padStart(12, '0')}`,
  state: 'FINISHED',
  outcome: 'INVALID_NUMBER',
  isTouched: true,
  isTestNumber: false,
  startedAt: '2026-10-20T09:12:00Z',
  ...over,
})

afterEach(() => vi.unstubAllGlobals())

/** The table row that shows this number. */
const row = async (number: string) => (await screen.findByText(number)).closest('tr') as HTMLElement

// A skipped number has no call page, so its rows draw no router link.
describe('the attempts list', () => {
  it('marks a call to a test number and no other', async () => {
    installBackend({
      handle: ({ path, method }) =>
        method === 'GET' && path.startsWith(`/campaigns/${CAMPAIGN_ID}/attempts`)
          ? { items: [attempt(1, { isTestNumber: true }), attempt(2, {})], total: 2 }
          : undefined,
    })
    renderWithProviders(<AttemptsTab campaign={campaign} view={{ page: 0 }} onView={() => {}} />)
    const mine = (await screen.findByText('+8613800000001')).closest('tr') as HTMLElement
    const other = screen.getByText('+8613800000002').closest('tr') as HTMLElement
    expect(within(mine).getByText(i18n.t('campaign.testNumber'))).toBeTruthy()
    expect(within(other).queryByText(i18n.t('campaign.testNumber'))).toBeNull()
  })
})

// A line's refusal and a callee's 603 share the cause CALL_REJECTED: the
// outcome names which it was, and the status follows the cause, for the 603
// too. A row the switch gave no status shows the cause alone.
describe('the hangup cause of an attempt', () => {
  it('carries the SIP status beside the cause when there is one', async () => {
    installBackend({
      handle: ({ path, method }) =>
        method === 'GET' && path.startsWith(`/campaigns/${CAMPAIGN_ID}/attempts`)
          ? {
              items: [
                attempt(1, { outcome: 'LINE_REJECTED', hangupCause: 'CALL_REJECTED', sipStatus: 402, isTouched: false }),
                attempt(2, { outcome: 'REJECTED', hangupCause: 'CALL_REJECTED', sipStatus: 603 }),
                attempt(3, { outcome: 'REJECTED', hangupCause: 'CALL_REJECTED' }),
              ],
              total: 3,
            }
          : undefined,
    })
    // These rows link to their call records, and a link needs a router that
    // knows the target.
    vi.stubGlobal('scrollTo', () => {})
    const root = createRootRoute({ component: Outlet })
    const tree = root.addChildren([
      createRoute({
        getParentRoute: () => root, path: '/attempts',
        component: () => <AttemptsTab campaign={campaign} view={{ page: 0 }} onView={() => {}} />,
      }),
      createRoute({ getParentRoute: () => root, path: '/admin/cdr/$callId', component: () => null }),
    ])
    const router = createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: ['/attempts'] }) })
    renderWithProviders(<RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />)
    const refused = await row('+8613800000001')
    expect(within(refused).getByText('Line rejected')).toBeTruthy()
    expect(within(refused).getByText('CALL_REJECTED · 402')).toBeTruthy()
    const declined = await row('+8613800000002')
    expect(within(declined).getByText(i18n.t('campaign.outcomes.REJECTED'))).toBeTruthy()
    expect(within(declined).getByText('CALL_REJECTED · 603')).toBeTruthy()
    const bare = await row('+8613800000003')
    expect(within(bare).getByText('CALL_REJECTED')).toBeTruthy()
    expect(within(bare).queryByText(/·/)).toBeNull()
  })
})
