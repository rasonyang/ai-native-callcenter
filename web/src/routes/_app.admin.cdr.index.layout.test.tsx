import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import i18n from '@/lib/i18n'
import type { CDR } from '@/lib/ledger'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, mixedNote } from '@/test/hostile'
import {
  LayoutViolation, expectLayout, expectNumbersRead, expectOneLine, layoutTest, restoreLanguage,
  type LayoutContext,
} from '@/test/layout'

/**
 * The CDR explorer. Its text cells are the caller's number and the dialled
 * number, which the contract leaves unbounded; the rest are enums and
 * durations, so the journey and status cells are filled to their fullest
 * (every leg kind, a missed reason, both badges) rather than with free text.
 *
 * The start column is a full date with the year and the time, in the
 * deployment's zone, beside two durations (the whole call and the billed
 * part); the filter bar above the table carries a date range, the number box
 * of either end (which also takes a pasted call id), the caller's and the
 * called number, the status and a billed range, and wraps rather than
 * widening the page.
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

/** A call the platform placed: it went out from the DID, to the customer. */
function aiOutbound(n: number, overrides: Partial<CDR> = {}): CDR {
  return cdr(n, {
    callType: 'OUTBOUND',
    fromNumber: '100200',
    toNumber: '13800000000',
    did: '100200',
    legs: [{ kind: 'BOT', label: 'collections', durationSec: 40 }],
    ...overrides,
  })
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
    aiOutbound(5, { legs: FULL_JOURNEY }),
  ]
}

function render(rows: CDR[], total = rows.length) {
  return renderApp('/admin/cdr', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path.startsWith('/cdrs?')) {
        return {
          items: rows,
          total,
          // Over the whole filtered set: a month of calls bills thousands of hours.
          billSecTotal: 3_600_000 + rows.reduce((sum, row) => sum + row.billSec, 0),
          timeZone: 'Asia/Shanghai',
        }
      }
      // One call by id, as a pasted id asks for it: none of these is on file.
      if (path.startsWith('/cdrs/')) {
        return new Response(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'cdr not found' } }), {
          status: 404,
          headers: { 'content-type': 'application/json' },
        })
      }
      return undefined
    },
  })
}

/** The duration and billed columns (by position; the journey cell is there, hidden, below 1280). */
const DURATION_COLUMNS = [5, 6]

/**
 * Each call's start reads whole: where the card is narrow its time may go
 * under its date, but neither the date nor the time is broken.
 */
function expectStartsWhole(ctx: LayoutContext) {
  const halves = [...document.querySelectorAll('tbody tr td:first-child a > span')]
  if (halves.length === 0) throw new LayoutViolation(`${ctx.screen}: no start to measure`)
  for (const half of halves) {
    const lines = new Set([...half.getClientRects()].map((rect) => Math.round(rect.top)))
    if (lines.size > 1) {
      throw new LayoutViolation(
        `${ctx.screen} @ ${ctx.viewport.width} [${ctx.lang}]: "${half.textContent}" breaks onto ${lines.size} lines`,
      )
    }
  }
}

/** The start, the duration and the billed duration each read in full. */
function expectTimesRead(ctx: LayoutContext) {
  expectOneLine(ctx, DURATION_COLUMNS)
  expectStartsWhole(ctx)
}

/** Types into the control labelled by that translation key, in the language under test. */
function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(i18n.t(label)), { target: { value } })
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
      aiOutbound(4),
    ])
    await waitForRows(4)
    await expectLayout(ctx)
    expectTimesRead(ctx)
    expectNumbersRead(ctx, 1)
    expectNumbersRead(ctx, 2)
  })
})

describe('CDR explorer with every filter in use', () => {
  // A date range (which enables the export and hides its hint), a number of
  // either end, the caller's and the called number, a status and both billed
  // bounds: the filter bar at its fullest.
  layoutTest('cdr (filters in use)', async (ctx) => {
    render(enumRows(), 500)
    await waitForRows(5)
    // The days can be picked once the first answer has named the zone.
    await waitFor(() => expect(screen.getByLabelText(i18n.t('cdr.firstDay'))).toBeEnabled())
    fill('cdr.firstDay', '2026-12-01')
    fill('cdr.lastDay', '2027-01-31')
    fill('cdr.filterNumber', '13800000000')
    fill('cdr.filterFrom', '02155550101')
    fill('cdr.filterTo', '13800000001')
    fill('cdr.billedMin', '60')
    fill('cdr.billedMax', '3600')
    expect(screen.getByRole('button', { name: i18n.t('cdr.export.start') })).toBeEnabled()
    await waitForRows(5)
    await expectLayout(ctx)
    expectTimesRead(ctx)
    expectNumbersRead(ctx, 1)
    expectNumbersRead(ctx, 2)
  })
})

