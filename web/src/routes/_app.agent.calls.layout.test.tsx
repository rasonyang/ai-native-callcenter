import { screen, waitFor } from '@testing-library/react'
import { describe } from 'vitest'

import type { CDR, RecordingRow } from '@/lib/ledger'
import i18n from '@/lib/i18n'
import { renderApp, waitForRows } from '@/test/app'
import { cdrFixture, AGENT_ID, type Backend } from '@/test/harness'
import {
  asciiRun, chineseRun, digits, longUrl, maxLengthOf, mixedNote, words,
} from '@/test/hostile'
import { expectLayout, layoutTest, restoreLanguage } from '@/test/layout'

/**
 * My Calls, as an agent sees it: inside the cockpit shell, with the softphone
 * bar in the top bar. Every free-text cell is the agent's own filing or a
 * number the customer's carrier supplied. The wrap-up note is bounded by the
 * contract at 4,000 characters; the disposition label, the queue name and the
 * number are not.
 */

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function wrapUp(overrides: Partial<NonNullable<CDR['wrapUp']>> = {}): NonNullable<CDR['wrapUp']> {
  return {
    agentId: AGENT_ID,
    dispositionCode: 'RESOLVED',
    dispositionLabel: 'Resolved',
    note: 'Replaced the router, customer confirmed the line is up.',
    isConfirmed: true,
    createdAt: '2026-10-20T09:20:00Z',
    ...overrides,
  }
}

function call(n: number, overrides: Partial<CDR> = {}): CDR {
  return cdrFixture({
    callId: id(n),
    startedAt: '2026-10-20T09:12:00Z',
    endedAt: '2026-10-20T09:17:00Z',
    wrapUp: wrapUp(),
    ...overrides,
  })
}

/** A tiny valid WAV, so the recording player draws instead of failing to decode. */
function wav(): Response {
  const samples = 4000
  const buffer = new ArrayBuffer(44 + samples * 2)
  const view = new DataView(buffer)
  const text = (offset: number, value: string) => {
    for (let i = 0; i < value.length; i++) view.setUint8(offset + i, value.charCodeAt(i))
  }
  text(0, 'RIFF'); view.setUint32(4, 36 + samples * 2, true); text(8, 'WAVE'); text(12, 'fmt ')
  view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true)
  view.setUint32(24, 8000, true); view.setUint32(28, 16000, true); view.setUint16(32, 2, true)
  view.setUint16(34, 16, true); text(36, 'data'); view.setUint32(40, samples * 2, true)
  for (let i = 0; i < samples; i++) view.setInt16(44 + i * 2, Math.round(Math.sin(i / 8) * 8000), true)
  return new Response(buffer, { status: 200, headers: { 'content-type': 'audio/wav' } })
}

const recording: RecordingRow = {
  id: id(900),
  callId: id(1),
  backend: 's3',
  objectKey: 'recordings/2026/10/20/call.wav',
  sizeBytes: 64_000,
  durationSec: 300,
  format: 'wav',
  createdAt: '2026-10-20T09:17:00Z',
}

/** The agent's calls, one of each status, every row with a recording to open. */
function ordinary(): CDR[] {
  return [
    call(1, { hasRecording: true }),
    call(2, { hasRecording: true, callType: 'OUTBOUND', fromNumber: '1001', toNumber: '+8613800000000', status: 'NO_ANSWER', wrapUp: wrapUp({ dispositionLabel: 'No Answer', note: '' }) }),
    call(3, { hasRecording: false, wrapUp: undefined }),
    call(4, { hasRecording: true, wrapUp: wrapUp({ dispositionLabel: 'Follow-up Required', note: 'Customer asked for a callback after 16:00 about the October invoice.' }) }),
  ]
}

function render(rows: CDR[], extra: Partial<Backend> = {}) {
  return renderApp('/agent/calls', {
    myCDRs: rows,
    recordings: [recording],
    handle: ({ path }) => (path.endsWith('/audio') ? wav() : undefined),
    ...extra,
  }, { role: 'AGENT' })
}

/** Opens the first row's recording and waits for the player to be drawn. */
async function openRecording(user: ReturnType<typeof render>['user']) {
  const [listen] = await screen.findAllByRole('button', { name: i18n.t('myCalls.listen') })
  await user.click(listen)
  await waitFor(() => {
    if (!document.querySelector('tbody audio')) throw new Error('the recording row has no player yet')
  })
}

restoreLanguage()

describe('my calls with ordinary calls', () => {
  layoutTest('agent/calls (ordinary data)', async (ctx) => {
    render(ordinary())
    await waitForRows(4)
    await expectLayout(ctx)
  })

  layoutTest('agent/calls (recording row open)', async (ctx) => {
    const { user } = render(ordinary())
    await waitForRows(4)
    await openRecording(user)
    await expectLayout(ctx)
  })
})

describe('my calls with notes of every kind', () => {
  // The note cell truncates by design (max-w-md); what the table does around
  // a long note is what is measured.
  layoutTest('agent/calls (notes at the contract limit)', async (ctx) => {
    render([
      call(1, { wrapUp: wrapUp({ note: maxLengthOf('WrapUpRequest', 'note', 1) }) }),
      call(2, { wrapUp: wrapUp({ note: chineseRun(1300, 2) }) }),
      call(3, { wrapUp: wrapUp({ note: words(4000, 3) }) }),
      call(4, { wrapUp: wrapUp({ note: mixedNote().repeat(30).slice(0, 4000) }) }),
      call(5, { wrapUp: wrapUp({ note: longUrl() }) }),
    ])
    await waitForRows(5)
    await expectLayout(ctx)
  })
})

describe('my calls with hostile numbers, queues and dispositions', () => {
  layoutTest('agent/calls (40-digit numbers)', async (ctx) => {
    render([
      call(1, { fromNumber: digits(40, 1), hasRecording: true }),
      call(2, { callType: 'OUTBOUND', toNumber: digits(40, 2), hasRecording: true }),
      call(3, { fromNumber: digits(40, 3) }),
    ])
    await waitForRows(3)
    await expectLayout(ctx)
  })

  // One column at a time; the other cells keep ordinary values.
  for (const [column, put] of [
    ['customer number', (v: string, i: number) => call(i, { fromNumber: v })],
    ['queue label', (v: string, i: number) => call(i, { legs: [{ kind: 'QUEUE', label: v, durationSec: 12 }] })],
    ['disposition label', (v: string, i: number) => call(i, { wrapUp: wrapUp({ dispositionLabel: v }) })],
  ] as const) {
    layoutTest(`agent/calls (unbreakable ${column})`, async (ctx) => {
      render([put(asciiRun(6000, 1), 1), put(chineseRun(3000, 2), 2), put(longUrl(), 3), call(4)])
      await waitForRows(4)
      await expectLayout(ctx)
    })
  }

  layoutTest('agent/calls (hostile calls, recording row open)', async (ctx) => {
    const { user } = render([
      call(1, { fromNumber: digits(40, 5), hasRecording: true, wrapUp: wrapUp({ note: maxLengthOf('WrapUpRequest', 'note', 6) }) }),
      call(2, { hasRecording: true }),
    ])
    await waitForRows(2)
    await openRecording(user)
    await expectLayout(ctx)
  })
})
