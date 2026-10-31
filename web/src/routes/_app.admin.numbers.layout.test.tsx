import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { DID, Queue } from '@/lib/catalog'
import type { Flow } from '@/lib/flows'
import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { asciiRun, chineseRun, digits, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { textWrap } from '@/test/layout-known-issues'

/**
 * Numbers: the DID (digits, no length bound), the description (no bound), the
 * bot flow's name (bounded at 120, looked up by id), the fallback queue's
 * display name (no bound), and the language tag (bounded at 8).
 */

const at = '2026-10-20T09:12:00Z'
const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const FLOW = uuid(700)
const QUEUE = uuid(800)

function did(n: number, over: Partial<DID> = {}): DID {
  return {
    id: uuid(n),
    number: `9500${n}`,
    language: 'en',
    isRecordingEnabled: true,
    description: `Support line ${n}`,
    isEnabled: true,
    allowInbound: true,
    allowOutbound: false,
    isDefaultOutbound: false,
    isCampaignReady: false,
    flowId: FLOW,
    fallbackQueueId: QUEUE,
    ...over,
  }
}

function flow(name: string, id = FLOW): Flow {
  return { flowId: id, slug: 'billing_followup', name, hasUnpublishedChanges: false, createdAt: at, updatedAt: at }
}

function queue(displayName: string): Queue {
  return {
    id: QUEUE, name: 'support', extNumber: '7001', displayName, strategy: 'LONGEST_IDLE_AGENT',
    mohSound: '$${hold_music}', maxWaitSec: 300, maxWaitNoAgentSec: 0, announceFrequencySec: 0,
    tierRules: { isApplied: false, waitSec: 300 }, discardAbandonedAfterSec: 60,
    isAbandonedResumeAllowed: false, slaThresholdSec: 20, isRecordingEnabled: true, hours: [],
    overflow: { type: 'ANNOUNCE_HANGUP' }, isEnabled: true,
  }
}

const ordinaryDIDs = [
  did(1),
  did(2, { language: 'zh', description: '售后热线', allowInbound: false, allowOutbound: true, isDefaultOutbound: true, maxOutboundChannels: 30 }),
  did(3, { allowOutbound: true, description: '' }),
]

/** One hostile field per set, so a failure names its cause. */
const numberDIDs = [
  did(1, { number: digits(40) }),
  did(2, { number: digits(40, 9), allowOutbound: true, maxOutboundChannels: 100000 }),
  did(3, { description: 'Ordinary' }),
]

const descriptionDIDs = [
  did(1, { description: asciiRun(6000, 91) }),
  did(2, { description: chineseRun(3000, 92) }),
  did(3, { description: mixedNote() }),
  did(4, { description: 'Ordinary' }),
]

const languageDIDs = [
  did(1, { language: maxLengthOf('DIDWrite', 'language') }),
  did(2, { language: 'zh-hans', description: 'Ordinary' }),
]

function renderNumbers(dids: DID[], options: { flowName?: string; queueName?: string } = {}) {
  return renderApp('/admin/numbers', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/dids') return { items: dids }
      if (path === '/flows') return { items: [flow(options.flowName ?? 'Billing follow-up')] }
      if (path === '/queues') return { items: [queue(options.queueName ?? 'Support')] }
      return undefined
    },
  })
}


restoreLanguage()

describe('numbers, ordinary data', () => {
  layoutTest('numbers (ordinary data)', async (ctx) => {
    renderNumbers(ordinaryDIDs)
    await screen.findByText('售后热线')
    await screen.findAllByText('Billing follow-up')
    await expectLayout(ctx)
  })
})

describe('numbers, 40-digit numbers', () => {
  layoutTest('numbers (long numbers)', async (ctx) => {
    renderNumbers(numberDIDs)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('numbers, hostile descriptions', () => {
  layoutTest('numbers (long descriptions)', async (ctx) => {
    renderNumbers(descriptionDIDs)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('numbers, an 8-character language tag', () => {
  layoutTest('numbers (language tags)', async (ctx) => {
    renderNumbers(languageDIDs)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('numbers, a bot flow named at the contract limit', () => {
  layoutTest('numbers (120-character flow name)', async (ctx) => {
    renderNumbers([did(1), did(2, { description: 'Ordinary' })], { flowName: maxLengthOf('FlowCreate', 'name') })
    await screen.findByText('Ordinary')
    await screen.findAllByText(maxLengthOf('FlowCreate', 'name'))
    await expectLayout(ctx)
  })
})

describe('numbers, a fallback queue with a hostile name', () => {
  layoutTest('numbers (long queue name)', async (ctx) => {
    renderNumbers([did(1), did(2, { description: 'Ordinary' })], { queueName: asciiRun(300, 93) })
    await screen.findByText('Ordinary')
    await screen.findAllByText(asciiRun(300, 93))
    await expectLayout(ctx)
  })
})

describe('numbers, deleting a 40-digit number', () => {
  layoutTest('numbers (delete confirmation)', async (ctx) => {
    const { user } = renderNumbers(numberDIDs)
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('common.delete'))[0])
    await screen.findByText(i18n.t('admin.deleteNumberConfirm', { number: digits(40) }))
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})
