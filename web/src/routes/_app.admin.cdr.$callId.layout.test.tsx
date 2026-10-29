import { screen } from '@testing-library/react'
import { describe } from 'vitest'

import i18n from '@/lib/i18n'
import type { CDR, RecordingRow, TranscriptLine } from '@/lib/ledger'
import { renderApp } from '@/test/app'
import { asciiRun, chineseRun, digits, longUrl, mixedNote, nestedJson } from '@/test/hostile'
import { expectLayout, LayoutViolation, layoutTest, restoreLanguage, settle, type LayoutContext } from '@/test/layout'
import { narrowTables, textWrap } from '@/test/layout-known-issues'

/**
 * One call's page: the journey, the facts, the call's userData (written by
 * flows and integrations, so any JSON), the recording and the transcript. It
 * has no table, so the page-scroll rule is the shared check; a card whose
 * content spills out of it is checked here, since a spill that still fits the
 * page would otherwise go unseen.
 */

const CALL_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const at = '2026-10-20T09:12:00Z'

function cdr(overrides: Partial<CDR> = {}): CDR {
  return {
    callId: CALL_ID,
    startedAt: at,
    endedAt: '2026-10-20T09:17:00Z',
    callType: 'INBOUND',
    fromNumber: '+861083550341',
    toNumber: '95001',
    did: '95001',
    ringSec: 3,
    botSec: 40,
    queueWaitSec: 12,
    talkSec: 245,
    billSec: 300,
    totalSec: 300,
    status: 'ANSWERED',
    hangupCause: 'NORMAL_CLEARING',
    isContained: false,
    hasRecording: true,
    legs: [
      { kind: 'TRUNK', label: 'carrier', durationSec: 2 },
      { kind: 'BOT', label: 'novanet_support', durationSec: 40 },
      { kind: 'QUEUE', label: 'support-zh', durationSec: 12 },
      { kind: 'AGENT', label: '1001', durationSec: 245 },
    ],
    userData: { ticketId: 'ORD-10391', language: 'zh', botReason: 'TRANSFER' },
    ...overrides,
  }
}

function recording(overrides: Partial<RecordingRow> = {}): RecordingRow {
  return {
    id: '0199f3a2-7c41-7d22-9b1e-00000000aa01',
    callId: CALL_ID,
    backend: 's3',
    objectKey: 'recordings/2026/10/20/call.wav',
    sizeBytes: 5_242_880,
    durationSec: 300,
    format: 'wav',
    createdAt: at,
    ...overrides,
  }
}

function line(seq: number, overrides: Partial<TranscriptLine> = {}): TranscriptLine {
  return {
    seq,
    occurredAt: `2026-10-20T09:12:${String(seq * 5).padStart(2, '0')}Z`,
    speaker: seq % 2 ? 'CUSTOMER' : 'BOT',
    kind: 'TEXT',
    content: { text: 'Hello, how can I help?' },
    offsetMs: seq * 5000,
    source: 'MODEL',
    ...overrides,
  }
}

const ordinaryTranscript = (): TranscriptLine[] => [
  line(1, { content: { text: 'Hi, I was charged twice this month.' } }),
  line(2, { content: { text: 'I am sorry to hear that. Let me look at your account.' } }),
  line(3, { kind: 'TOOL_CALL', content: { name: 'lookup_account' } }),
  line(4, { kind: 'TOOL_RESULT', content: { name: 'lookup_account' } }),
  line(5, { speaker: 'HUMAN_AGENT', content: { text: '好的，我马上帮您处理退款。' } }),
]

function render(
  detail: { cdr?: CDR; transcript?: TranscriptLine[]; recordings?: RecordingRow[] } = {},
) {
  const body = {
    cdr: detail.cdr ?? cdr(),
    transcript: detail.transcript ?? ordinaryTranscript(),
    recordings: detail.recordings ?? [recording()],
  }
  return renderApp(`/admin/cdr/${CALL_ID}`, {
    handle: ({ path, method }) => (method === 'GET' && path === `/cdrs/${CALL_ID}` ? body : undefined),
  })
}

/** The page has drawn the call (its Facts and transcript headings are there). */
async function drawn() {
  await screen.findByText(CALL_ID, { exact: false })
  await screen.findByText(i18n.t('cdr.transcript'))
}

/** No card on the page has content wider than itself. */
async function expectCardsContain(ctx: LayoutContext) {
  await settle()
  const spills = [...document.querySelectorAll('main .rounded-md.border')]
    .filter((card) => card.scrollWidth > card.clientWidth + 1)
    .map((card) => {
      const heading = card.querySelector('h2')?.textContent
      const right = card.getBoundingClientRect().right
      // The elements that stick out are who to fix; the card is only where it shows.
      const outside = [...card.querySelectorAll('*')]
        .filter((el) => el.getBoundingClientRect().right > right + 1 || el.scrollWidth > el.clientWidth + 1)
        .slice(0, 3)
        .map((el) => `<${el.tagName.toLowerCase()}> "${(el.textContent ?? '').slice(0, 40)}"`)
      return `  [card-contains-content] card "${heading}" is ${card.clientWidth}px wide, its content ${card.scrollWidth}px; overflowing: ${outside.join('; ')}`
    })
  if (spills.length > 0) {
    throw new LayoutViolation(
      `${ctx.screen} @ ${ctx.viewport.width}x${ctx.viewport.height} [${ctx.lang}]\n${spills.join('\n')}`,
    )
  }
}

