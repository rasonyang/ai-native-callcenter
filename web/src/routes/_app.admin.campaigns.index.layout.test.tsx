import { describe } from 'vitest'

import type { Campaign } from '@/lib/campaigns'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * The campaign list. A campaign's name is bounded by the contract (200
 * characters) and its DID number is not; the state cell is a sentence built
 * from enums, at its longest when RUNNING and waiting for the calling hours.
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function campaign(n: number, overrides: Partial<Campaign> = {}): Campaign {
  return {
    id: id(n),
    name: `October renewal reminders ${n}`,
    didId: id(500 + n),
    didNumber: '95099',
    status: 'DRAFT',
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
    createdAt: '2026-10-20T09:12:00Z',
    contacts: { total: 12000, active: 8300, done: 3100, exhausted: 400, invalidNumber: 120, dnc: 80 },
    ...overrides,
  }
}

/** One of each status, the running one in its wordiest state. */
function statusRows(overrides: Partial<Campaign> = {}): Campaign[] {
  return [
    campaign(1, {
      status: 'RUNNING', activity: 'WAITING_WINDOW', nextDialAt: '2026-10-30T01:00:00Z', inFlight: 5, ...overrides,
    }),
    campaign(2, { status: 'PAUSED', pauseReason: 'PROVIDER_FAILURE', ...overrides }),
    campaign(3, { status: 'COMPLETED', ...overrides }),
    campaign(4, { status: 'DRAFT', contacts: { total: 0, active: 0, done: 0, exhausted: 0, invalidNumber: 0, dnc: 0 }, ...overrides }),
    campaign(5, { status: 'CANCELLED', ...overrides }),
  ]
}

function render(rows: Campaign[], total = rows.length) {
  return renderApp('/admin/campaigns', {
    handle: ({ path, method }) =>
      method === 'GET' && path.startsWith('/campaigns?') ? { items: rows, total } : undefined,
  })
}


restoreLanguage()

describe('campaign list with ordinary campaigns', () => {
  layoutTest('campaigns (ordinary data)', async (ctx) => {
    render(statusRows(), 120)
    await waitForRows(5)
    await expectLayout(ctx)
  })
})

describe('campaign list with names at the contract limit', () => {
  layoutTest('campaigns (200-character names, no spaces)', async (ctx) => {
    render(statusRows({ name: maxLengthOf('CampaignWrite', 'name') }), 120)
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns (200-character names, Chinese)', async (ctx) => {
    render(statusRows({ name: chineseRun(200) }))
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns (mixed names)', async (ctx) => {
    render(statusRows({ name: mixedNote().slice(0, 200) }))
    await waitForRows(5)
    await expectLayout(ctx)
  })
})

describe('campaign list with hostile DID numbers and counters', () => {
  layoutTest('campaigns (40-digit DID numbers)', async (ctx) => {
    render(statusRows({ didNumber: digits(40) }))
    await waitForRows(5)
    await expectLayout(ctx)
  })

  layoutTest('campaigns (unbreakable DID numbers)', async (ctx) => {
    render([
      campaign(1, { didNumber: asciiRun(6000, 1) }),
      campaign(2, { didNumber: longUrl() }),
      campaign(3, { didNumber: chineseRun(3000) }),
    ])
    await waitForRows(3)
    await expectLayout(ctx)
  })

  // Counters in the millions: the "37 active · 120 done ..." sentence grows.
  layoutTest('campaigns (counters in the millions)', async (ctx) => {
    render(statusRows({
      inFlight: 999999,
      contacts: {
        total: 99999999, active: 88888888, done: 77777777, exhausted: 66666666, invalidNumber: 55555555, dnc: 44444444,
      },
    }))
    await waitForRows(5)
    await expectLayout(ctx)
  })
})
