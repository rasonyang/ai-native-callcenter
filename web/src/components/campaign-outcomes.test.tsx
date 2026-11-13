import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Campaign, CampaignOutcomeSummary } from '@/lib/campaigns'
import i18n from '@/lib/i18n'
import { installBackend, renderWithProviders } from '@/test/harness'

import { CampaignOutcomes } from './campaign-outcomes'

/**
 * The campaign's outcome tab: what the server counted, row for row in the
 * contract's order, zeros included, and a column total where one adds up.
 */

const CAMPAIGN_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'

const campaign = { id: CAMPAIGN_ID, name: 'Renewal wave 1' } as Campaign

function summary(): CampaignOutcomeSummary {
  return {
    campaignId: CAMPAIGN_ID,
    attemptsByOutcome: [
      { outcome: 'ANSWERED', count: 320 }, { outcome: 'BOT_FAILED', count: 113 },
      { outcome: 'OVERFLOW', count: 46 }, { outcome: 'NO_ANSWER', count: 925 },
      { outcome: 'BUSY', count: 420 }, { outcome: 'REJECTED', count: 160 },
      { outcome: 'UNKNOWN', count: 45 }, { outcome: 'TRANSIENT', count: 101 },
      { outcome: 'ORIGINATE_FAILED', count: 59 }, { outcome: 'INVALID_NUMBER', count: 30 },
      { outcome: 'DNC_SKIPPED', count: 0 },
    ],
    contactsByLastOutcome: [
      { outcome: 'ANSWERED', count: 317 }, { outcome: 'BOT_FAILED', count: 113 },
      { outcome: 'OVERFLOW', count: 46 }, { outcome: 'NO_ANSWER', count: 140 },
      { outcome: 'BUSY', count: 60 }, { outcome: 'REJECTED', count: 25 },
      { outcome: 'UNKNOWN', count: 6 }, { outcome: 'TRANSIENT', count: 9 },
      { outcome: 'ORIGINATE_FAILED', count: 4 }, { outcome: 'INVALID_NUMBER', count: 30 },
      { outcome: 'DNC_SKIPPED', count: 0 },
    ],
    contactsByLeadIntent: [
      { leadIntent: 'HIGH', count: 30 }, { leadIntent: 'MEDIUM', count: 50 },
      { leadIntent: 'LOW', count: 70 }, { leadIntent: 'NONE', count: 100 },
      { leadIntent: 'DO_NOT_CALL', count: 20 },
    ],
    optOuts: { saidInConversation: 20, alreadyListed: 0 },
    callbacksByStatus: [
      { status: 'OPEN', count: 12 }, { status: 'CLAIMED', count: 8 },
      { status: 'DONE', count: 6 }, { status: 'DISMISSED', count: 4 },
    ],
  }
}

afterEach(() => vi.unstubAllGlobals())

function render(reply: () => unknown) {
  const api = installBackend({
    handle: ({ path, method }) =>
      method === 'GET' && path === `/campaigns/${CAMPAIGN_ID}/outcome-summary` ? reply() : undefined,
  })
  return { api, ...renderWithProviders(<CampaignOutcomes campaign={campaign} />) }
}

/** The cells of the row whose first cell reads label, in the table under heading. */
function row(heading: string, label: string): string[] {
  const section = screen.getByRole('heading', { name: heading }).closest('section')
  if (!section) throw new Error(`no section ${heading}`)
  const cell = within(section).getByText(label, { selector: 'td' })
  return [...(cell.closest('tr')?.querySelectorAll('td') ?? [])].map((td) => td.textContent ?? '')
}

