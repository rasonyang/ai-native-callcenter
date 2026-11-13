import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import {
  CAMPAIGN_OUTCOMES, LEAD_INTENTS,
  type Campaign, type CampaignAttempt, type CampaignContact, type CampaignOutcomeSummary,
} from '@/lib/campaigns'
import i18n from '@/lib/i18n'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * One campaign: header, counters, and the contacts, leads, attempts and
 * outcomes tabs (the settings tab is a form, covered with the other forms). The name is
 * bounded by the contract; the DID number, the time zone, a contact's number,
 * the lead notes and an attempt's hangup cause are not.
 */

const CAMPAIGN_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const at = '2026-10-20T09:12:00Z'

function campaign(overrides: Partial<Campaign> = {}): Campaign {
  return {
    id: CAMPAIGN_ID,
    name: 'October renewal reminders',
    didId: id(500),
    didNumber: '95099',
    status: 'RUNNING',
    activity: 'WAITING_WINDOW',
    nextDialAt: '2026-10-30T01:00:00Z',
    inFlight: 5,
    timeZone: 'Asia/Shanghai',
    callingWindow: { days: ['MON', 'TUE', 'WED', 'THU', 'FRI'], ranges: [{ start: '09:00', end: '18:00' }] },
    maxConcurrent: 5,
    ringTimeoutSec: 30,
    retryPolicy: {
      maxAttempts: 3,
      maxSystemRetries: 5,
      retryOutcomes: ['NO_ANSWER', 'BUSY'],
      intervalSec: { NO_ANSWER: 7200, BUSY: 1800 },
      transientBackoff: { baseSec: 300, factor: 3, capSec: 14400 },
    },
    createdAt: at,
    contacts: { total: 12000, active: 8300, done: 3100, exhausted: 400, invalidNumber: 120, dnc: 80 },
    ...overrides,
  }
}

function contact(n: number, overrides: Partial<CampaignContact> = {}): CampaignContact {
  return {
    id: id(1000 + n),
    campaignId: CAMPAIGN_ID,
    number: '+8613800000000',
    numberRaw: '13800000000',
    slots: { name: 'Zhang Wei' },
    status: 'ACTIVE',
    attempts: 2,
    systemFailures: 1,
    nextAttemptAt: '2026-10-30T01:00:00Z',
    lastOutcome: 'NO_ANSWER',
    leadIntent: 'MEDIUM',
    leadNotes: 'Asked to be called after the holiday.',
    lastCallId: id(2000 + n),
    lastAttemptId: id(3000 + n),
    createdAt: at,
    ...overrides,
  }
}

