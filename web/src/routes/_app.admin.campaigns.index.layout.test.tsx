import { describe } from 'vitest'

import {
  CAMPAIGN_STATUSES, type Campaign, type CampaignActivity, type CampaignPauseReason,
} from '@/lib/campaigns'
import i18n from '@/lib/i18n'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote, words } from '@/test/hostile'
import {
  expectLayout, layoutTest, LayoutViolation, restoreLanguage, type LayoutContext,
} from '@/test/layout'

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

/** Each activity of a running campaign, and whether it reads with a time. */
const ACTIVITIES: Record<CampaignActivity, boolean> = {
  DIALING: false,
  WAITING_WINDOW: true,
  WAITING_RETRY: true,
  WAITING_FREQUENCY: true,
  WAITING_DAILY_CAP: true,
  AT_CAPACITY: false,
}

const PAUSE_REASONS: Record<CampaignPauseReason, true> = {
  MANUAL: true,
  TRUNK_FAILURE: true,
  PROVIDER_FAILURE: true,
  OUTBOUND_NOT_CONFIGURED: true,
}

/**
 * A row for every sentence the State column can read: each activity (a waiting
 * one with its time, in a zone other than the browser's, where the time also
 * carries the zone's name) and each pause reason.
 */
function everyStateRows(overrides: Partial<Campaign> = {}): Campaign[] {
  const activities = Object.entries(ACTIVITIES) as Array<[CampaignActivity, boolean]>
  const reasons = Object.keys(PAUSE_REASONS) as CampaignPauseReason[]
  return [
    ...activities.map(([activity, hasTime], i) => campaign(i + 1, {
      status: 'RUNNING',
      activity,
      nextDialAt: hasTime ? '2026-10-30T01:00:00Z' : undefined,
      timeZone: 'America/Los_Angeles',
      inFlight: 5,
      ...overrides,
    })),
    ...reasons.map((pauseReason, i) => campaign(activities.length + i + 1, {
      status: 'PAUSED', pauseReason, ...overrides,
    })),
  ]
}

const MILLIONS = {
  total: 99999999, active: 88888888, done: 77777777, exhausted: 66666666, invalidNumber: 55555555, dnc: 44444444,
}

/**
 * The status word ("Running", 「运行中」) is one line high in every row: the
 * activity or pause text after it is what wraps (#648). A Chinese word has a
 * break between every two characters, so a squeezed one stacks.
 */
function expectStatusWordOnOneLine(ctx: LayoutContext) {
  const statusWords = new Set(CAMPAIGN_STATUSES.map((s) => i18n.t(`campaign.statuses.${s}`)))
  const problems: string[] = []
  const rows = [...document.querySelectorAll('tbody tr')]
  if (rows.length === 0) problems.push('  no table rows to measure')
  for (const [r, tr] of rows.entries()) {
    const cell = tr.querySelectorAll('td')[1]
    const word = [...cell.querySelectorAll('span')]
      .find((span) => span.childElementCount === 0 && statusWords.has(span.textContent ?? ''))
    if (!word) {
      problems.push(`  row ${r + 1}: no status word in "${cell.textContent}"`)
      continue
    }
    const height = word.getBoundingClientRect().height
    const line = Number.parseFloat(getComputedStyle(word).lineHeight)
    if (height > line + 1) {
      problems.push(`  row ${r + 1}: "${word.textContent}" is ${height}px high, a line is ${line}px `
        + `(width ${word.getBoundingClientRect().width}px), in "${cell.textContent}"`)
    }
  }
  if (problems.length > 0) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]: the status word wraps:\n${problems.join('\n')}`,
    )
  }
}

/**
 * #650: a header label and a Created date are each one line, however narrow
 * their columns are squeezed — Chinese breaks between any two characters, so
 * the check has to measure, not read. Line tops are counted on the text nodes
 * themselves, the same way expectOneLine does for body cells.
 */
function textLineCount(root: Element) {
  const tops = new Set<number>()
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (!node.textContent?.trim()) continue
    const range = document.createRange()
    range.selectNodeContents(node)
    for (const rect of range.getClientRects()) if (rect.width > 0) tops.add(Math.round(rect.top))
  }
  return tops.size
}

function expectHeadersAndDatesOnOneLine(ctx: LayoutContext) {
  const problems: string[] = []
  const headers = [...document.querySelectorAll('thead th')]
  if (headers.length === 0) problems.push('  no table headers to measure')
  for (const [i, th] of headers.entries()) {
    const lines = textLineCount(th)
    if (lines > 1) problems.push(`  header ${i + 1} "${th.textContent}" wraps onto ${lines} lines`)
  }
  for (const [r, tr] of [...document.querySelectorAll('tbody tr')].entries()) {
    const cell = tr.querySelectorAll('td')[5]
    if (!cell) continue
    const lines = textLineCount(cell)
    if (lines > 1) problems.push(`  row ${r + 1}: the date "${cell.textContent}" wraps onto ${lines} lines`)
  }
  if (problems.length > 0) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]: a label or a date wraps:\n${problems.join('\n')}`,
    )
  }
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
    expectHeadersAndDatesOnOneLine(ctx)
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
    render(statusRows({ inFlight: 999999, contacts: MILLIONS }))
    await waitForRows(5)
    await expectLayout(ctx)
  })
})

// The status word stays one line however much the activity or pause text after
// it wraps (#648): every activity and pause reason, beside long names and
// counters in the millions, which leave the State column least room.
describe('campaign list in every state', () => {
  const names: Array<[string, string]> = [
    ['ordinary names', 'October renewal reminders'],
    ['200-character names, English', words(200)],
    ['200-character names, no spaces', maxLengthOf('CampaignWrite', 'name')],
    ['200-character names, Chinese', chineseRun(200)],
  ]
  for (const [kind, name] of names) {
    layoutTest(`campaigns (every activity and pause reason, ${kind})`, async (ctx) => {
      const rows = everyStateRows({ name, inFlight: 999999, contacts: MILLIONS })
      render(rows)
      await waitForRows(rows.length)
      await expectLayout(ctx)
      expectStatusWordOnOneLine(ctx)
      expectHeadersAndDatesOnOneLine(ctx)
    })
  }
})