describe('the outcome tab of a campaign', () => {
  it('asks the server for that campaign’s summary and nothing else', async () => {
    const { api } = render(summary)
    await screen.findByRole('heading', { name: i18n.t('campaign.summary.byOutcome') })
    expect(api.requests.map((r) => r.path)).toContain(`/campaigns/${CAMPAIGN_ID}/outcome-summary`)
    expect(api.requests.filter((r) => r.path.startsWith('/campaigns/'))).toHaveLength(1)
  })

  it('shows attempts and contacts side by side for every outcome, zeros too, with the totals', async () => {
    render(summary)
    const heading = i18n.t('campaign.summary.byOutcome')
    await screen.findByRole('heading', { name: heading })
    expect(row(heading, i18n.t('campaign.outcomes.ANSWERED'))).toEqual(['Answered', '320', '317'])
    expect(row(heading, i18n.t('campaign.outcomes.BOT_FAILED'))).toEqual(['Bot failed', '113', '113'])
    expect(row(heading, i18n.t('campaign.outcomes.DNC_SKIPPED'))).toEqual(['Skipped (do not call)', '0', '0'])
    expect(row(heading, i18n.t('campaign.summary.total'))).toEqual(['Total', '2219', '750'])
    // Eleven outcomes and the total, in the contract's order.
    const section = screen.getByRole('heading', { name: heading }).closest('section') as HTMLElement
    const firstCells = within(section).getAllByRole('row').slice(1).map((r) => r.querySelector('td')?.textContent)
    expect(firstCells).toEqual([
      'Answered', 'Bot failed', 'Dropped at capacity', 'No answer', 'Busy', 'Rejected', 'Unknown',
      'Network error', 'Could not place', 'Invalid number', 'Skipped (do not call)', 'Total',
    ])
  })

  it('shows the leads, the two kinds of opt-out and where the callbacks stand', async () => {
    render(summary)
    await screen.findByRole('heading', { name: i18n.t('campaign.summary.byLead') })
    const leads = i18n.t('campaign.summary.byLead')
    expect(row(leads, i18n.t('campaign.intents.HIGH'))).toEqual(['High', '30'])
    expect(row(leads, i18n.t('campaign.intents.DO_NOT_CALL'))).toEqual(['Do not call', '20'])

    const optOuts = i18n.t('campaign.summary.optOuts')
    expect(row(optOuts, i18n.t('campaign.summary.saidInConversation'))).toEqual(['Asked on the call', '20'])
    expect(row(optOuts, i18n.t('campaign.summary.alreadyListed'))).toEqual(['Already on the list, skipped', '0'])
    expect(screen.getByText(i18n.t('campaign.summary.optOutsNote'))).toBeInTheDocument()

    const callbacks = i18n.t('campaign.summary.callbacks')
    expect(row(callbacks, i18n.t('callbacks.statuses.OPEN'))).toEqual(['Open', '12'])
    expect(row(callbacks, i18n.t('callbacks.statuses.CLAIMED'))).toEqual(['Claimed', '8'])
    expect(row(callbacks, i18n.t('callbacks.statuses.DONE'))).toEqual(['Done', '6'])
    expect(row(callbacks, i18n.t('callbacks.statuses.DISMISSED'))).toEqual(['Dismissed', '4'])
    expect(row(callbacks, i18n.t('campaign.summary.total'))).toEqual(['Total', '30'])
  })

  it('says why when the summary cannot be read', async () => {
    render(() => new Response(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'no such campaign' } }),
      { status: 404, headers: { 'content-type': 'application/json' } }))
    expect(await screen.findByText(i18n.t('errors.NOT_FOUND'))).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })
})

describe('the outcome strings', () => {
  it('exist in both languages', () => {
    for (const lng of ['en', 'zh']) {
      const t = i18n.getFixedT(lng)
      for (const key of [
        'campaign.tabs.outcomes', 'campaign.summary.byOutcome', 'campaign.summary.attempts',
        'campaign.summary.contactsLastOutcome', 'campaign.summary.byLead', 'campaign.summary.contacts',
        'campaign.summary.optOuts', 'campaign.summary.reason', 'campaign.summary.saidInConversation',
        'campaign.summary.alreadyListed', 'campaign.summary.optOutsNote', 'campaign.summary.callbacks',
        'campaign.summary.callbackStatus', 'campaign.summary.callbackCount', 'campaign.summary.total',
      ]) {
        expect(i18n.exists(key, { lng, fallbackLng: false }), `${lng} ${key}`).toBe(true)
        expect(t(key)).not.toBe(key)
      }
    }
  })
})
