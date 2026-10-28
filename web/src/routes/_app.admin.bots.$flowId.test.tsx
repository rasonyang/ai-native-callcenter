import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { act, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import type { ApiErrorBody } from '@/lib/api'
import { sameSpec, type Flow, type FlowDetail, type FlowSpec } from '@/lib/flows'
import i18n from '@/lib/i18n'
import { renderWithProviders } from '@/test/harness'

import { Route } from './_app.admin.bots.$flowId'

/**
 * A refused Publish says what the server named (#361): the loader's problems
 * when the stored draft no longer loads, the phases a deployment rule finds at
 * fault, and the campaigns that stand in the way of a revision that can
 * transfer. Before, the notice and the dialog both showed the code's sentence
 * and dropped the rest.
 */

const FlowDesigner = Route.options.component as () => ReactElement

const FLOW_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const NOTE = 'Adds the October outage credit to the billing follow-up'

/** The phases a call ends in, every one without a line of its own. */
const GOODBYES = [
  ['dispute_logged_goodbye', 'DISPUTE_LOGGED'],
  ['refund_already_issued_goodbye', 'REFUND_ISSUED'],
  ['payment_plan_offered_goodbye', 'PAYMENT_PLAN'],
  ['outage_credit_applied_goodbye', 'OUTAGE_CREDIT'],
  ['caller_declined_goodbye', 'DECLINED'],
  ['account_not_found_goodbye', 'ACCOUNT_NOT_FOUND'],
  ['handed_to_billing_team', 'ESCALATED'],
] as const

const SPEC: FlowSpec = {
  id: 'novanet_billing_followup',
  specVersion: 'v2',
  initialNode: 'verify_account_holder',
  global: {
    persona: {
      en: 'You are the billing assistant for NovaNet Broadband.',
      zh: '你是 NovaNet 宽带的账单助手。',
    },
    closingTarget: 'caller_declined_goodbye',
    alwaysAllowedTools: ['hangup'],
  },
  nodes: {
    verify_account_holder: {
      instruction: 'Confirm the account holder by their postcode and the last four digits.',
      tools: ['lookup_account'],
      transitions: [
        { on: 'TOOL_RESULT', tool: 'lookup_account', target: 'review_disputed_charge' },
      ],
    },
    review_disputed_charge: {
      instruction: 'Find out which charge the caller disputes and what they want done about it.',
      tools: ['open_billing_dispute'],
      transitions: GOODBYES.map(([target, outcome]) => ({
        on: 'TOOL_RESULT',
        tool: 'open_billing_dispute',
        condition: { slot: 'result.outcome', op: 'EQ', value: outcome },
        target,
      })),
    },
    ...Object.fromEntries(
      GOODBYES.map(([id]) => [
        id,
        { instruction: 'Close the call politely; ask nothing further.', isTerminal: true },
      ]),
    ),
  },
}

/** What the loader says about a draft stored before an upgrade tightened it. */
const PROBLEMS = [
  'phase "verify_account_holder" allows unknown tool "lookup_account_by_postcode"',
  'phase "review_disputed_charge" transition 2 targets "escalate_to_collections_review", which is not a phase in this flow',
  'phase "review_disputed_charge" transition 4 fires on NO_INPUT but names tool "open_billing_dispute", which a silence never carries',
  'phase "review_disputed_charge" transition 6: unknown operator "AT_LEAST"',
  'phase "payment_plan_offered_goodbye" is terminal but declares transitions',
  'phase "outage_credit_applied_goodbye" has no instruction',
  'phase "dispute_logged_goodbye" lead intent must be a string',
  'tool "lookup_account_by_postcode" has no description, so the model cannot tell when to use it',
  'tool "open_billing_dispute" declares no path',
  'tool "record_preferred_contact_time" has unknown kind "form"; it is "http" or "slot"',
  'global.maxDurationSec is 5400; it is a number of seconds from 60 to 3600, 0 to turn the limit off',
  'global.toolWaitLine "Could you bear with me for a moment while I pull up the billing history on your NovaNet Broadband account and check whether the October fibre upgrade credit has already been applied to it?" is a question; it must be a statement, because a caller answers a question and the answer talks over the result',
  'global.toolWaitLine "请您稍等一下，我这边帮您查一下 NovaNet 宽带账户最近三个月的账单记录，看看十月份光纤升级的优惠是不是已经返还到您的账户上了，还有上个月因为片区停网补偿的那笔费用有没有一起到账，可以吗？" is a question; it must be a statement, because a caller answers a question and the answer talks over the result',
  'cannot parse nodes.refund_already_issued_goodbye.announce: it is number, want string or an object keyed by language',
]

const PHASES = GOODBYES.map(([id]) => id)

const at = '2026-10-20T09:12:00Z'

function flowFixture(over: Partial<Flow> = {}): Flow {
  return {
    flowId: FLOW_ID,
    slug: 'novanet_billing_followup',
    name: 'NovaNet billing follow-up',
    publishedRevisionId: '0199e1c0-1b2a-7f00-8c3d-2e4f6a8b0c11',
    publishedAt: at,
    hasUnpublishedChanges: true,
    createdAt: '2026-10-02T03:40:00Z',
    updatedAt: '2026-10-27T08:05:00Z',
    ...over,
  }
}

function detailFixture(): FlowDetail {
  return {
    flow: flowFixture(),
    draftSpec: SPEC as FlowDetail['draftSpec'],
    revisions: [
      {
        revisionId: '0199e1c0-1b2a-7f00-8c3d-2e4f6a8b0c11',
        note: 'First billing follow-up',
        createdAt: at,
        isPublished: true,
      },
    ],
  }
}

/** The same flow once a publish went through: the draft is the new revision. */
function publishedDetail(before: FlowDetail, note: string): FlowDetail {
  const revisionId = '0199fd60-4e2b-7a10-9f3c-1d5e7b9a2c40'
  return {
    ...before,
    flow: { ...before.flow, publishedRevisionId: revisionId, hasUnpublishedChanges: false },
    revisions: [
      { revisionId, note, createdAt: '2026-10-28T02:30:00Z', isPublished: true },
      ...before.revisions.map((revision) => ({ ...revision, isPublished: false })),
    ],
  }
}

/** A refusal, as the server writes one. */
type Refused = { status: number; error: ApiErrorBody }

/** How the server answers one press of Publish. */
type Answer = Refused | 'PUBLISHED' | 'UNREACHABLE'

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

/**
 * The flow's endpoints. Each publish takes the next answer, then publishes;
 * a draft save is refused with `saveRefusal` when one is given.
 */
function stubServer(answers: Answer[], saveRefusal?: Refused) {
  const publishes: Array<{ note?: string }> = []
  let detail = detailFixture()
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      const method = init.method ?? 'GET'
      if (path === `/flows/${FLOW_ID}` && method === 'GET') return json(detail)
      if (path === `/flows/${FLOW_ID}` && method === 'PUT' && saveRefusal) {
        return json({ error: saveRefusal.error }, saveRefusal.status)
      }
      if (path === `/flows/${FLOW_ID}/publish` && method === 'POST') {
        const body = (typeof init.body === 'string' ? JSON.parse(init.body) : {}) as {
          note?: string
        }
        publishes.push(body)
        const answer = answers.shift() ?? 'PUBLISHED'
        if (answer === 'UNREACHABLE') throw new TypeError('Failed to fetch')
        if (answer === 'PUBLISHED') {
          detail = publishedDetail(detail, body.note ?? '')
          return json(detail.flow)
        }
        return json({ error: answer.error }, answer.status)
      }
      return json({ items: [] })
    }),
  )
  return { publishes }
}

class NoResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

beforeEach(() => {
  // The phase graph fits itself to its box; jsdom has no layout to observe,
  // and no scrolling for the router to restore.
  vi.stubGlobal('ResizeObserver', NoResizeObserver)
  vi.stubGlobal('scrollTo', () => {})
})

afterEach(async () => {
  vi.unstubAllGlobals()
  // The page is still mounted here, and it re-renders in the new language.
  await act(async () => {
    await i18n.changeLanguage('en')
  })
})

function renderEditor() {
  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({ getParentRoute: () => root, path: '/admin/bots/$flowId', component: FlowDesigner }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: [`/admin/bots/${FLOW_ID}`] }),
  })
  return renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
  )
}

/**
 * Opens the publish dialog, types a note and presses Publish in it, which the
 * server refuses with the first answer. Returns the dialog and both places
 * the refusal is written: the dialog's own alert and the notice above the
 * editor (behind the modal, so hidden from the accessibility tree until the
 * dialog closes).
 */
async function publishRefused(answers: Answer[], publishLabel = 'Publish', noteLabel = 'Note') {
  const server = stubServer(answers)
  const { user } = renderEditor()
  await user.click(await screen.findByRole('button', { name: publishLabel }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText(noteLabel), NOTE)
  await user.click(within(dialog).getByRole('button', { name: publishLabel }))
  const inDialog = await within(dialog).findByRole('alert')
  const notice = await waitFor(() => {
    const found = screen
      .getAllByRole('alert', { hidden: true })
      .find((alert) => !dialog.contains(alert))
    expect(found).toBeDefined()
    return found as HTMLElement
  })
  return { server, user, dialog, inDialog, notice, publishLabel, noteLabel }
}

const listed = (place: HTMLElement) =>
  within(place)
    .queryAllByRole('listitem', { hidden: true })
    .map((item) => item.textContent)

/**
 * The dialog is still open with the note the author typed, and pressing
 * Publish again, which the server now accepts, closes it and clears the
 * notice: the refusal belonged to the attempt, not to the flow.
 */
async function republishes(refused: Awaited<ReturnType<typeof publishRefused>>) {
  const { server, user, dialog, publishLabel, noteLabel } = refused
  expect(screen.getByRole('dialog')).toBe(dialog)
  expect(within(dialog).getByLabelText(noteLabel)).toHaveValue(NOTE)

  await user.click(within(dialog).getByRole('button', { name: publishLabel }))

  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  // The page has read the flow again: two revisions now, the new one live.
  await screen.findByText(/^novanet_billing_followup · 2\b/)
  expect(screen.queryAllByRole('alert', { hidden: true })).toEqual([])
  expect(server.publishes).toEqual([{ note: NOTE }, { note: NOTE }])
}

const VALIDATION_FAILED: Refused = {
  status: 422,
  error: {
    code: 'VALIDATION_FAILED',
    message: 'flow novanet_billing_followup is not usable',
    params: { problems: PROBLEMS },
  },
}

const TERMINAL_ANNOUNCE_REQUIRED: Refused = {
  status: 422,
  error: {
    code: 'TERMINAL_ANNOUNCE_REQUIRED',
    message: 'flow novanet_billing_followup cannot run on this deployment',
    params: { nodes: PHASES },
  },
}

const PUBLISH_BLOCKED: Refused = {
  status: 409,
  error: {
    code: 'CAMPAIGN_FLOW_HAS_TRANSFER',
    message:
      'this revision can transfer the caller to a person, and a campaign that has not finished dials with this flow; cancel the campaign or remove the transfer',
    params: { campaigns: 3, rule: 'PUBLISH_BLOCKED' },
  },
}

describe('a publish the loader refuses', () => {
  it('fixture: fourteen problems, some longer than 200 characters', () => {
    expect(PROBLEMS).toHaveLength(14)
    expect(PROBLEMS.filter((problem) => problem.length > 200).length).toBeGreaterThanOrEqual(2)
  })

  it('lists every problem verbatim in the notice and in the dialog', async () => {
    const refused = await publishRefused([VALIDATION_FAILED])

    for (const place of [refused.inDialog, refused.notice]) {
      expect(place).toHaveTextContent('The server refused this spec:')
      expect(listed(place)).toEqual(PROBLEMS)
    }

    await republishes(refused)
  })
})

describe('a save the loader refuses', () => {
  // The rendering a publish now shares: unchanged for the save it came from.
  it('still lists every problem verbatim above the editor', async () => {
    stubServer([], VALIDATION_FAILED)
    const { user } = renderEditor()
    const name = await screen.findByLabelText('Name')
    await user.type(name, ' (October)')
    await user.click(screen.getByRole('button', { name: 'Save draft' }))

    const notice = await screen.findByRole('alert')
    expect(notice).toHaveTextContent('The server refused this spec:')
    expect(listed(notice)).toEqual(PROBLEMS)
  })
})

describe('a publish a deployment rule refuses', () => {
  it('names the seven phases at fault in the notice and in the dialog', async () => {
    const refused = await publishRefused([TERMINAL_ANNOUNCE_REQUIRED])

    for (const place of [refused.inDialog, refused.notice]) {
      expect(place).toHaveTextContent(
        "This deployment's voice provider says only what it is given, so every phase a call does not leave needs its own announce line. These phases have none:",
      )
      expect(listed(place)).toEqual(PHASES)
    }

    await republishes(refused)
  })
})

describe('a publish unfinished campaigns refuse', () => {
  it('says how many campaigns and the way out, in words for someone publishing', async () => {
    const refused = await publishRefused([PUBLISH_BLOCKED])

    for (const place of [refused.inDialog, refused.notice]) {
      expect(place.textContent).toBe(
        'This revision can transfer the caller to a person, and 3 campaigns that have not finished (Draft, Running or Paused) dial with this flow; a campaign call has no way to a person. Finish or cancel those campaigns, or remove the transfer from the flow. Nothing was published.',
      )
      // The campaign form's copy sends a publisher to a field they do not have.
      expect(place).not.toHaveTextContent('choose another number')
      expect(listed(place)).toEqual([])
    }

    await republishes(refused)
  })

  it('counts one campaign in the singular', async () => {
    const one: Refused = {
      status: 409,
      error: { ...PUBLISH_BLOCKED.error, params: { campaigns: 1, rule: 'PUBLISH_BLOCKED' } },
    }
    const { inDialog } = await publishRefused([one])
    expect(inDialog).toHaveTextContent(
      'and 1 campaign that has not finished (Draft, Running or Paused) dials with this flow',
    )
    expect(inDialog).toHaveTextContent('Finish or cancel that campaign')
  })
})

describe('the same refusals in Chinese', () => {
  it.each([
    ['the loader', VALIDATION_FAILED, '服务端拒绝了这份配置：', PROBLEMS],
    [
      'a deployment rule',
      TERMINAL_ANNOUNCE_REQUIRED,
      '本部署的语音引擎只说给定的话，所以通话不会再离开的每个阶段都要有自己的固定话术（announce）。以下阶段没有：',
      PHASES,
    ],
    [
      'unfinished campaigns',
      PUBLISH_BLOCKED,
      '这个版本可以把来电转给人工，而有 3 个尚未结束（草稿、运行中或已暂停）的外呼活动在用这个流程拨打，外呼活动的通话没有转人工的途径。请结束或取消这些活动，或从流程中去掉转接。本次没有发布。',
      [],
    ],
  ] as const)('names what %s named, verbatim', async (_name, answer, lead, items) => {
    await i18n.changeLanguage('zh')
    const refused = await publishRefused([answer], '发布', '备注')

    for (const place of [refused.inDialog, refused.notice]) {
      expect(place).toHaveTextContent(lead)
      expect(listed(place)).toEqual(items)
    }

    await republishes(refused)
  })
})

describe('a publish that fails for any other reason', () => {
  // Unchanged: the code's own sentence, and never an empty list.
  it.each<[string, Answer, string]>([
    [
      'STORAGE_DOWN',
      { status: 503, error: { code: 'STORAGE_DOWN', message: 'cannot complete the change' } },
      'The database is unreachable.',
    ],
    [
      'NOT_FOUND',
      { status: 404, error: { code: 'NOT_FOUND', message: 'no such flow' } },
      'Not found.',
    ],
    ['a network failure', 'UNREACHABLE', 'Unexpected error.'],
    [
      'a 422 without params',
      { status: 422, error: { code: 'VALIDATION_FAILED', message: 'malformed request body' } },
      'Some fields did not pass validation; check each one.',
    ],
    [
      'a 422 with an empty problem list',
      {
        status: 422,
        error: { code: 'VALIDATION_FAILED', message: 'not usable', params: { problems: [] } },
      },
      'Some fields did not pass validation; check each one.',
    ],
  ])('shows the generic copy for %s', async (_name, answer, sentence) => {
    const refused = await publishRefused([answer])

    for (const place of [refused.inDialog, refused.notice]) {
      expect(place.textContent).toBe(sentence)
      expect(within(place).queryByRole('list', { hidden: true })).not.toBeInTheDocument()
    }

    await republishes(refused)
  })
})

/**
 * The History tab (#363): a revision opens read-only, and Copy to draft saves
 * its spec as the draft through the ordinary PUT, asking first only when that
 * loses something. Publishing it is the ordinary Publish, its note prefilled.
 */

const LIVE_ID = '01a18100-b38c-7d05-a1b3-6e2f4c8d9a17'
const OLD_ID = '01a18100-b369-7c1e-8f42-3a9d5e7b1c60'
const REFUSED_ID = '01a18100-a7f0-7e2a-b6c1-0d4e9a3f7b25'
const OLD_NOTE = '十月停网补偿话术，按客服组意见调整'
const OLD_AT = '2026-10-12T06:30:00Z'

/** The billing follow-up as it was before the October credit moved. */
const OLD_SPEC: FlowSpec = {
  ...SPEC,
  global: {
    ...SPEC.global,
    persona: {
      en: 'You are the billing assistant for NovaNet Broadband. Mention the October outage credit first.',
      zh: '你是 NovaNet 宽带的账单助手。先告知十月停网补偿。',
    },
  },
}

/** A revision an older version stored, which today's loader refuses. */
const REFUSED_SPEC = {
  ...SPEC,
  nodes: {
    ...SPEC.nodes,
    verify_account_holder: {
      instruction: 'Confirm the account holder by their postcode.',
      anounce: 'Thanks for calling NovaNet billing.',
      tools: ['lookup_account_by_postcode'],
    },
  },
} as FlowSpec

const REFUSED_PROBLEMS = [
  'phase "verify_account_holder" has unknown field "anounce"; did you mean "announce"?',
  'phase "verify_account_holder" allows unknown tool "lookup_account_by_postcode"',
]

/** The draft with an edit saved but not published. */
const EDITED_SPEC: FlowSpec = {
  ...SPEC,
  global: { ...SPEC.global, closingTarget: 'handed_to_billing_team' },
}

const SPECS: Record<string, FlowSpec> = {
  [LIVE_ID]: SPEC,
  [OLD_ID]: OLD_SPEC,
  [REFUSED_ID]: REFUSED_SPEC,
}

/**
 * The flow with three revisions, newest first. `draft` is what is saved; the
 * live revision is SPEC, so any other draft has unpublished changes. A PUT of
 * the refused revision's spec is answered as the loader answers it.
 */
function historyServer(draft: FlowSpec) {
  const puts: Array<{ name: string; spec: FlowSpec }> = []
  const publishes: Array<{ note?: string }> = []
  const reads: string[] = []
  let detail: FlowDetail = {
    flow: flowFixture({ publishedRevisionId: LIVE_ID, hasUnpublishedChanges: !sameSpec(draft, SPEC) }),
    draftSpec: draft as FlowDetail['draftSpec'],
    revisions: [
      { revisionId: LIVE_ID, note: 'First billing follow-up', createdAt: at, isPublished: true },
      { revisionId: OLD_ID, note: OLD_NOTE, createdAt: OLD_AT, isPublished: false },
      { revisionId: REFUSED_ID, note: '', createdAt: '2026-10-05T02:00:00Z', isPublished: false },
    ],
  }
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      const method = init.method ?? 'GET'
      const body = typeof init.body === 'string' ? JSON.parse(init.body) : {}
      if (path === `/flows/${FLOW_ID}` && method === 'GET') return json(detail)
      const revision = /^\/flows\/[^/]+\/revisions\/([^/]+)$/.exec(path)
      if (revision && method === 'GET') {
        const id = revision[1]
        reads.push(id)
        const summary = detail.revisions.find((r) => r.revisionId === id)
        return json({ ...summary, spec: SPECS[id] })
      }
      if (path === `/flows/${FLOW_ID}` && method === 'PUT') {
        puts.push(body)
        if (sameSpec(body.spec, REFUSED_SPEC)) {
          return json(
            {
              error: {
                code: 'VALIDATION_FAILED',
                message: 'flow novanet_billing_followup is not usable',
                params: { problems: REFUSED_PROBLEMS },
              },
            },
            422,
          )
        }
        detail = {
          ...detail,
          draftSpec: body.spec,
          flow: { ...detail.flow, hasUnpublishedChanges: !sameSpec(body.spec, SPEC) },
        }
        return json(detail.flow)
      }
      if (path === `/flows/${FLOW_ID}/publish` && method === 'POST') {
        publishes.push(body)
        detail = publishedDetail(detail, body.note ?? '')
        return json(detail.flow)
      }
      return json({ items: [] })
    }),
  )
  return { puts, publishes, reads }
}