/** Every status and intent the contacts and leads tables can show. */
function contacts(overrides: Partial<CampaignContact> = {}): CampaignContact[] {
  return [
    contact(1, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', ...overrides }),
    contact(2, { status: 'DONE', leadIntent: 'LOW', lastOutcome: 'ANSWERED', ...overrides }),
    contact(3, { status: 'EXHAUSTED', leadIntent: 'NONE', lastOutcome: 'BUSY', ...overrides }),
    contact(4, { status: 'INVALID_NUMBER', leadIntent: undefined, lastOutcome: 'INVALID_NUMBER', ...overrides }),
    contact(5, { status: 'DNC', leadIntent: 'DO_NOT_CALL', lastOutcome: 'DNC_SKIPPED', ...overrides }),
  ]
}

function attempt(n: number, overrides: Partial<CampaignAttempt> = {}): CampaignAttempt {
  return {
    id: id(4000 + n),
    campaignId: CAMPAIGN_ID,
    contactId: id(1000 + n),
    number: '+8613800000000',
    callId: id(2000 + n),
    legId: id(5000 + n),
    state: 'FINISHED',
    outcome: 'ANSWERED',
    hangupCause: 'NORMAL_CLEARING',
    talkSec: 245,
    isTouched: true,
    startedAt: at,
    finishedAt: '2026-10-20T09:17:00Z',
    ...overrides,
  }
}

function attempts(overrides: Partial<CampaignAttempt> = {}): CampaignAttempt[] {
  return [
    attempt(1, overrides),
    attempt(2, { outcome: 'NO_ANSWER', hangupCause: 'NO_ANSWER', talkSec: undefined, ...overrides }),
    attempt(3, { outcome: 'ORIGINATE_FAILED', hangupCause: 'USER_NOT_REGISTERED', ...overrides }),
    attempt(4, { state: 'DIALING', outcome: undefined, hangupCause: undefined, talkSec: undefined, finishedAt: undefined, ...overrides }),
    attempt(5, { outcome: 'DNC_SKIPPED', ...overrides }),
  ]
}

/** Every outcome, intent and callback status with the count count(i) gives the i-th. */
function outcomeSummary(count: (i: number) => number): CampaignOutcomeSummary {
  return {
    campaignId: CAMPAIGN_ID,
    attemptsByOutcome: CAMPAIGN_OUTCOMES.map((outcome, i) => ({ outcome, count: count(i) })),
    contactsByLastOutcome: CAMPAIGN_OUTCOMES.map((outcome, i) => ({ outcome, count: count(i + 11) })),
    contactsByLeadIntent: LEAD_INTENTS.map((leadIntent, i) => ({ leadIntent, count: count(i + 22) })),
    optOuts: { saidInConversation: count(27), alreadyListed: count(28) },
    callbacksByStatus: (['OPEN', 'CLAIMED', 'DONE', 'DISMISSED'] as const).map((status, i) => ({
      status, count: count(i + 29),
    })),
  }
}

/** A campaign a few days in: 12,000 contacts, about 20,000 attempts. */
const ordinarySummary = outcomeSummary((i) => [
  2900, 610, 240, 9800, 3100, 820, 140, 600, 310, 120, 80,
  2850, 600, 240, 2300, 700, 210, 40, 30, 15, 120, 80,
  95, 180, 260, 410, 60, 60, 80, 40, 25, 20, 10,
][i])

function render(
  data: {
    campaign?: Campaign; contacts?: CampaignContact[]; attempts?: CampaignAttempt[]
    summary?: CampaignOutcomeSummary
  } = {},
) {
  const detail = data.campaign ?? campaign()
  const contactRows = data.contacts ?? contacts()
  const attemptRows = data.attempts ?? attempts()
  const summaryReply = data.summary ?? ordinarySummary
  return renderApp(`/admin/campaigns/${CAMPAIGN_ID}`, {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === `/campaigns/${CAMPAIGN_ID}`) return detail
      if (path.startsWith(`/campaigns/${CAMPAIGN_ID}/contacts`)) {
        // The leads tab asks for one intent at a time.
        const intent = new URLSearchParams(path.split('?')[1]).get('leadIntent')
        const items = intent ? contactRows.filter((row) => row.leadIntent === intent) : contactRows
        return { items, total: items.length === contactRows.length ? 120 : items.length }
      }
      if (path.startsWith(`/campaigns/${CAMPAIGN_ID}/attempts`)) return { items: attemptRows, total: 120 }
      if (path === `/campaigns/${CAMPAIGN_ID}/outcome-summary`) return summaryReply
      return undefined
    },
  })
}

type User = ReturnType<typeof render>['user']

async function openTab(user: User, key: string) {
  await user.click(await screen.findByRole('tab', { name: i18n.t(key) }))
}

/** The page has drawn the campaign (its tabs are there). */
async function drawn() {
  await screen.findByRole('tab', { name: i18n.t('campaign.tabs.contacts') })
}


restoreLanguage()

