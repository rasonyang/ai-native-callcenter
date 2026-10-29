import { screen, within } from '@testing-library/react'
import { describe } from 'vitest'

import type { FlowDetail, FlowRevisionDetail, FlowSpec } from '@/lib/flows'
import i18n from '@/lib/i18n'
import { renderApp } from '@/test/app'
import {
  asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote, nestedJson, words,
} from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'
import { cellBounds, narrowTables, shellChrome, textWrap } from '@/test/layout-known-issues'

/**
 * The bot editor's History, Tools and Persona tabs, fed values that cannot
 * break: a revision's spec once widened the History table to 8158px and pushed
 * "Roll back to this revision" off screen while every jsdom test passed (#363).
 */

const FLOW_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const PUBLISHED = '0199e1c0-1b2a-7f00-8c3d-2e4f6a8b0c11'
const OLDER = '0199d0aa-0b1a-7e00-8c3d-2e4f6a8b0c22'
const at = '2026-10-20T09:12:00Z'


/**
 * Code-like names (phase ids, tools, slots, voices). The loader lets them be
 * long, and they never wrap; a flow without `unbreakable` keeps them to a
 * length a person would type.
 */
function identifiers(unbreakable: boolean) {
  return (length: number, seed: number) => asciiRun(unbreakable ? length : Math.min(length, 10), seed)
}

/**
 * Forty phases. Ids and instructions are as long as an author can make them;
 * `unbreakable` decides whether the text has a place to wrap.
 */
function phases(unbreakable: boolean): NonNullable<FlowSpec['nodes']> {
  const ident = identifiers(unbreakable)
  const nodes: NonNullable<FlowSpec['nodes']> = {}
  for (let i = 0; i < 40; i++) {
    const id = `phase_${String(i).padStart(2, '0')}_${ident(40, 100 + i)}`
    nodes[id] = {
      instruction: {
        en: unbreakable ? asciiRun(300, 200 + i) : words(300, 200 + i),
        zh: chineseRun(150, 300 + i),
      },
      tools: ['lookup_account'],
      transitions: [],
    }
  }
  nodes.goodbye = { instruction: 'Close the call.', isTerminal: true }
  return nodes
}

/**
 * A flow made of hostile values. With `unbreakable` the free text is runs the
 * browser cannot wrap (a token, a URL); without it, the same lengths as prose.
 * Identifiers (tool and phase names, slots) are unbreakable either way: they
 * are code, and the loader allows them to be long.
 */
function hostileSpec(unbreakable: boolean): FlowSpec {
  const prose = (length: number, seed: number) =>
    unbreakable ? asciiRun(length, seed) : words(length, seed)
  const ident = identifiers(unbreakable)
  const paramName = unbreakable ? `postcode_${ident(60, 13)}` : 'postcode'
  return {
    id: 'hostile_flow',
    specVersion: 'v2',
    initialNode: Object.keys(phases(unbreakable))[0],
    global: {
      persona: { en: prose(6000, 1), zh: chineseRun(3000, 2) },
      rules: {
        en: [unbreakable ? longUrl() : words(900, 3), mixedNote(), unbreakable ? digits() : words(40, 4), prose(2000, 7)],
        zh: [chineseRun(1500, 8), mixedNote(), unbreakable ? longUrl() : words(900, 5)],
      },
      voice: ident(120, 9),
      fallbackTarget: ident(200, 10),
      closingTarget: 'goodbye',
      alwaysAllowedTools: Array.from({ length: 12 }, (_, i) => `tool_${i}_${ident(30, i)}`),
    },
    nodes: phases(unbreakable),
    tools: {
      lookup_account: {
        kind: 'http',
        description: { en: prose(6000, 11), zh: chineseRun(3000, 12) },
        parameters: {
          properties: {
            [paramName]: {
              type: 'string',
              description: prose(2000, 14),
              enum: unbreakable ? [digits(), longUrl(), mixedNote()] : ['PENDING', words(300, 6), mixedNote()],
            },
          },
          required: [paramName],
        },
        http: {
          method: 'POST',
          path: unbreakable ? `/${longUrl()}` : '/customers/lookup',
          body: { nested: nestedJson(30) },
          result: unbreakable ? { [ident(30, 15)]: `$.${ident(40, 16)}` } : { balance: '$.balance' },
        },
      },
      [unbreakable ? `tool_${ident(100, 17)}` : 'record_language']: {
        kind: 'slot',
        slot: unbreakable ? ident(60, 18) : 'language',
        description: mixedNote(),
        enum: unbreakable ? [digits(), ident(60, 19)] : ['en', 'zh'],
      },
    },
  }
}

