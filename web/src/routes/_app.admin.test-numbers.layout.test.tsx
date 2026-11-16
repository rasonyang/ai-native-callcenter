import { fireEvent, screen } from '@testing-library/react'
import { describe } from 'vitest'

import i18n from '@/lib/i18n'
import type { TestNumber } from '@/lib/testNumbers'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { digits } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * Test numbers: the number and the free-text note are the free-form values,
 * shown in the row, in the delete confirmation and in the "added" notice.
 */

const at = '2026-10-20T09:12:00Z'
const LONG = `+${digits(40)}`
const LONG_NOTE = 'QA'.repeat(100)

function entry(n: number, over: Partial<TestNumber> = {}): TestNumber {
  return {
    number: `+86138001380${String(n).padStart(2, '0')}`,
    note: 'QA desk phone',
    createdAt: at,
    ...over,
  }
}

const ordinary = [entry(1), entry(2, { note: undefined }), entry(3), entry(4, { number: '+14155550100' })]
const hostile = [
  entry(1, { number: LONG, note: LONG_NOTE }),
  entry(2, { number: `+${digits(40, 9)}`, note: 'x'.repeat(200) }),
  entry(4, { number: '+14155550100' }),
]

function renderList(items: TestNumber[], total = items.length) {
  return renderApp('/admin/test-numbers', {
    handle: ({ path, method }) => {
      if (method === 'GET' && path.startsWith('/test-numbers')) return { items, total }
      if (method === 'POST' && path === '/test-numbers') return entry(99, { number: LONG })
      return undefined
    },
  })
}

restoreLanguage()

describe('test numbers, ordinary data', () => {
  layoutTest('test-numbers (ordinary data)', async (ctx) => {
    renderList(ordinary, 100)
    await screen.findByText('+14155550100')
    await expectLayout(ctx)
  })
})

describe('test numbers, 40-digit numbers and 200-character notes', () => {
  layoutTest('test-numbers (long values)', async (ctx) => {
    renderList(hostile, 100)
    await screen.findByText('+14155550100')
    await expectLayout(ctx)
  })
})

describe('test numbers, a long number just added', () => {
  layoutTest('test-numbers (added notice)', async (ctx) => {
    const { user } = renderList(ordinary)
    await screen.findByText('+14155550100')
    fireEvent.change(screen.getByLabelText(i18n.t('testNumbers.numberToAdd')), { target: { value: LONG } })
    await user.click(screen.getByRole('button', { name: i18n.t('testNumbers.add') }))
    await screen.findByRole('status')
    await expectLayout(ctx)
  })
})

describe('test numbers, the list is full', () => {
  layoutTest('test-numbers (cap refusal)', async (ctx) => {
    const { user } = renderApp('/admin/test-numbers', {
      handle: ({ path, method }) => {
        if (method === 'GET' && path.startsWith('/test-numbers')) return { items: ordinary, total: 100 }
        if (method === 'POST' && path === '/test-numbers') {
          return new Response(
            JSON.stringify({ error: { code: 'TEST_NUMBER_LIMIT_REACHED', message: 'full', params: { limit: 100 } } }),
            { status: 409, headers: { 'content-type': 'application/json' } },
          )
        }
        return undefined
      },
    })
    await screen.findByText('+14155550100')
    fireEvent.change(screen.getByLabelText(i18n.t('testNumbers.numberToAdd')), { target: { value: LONG } })
    await user.click(screen.getByRole('button', { name: i18n.t('testNumbers.add') }))
    await screen.findByRole('alert')
    await expectLayout(ctx)
  })
})

describe('test numbers, removing a 40-digit number', () => {
  layoutTest('test-numbers (delete confirmation)', async (ctx) => {
    const { user } = renderList(hostile)
    await screen.findByText('+14155550100')
    await user.click(screen.getAllByTitle(i18n.t('common.delete'))[0])
    await screen.findByText(i18n.t('testNumbers.removeConfirm', { number: LONG }))
    expectDialogFits(ctx)
  })
})