describe('campaign detail header', () => {
  // The control: a running campaign in its wordiest state, every button.
  layoutTest('campaigns/$campaignId header (ordinary data)', async (ctx) => {
    render()
    await drawn()
    await waitForRows(5)
    await expectLayout(ctx)
  })

  // A breaker pause adds the notice with its own Resume button, beside the
  // header's Resume, Cancel and Import.
  layoutTest('campaigns/$campaignId header (breaker pause)', async (ctx) => {
    render({ campaign: campaign({ status: 'PAUSED', pauseReason: 'OUTBOUND_NOT_CONFIGURED', activity: undefined }) })
    await drawn()
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (200-character name, no spaces)', async (ctx) => {
    render({ campaign: campaign({ name: maxLengthOf('CampaignWrite', 'name') }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (200-character name, Chinese)', async (ctx) => {
    render({ campaign: campaign({ name: chineseRun(200) }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (mixed name)', async (ctx) => {
    render({ campaign: campaign({ name: mixedNote().slice(0, 200) }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (40-digit DID number)', async (ctx) => {
    render({ campaign: campaign({ didNumber: digits(40) }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (longest time zone)', async (ctx) => {
    render({ campaign: campaign({ timeZone: 'America/Argentina/ComodRivadavia' }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId header (unbreakable DID number)', async (ctx) => {
    render({ campaign: campaign({ didNumber: asciiRun(6000, 1) }) })
    await drawn()
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId counters in the millions', async (ctx) => {
    render({
      campaign: campaign({
        inFlight: 99999999,
        contacts: {
          total: 99999999, active: 88888888, done: 77777777, exhausted: 66666666, invalidNumber: 55555555, dnc: 44444444,
        },
      }),
    })
    await drawn()
    await expectLayout(ctx)
  })
})

describe('campaign detail, contacts tab', () => {
  layoutTest('campaigns/$campaignId contacts (ordinary data)', async (ctx) => {
    render()
    await drawn()
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId contacts (40-digit numbers)', async (ctx) => {
    render({ contacts: contacts({ number: digits(40, 3), numberRaw: digits(40, 4) }) })
    await drawn()
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId contacts (unbreakable numbers)', async (ctx) => {
    render({
      contacts: [
        contact(1, { number: asciiRun(6000, 5) }),
        contact(2, { number: longUrl() }),
        contact(3, { number: chineseRun(3000) }),
      ],
    })
    await drawn()
    await waitForRows(3)
    await expectLayout(ctx)
  })
})

describe('campaign detail, attempts tab', () => {
  layoutTest('campaigns/$campaignId attempts (ordinary data)', async (ctx) => {
    const { user } = render()
    await drawn()
    await openTab(user, 'campaign.tabs.attempts')
    await screen.findAllByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId attempts (40-digit numbers)', async (ctx) => {
    const { user } = render({ attempts: attempts({ number: digits(40, 6) }) })
    await drawn()
    await openTab(user, 'campaign.tabs.attempts')
    await screen.findAllByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })

  // The hangup cause is the switch's token on a real call; on an attempt the
  // contract leaves it a free string, and "Open call" must stay reachable.
  // One column at a time, the other keeps its ordinary value.
  for (const [column, put] of [
    ['number', (v: string) => ({ number: v })],
    ['hangup cause', (v: string) => ({ hangupCause: v })],
  ] as const) {
    layoutTest(`campaigns/$campaignId attempts (unbreakable ${column})`, async (ctx) => {
      const { user } = render({
        attempts: [attempt(1, put(asciiRun(6000, 7))), attempt(2, put(longUrl())), attempt(3, put(chineseRun(3000)))],
      })
      await drawn()
      await openTab(user, 'campaign.tabs.attempts')
      await screen.findAllByText(i18n.t('campaign.openCall'))
      await expectLayout(ctx)
    })
  }
})

describe('campaign detail, leads tab', () => {
  layoutTest('campaigns/$campaignId leads (ordinary data)', async (ctx) => {
    const { user } = render()
    await drawn()
    await openTab(user, 'campaign.tabs.leads')
    await screen.findByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })

  // Lead notes are written by the bot: a sentence in ordinary use, anything
  // in a hostile flow. The cell truncates them by design.
  layoutTest('campaigns/$campaignId leads (long notes)', async (ctx) => {
    const { user } = render({
      contacts: contacts({ leadIntent: 'HIGH', leadNotes: mixedNote().repeat(8), lastOutcome: 'ANSWERED' }),
    })
    await drawn()
    await openTab(user, 'campaign.tabs.leads')
    await screen.findAllByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })

  // One column at a time. The notes cell truncates, so only the number can
  // size the table.
  layoutTest('campaigns/$campaignId leads (unbreakable notes)', async (ctx) => {
    const { user } = render({
      contacts: [
        contact(1, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', leadNotes: asciiRun(6000, 9) }),
        contact(2, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', leadNotes: chineseRun(3000) }),
        contact(3, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', leadNotes: longUrl() }),
      ],
    })
    await drawn()
    await openTab(user, 'campaign.tabs.leads')
    await screen.findAllByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })

  layoutTest('campaigns/$campaignId leads (unbreakable numbers)', async (ctx) => {
    const { user } = render({
      contacts: [
        contact(1, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', number: digits(40, 10) }),
        contact(2, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', number: asciiRun(6000, 11) }),
        contact(3, { leadIntent: 'HIGH', lastOutcome: 'ANSWERED', number: longUrl() }),
      ],
    })
    await drawn()
    await openTab(user, 'campaign.tabs.leads')
    await screen.findAllByText(i18n.t('campaign.openCall'))
    await expectLayout(ctx)
  })
})

describe('campaign detail, outcomes tab', () => {
  layoutTest('campaigns/$campaignId outcomes (ordinary data)', async (ctx) => {
    const { user } = render()
    await drawn()
    await openTab(user, 'campaign.tabs.outcomes')
    await screen.findByText(i18n.t('campaign.summary.optOutsNote'))
    await expectLayout(ctx)
  })

  // The largest count the contract's integer carries, in every row: the
  // column totals are eleven of them, an eleven-digit number.
  layoutTest('campaigns/$campaignId outcomes (largest counts)', async (ctx) => {
    const { user } = render({ summary: outcomeSummary(() => 2147483647) })
    await drawn()
    await openTab(user, 'campaign.tabs.outcomes')
    await screen.findAllByText('23622320117')
    await expectLayout(ctx)
  })
})