type Driver = ReturnType<typeof renderEditor>['user']

async function openTab(user: Driver, tab = 'History') {
  await user.click(await screen.findByRole('tab', { name: tab }))
}

const rowOf = (label: string) => screen.getByText(label).closest('tr') as HTMLElement

const editorText = () =>
  screen.getByLabelText<HTMLTextAreaElement>(/^(Flow spec JSON|流程配置 JSON)$/).value

const pretty = (spec: unknown) => JSON.stringify(spec, null, 2)

describe('a revision in the history', () => {
  // Revision ids are UUIDv7: the first eight characters are the top of the
  // millisecond clock and change about once a minute, so publishes seconds
  // apart share them. They are named by their random tail instead.
  it('names two revisions published in the same minute apart, everywhere', async () => {
    expect(LIVE_ID.slice(0, 8)).toBe(OLD_ID.slice(0, 8))
    expect(OLD_ID.slice(0, 8)).toBe(REFUSED_ID.slice(0, 8))
    const server = historyServer(EDITED_SPEC)
    const { user } = renderEditor()
    await openTab(user)

    const ids = screen
      .getAllByRole('row')
      .slice(1)
      .map((row) => within(row).getAllByRole('cell')[0].firstChild?.textContent)
    expect(ids).toEqual(['4c8d9a17', '5e7b1c60', '9a3f7b25'])
    expect(screen.queryByText('01a18100')).not.toBeInTheDocument()

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Copy to draft' }))
    const dialog = await screen.findByRole('dialog', {
      name: 'Replace the draft with revision 5e7b1c60?',
    })
    expect(dialog).not.toHaveTextContent('4c8d9a17')
    await user.click(within(dialog).getByRole('button', { name: 'Replace draft' }))

    expect(await screen.findByRole('status')).toHaveTextContent(
      'Revision 5e7b1c60 is now the draft. Publish it to put it live.',
    )
    const publish = screen.getByRole('button', { name: 'Publish' })
    await waitFor(() => expect(publish).toBeEnabled())
    await user.click(publish)
    const publishing = await screen.findByRole('dialog')
    expect(within(publishing).getByLabelText('Note')).toHaveValue('Restore of 5e7b1c60')
    await user.click(within(publishing).getByRole('button', { name: 'Publish' }))
    await waitFor(() => expect(server.publishes).toEqual([{ note: 'Restore of 5e7b1c60' }]))
  })

  it('opens read-only and closes again, changing nothing', async () => {
    const server = historyServer(SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'View' }))

    const view = await screen.findByRole('region', { name: 'Spec of revision 5e7b1c60' })
    expect(view).toHaveTextContent('Mention the October outage credit first.')
    expect(view).toHaveTextContent('先告知十月停网补偿。')
    expect(view.tagName).toBe('PRE')
    expect(within(view).queryByRole('textbox')).not.toBeInTheDocument()
    expect(view).not.toHaveAttribute('contenteditable')
    expect(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Hide' })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
    expect(server.reads).toEqual([OLD_ID])

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Hide' }))
    expect(
      screen.queryByRole('region', { name: 'Spec of revision 5e7b1c60' }),
    ).not.toBeInTheDocument()
    expect(server.puts).toEqual([])
  })

  it('shows a revision the loader refuses as it was stored', async () => {
    historyServer(SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('9a3f7b25')).getByRole('button', { name: 'View' }))

    const view = await screen.findByRole('region', { name: 'Spec of revision 9a3f7b25' })
    expect(view).toHaveTextContent('"anounce"')
    expect(view).toHaveTextContent('lookup_account_by_postcode')
  })
})

