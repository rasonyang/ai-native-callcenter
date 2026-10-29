import { describe } from 'vitest'

import type { CDR } from '@/lib/ledger'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { cellBounds, narrowTables } from '@/test/layout-known-issues'

/**
 * The CDR explorer. Its text cells are the caller's number and the dialled
 * number, which the contract leaves unbounded; the rest are enums and
 * durations, so the journey and status cells are filled to their fullest
 * (every leg kind, a missed reason, both badges) rather than with free text.
 */


const at = '2026-10-20T09:12:00Z'

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function cdr(n: number, overrides: Partial<CDR> = {}): CDR {
  return {
    callId: id(n),
    startedAt: at,
    endedAt: '2026-10-20T09:17:00Z',
    callType: 'INBOUND',
    fromNumber: '+861083550341',
    toNumber: '95001',
    did: '95001',
    ringSec: 3,
    botSec: 40,
    queueWaitSec: 12,
    talkSec: 245,
    billSec: 300,
    totalSec: 300,
    status: 'ANSWERED',
    isContained: false,
    hasRecording: false,
    legs: [{ kind: 'BOT', label: 'novanet_support', durationSec: 40 }],
    ...overrides,
  }
}

/** Every leg kind in one journey, the longest sentence the column can hold. */
const FULL_JOURNEY: CDR['legs'] = [
  { kind: 'TRUNK', label: 'carrier', durationSec: 2 },
  { kind: 'BOT', label: 'novanet_support', durationSec: 40 },
  { kind: 'QUEUE', label: 'support-zh', durationSec: 12 },
  { kind: 'AGENT', label: '1001', durationSec: 245 },
  { kind: 'DIALING', label: '13800000000', durationSec: 5 },
]

/** The finished rows a screen shows when every enum column is at its widest. */
function enumRows(): CDR[] {
  return [
    cdr(1, { legs: FULL_JOURNEY, isContained: true, hasRecording: true }),
    cdr(2, { status: 'NO_ANSWER', missedReason: 'AGENTS_DID_NOT_ANSWER', legs: FULL_JOURNEY, hasRecording: true }),
    cdr(3, { status: 'BUSY', callType: 'OUTBOUND', legs: FULL_JOURNEY }),
    cdr(4, { status: 'FAILED', legs: FULL_JOURNEY, isContained: true, hasRecording: true }),
  ]
}

function render(rows: CDR[], total = rows.length) {
  return renderApp('/admin/cdr', {
    handle: ({ path, method }) =>
      method === 'GET' && path.startsWith('/cdrs?') ? { items: rows, total } : undefined,
  })
}

restoreLanguage()

describe('CDR explorer with ordinary calls', () => {
  // The control: realistic numbers and the journeys real calls take (a bot
  // alone, a bot that hands over to a person), no hostile value.
  layoutTest('cdr (ordinary data)', async (ctx) => {
    render([
      cdr(1),
      cdr(2, {
        legs: [
          { kind: 'TRUNK', label: 'carrier', durationSec: 2 },
          { kind: 'BOT', label: 'novanet_support', durationSec: 40 },
          { kind: 'QUEUE', label: 'support-zh', durationSec: 12 },
          { kind: 'AGENT', label: '1001', durationSec: 245 },
        ],
        hasRecording: true,
      }),
      cdr(3, { status: 'NO_ANSWER', callType: 'OUTBOUND', fromNumber: '1001', did: '95001', legs: [{ kind: 'DIALING', label: '13800000000', durationSec: 30 }] }),
    ])
    await waitForRows(3)
    await expectLayout(ctx)
  })
})

describe('CDR explorer with every enum cell at its widest', () => {
  // All five leg kinds in one journey, a missed reason, both badges: the
  // contract allows each, and the journey and status columns are text.
  layoutTest('cdr (widest enum cells)', async (ctx) => {
    render(enumRows())
    await waitForRows(4)
    await expectLayout(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 && ctx.lang === 'en' })
})

// One column at a time: the other keeps ordinary numbers.
const numberColumns: Array<[name: string, put: (value: string, i: number) => Partial<CDR>]> = [
  ['caller id', (v) => ({ fromNumber: v })],
  ['dialed number', (v) => ({ toNumber: v, did: v })],
]

describe('CDR explorer with 40-digit numbers', () => {
  for (const [name, put] of numberColumns) {
    layoutTest(`cdr (40-digit ${name})`, async (ctx) => {
      render(enumRows().map((row, i) => Object.assign(row, put(digits(40, 10 + i), i))), 500)
      await waitForRows(4)
      await expectLayout(ctx)
    }, { knownIssue: cellBounds, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 || (ctx.viewport.width === 1280 && ctx.lang === 'en') })
  }
})

describe('CDR explorer with numbers that cannot break', () => {
  // A trunk can hand over any string as the caller id; the contract does not
  // bound fromNumber or did.
  for (const [name, put] of numberColumns) {
    layoutTest(`cdr (unbreakable ${name})`, async (ctx) => {
      render([
        cdr(1, put(asciiRun(6000, 1), 1)),
        cdr(2, put(longUrl(), 2)),
        cdr(3, { ...put(mixedNote(), 3), hasRecording: true }),
        cdr(4, { ...put(chineseRun(3000, 9), 4), hasRecording: true }),
      ])
      await waitForRows(4)
      await expectLayout(ctx)
    }, { knownIssue: cellBounds })
  }
})
