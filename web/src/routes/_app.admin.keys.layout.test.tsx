import { fireEvent, screen, within } from '@testing-library/react'
import { describe } from 'vitest'

import type { APIKey } from '@/lib/keys'
import { SCOPES } from '@/lib/keys'
import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import { expectDialogFits } from '@/test/dialog-layout'
import { asciiRun, chineseRun, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { cellBounds, textWrap } from '@/test/layout-known-issues'

/**
 * API Keys: name (contract limit 128, #370), the non-secret prefix, the scope
 * list, and the dialogs that name a key (edit, the one-time secret). A key
 * with every scope is ordinary on a real deployment, not a hostile value.
 */

const at = '2026-10-20T09:12:00Z'
const uuid = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function apiKey(n: number, over: Partial<APIKey> = {}): APIKey {
  return {
    id: uuid(n),
    name: `CRM integration ${n}`,
    keyPrefix: `aicc_k${n}x7`,
    status: 'ENABLED',
    scopes: ['calls:read', 'history:read'],
    createdAt: at,
    lastUsedAt: '2026-10-27T08:05:00Z',
    ...over,
  }
}

const ordinaryKeys = [
  apiKey(1),
  apiKey(2, { name: 'Nightly export', status: 'REVOKED', lastUsedAt: null }),
  apiKey(3, { name: '报表导出', scopes: [] }),
]

/** One hostile field per key set, so a failure names its cause. */
const nameKeys = [
  apiKey(1, { name: maxLengthOf('APIKeyWrite', 'name') }),
  apiKey(2, { name: chineseRun(128) }),
  apiKey(3, { name: mixedNote().slice(0, 128), status: 'REVOKED' }),
  apiKey(4, { name: 'Ordinary' }),
]

const prefixKeys = [
  apiKey(1, { keyPrefix: asciiRun(6000, 31) }),
  apiKey(2, { keyPrefix: chineseRun(3000, 32) }),
  apiKey(3, { name: 'Ordinary' }),
]

const scopeKeys = [
  apiKey(1, { scopes: [...SCOPES] }),
  apiKey(2, { scopes: SCOPES.slice(0, 6), status: 'REVOKED' }),
  apiKey(3, { name: 'Ordinary' }),
]

function renderKeys(keys: APIKey[]) {
  return renderApp('/admin/keys', {
    handle: ({ path, method, body }) => {
      if (method === 'GET' && path === '/api-keys') return { items: keys }
      if (method === 'POST' && path === '/api-keys') {
        const { name, scopes } = body as { name: string; scopes: string[] }
        // The secret is exactly as long as the cell is allowed to be hostile.
        return { ...apiKey(99, { name, scopes }), secret: `aicc_${asciiRun(300, 41)}` }
      }
      return undefined
    },
  })
}


restoreLanguage()

describe('API keys, ordinary data', () => {
  layoutTest('keys (ordinary data)', async (ctx) => {
    renderKeys(ordinaryKeys)
    await screen.findByText('Nightly export')
    await expectLayout(ctx)
  })
})

describe('API keys, names at the contract limit', () => {
  layoutTest('keys (128-character names)', async (ctx) => {
    renderKeys(nameKeys)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, {
    // 1508px (zh) / 1536px (en) of table in a 754 / 1010px card at 1024 / 1280;
    // the actions end up past the card's right edge. 1920 holds it.
    knownIssue: cellBounds,
    knownIssueWhen: (ctx) => ctx.viewport.width < 1920,
  })
})

describe('API keys, an unbreakable key prefix', () => {
  layoutTest('keys (long prefix)', async (ctx) => {
    renderKeys(prefixKeys)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('API keys, every scope', () => {
  layoutTest('keys (every scope)', async (ctx) => {
    renderKeys(scopeKeys)
    await screen.findByText('Ordinary')
    await expectLayout(ctx)
  })
})

describe('API keys, edit dialog for a key with a 128-character name', () => {
  layoutTest('keys (edit dialog)', async (ctx) => {
    const { user } = renderKeys(nameKeys)
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('common.edit'))[0])
    const dialog = await screen.findByRole('dialog')
    expectDialogFits(ctx)
    // The table behind it is the names test's business; here only the page.
    await expectLayout(ctx, { within: dialog })
  })
})

describe('API keys, revoking a key with a 128-character name', () => {
  layoutTest('keys (revoke confirmation)', async (ctx) => {
    const { user } = renderKeys(nameKeys)
    await screen.findByText('Ordinary')
    await user.click(screen.getAllByTitle(i18n.t('keys.revoke'))[0])
    await screen.findByText(i18n.t('keys.revokeConfirm', { name: maxLengthOf('APIKeyWrite', 'name') }))
    expectDialogFits(ctx)
  }, { knownIssue: textWrap })
})

describe('API keys, the one-time secret dialog', () => {
  // Title names the key (128 characters), the secret is a 300-character token.
  layoutTest('keys (issued secret)', async (ctx) => {
    const { user } = renderKeys(ordinaryKeys)
    await screen.findByText('Nightly export')
    await user.click(screen.getByRole('button', { name: i18n.t('keys.issue') }))
    const form = await screen.findByRole('dialog')
    // One change event: typing 128 characters one key at a time would make
    // the test depend on how fast a loaded machine delivers 128 events.
    fireEvent.change(within(form).getByRole('textbox'), { target: { value: maxLengthOf('APIKeyWrite', 'name') } })
    await user.click(within(form).getByLabelText(i18n.t('keys.selectGroup', { resource: 'calls' })))
    await user.click(within(form).getByRole('button', { name: i18n.t('keys.issue') }))
    await screen.findByText(/^Authorization: Bearer /)
    expectDialogFits(ctx)
    await expectLayout(ctx)
  }, { knownIssue: textWrap })
})