describe('copying a revision to the draft', () => {
  it('copies at once when there is nothing to lose, shows the spec and prefills the note', async () => {
    const server = historyServer(SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Copy to draft' }))

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await waitFor(() => expect(server.puts).toHaveLength(1))
    expect(server.puts[0].name).toBe('NovaNet billing follow-up')
    expect(server.puts[0].spec).toEqual(OLD_SPEC)

    // Back in the designer, showing the restored spec, and pointed at Publish.
    await waitFor(() => expect(editorText()).toBe(pretty(OLD_SPEC)))
    expect(await screen.findByRole('status')).toHaveTextContent(
      'Revision 5e7b1c60 is now the draft. Publish it to put it live.',
    )
    expect(screen.getByText('Unpublished changes')).toBeInTheDocument()
    expect(screen.queryByText('Unsaved changes')).not.toBeInTheDocument()

    const publish = screen.getByRole('button', { name: 'Publish' })
    await waitFor(() => expect(publish).toBeEnabled())
    await user.click(publish)
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText('Note')).toHaveValue('Restore of 5e7b1c60')
    await user.click(within(dialog).getByRole('button', { name: 'Publish' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(server.publishes).toEqual([{ note: 'Restore of 5e7b1c60' }])
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('asks first when the saved draft is not what is live, naming the revision', async () => {
    const server = historyServer(EDITED_SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Copy to draft' }))

    const dialog = await screen.findByRole('dialog', {
      name: 'Replace the draft with revision 5e7b1c60?',
    })
    const when = new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(
      new Date(OLD_AT),
    )
    expect(dialog).toHaveTextContent('5e7b1c60')
    expect(dialog).toHaveTextContent(when)
    expect(dialog).toHaveTextContent(OLD_NOTE)
    expect(dialog).toHaveTextContent(
      "The draft becomes this revision's spec. Edits that are not saved and changes that are not published are lost. Callers hear nothing new until you publish.",
    )
    expect(server.puts).toEqual([])

    await user.click(within(dialog).getByRole('button', { name: 'Replace draft' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(server.puts.map((put) => put.spec)).toEqual([OLD_SPEC])
    await waitFor(() => expect(editorText()).toBe(pretty(OLD_SPEC)))
  })

  it('asks first when the editor holds edits that are not saved', async () => {
    const server = historyServer(SPEC)
    const { user } = renderEditor()
    await user.type(await screen.findByLabelText('Name'), ' (October)')
    await openTab(user)

    await user.click(within(rowOf('9a3f7b25')).getByRole('button', { name: 'Copy to draft' }))

    const dialog = await screen.findByRole('dialog', {
      name: 'Replace the draft with revision 9a3f7b25?',
    })
    // That revision was published without a note.
    expect(dialog).toHaveTextContent('No note')
    expect(server.puts).toEqual([])
  })

  it('changes nothing when the question is cancelled', async () => {
    const server = historyServer(EDITED_SPEC)
    const { user } = renderEditor()
    await waitFor(() => expect(editorText()).toBe(pretty(EDITED_SPEC)))
    await openTab(user)

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: 'Copy to draft' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(server.puts).toEqual([])
    expect(server.reads).toEqual([])
    expect(screen.getByRole('tab', { name: 'History' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    await openTab(user, 'Designer')
    expect(editorText()).toBe(pretty(EDITED_SPEC))
    await user.click(screen.getByRole('button', { name: 'Publish' }))
    expect(within(await screen.findByRole('dialog')).getByLabelText('Note')).toHaveValue('')
  })

  it('lists what the loader refused, and leaves the draft as it was', async () => {
    const server = historyServer(SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('9a3f7b25')).getByRole('button', { name: 'Copy to draft' }))

    const notice = await screen.findByRole('alert')
    expect(notice).toHaveTextContent('Revision 9a3f7b25 was not copied; the draft is unchanged.')
    expect(notice).toHaveTextContent('The server refused this spec:')
    expect(listed(notice)).toEqual(REFUSED_PROBLEMS)
    expect(server.puts).toHaveLength(1)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    await openTab(user, 'Designer')
    expect(editorText()).toBe(pretty(SPEC))
  })

  it('lists the refusal in the question too, which stays open', async () => {
    historyServer(EDITED_SPEC)
    const { user } = renderEditor()
    await openTab(user)

    await user.click(within(rowOf('9a3f7b25')).getByRole('button', { name: 'Copy to draft' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Replace draft' }))

    const inDialog = await within(dialog).findByRole('alert')
    expect(inDialog).toHaveTextContent('The server refused this spec:')
    expect(listed(inDialog)).toEqual(REFUSED_PROBLEMS)
    expect(screen.getByRole('dialog')).toBe(dialog)
  })

  it('says it all in Chinese', async () => {
    await i18n.changeLanguage('zh')
    historyServer(EDITED_SPEC)
    const { user } = renderEditor()
    await openTab(user, '发布历史')

    await user.click(within(rowOf('5e7b1c60')).getByRole('button', { name: '复制到草稿' }))
    const dialog = await screen.findByRole('dialog', { name: '用版本 5e7b1c60 替换草稿？' })
    expect(dialog).toHaveTextContent(
      '草稿将替换为这个版本的配置，未保存的编辑和未发布的改动都会丢失。发布之前，来电听到的内容不会改变。',
    )
    await user.click(within(dialog).getByRole('button', { name: '替换草稿' }))

    expect(await screen.findByRole('status')).toHaveTextContent(
      '版本 5e7b1c60 已复制为草稿，发布后才会生效。',
    )
    await user.click(screen.getByRole('button', { name: '发布' }))
    expect(within(await screen.findByRole('dialog')).getByLabelText('备注')).toHaveValue(
      '恢复自 5e7b1c60',
    )
  })
})
