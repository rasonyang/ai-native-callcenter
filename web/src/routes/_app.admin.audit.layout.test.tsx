import { describe } from 'vitest'

import type { AuditEntry } from '@/lib/ledger'
import { renderApp, waitForRows } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, mixedNote, nestedJson } from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage, type LayoutContext } from '@/test/layout'
import { at1024, below1920, cellBounds, knownBy, narrowTables, truncateFloor } from '@/test/layout-known-issues'

/**
 * The audit trail. The contract bounds none of its text fields: the actor's
 * name, the stored "METHOD /route" action, the target kind and the address
 * come from whoever made the request, and the detail is a stored request body
 * (the cell truncates it, so it is hostile only in what the row's neighbours
 * make of it).
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`
const at = '2026-10-20T09:12:00Z'

function entry(n: number, overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    auditId: n,
    occurredAt: at,
    actorId: id(100 + n),
    actorUsername: 'admin',
    action: 'PUT /api/v1/queues/{queueId}',
    targetKind: 'queue',
    targetId: id(200 + n),
    detail: { request: '{"displayName":"Billing","slaThresholdSec":20}' },
    ip: '192.168.31.55',
    ...overrides,
  }
}

function render(rows: AuditEntry[], total = rows.length) {
  return renderApp('/admin/audit', {
    handle: ({ path, method }) =>
      method === 'GET' && path.startsWith('/audit-logs') ? { items: rows, total } : undefined,
  })
}


restoreLanguage()

describe('audit trail with ordinary entries', () => {
  // The control: typical routes, a short request body, an IPv4 address, an
  // account whose name was deleted (its id shows instead).
  layoutTest('audit (ordinary data)', async (ctx) => {
    render([
      entry(1),
      entry(2, { actorUsername: undefined, action: 'POST /api/v1/users', targetKind: 'user', detail: { request: '{"username":"wei"}' } }),
      entry(3, { action: 'DELETE /api/v1/queues/{queueId}', detail: {} }),
    ], 130)
    await waitForRows(3)
    await expectLayout(ctx)
  }, { knownIssue: truncateFloor, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 })
})

describe('audit trail with the longest real values', () => {
  // The longest templated route the API has, an IPv6 address and an e-mail
  // as the account name: nothing hostile, only the widest honest row.
  layoutTest('audit (longest ordinary values)', async (ctx) => {
    render([
      entry(1, {
        actorUsername: 'zhang.wei@example.com',
        action: 'POST /api/v1/campaigns/{campaignId}/contacts',
        targetKind: 'campaign',
        ip: '2001:0db8:85a3:0000:0000:8a2e:0370:7334',
      }),
      entry(2, { action: 'PATCH /api/v1/campaigns/{campaignId}', targetKind: 'campaign' }),
    ])
    await waitForRows(2)
    await expectLayout(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width < 1920 })
})

describe('audit trail with a long request body', () => {
  // A stored request is shown on one line and cut by the cell (max-w-96
  // truncate), so the cell is contained. Its 384px still widens a table that
  // has no room for it at 1024.
  layoutTest('audit (long request body)', async (ctx) => {
    render([
      entry(1, {
        detail: {
          request: JSON.stringify({ contacts: Array.from({ length: 40 }, (_, i) => ({ number: `1380000${i}` })) }),
        },
      }),
    ])
    await waitForRows(1)
    await expectLayout(ctx)
  }, { knownIssue: truncateFloor, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 })
})

/**
 * One column at a time: the other cells keep ordinary values, so a column that
 * is fixed turns its own group green and no known issue hides another column.
 */
const columns: Array<[name: string, put: (value: string) => Partial<AuditEntry>]> = [
  ['actor', (v) => ({ actorUsername: v })],
  ['action', (v) => ({ action: `POST /${v}` })],
  ['target kind', (v) => ({ targetKind: v })],
  ['address', (v) => ({ ip: v })],
  ['request', (v) => ({ detail: { request: v } })],
]

/**
 * Even the ordinary rows overflow the card at 1024 (the group above; the 384px Request cell is the column's floor, #379), so every
 * group fails there for that reason as well as for its own. The request cell
 * is clipped, so it has no reason of its own. A 40-digit value is 369px: it
 * fits the larger cards but not the 754px one, and not the action column at
 * 1280.
 */
const knownFor = (name: string, cellWhen: (ctx: LayoutContext) => boolean) =>
  name === 'request'
    ? knownBy([at1024, truncateFloor])
    : knownBy([at1024, truncateFloor], [cellWhen, cellBounds])

describe('audit trail with unbreakable text in one column', () => {
  for (const [name, put] of columns) {
    layoutTest(`audit (unbreakable ${name})`, async (ctx) => {
      render([
        entry(1, put(asciiRun(6000, 1))),
        entry(2, put(chineseRun(3000))),
        entry(3, put(longUrl())),
        entry(4),
      ])
      await waitForRows(4)
      await expectLayout(ctx)
    }, { knownIssue: knownFor(name, () => true) })
  }

  layoutTest('audit (nested JSON request)', async (ctx) => {
    render([entry(1, { detail: { request: JSON.stringify(nestedJson(30)) } }), entry(2, { detail: nestedJson(30) as never })])
    await waitForRows(2)
    await expectLayout(ctx)
  }, { knownIssue: knownFor('request', () => false) })

  layoutTest('audit (mixed Chinese, English and emoji actor)', async (ctx) => {
    render([entry(1, { actorUsername: mixedNote() }), entry(2)])
    await waitForRows(2)
    await expectLayout(ctx)
  }, { knownIssue: knownFor('request', () => false) })
})

describe('audit trail with 40-digit values in one column', () => {
  for (const [name, put] of columns) {
    layoutTest(`audit (40-digit ${name})`, async (ctx) => {
      render([entry(1, put(digits(40, 1))), entry(2)])
      await waitForRows(2)
      await expectLayout(ctx)
    }, { knownIssue: knownFor(name, name === 'action' ? at1024 : below1920) })
  }
})
