import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { Callback } from '@/lib/ledger'
import i18n from '@/lib/i18n'
import { renderApp, waitForRows } from '@/test/app'
import { AGENT_ID } from '@/test/harness'
import { asciiRun, chineseRun, digits, longUrl, mixedNote, words } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import {
  at1024, cellBounds, knownBy, truncateFloor,
} from '@/test/layout-known-issues'

/**
 * The callbacks queue. A callback is made by the bot from what the caller
 * said, so its message and the number it recorded are the caller's words: the
 * contract bounds neither. The row's actions (claim, call back, done, dismiss,
 * release) must stay on screen whatever those hold.
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const OTHER_AGENT = '0199f3a2-7c41-7d22-9b1e-0000000000ff'
const at = '2026-10-20T09:12:00Z'

function callback(n: number, overrides: Partial<Callback> = {}): Callback {
  return {
    id: id(n),
    callId: id(100 + n),
    phoneNumber: '+8613800000000',
    message: 'Please call me back about the October invoice.',
    status: 'OPEN',
    createdAt: at,
    ...overrides,
  }
}

/** One of each state a row can be in, so every action set is drawn. */
function oneOfEach(overrides: Partial<Callback> = {}): Callback[] {
  return [
    callback(1, { ...overrides }),
    callback(2, { status: 'CLAIMED', handledBy: AGENT_ID, handledAt: at, lastAttemptAt: at, lastAttemptStatus: 'NO_ANSWER', ...overrides }),
    callback(3, { status: 'CLAIMED', handledBy: OTHER_AGENT, handledAt: at, ...overrides }),
    callback(4, { status: 'DONE', handledBy: AGENT_ID, lastAttemptAt: at, lastAttemptStatus: 'ANSWERED', ...overrides }),
    callback(5, { status: 'DISMISSED', handledBy: AGENT_ID, ...overrides }),
  ]
}

function render(rows: Callback[]) {
  return renderApp('/agent/callbacks', {
    handle: ({ path, method }) => (method === 'GET' && path.startsWith('/callbacks') ? { items: rows } : undefined),
  }, { role: 'AGENT' })
}

/** Rows are drawn, and the signed-in agent's own claimed row shows its buttons. */
async function loaded(count: number) {
  await waitForRows(count)
  await screen.findAllByRole('button', { name: i18n.t('callbacks.markDone') })
}

restoreLanguage()

describe('callbacks with ordinary requests', () => {
  layoutTest('agent/callbacks (ordinary data)', async (ctx) => {
    render(oneOfEach())
    await loaded(5)
    await expectLayout(ctx)
  }, { knownIssue: knownBy([at1024, truncateFloor]) })
})

describe('callbacks with hostile messages', () => {
  // The message is cut by the cell (max-w-md, truncate inside a span).
  layoutTest('agent/callbacks (long messages)', async (ctx) => {
    render([
      callback(1, { message: asciiRun(6000, 1) }),
      callback(2, { message: chineseRun(3000, 2), status: 'CLAIMED', handledBy: AGENT_ID }),
      callback(3, { message: longUrl() }),
      callback(4, { message: mixedNote().repeat(20), status: 'CLAIMED', handledBy: AGENT_ID }),
      callback(5, { message: words(6000, 3), status: 'CLAIMED', handledBy: AGENT_ID }),
    ])
    await loaded(5)
    await expectLayout(ctx)
  }, { knownIssue: knownBy([at1024, truncateFloor]) })
})

describe('callbacks with hostile numbers', () => {
  layoutTest('agent/callbacks (40-digit numbers)', async (ctx) => {
    render(oneOfEach({ phoneNumber: digits(40, 1) }))
    await loaded(5)
    await expectLayout(ctx)
  }, {
    // The phone column is the one that widens the table, and only where the
    // card is narrow enough for it to reach the card's edge.
    knownIssue: cellBounds,
    knownIssueWhen: (ctx) => ctx.viewport.width === 1024 || (ctx.viewport.width === 1280 && ctx.lang === 'en'),
  })

  layoutTest('agent/callbacks (unbreakable numbers)', async (ctx) => {
    render([
      callback(1, { phoneNumber: asciiRun(6000, 4) }),
      callback(2, { phoneNumber: chineseRun(3000, 5), status: 'CLAIMED', handledBy: AGENT_ID }),
      callback(3, { phoneNumber: longUrl(), status: 'CLAIMED', handledBy: AGENT_ID }),
    ])
    await loaded(3)
    await expectLayout(ctx)
  }, { knownIssue: knownBy([() => true, cellBounds]) })
})