describe('CDR explorer with every enum cell at its widest', () => {
  // All five leg kinds in one journey, a missed reason, both badges: the
  // contract allows each, and the journey and status columns are text.
  layoutTest('cdr (widest enum cells)', async (ctx) => {
    render(enumRows())
    await waitForRows(5)
    await expectLayout(ctx)
    expectTimesRead(ctx)
  })
})

describe('CDR explorer with the copy-id button on every row', () => {
  // The id has no column (#714): each row carries an icon button beside the
  // play button, with a recording and without one, and the widest enum cells
  // leave it the least room.
  layoutTest('cdr (copy-id buttons)', async (ctx) => {
    render(enumRows())
    await waitForRows(5)
    expect(screen.getAllByRole('button', { name: i18n.t('agent.copyCallId') })).toHaveLength(5)
    await expectLayout(ctx)
  })

  // A pasted id that names no call says so above the table, which stays.
  layoutTest('cdr (a pasted id no call has)', async (ctx) => {
    render(enumRows())
    await waitForRows(5)
    fireEvent.change(screen.getByPlaceholderText(i18n.t('cdr.filterNumber')), {
      target: { value: '019ac2e4-7b10-7c3e-9a51-0f6d2b8e4aff' },
    })
    await screen.findByRole('alert')
    await waitForRows(5)
    await expectLayout(ctx)
  })
})

// One column at a time: the other keeps ordinary numbers.
const numberColumns: Array<[name: string, put: (value: string, i: number) => Partial<CDR>]> = [
  ['caller id', (v) => ({ fromNumber: v })],
  ['called number', (v) => ({ toNumber: v, did: v })],
]

describe('CDR explorer with 40-digit numbers', () => {
  for (const [name, put] of numberColumns) {
    layoutTest(`cdr (40-digit ${name})`, async (ctx) => {
      render(enumRows().map((row, i) => Object.assign(row, put(digits(40, 10 + i), i))), 500)
      await waitForRows(5)
      await expectLayout(ctx)
    })
  }
})

describe('CDR explorer with numbers that cannot break', () => {
  // A trunk can hand over any string as the caller id; the contract does not
  // bound fromNumber or toNumber.
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
    })
  }
})

describe('CDR explorer on a call the platform placed', () => {
  // Stored as from = did = the line it went out from, to = the customer: the
  // customer's number is the To column, and the called-number filter finds it.
  it('shows the customer under To and filters by the called number', async () => {
    const { backend } = render([aiOutbound(1)])
    await waitForRows(1)

    const row = screen.getAllByRole('row')[1]
    const cells = within(row).getAllByRole('cell')
    expect(cells[1]).toHaveTextContent('100200')
    expect(cells[2]).toHaveTextContent('13800000000')

    fireEvent.change(screen.getByPlaceholderText('To number…'), { target: { value: '13800' } })
    await waitFor(() =>
      expect(backend.requests.some((r) => r.path.startsWith('/cdrs?') && r.path.includes('toNumber=13800'))).toBe(true),
    )
    expect(backend.requests.some((r) => r.path.includes('fromNumber='))).toBe(false)
  })

  // The number box matches either end, so it finds the customer too.
  it('finds the customer by the number box of either end', async () => {
    const { backend } = render([aiOutbound(1)])
    await waitForRows(1)

    fireEvent.change(screen.getByPlaceholderText('Number…'), { target: { value: '13800' } })
    const listed = () =>
      backend.requests
        .filter((r) => r.path.startsWith('/cdrs?'))
        .map((r) => new URLSearchParams(r.path.split('?')[1]))
    await waitFor(() => expect(listed().at(-1)?.get('number')).toBe('13800'))
    expect(listed().some((q) => q.has('fromNumber') || q.has('toNumber'))).toBe(false)
  })
})
