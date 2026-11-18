import { act, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Campaign, CampaignContact } from '@/lib/campaigns'
import i18n from '@/lib/i18n'
import { installBackend, renderWithProviders } from '@/test/harness'

import { ContactsTab } from './_app.admin.campaigns.$campaignId'

const CAMPAIGN_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const campaign = { id: CAMPAIGN_ID, name: 'Renewal wave 1' } as Campaign

const contact = (n: number, over: Partial<CampaignContact>): CampaignContact => ({
  id: `0199f3a2-7c41-7d22-9b1e-${String(1000 + n).padStart(12, '0')}`,
  campaignId: CAMPAIGN_ID,
  number: `+861380000000${n}`,
  numberRaw: `1380000000${n}`,
  slots: {},
  status: 'ACTIVE',
  attempts: 0,
  lastCallId: null,
  lastAttemptId: null,
  systemFailures: 0,
  createdAt: '2026-10-20T09:12:00Z',
  ...over,
})

const HOLD = '2026-10-31T01:00:00Z'
const NEXT = '2026-10-30T05:30:00Z'

/** The cell text the screen draws for a time, in the active language. */
const shown = (iso: string) =>
  new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  }).format(new Date(iso))

function render() {
  installBackend({
    handle: ({ path, method }) =>
      method === 'GET' && path.startsWith(`/campaigns/${CAMPAIGN_ID}/contacts`)
        ? {
            items: [
              contact(1, { frequencyHoldEndsAt: HOLD, nextAttemptAt: NEXT }),
              contact(2, { nextAttemptAt: NEXT }),
            ],
            total: 2,
          }
        : undefined,
  })
  return renderWithProviders(<ContactsTab campaign={campaign} view={{ page: 0 }} onView={() => {}} />)
}

afterEach(async () => {
  vi.unstubAllGlobals()
  await act(async () => {
    await i18n.changeLanguage('en')
  })
})

describe('the contacts tab next-attempt cell', () => {
  it.each([
    ['en', (when: string) => `held by another campaign's call until ${when}`],
    ['zh', (when: string) => `被其他活动的呼叫占用，至 ${when}`],
  ])('says a held contact is held by another campaign (%s)', async (lang, words) => {
    await act(async () => {
      await i18n.changeLanguage(lang)
    })
    render()
    const held = (await screen.findByText('+8613800000001')).closest('tr') as HTMLElement
    const plain = screen.getByText('+8613800000002').closest('tr') as HTMLElement
    expect(within(held).getByText(words(shown(HOLD)))).toBeTruthy()
    expect(within(held).queryByText(shown(NEXT))).toBeNull()
    // Without a hold the cell shows the next attempt time, as before.
    expect(within(plain).getByText(shown(NEXT))).toBeTruthy()
    expect(within(plain).queryByText(/another campaign|其他活动/)).toBeNull()
  })

  describe('wording', () => {
    it('is the contract’s sentence', () => {
      expect(i18n.t('campaign.frequencyHeld', { when: 'X', lng: 'en' })).toBe("held by another campaign's call until X")
    })
  })
})
