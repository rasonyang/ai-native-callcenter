import { fireEvent, screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { DncNumber } from '@/lib/dnc'
import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { digits } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { textWrap } from '@/test/layout-known-issues'

/**
 * Do not call: the number is the only free-form value on the list (canonical
 * E.164 in practice, but the contract sets no length), shown in the row, in
 * the delete confirmation and in the "added" notice after a manual add.
 */

const at = '2026-10-20T09:12:00Z'
const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const LONG = `+${digits(40)}`

function entry(n: number, over: Partial<DncNumber> = {}): DncNumber {
  return {
    number: `+86138001380${String(n).padStart(2, '0')}`,
    source: 'MANUAL',
    createdAt: at,
    ...over,
  }
}

const ordinaryNumbers = [
  entry(1),
  entry(2, { source: 'IMPORT' }),
  entry(3, { source: 'CONVERSATION', callId: uuid(3) }),
  entry(4, { source: 'MANUAL', number: '+14155550100' }),
]

const longNumbers = [
  entry(1, { number: LONG }),
  entry(2, { number: `+${digits(40, 9)}`, source: 'IMPORT' }),
  entry(3, { number: `+${digits(40, 10)}`, source: 'CONVERSATION', callId: uuid(3) }),
  entry(4, { number: '+14155550100' }),
]

function renderDnc(items: DncNumber[], total = items.length) {
  return renderApp('/admin/dnc', {
    handle: ({ path, method }) => {
      if (method === 'GET' && path.startsWith('/dnc-numbers')) return { items, total }
      if (method === 'POST' && path === '/dnc-numbers') return entry(99, { number: LONG })
      return undefined
    },
  })
}



restoreLanguage()

describe('do not call, ordinary data', () => {
  layoutTest('dnc (ordinary data)', async (ctx) => {
    renderDnc(ordinaryNumbers, 120)
    await screen.findByText('+14155550100')
    await expectLayout(ctx)
  })
})

describe('do not call, 40-digit numbers', () => {
  layoutTest('dnc (long numbers)', async (ctx) => {
    renderDnc(longNumbers, 120)
    await screen.findByText('+14155550100')
    await expectLayout(ctx)
  })
})

describe('do not call, a long number just added', () => {
  // The notice under the input echoes the canonical number the server returned.
  layoutTest('dnc (added notice)', async (ctx) => {
    const { user } = renderDnc(ordinaryNumbers)
    await screen.findByText('+14155550100')
    fireEvent.change(screen.getByLabelText(i18n.t('dnc.numberToAdd')), { target: { value: LONG } })
    await user.click(screen.getByRole('button', { name: i18n.t('dnc.add') }))
    await screen.findByRole('status')
    await expectLayout(ctx)
  })
})

describe('do not call, removing a 40-digit number', () => {
  layoutTest('dnc (delete confirmation)', async (ctx) => {
    const { user } = renderDnc(longNumbers)
    await screen.findByText('+14155550100')
    await user.click(screen.getAllByTitle(i18n.t('common.delete'))[0])
    await screen.findByText(i18n.t('dnc.removeConfirm', { number: LONG }))
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})
