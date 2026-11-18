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
