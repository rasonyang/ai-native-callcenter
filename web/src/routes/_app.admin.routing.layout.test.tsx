import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { Queue, QueueAgent } from '@/lib/catalog'
import i18n from '@/lib/i18n'
import type { User } from '@/lib/users'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { asciiRun, chineseRun, digits, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { cellBounds, textWrap } from '@/test/layout-known-issues'

/**
 * Queues & Routing: a queue's display name and switch name (neither bounded in
 * the contract), its extension number, and the dialogs that name it (staffing,
 * delete). The staffing list shows agents' display names (bounded at 120).
 */

const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function queue(n: number, over: Partial<Queue> = {}): Queue {
  return {
    id: uuid(n),
    name: `support_${n}`,
    extNumber: `700${n}`,
    displayName: `Support line ${n}`,
    strategy: 'LONGEST_IDLE_AGENT',
    mohSound: '$${hold_music}',
    maxWaitSec: 300,
    maxWaitNoAgentSec: 0,
    announceFrequencySec: 0,
    tierRules: { isApplied: false, waitSec: 300 },
    discardAbandonedAfterSec: 60,
    isAbandonedResumeAllowed: false,
    slaThresholdSec: 20,
    isRecordingEnabled: true,
    hours: [],
    overflow: { type: 'ANNOUNCE_HANGUP' },
    isEnabled: true,
    ...over,
  }
}

function staffed(queueId: string, n: number, displayName: string): QueueAgent {
  return { queueId, agentId: uuid(500 + n), displayName, level: 1, position: n }
}

function account(n: number, over: Partial<User> = {}): User {
  return {
    userId: uuid(300 + n),
    username: `agent${n}`,
    displayName: `Agent Number ${n}`,
    role: 'AGENT',
    status: 'ACTIVE',
    agentId: uuid(500 + n),
    locale: 'en',
    ...over,
  }
}

const ordinaryQueues = [
  queue(1),
  queue(2, { displayName: '售后支持', name: 'after_sales', strategy: 'ROUND_ROBIN', maxWaitSec: 0 }),
  queue(3, { overflow: { type: 'FORWARD', target: '1001' }, isRecordingEnabled: false }),
]

/** One hostile field per set, so a failure names its cause. */
const displayNameQueues = [
  queue(1, { displayName: asciiRun(6000, 81) }),
  queue(2, { displayName: chineseRun(3000, 82) }),
  queue(3, { displayName: mixedNote() }),
  queue(4, { displayName: 'Ordinary' }),
]

const nameQueues = [
  queue(1, { name: asciiRun(6000, 83) }),
  queue(2, { name: asciiRun(200, 84), displayName: chineseRun(60) }),
  queue(3, { displayName: 'Ordinary' }),
]

const extensionQueues = [
  queue(1, { extNumber: digits(40) }),
  queue(2, { extNumber: digits(40, 9) }),
  queue(3, { displayName: 'Ordinary' }),
]

function renderRouting(
  queues: Queue[],
  options: { agents?: (queueId: string) => QueueAgent[]; users?: User[] } = {},
) {
  return renderApp('/admin/routing', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/queues') return { items: queues }
      const roster = /^\/queues\/([^/]+)\/agents$/.exec(path)
      if (roster) return { items: options.agents?.(roster[1]) ?? [] }
      if (path === '/users') return { items: options.users ?? [] }
      return undefined
    },
  })
}


restoreLanguage()

describe('queues, ordinary data', () => {
  layoutTest('routing (ordinary data)', async (ctx) => {
    renderRouting(ordinaryQueues)
    await screen.findByText('售后支持')
    await expectLayout(ctx)
  })
})

describe('queues, hostile display names', () => {
  layoutTest('routing (long display names)', async (ctx) => {
    renderRouting(displayNameQueues)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('queues, hostile switch names', () => {
  layoutTest('routing (long switch names)', async (ctx) => {
    renderRouting(nameQueues)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('queues, 40-digit extensions', () => {
  layoutTest('routing (long extensions)', async (ctx) => {
    renderRouting(extensionQueues)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, {
    // 1063px of table in a 1010px card at 1280 (English), and in 754px at 1024
    // in both languages (817px in Chinese); 1920 holds it.
    knownIssue: cellBounds,
    knownIssueWhen: (ctx) => ctx.viewport.width === 1024 || (ctx.viewport.width === 1280 && ctx.lang === 'en'),
  })
})

describe('queues, the staffing dialog of a queue with a hostile name', () => {
  // The title is "Agents on {name}"; the roster lists display names up to
  // the contract's 120 characters, and the picker offers "name (username)".
  layoutTest('routing (staffing dialog)', async (ctx) => {
    const target = queue(1, { displayName: asciiRun(300, 85) })
    const agents = [
      staffed(target.id, 1, maxLengthOf('UserCreate', 'displayName')),
      staffed(target.id, 2, chineseRun(120)),
      staffed(target.id, 3, 'Ordinary agent'),
    ]
    const users = [
      account(1, { displayName: maxLengthOf('UserCreate', 'displayName'), username: maxLengthOf('UserCreate', 'username', 8) }),
      account(2, { displayName: chineseRun(120), username: asciiRun(64, 9) }),
      account(9),
    ]
    const { user } = renderRouting([target, queue(2, { displayName: 'Ordinary' })], {
      agents: (id) => (id === target.id ? agents : []),
      users,
    })
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('admin.staff'))[0])
    await screen.findByText('Ordinary agent')
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})

describe('queues, deleting a queue with a hostile name', () => {
  layoutTest('routing (delete confirmation)', async (ctx) => {
    const target = queue(1, { displayName: asciiRun(300, 85) })
    const { user } = renderRouting([target, queue(2, { displayName: 'Ordinary' })])
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('common.delete'))[0])
    await screen.findByText(i18n.t('admin.deleteQueueConfirm', { name: target.displayName }))
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})
