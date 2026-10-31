import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import i18n from '@/lib/i18n'
import type { User } from '@/lib/users'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { asciiRun, chineseRun, digits, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { textWrap } from '@/test/layout-known-issues'

/**
 * Users: username (contract limit 64), display name (120), the allocated
 * extension (unbounded in the contract), and the dialogs that name an account.
 */

const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function user(n: number, over: Partial<User> = {}): User {
  return {
    userId: uuid(n),
    username: `agent${n}`,
    displayName: `Agent Number ${n}`,
    role: 'AGENT',
    status: 'ACTIVE',
    agentId: uuid(500 + n),
    extensionNumber: `10${n}`,
    locale: 'en',
    ...over,
  }
}

const ordinaryUsers = [
  user(1),
  user(2, { role: 'SUPERVISOR', displayName: '王小明' }),
  user(3, { status: 'SUSPENDED', extensionNumber: undefined }),
  user(4, { role: 'ADMIN', username: 'admin', displayName: 'Ordinary Admin', agentId: undefined, extensionNumber: undefined }),
]

/** One hostile field per set, so a failure names its cause. */
const usernameUsers = [
  user(1, { username: maxLengthOf('UserCreate', 'username') }),
  user(2, { username: asciiRun(64, 61), role: 'SUPERVISOR' }),
  user(3, { username: 'ordinary_user' }),
]

const displayNameUsers = [
  user(1, { displayName: maxLengthOf('UserCreate', 'displayName') }),
  user(2, { displayName: chineseRun(120) }),
  user(3, { displayName: mixedNote().slice(0, 120), status: 'SUSPENDED' }),
  user(4, { displayName: 'Ordinary Name' }),
]

const extensionUsers = [
  user(1, { extensionNumber: digits(40) }),
  user(2, { extensionNumber: digits(40, 9), status: 'SUSPENDED' }),
  user(3, { displayName: 'Ordinary Name' }),
]

function renderUsers(users: User[]) {
  return renderApp('/admin/users', {
    handle: ({ path, method }) => {
      if (method === 'GET' && path === '/users') return { items: users }
      return undefined
    },
  })
}


restoreLanguage()

describe('users, ordinary data', () => {
  layoutTest('users (ordinary data)', async (ctx) => {
    renderUsers(ordinaryUsers)
    await screen.findByText('Ordinary Admin')
    await expectLayout(ctx)
  })
})

describe('users, usernames at the contract limit', () => {
  layoutTest('users (64-character usernames)', async (ctx) => {
    renderUsers(usernameUsers)
    await screen.findByText('ordinary_user')
    await expectLayout(ctx)
  })
})

describe('users, display names at the contract limit', () => {
  layoutTest('users (120-character display names)', async (ctx) => {
    renderUsers(displayNameUsers)
    await screen.findByText('Ordinary Name')
    await expectLayout(ctx)
  })
})

describe('users, a 40-digit extension', () => {
  layoutTest('users (long extension)', async (ctx) => {
    renderUsers(extensionUsers)
    await screen.findByText('Ordinary Name')
    await expectLayout(ctx)
  })
})

describe('users, the password dialog of a 64-character username', () => {
  // The title is "Set a password for {username}".
  layoutTest('users (reset password dialog)', async (ctx) => {
    const { user: pointer } = renderUsers(usernameUsers)
    await screen.findByText('ordinary_user')
    await pointer.click(screen.getAllByTitle(i18n.t('users.resetPassword'))[0])
    await screen.findByRole('dialog')
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})

describe('users, the edit dialog of a 120-character display name', () => {
  layoutTest('users (edit dialog)', async (ctx) => {
    const { user: pointer } = renderUsers(displayNameUsers)
    await screen.findByText('Ordinary Name')
    await pointer.click(screen.getAllByTitle(i18n.t('common.edit'))[0])
    const dialog = await screen.findByRole('dialog')
    expectDialogFits(ctx)
    await expectLayout(ctx, { within: dialog })
  })
})
