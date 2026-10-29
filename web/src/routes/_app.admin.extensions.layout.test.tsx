import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { Extension } from '@/lib/catalog'
import i18n from '@/lib/i18n'
import type { User } from '@/lib/users'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { asciiRun, chineseRun, digits, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { cellBounds, textWrap } from '@/test/layout-known-issues'

/**
 * Extensions: the number (digits, no length bound in the contract), the
 * display name (no bound), and whose phone it is (an account's display name,
 * bounded at 120, and username, bounded at 64, joined into one cell).
 */

const at = '2026-10-20T09:12:00Z'
const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function extension(n: number, over: Partial<Extension> = {}): Extension {
  return {
    id: uuid(n),
    number: `10${n}`,
    displayName: `Desk ${n}`,
    isEnabled: true,
    createdAt: at,
    ...over,
  }
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

const ordinaryExtensions = [
  extension(1, { agentId: uuid(501) }),
  extension(2, { displayName: '前台电话', isEnabled: false }),
  extension(3, { displayName: 'Lobby handset' }),
]
const ordinaryUsers = [account(1)]

/** One hostile field per set, so a failure names its cause. */
const numberExtensions = [
  extension(1, { number: digits(40) }),
  extension(2, { number: digits(40, 9), isEnabled: false }),
  extension(3, { displayName: 'Ordinary' }),
]

const displayNameExtensions = [
  extension(1, { displayName: asciiRun(6000, 71) }),
  extension(2, { displayName: chineseRun(3000, 72) }),
  extension(3, { displayName: mixedNote() }),
  extension(4, { displayName: 'Ordinary' }),
]

const ownerExtensions = [
  extension(1, { agentId: uuid(501) }),
  extension(2, { agentId: uuid(502) }),
  extension(3, { displayName: 'Ordinary' }),
]
const ownerUsers = [
  account(1, {
    displayName: maxLengthOf('UserCreate', 'displayName'),
    username: maxLengthOf('UserCreate', 'username', 8),
  }),
  account(2, { displayName: chineseRun(120), username: asciiRun(64, 9) }),
]

function renderExtensions(extensions: Extension[], users: User[] = []) {
  return renderApp('/admin/extensions', {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === '/extensions') return { items: extensions }
      if (path === '/users') return { items: users }
      if (/^\/extensions\/[^/]+\/password$/.test(path)) return { password: asciiRun(300, 73) }
      return undefined
    },
  })
}


restoreLanguage()

describe('extensions, ordinary data', () => {
  layoutTest('extensions (ordinary data)', async (ctx) => {
    renderExtensions(ordinaryExtensions, ordinaryUsers)
    await screen.findByText('Lobby handset')
    await expectLayout(ctx)
  })
})

describe('extensions, 40-digit numbers', () => {
  layoutTest('extensions (long numbers)', async (ctx) => {
    renderExtensions(numberExtensions)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, {
    // 776px of table in a 754px card, English only: the Chinese headers are shorter.
    knownIssue: cellBounds,
    knownIssueWhen: (ctx) => ctx.viewport.width === 1024 && ctx.lang === 'en',
  })
})

describe('extensions, hostile display names', () => {
  layoutTest('extensions (long display names)', async (ctx) => {
    renderExtensions(displayNameExtensions)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('extensions, owners with the longest name and username the contract accepts', () => {
  layoutTest('extensions (long owners)', async (ctx) => {
    renderExtensions(ownerExtensions, ownerUsers)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds, knownIssueWhen: (ctx) => ctx.viewport.width < 1920 })
})

describe('extensions, the reveal dialog of a 40-digit number', () => {
  layoutTest('extensions (reveal dialog)', async (ctx) => {
    const { user } = renderExtensions(numberExtensions)
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('admin.reveal'))[0])
    await screen.findByRole('dialog')
    expectDialogFits(ctx)
  })
})

describe('extensions, deleting a 40-digit number', () => {
  layoutTest('extensions (delete confirmation)', async (ctx) => {
    const { user } = renderExtensions(numberExtensions)
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('common.delete'))[0])
    await screen.findByText(i18n.t('admin.deleteExtensionConfirm', { number: digits(40) }))
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})
