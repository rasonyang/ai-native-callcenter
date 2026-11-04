import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { Callback } from '@/lib/ledger'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * The team's callback queue. The message and the number are the caller's
 * words, unbounded by the contract; the claimant's name is a display name; the
 * last attempt is a sentence. A pager sits under the table (120 rows).
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const at = '2026-10-20T09:12:00Z'
const AGENT = id(900)
const GONE = id(901)

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

function oneOfEach(overrides: Partial<Callback> = {}): Callback[] {
  return [
    callback(1, { ...overrides }),
    callback(2, { status: 'CLAIMED', handledBy: AGENT, lastAttemptAt: at, lastAttemptStatus: 'NO_ANSWER', ...overrides }),
    callback(3, { status: 'CLAIMED', handledBy: GONE, ...overrides }),
    callback(4, { status: 'DONE', handledBy: AGENT, lastAttemptAt: at, lastAttemptStatus: 'ANSWERED', ...overrides }),
    callback(5, { status: 'DISMISSED', handledBy: AGENT, ...overrides }),
  ]
}

function render(rows: Callback[], role: 'ADMIN' | 'SUPERVISOR' = 'ADMIN') {
  return renderApp('/admin/callbacks', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/agents') {
        return { items: [{ agentId: id(800), userId: AGENT, username: 'mina', displayName: 'Mina Chen-Whitfield' }] }
      }
      return path.startsWith('/callbacks') ? { items: rows, total: 120 } : undefined
    },
  }, { role })
}

async function loaded(count: number) {
  await waitForRows(count)
  await screen.findAllByText('Mina Chen-Whitfield')
}

restoreLanguage()

describe('team callbacks with ordinary requests', () => {
  layoutTest('admin/callbacks (ordinary data)', async (ctx) => {
    render(oneOfEach())
    await loaded(5)
    await expectLayout(ctx)
  })

  layoutTest('admin/callbacks (supervisor)', async (ctx) => {
    render(oneOfEach(), 'SUPERVISOR')
    await loaded(5)
    await expectLayout(ctx)
  })
})

describe('team callbacks with hostile messages', () => {
  layoutTest('admin/callbacks (2,000-character zh message)', async (ctx) => {
    render(oneOfEach({ message: chineseRun(2000, 7) }))
    await loaded(5)
    await expectLayout(ctx)
  })

  layoutTest('admin/callbacks (long messages)', async (ctx) => {
    render([
      callback(1, { message: asciiRun(6000, 1) }),
      callback(2, { message: chineseRun(3000, 2), status: 'CLAIMED', handledBy: AGENT }),
      callback(3, { message: longUrl() }),
      callback(4, { message: mixedNote().repeat(20), status: 'CLAIMED', handledBy: AGENT }),
    ])
    await loaded(4)
    await expectLayout(ctx)
  })
})

describe('team callbacks with hostile numbers', () => {
  layoutTest('admin/callbacks (40-digit numbers)', async (ctx) => {
    render(oneOfEach({ phoneNumber: digits(40, 1) }))
    await loaded(5)
    await expectLayout(ctx)
  })

  layoutTest('admin/callbacks (unbreakable numbers)', async (ctx) => {
    render([
      callback(1, { phoneNumber: asciiRun(6000, 4), status: 'CLAIMED', handledBy: AGENT }),
      callback(2, { phoneNumber: chineseRun(3000, 5), status: 'CLAIMED', handledBy: AGENT }),
      callback(3, { phoneNumber: longUrl(), status: 'CLAIMED', handledBy: AGENT }),
    ])
    await loaded(3)
    await expectLayout(ctx)
  })
})