/** The control: a small flow written the way an author would, no hostile value. */
function ordinarySpec(): FlowSpec {
  return {
    id: 'billing_followup',
    specVersion: 'v2',
    initialNode: 'verify',
    global: {
      persona: { en: 'You are the billing assistant.', zh: '你是账单助手。' },
      rules: { en: ['Be brief.'], zh: ['请简短回答。'] },
      closingTarget: 'goodbye',
    },
    nodes: {
      verify: { instruction: 'Confirm the account holder.', tools: ['lookup_account'] },
      goodbye: { instruction: 'Close the call.', isTerminal: true },
    },
    tools: {
      lookup_account: {
        kind: 'http',
        description: { en: 'Looks up the caller account by customer id.', zh: '按客户编号查询账户。' },
        parameters: {
          properties: { customerId: { type: 'string', description: 'The customer id.' } },
          required: ['customerId'],
        },
        http: {
          method: 'GET',
          path: '/customers/{args.customerId}',
          result: { balance: '$.balance' },
        },
      },
    },
  }
}

/**
 * Three revisions. `unbreakableNote` gives the newest one a note at the
 * contract's 200-character limit with no break opportunity, which is a
 * separate screen below: it overflows today, and keeping it out of the others
 * lets them guard what they are about (the opened spec, the other tabs).
 */
function revisions(unbreakableNote: boolean): FlowDetail['revisions'] {
  return [
    {
      revisionId: PUBLISHED,
      note: unbreakableNote
        ? maxLengthOf('FlowPublish', 'note')
        : `${mixedNote()} ${mixedNote()}`.slice(0, 200),
      createdAt: at,
      isPublished: true,
    },
    { revisionId: OLDER, note: mixedNote(), createdAt: '2026-10-10T09:12:00Z', isPublished: false },
    {
      revisionId: '0199c000-0b1a-7e00-8c3d-2e4f6a8b0c33',
      note: chineseRun(200, 20),
      createdAt: '2026-10-01T09:12:00Z',
      isPublished: false,
    },
  ]
}

function renderEditor(
  options: {
    unbreakableNote?: boolean
    unbreakable?: boolean
    ordinary?: boolean
    name?: 'max' | 'max-unbreakable'
  } = {},
) {
  const spec = options.ordinary ? ordinarySpec() : hostileSpec(options.unbreakable ?? false)
  const REVISIONS = revisions(options.unbreakableNote ?? false)
  const detail: FlowDetail = {
    flow: {
      flowId: FLOW_ID,
      slug: 'hostile_flow',
      name:
        options.name === 'max-unbreakable'
          ? maxLengthOf('FlowCreate', 'name')
          : options.name === 'max'
            ? words(120, 22)
            : 'NovaNet billing follow-up, October outage credit',
      publishedRevisionId: PUBLISHED,
      publishedAt: at,
      hasUnpublishedChanges: false,
      createdAt: '2026-10-02T03:40:00Z',
      updatedAt: '2026-10-27T08:05:00Z',
    },
    draftSpec: spec as FlowDetail['draftSpec'],
    revisions: REVISIONS,
  }
  return renderApp(`/admin/bots/${FLOW_ID}`, {
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path === `/flows/${FLOW_ID}`) return detail
      const revision = REVISIONS.find((r) => path === `/flows/${FLOW_ID}/revisions/${r.revisionId}`)
      if (revision) return { ...revision, spec } as FlowRevisionDetail
      return undefined
    },
  })
}

async function openTab(user: ReturnType<typeof renderApp>['user'], key: string) {
  await user.click(await screen.findByRole('tab', { name: i18n.t(key) }))
}

restoreLanguage()

