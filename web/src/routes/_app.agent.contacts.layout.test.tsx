import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import type { Contact } from '@/lib/contacts'
import i18n from '@/lib/i18n'
import { renderApp, waitForRows } from '@/test/app'
import { contactFixture } from '@/test/harness'
import { asciiRun, chineseRun, digits, maxLengthOf, mixedNote } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import {
  agentBarOverflows, at1024, cellBounds, knownBy, narrowTables, shellChrome,
} from '@/test/layout-known-issues'

/**
 * The agent's contact book. The contract bounds the phone number (32), the
 * name and company (200), each tag (40, at most 20 of them) and the notes
 * (4,000, not shown in the table), so those use their bounds; the table is
 * measured with every row carrying the most the contract lets it.
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const at = '2026-10-20T09:12:00Z'

function contact(n: number, overrides: Partial<Contact> = {}): Contact {
  return contactFixture({ id: id(n), lastCallAt: at, ...overrides })
}

const render = (rows: Contact[]) => renderApp('/agent/contacts', { contacts: rows }, { role: 'AGENT' })

/** Twenty tags of forty characters, the most the contract allows. */
const maxTags = (seed: number) => Array.from({ length: 20 }, (_, i) => asciiRun(40, seed * 100 + i))

restoreLanguage()

async function loaded(count: number) {
  await screen.findAllByTitle(i18n.t('common.edit'))
  await waitForRows(count)
}

describe('contacts with ordinary people', () => {
  // The control: a name, a number, a company and a couple of tags each.
  layoutTest('agent/contacts (ordinary data)', async (ctx) => {
    render([
      contact(1),
      contact(2, { name: '李娜', company: '星河网络有限公司', tags: ['VIP', '续约'], notes: '' }),
      contact(3, { name: 'Margaret Thompson-Whitfield', company: 'Northern Lights Telecommunications Ltd', tags: [], lastCallAt: undefined }),
      contact(4, { name: '', phoneNumber: '+4420794600123', company: '', tags: ['complaint', 'priority', 'follow-up'] }),
    ])
    await loaded(4)
    await expectLayout(ctx)
  }, { knownIssue: shellChrome, knownIssueWhen: agentBarOverflows })
})

describe('contacts at the contract limits', () => {
  layoutTest('agent/contacts (40-digit numbers)', async (ctx) => {
    render([
      contact(1, { phoneNumber: digits(32, 1) }),
      contact(2, { phoneNumber: digits(32, 2), name: '' }),
    ])
    await loaded(2)
    await expectLayout(ctx)
  }, { knownIssue: shellChrome, knownIssueWhen: agentBarOverflows })

  layoutTest('agent/contacts (200-character names and companies, with spaces)', async (ctx) => {
    render([
      contact(1, { name: mixedNote().slice(0, 200), company: mixedNote().slice(0, 200) }),
      contact(2, { name: chineseRun(200, 1), company: chineseRun(200, 2) }),
    ])
    await loaded(2)
    await expectLayout(ctx)
  }, { knownIssue: shellChrome, knownIssueWhen: agentBarOverflows })

  // One column at a time, at the contract limit and as other unbreakable runs;
  // the other columns keep ordinary values.
  for (const [column, put] of [
    ['name', (v: string) => ({ name: v })],
    ['company', (v: string) => ({ company: v })],
    ['phone number', (v: string) => ({ phoneNumber: v })],
  ] as const) {
    layoutTest(`agent/contacts (unbreakable ${column})`, async (ctx) => {
      render([
        contact(1, put(asciiRun(200, 1))),
        contact(2, put(chineseRun(200, 2))),
        contact(3, put(digits(40, 3))),
        contact(4),
      ])
      await loaded(4)
      await expectLayout(ctx)
    }, { knownIssue: knownBy([agentBarOverflows, shellChrome], [() => true, cellBounds]) })
  }

  // Also overflows the card on its own at 1024 (830px in 754px); that is
  // hidden behind the top bar while the bar fails, and shows once it is fixed.
  layoutTest('agent/contacts (twenty 40-character tags)', async (ctx) => {
    render([contact(1, { tags: maxTags(1) }), contact(2, { tags: maxTags(2) }), contact(3)])
    await loaded(3)
    await expectLayout(ctx)
  }, { knownIssue: knownBy([agentBarOverflows, shellChrome], [at1024, narrowTables]) })

  layoutTest('agent/contacts (twenty short tags)', async (ctx) => {
    render([contact(1, { tags: Array.from({ length: 20 }, (_, i) => `tag-${i}`) }), contact(2)])
    await loaded(2)
    await expectLayout(ctx)
  }, { knownIssue: shellChrome, knownIssueWhen: agentBarOverflows })

  layoutTest('agent/contacts (notes at the contract limit)', async (ctx) => {
    // Not a table column, so the notes must not reach the page at all.
    render([contact(1, { notes: maxLengthOf('ContactWrite', 'notes', 7) }), contact(2, { notes: mixedNote().repeat(40).slice(0, 4000) })])
    await loaded(2)
    await expectLayout(ctx)
  }, { knownIssue: shellChrome, knownIssueWhen: agentBarOverflows })
})