async function check(ctx: LayoutContext) {
  await expectLayout(ctx)
  await expectCardsContain(ctx)
}


restoreLanguage()

describe('call detail with an ordinary call', () => {
  // The control: a transferred call with a short transcript and a few userData keys.
  layoutTest('cdr/$callId (ordinary data)', async (ctx) => {
    render()
    await drawn()
    await check(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 })
})

describe('call detail with hostile userData, facts and hangup cause', () => {
  // userData is arbitrary JSON from flows and integrations: a key and a value
  // with no break opportunity, a URL, a value nested 30 deep (shown as one
  // line of JSON), Chinese with no punctuation.
  // One field at a time, so a fixed one turns its own group green.
  const hostile: Array<[name: string, call: () => CDR, only1024?: boolean]> = [
    ['hangup cause', () => cdr({ hangupCause: asciiRun(6000, 31) })],
    ['userData value', () => cdr({ userData: { token: asciiRun(6000, 2), crmLookup: longUrl(), note: chineseRun(3000) } })],
    ['userData key', () => cdr({ userData: { [asciiRun(300, 1)]: 'x', [chineseRun(100, 5)]: digits(40) } })],
    ['userData nested JSON', () => cdr({ userData: { context: nestedJson(30), summary: mixedNote() } }), true],
  ]
  for (const [name, call, only1024] of hostile) {
    layoutTest(`cdr/$callId facts and userData (${name})`, async (ctx) => {
      render({ cdr: call() })
      await drawn()
      await check(ctx)
    }, only1024
      // A value nested 30 deep scrolls inside its own box; what fails at 1024
      // is the ordinary Journey card above.
      ? { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 }
      : { knownIssue: textWrap })
  }
})

describe('call detail with hostile leg labels', () => {
  // A leg's label is a queue name, an extension or a dialled number: the
  // dialled number of a DIALING leg is whatever the customer's number was.
  layoutTest('cdr/$callId journey', async (ctx) => {
    render({
      cdr: cdr({
        legs: [
          { kind: 'TRUNK', label: asciiRun(300, 11), durationSec: 2 },
          { kind: 'BOT', label: asciiRun(6000, 12), durationSec: 40 },
          { kind: 'QUEUE', label: chineseRun(3000, 13), durationSec: 12 },
          { kind: 'AGENT', label: digits(40, 14), durationSec: 245 },
          { kind: 'DIALING', label: longUrl(), durationSec: 5 },
        ],
      }),
    })
    await drawn()
    await check(ctx)
  }, { knownIssue: textWrap })
})

describe('call detail with 40-digit numbers in the title', () => {
  layoutTest('cdr/$callId title (40-digit numbers)', async (ctx) => {
    render({ cdr: cdr({ fromNumber: digits(40, 21), did: digits(40, 22), toNumber: digits(40, 22) }) })
    await drawn()
    await check(ctx)
  }, { knownIssue: narrowTables, knownIssueWhen: (ctx) => ctx.viewport.width === 1024 })

  layoutTest('cdr/$callId title (unbreakable numbers)', async (ctx) => {
    render({ cdr: cdr({ fromNumber: asciiRun(6000, 23), did: longUrl() }) })
    await drawn()
    await check(ctx)
  }, { knownIssue: textWrap })
})

describe('call detail with a hostile transcript', () => {
  // Every kind of line: text a customer or a bot said without a space, a URL
  // the bot read out, a tool whose name is long, mixed zh/en/emoji.
  layoutTest('cdr/$callId transcript', async (ctx) => {
    render({
      transcript: [
        line(1, { content: { text: asciiRun(6000, 41) } }),
        line(2, { content: { text: chineseRun(3000, 42) } }),
        line(3, { content: { text: longUrl() } }),
        line(4, { content: { text: mixedNote() } }),
        line(5, { kind: 'TOOL_CALL', content: { name: asciiRun(300, 43) } }),
        line(6, { kind: 'TOOL_RESULT', content: { name: asciiRun(300, 44) } }),
        line(7, { speaker: 'HUMAN_AGENT', content: { text: digits(40, 45) } }),
      ],
    })
    await drawn()
    await check(ctx)
  }, { knownIssue: textWrap })
})

describe('call detail with a hostile recording', () => {
  layoutTest('cdr/$callId recording', async (ctx) => {
    render({
      recordings: [
        recording({ backend: asciiRun(6000, 51) }),
        recording({ id: '0199f3a2-7c41-7d22-9b1e-00000000aa02', backend: chineseRun(3000, 52) }),
      ],
    })
    await drawn()
    await check(ctx)
  }, { knownIssue: textWrap })
})