describe('bot editor, History tab with a hostile revision open', () => {
  // The spec is the hostile part here (#363): its long lines must scroll
  // inside the revision's own box.
  layoutTest('bots/$flowId History', async (ctx) => {
    const { user } = renderEditor({ unbreakable: true })
    await openTab(user, 'bots.tabs.history')
    // Open two revisions, so two detail rows are drawn at once.
    await user.click((await screen.findAllByRole('button', { name: i18n.t('bots.view') }))[0])
    await user.click((await screen.findAllByRole('button', { name: i18n.t('bots.view') }))[1])
    await screen.findAllByRole('region', { name: /./ })
    await expectLayout(ctx)
  })
})

describe('bot editor, History tab with every revision closed', () => {
  layoutTest('bots/$flowId History (closed)', async (ctx) => {
    const { user } = renderEditor({ unbreakable: true })
    await openTab(user, 'bots.tabs.history')
    await screen.findAllByRole('button', { name: i18n.t('bots.rollBack') })
    await expectLayout(ctx)
  })
})

describe('bot editor, History tab with a 200-character revision note and no spaces', () => {
  // The note cell has no width bound and nothing to break an unbroken run at,
  // so the table outgrows its card and View / Roll back sit off screen.
  layoutTest('bots/$flowId History (unbreakable note)', async (ctx) => {
    const { user } = renderEditor({ unbreakableNote: true })
    await openTab(user, 'bots.tabs.history')
    await screen.findAllByRole('button', { name: i18n.t('bots.rollBack') })
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('bot editor, Tools tab', () => {
  // The control. If ordinary data does not fit, no hostile result means anything;
  // today it is 10px too wide at 1024 in English.
  layoutTest('bots/$flowId Tools (ordinary data)', async (ctx) => {
    const { user } = renderEditor({ ordinary: true })
    await openTab(user, 'bots.tabs.tools')
    await within(await screen.findByRole('table')).findByText('lookup_account')
    await expectLayout(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 && ctx.lang === 'en' })

  layoutTest('bots/$flowId Tools', async (ctx) => {
    const { user } = renderEditor()
    await openTab(user, 'bots.tabs.tools')
    await within(await screen.findByRole('table')).findByText('lookup_account')
    await expectLayout(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 })

  // A 2,000-character URL as a tool's path, a 6,000-character token in its
  // description: the HTTP call and description cells size the table.
  layoutTest('bots/$flowId Tools (unbreakable values)', async (ctx) => {
    const { user } = renderEditor({ unbreakable: true })
    await openTab(user, 'bots.tabs.tools')
    await within(await screen.findByRole('table')).findByText('lookup_account')
    await expectLayout(ctx)
  }, { knownIssue: cellBounds })
})

describe('bot editor, Persona and rules tab', () => {
  layoutTest('bots/$flowId Persona', async (ctx) => {
    const { user } = renderEditor()
    await openTab(user, 'bots.tabs.persona')
    await screen.findByText(i18n.t('bots.runtime'))
    await expectLayout(ctx)
  })

  layoutTest('bots/$flowId Persona (unbreakable values)', async (ctx) => {
    const { user } = renderEditor({ unbreakable: true })
    await openTab(user, 'bots.tabs.persona')
    await screen.findByText(i18n.t('bots.runtime'))
    await expectLayout(ctx)
  }, { knownIssue: textWrap })
})

describe('bot editor, a flow name at the contract limit', () => {
  // The name is the page title, the breadcrumb's last segment and the label
  // of the actions beside it.
  layoutTest('bots/$flowId header (120-character name)', async (ctx) => {
    const { user } = renderEditor({ name: 'max' })
    await openTab(user, 'bots.tabs.persona')
    await screen.findByText(i18n.t('bots.runtime'))
    await expectLayout(ctx)
  }, {
    // The breadcrumb takes the whole name on one line and pushes the top bar's
    // controls out of the window; there is room for it from 1280 in Chinese.
    knownIssue: shellChrome,
    knownIssueWhen: (ctx) => ctx.viewport.width < 1280 || (ctx.viewport.width === 1280 && ctx.lang === 'en'),
  })

  layoutTest('bots/$flowId header (120-character name, no spaces)', async (ctx) => {
    const { user } = renderEditor({ name: 'max-unbreakable' })
    await openTab(user, 'bots.tabs.persona')
    await screen.findByText(i18n.t('bots.runtime'))
    await expectLayout(ctx)
  }, { knownIssue: shellChrome })
})
