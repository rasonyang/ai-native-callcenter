import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { cdp, page, userEvent } from '@vitest/browser/context'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LiveTranscript } from '@/components/live-transcript'
import i18n from '@/lib/i18n'
import type { AiccEvent, EventType } from '@/lib/events'
import { EventStreamProvider } from '@/lib/use-event-stream'
import { renderApp } from '@/test/app'
import { AGENT_ID, CALL_ID, callFixture, presenceFixture } from '@/test/harness'
import { LANGUAGES, layoutTest, restoreLanguage, settle } from '@/test/layout'

/**
 * The live transcript follows the latest line (#456). jsdom computes no layout,
 * so none of this can be seen there: every case below measures real rows in a
 * real scroller, at the cockpit's real widths.
 *
 * "The last line is in view" always means the bottom edge of the last row lies
 * inside the scroller's client rect.
 */

restoreLanguage()

const CALL_B = '00000000-0000-4000-8000-0000000000c2'

// ---------------------------------------------------------------------------
// Data: a long, messy conversation.
// ---------------------------------------------------------------------------

const EN_LONG =
  'I have been trying to reach someone about the invoice for the last three weeks and every time I call I get transferred to a different department that tells me it is somebody else’s job, so please tell me who actually owns this.'
const ZH_LONG =
  '我已经连续三个星期在联系你们处理这张发票的问题了，每次打电话都被转到不同的部门，每个部门都说不是他们负责的，请你告诉我到底应该找谁来处理这件事情。'

/** One persisted row, as the snapshot serves it. Text is unique per seq. */
function row(seq: number) {
  const k = seq % 10
  let speaker = seq % 2 === 0 ? 'BOT' : 'CUSTOMER'
  let kind = 'TEXT'
  let content: Record<string, unknown>
  switch (k) {
    case 0: content = { text: `#${seq} 哦。` }; break
    case 1: content = { text: `#${seq} Okay.` }; break
    case 2: content = { text: `#${seq} 我在 home 办公，不在 office` }; break
    case 3: content = { text: `#${seq} ${EN_LONG}` }; break
    case 4: content = { text: `#${seq} ${ZH_LONG}` }; break
    case 5: speaker = 'BOT'; kind = 'TOOL_RESULT'; content = { name: `lookupFact${seq}` }; break
    case 6: speaker = 'HUMAN_AGENT'; content = { text: `#${seq} Let me check that for you. ${EN_LONG.slice(0, 90)}` }; break
    default: content = { text: `#${seq} 好的，请稍等一下 ok` }
  }
  return {
    seq,
    occurredAt: new Date().toISOString(),
    speaker,
    kind,
    content,
    offsetMs: seq * 1000,
    source: 'ASR',
    utteranceId: `u-${seq}`,
    agentId: speaker === 'HUMAN_AGENT' ? AGENT_ID : undefined,
  }
}

const rows = (from: number, to: number) =>
  Array.from({ length: to - from + 1 }, (_, i) => row(from + i))

/** What the stream carries for a final line (the same shape as the rows). */
const finalEvent = (seq: number, text: string, extra: Record<string, unknown> = {}) => ({
  utteranceId: `u-${seq}`,
  speaker: 'CUSTOMER',
  kind: 'TEXT',
  isFinal: true,
  seq,
  text,
  source: 'ASR',
  ...extra,
})

// ---------------------------------------------------------------------------
// Measuring.
// ---------------------------------------------------------------------------

const frame = () => new Promise<void>((resolve) => requestAnimationFrame(() => resolve()))
const frames = async (n = 3) => {
  for (let i = 0; i < n; i++) {
    // Sequential on purpose: each frame follows the one before it.
    // oxlint-disable-next-line no-await-in-loop
    await frame()
  }
}

const scrollerEl = () => screen.getByRole('log')
const rowsIn = () => [...(document.querySelector('[role="log"]')?.querySelectorAll('li') ?? [])]

/** Whether the last row's bottom edge lies inside the scroller; says why not. */
function lastRowProblem(): string | undefined {
  const box = scrollerEl().getBoundingClientRect()
  const last = rowsIn().at(-1)
  if (!last) return 'no rows'
  const r = last.getBoundingClientRect()
  if (r.bottom > box.bottom + 1 || r.bottom < box.top) {
    return `last row "${(last.textContent ?? '').slice(0, 30)}" ends at ${Math.round(r.bottom)}, `
      + `scroller is ${Math.round(box.top)}..${Math.round(box.bottom)} `
      + `(scrollTop ${Math.round(scrollerEl().scrollTop)} of ${scrollerEl().scrollHeight})`
  }
}

async function expectLastRowInView() {
  await frames(2)
  const problem = lastRowProblem()
  if (problem) throw new Error(problem)
}

const jumpButton = () => screen.queryByRole('button', { name: i18n.t('transcript.jumpToLatest') })

/** The text of the first row that is at least partly in view, and where it sits. */
function topRow() {
  const box = scrollerEl().getBoundingClientRect()
  const li = rowsIn().find((r) => r.getBoundingClientRect().bottom > box.top + 1)!
  return { text: (li.textContent ?? '').slice(0, 24), offset: li.getBoundingClientRect().top - box.top }
}

/**
 * Resolves with whether the last row was in view at the moment `text` first
 * appeared in the DOM. A MutationObserver callback runs after the commit's
 * layout effects and before the browser paints, so this is the first paint.
 */
const observers: MutationObserver[] = []
function watchFirstAppearance(text: string) {
  return new Promise<string | undefined>((resolve) => {
    const observer = new MutationObserver(() => {
      if (!rowsIn().some((r) => r.textContent?.includes(text))) return
      observer.disconnect()
      resolve(lastRowProblem())
    })
    observer.observe(document.body, { childList: true, subtree: true })
    observers.push(observer)
  })
}

/** A real wheel gesture over the transcript, through the browser's input pipeline. */
async function wheel(deltaY: number) {
  const box = scrollerEl().getBoundingClientRect()
  const session = cdp() as unknown as { send: (method: string, params: object) => Promise<unknown> }
  await session.send('Input.dispatchMouseEvent', {
    type: 'mouseWheel',
    x: Math.round(box.left + box.width / 2),
    y: Math.round(box.top + box.height / 2),
    deltaX: 0,
    deltaY,
  })
  await frames(6)
}

// ---------------------------------------------------------------------------
// The real cockpit: the first paint of a long call.
// ---------------------------------------------------------------------------

const onCall = (transcriptState?: string) => ({
  presence: presenceFixture({ state: 'READY', availability: 'ON_CALL' }),
  calls: [callFixture('TALKING')],
  transcript: rows(1, 200),
  transcriptState,
})

describe('the agent page opens a long call at its latest line', () => {
  layoutTest('agent transcript (200 mixed lines, live)', async () => {
    const first = watchFirstAppearance('lookupFact195')
    renderApp('/agent', onCall(), { role: 'AGENT' })
    await screen.findByText(/#199 /)
    expect(await first, 'on first paint').toBeUndefined()
    await settle()
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
    expect(scrollerEl().scrollTop).toBeGreaterThan(0)
  })

  layoutTest('agent transcript (200 mixed lines, call ended)', async () => {
    const first = watchFirstAppearance('lookupFact195')
    renderApp('/agent', onCall('ENDED'), { role: 'AGENT' })
    await screen.findByText(/#199 /)
    expect(await first, 'on first paint').toBeUndefined()
    await settle()
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
  })
})

// ---------------------------------------------------------------------------
// The panel on its own, with a stream the test controls.
// ---------------------------------------------------------------------------

type Listener = (event: AiccEvent) => void

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'content-type': 'application/json' } })

/** The second call: the same shape of rows under other ids and text. */
const callBRows = () =>
  rows(1, 120).map((r) =>
    Object.assign({}, r, { utteranceId: `b-${r.seq}`, content: { text: `B${r.seq} ${EN_LONG}` } }),
  )

interface Server {
  /** Everything the server holds for each call. */
  all: Record<string, ReturnType<typeof row>[]>
  /** How many of the newest rows the first read returns. */
  tail?: number
  state: string
}

function mount(server: Server, startWith: string | undefined = CALL_ID) {
  const listeners = new Map<EventType | '*', Listener[]>()
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://x')
      const m = url.pathname.match(/\/calls\/([^/]+)\/transcript/)
      if (!m) return json({ items: [] })
      const all = server.all[m[1]] ?? []
      const sinceSeq = Number(url.searchParams.get('sinceSeq') ?? 0)
      const limit = Number(url.searchParams.get('limit') ?? 500)
      // The first read of a call is "what there is now"; a page the reader
      // asked for is the rows after its sinceSeq.
      const items = limit === 500 && server.tail ? all.slice(-server.tail) : all.filter((r) => r.seq > sinceSeq).slice(0, limit)
      return json({ items, nextSinceSeq: 0, isLive: true, state: server.state })
    }),
  )

  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  // The cockpit's own box: the panel is h-64 below xl and fills the middle
  // column's height from xl, with a card above it.
  const isXl = window.innerWidth >= 1280
  const width = isXl ? window.innerWidth - 220 - 48 - 600 - 32 : 420
  const tree = (callId: string | undefined) => (
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <EventStreamProvider value={{ status: 'connected', listeners }}>
          <div className="flex flex-col gap-4" style={{ width, height: isXl ? window.innerHeight - 100 : undefined }}>
            <div className="h-24 shrink-0 rounded-md border" />
            <LiveTranscript
              callId={callId}
              myAgentId={AGENT_ID}
              streamStatus="connected"
              className="h-64 xl:h-auto xl:min-h-0 xl:flex-1"
            />
          </div>
        </EventStreamProvider>
      </I18nextProvider>
    </QueryClientProvider>
  )
  const view = render(tree(startWith))
  const emit = (payload: Record<string, unknown>, type: EventType = 'CALL_TRANSCRIPT', callId = CALL_ID) => {
    const event = { type, callId, payload } as unknown as AiccEvent
    for (const listener of listeners.get(type) ?? []) listener(event)
  }
  return {
    emitNow: emit,
    emit: (payload: Record<string, unknown>, type?: EventType, callId?: string) =>
      act(async () => emit(payload, type, callId)),
    show: (callId: string | undefined) => act(async () => view.rerender(tree(callId))),
  }
}

afterEach(() => {
  for (const observer of observers.splice(0)) observer.disconnect()
  cleanup()
  vi.unstubAllGlobals()
  document.getSelection()?.removeAllRanges()
})

const server = (n = 200, over: Partial<Server> = {}): Server => ({
  all: { [CALL_ID]: rows(1, n) },
  state: 'LIVE',
  ...over,
})

/** Runs a case at the narrow (h-64) layout and at an xl one, in both languages. */
function panelTest(name: string, body: () => Promise<void>) {
  for (const width of [1024, 1280]) {
    for (const lang of LANGUAGES) {
      it(`${name} (${width}px, ${lang})`, async () => {
        await page.viewport(width, 800)
        await act(async () => {
          await i18n.changeLanguage(lang)
        })
        await body()
      }, 60_000)
    }
  }
}

describe('while the reader is following', () => {
  panelTest('opens at the last line on the first paint', async () => {
    const first = watchFirstAppearance('lookupFact195')
    mount(server())
    await screen.findByText(/#199 /)
    expect(await first, 'on first paint').toBeUndefined()
    expect(jumpButton()).toBeNull()
  })

  panelTest('keeps 50 new lines in view, one after another', async () => {
    const { emit } = mount(server(60))
    await screen.findByText(/#59 /)
    // The interval is 40 ms rather than 300 ms: every step is asserted on its
    // own frame, so the wait between steps adds nothing but run time.
    for (let seq = 61; seq <= 110; seq++) {
      // oxlint-disable-next-line no-await-in-loop
      await emit(finalEvent(seq, seq % 3 === 0 ? `#${seq} ${ZH_LONG}` : `#${seq} ${EN_LONG}`))
      // oxlint-disable-next-line no-await-in-loop
      await expectLastRowInView()
      // oxlint-disable-next-line no-await-in-loop
      await new Promise((resolve) => setTimeout(resolve, 40))
    }
    expect(jumpButton()).toBeNull()
  })

  panelTest('keeps a burst of ten lines in view', async () => {
    const { emitNow } = mount(server(60))
    await screen.findByText(/#59 /)
    await act(async () => {
      for (let seq = 61; seq <= 70; seq++) {
        // The stream delivers all ten inside one task; React batches the commit.
        emitNow(finalEvent(seq, `#${seq} ${EN_LONG}`))
      }
    })
    await screen.findByText(/#70 /)
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
  })

  panelTest('keeps a partial in view as it grows from one word to four rows, then final', async () => {
    const { emit } = mount(server(60))
    await screen.findByText(/#59 /)
    const words = EN_LONG.split(' ')
    for (const n of [1, 4, 12, 25, words.length]) {
      // oxlint-disable-next-line no-await-in-loop
      await emit({ utteranceId: 'p1', speaker: 'CUSTOMER', kind: 'TEXT', isFinal: false, text: words.slice(0, n).join(' ') })
      // oxlint-disable-next-line no-await-in-loop
      await expectLastRowInView()
    }
    await emit(finalEvent(61, `#61 ${EN_LONG}`, { utteranceId: 'p1' }))
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
  })

  panelTest('keeps the end of a single line taller than the panel in view', async () => {
    const { emit } = mount(server(60))
    await screen.findByText(/#59 /)
    await emit(finalEvent(61, `#61 ${Array.from({ length: 30 }, () => EN_LONG).join(' ')}`))
    expect(rowsIn().at(-1)!.getBoundingClientRect().height).toBeGreaterThan(scrollerEl().clientHeight)
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
  })

  panelTest('stays at the end when the panel is resized and the text rewraps', async () => {
    const { emit } = mount(server(60))
    await screen.findByText(/#59 /)
    await page.viewport(1536, 900)
    await emit(finalEvent(61, '#61 after the resize'))
    await expectLastRowInView()
    await page.viewport(1024, 800)
    await expectLastRowInView()
  })

  panelTest('is not turned off by a scroll the reader did not make', async () => {
    const { emit } = mount(server(200))
    await screen.findByText(/#199 /)
    // What scroll anchoring or a clamp does: moves the view with no gesture.
    scrollerEl().scrollTop = 120
    await frames(3)
    expect(jumpButton(), 'a programmatic scroll away from the end').toBeNull()
    await emit(finalEvent(201, '#201 next'))
    await expectLastRowInView()
  })
})

describe('when the reader scrolls up', () => {
  panelTest('new lines do not move the view, and the button brings it back', async () => {
    const { emit } = mount(server(200))
    await screen.findByText(/#199 /)
    await wheel(-1500)
    const button = await screen.findByRole('button', { name: i18n.t('transcript.jumpToLatest') })
    const before = topRow()
    const top = scrollerEl().scrollTop

    for (let seq = 201; seq <= 205; seq++) {
      // oxlint-disable-next-line no-await-in-loop
      await emit(finalEvent(seq, `#${seq} ${EN_LONG}`))
    }
    await frames(3)
    expect(topRow()).toEqual(before)
    expect(scrollerEl().scrollTop).toBe(top)
    expect(jumpButton()).not.toBeNull()

    button.click()
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
    // The next line confirms that following resumed.
    await emit(finalEvent(206, '#206 after the jump'))
    await expectLastRowInView()
  })

  panelTest('scrolling back to the end by hand resumes following without a click', async () => {
    const { emit } = mount(server(200))
    await screen.findByText(/#199 /)
    await wheel(-1500)
    await screen.findByRole('button', { name: i18n.t('transcript.jumpToLatest') })
    await wheel(100000)
    await waitFor(() => expect(jumpButton()).toBeNull())
    await emit(finalEvent(201, '#201 after scrolling back'))
    await expectLastRowInView()
  })

  panelTest('a selection inside the panel is never scrolled away', async () => {
    const { emit } = mount(server(200))
    await screen.findByText(/#199 /)
    const target = rowsIn().at(-3)!.querySelector('span:last-child')!
    const range = document.createRange()
    range.selectNodeContents(target)
    document.getSelection()!.removeAllRanges()
    document.getSelection()!.addRange(range)
    const top = scrollerEl().scrollTop
    for (let seq = 201; seq <= 204; seq++) {
      // oxlint-disable-next-line no-await-in-loop
      await emit(finalEvent(seq, `#${seq} ${EN_LONG}`))
    }
    await frames(3)
    expect(scrollerEl().scrollTop).toBe(top)
    expect(document.getSelection()!.isCollapsed).toBe(false)
  })
})

describe('Load earlier', () => {
  panelTest('prepending 100 earlier lines keeps the reader on the line they were reading', async () => {
    mount(server(250, { tail: 150 }))
    await screen.findByText(/#249 /)
    await wheel(-100000)
    scrollerEl().scrollTop = 0
    await frames(2)
    const before = topRow()
    const button = await screen.findByRole('button', { name: i18n.t('transcript.loadEarlier') })
    button.click()
    await screen.findByText(/#4 /)
    await frames(3)
    expect(rowsIn()).toHaveLength(250)
    const kept = rowsIn().find((r) => (r.textContent ?? '').startsWith(before.text))!
    const delta = kept.getBoundingClientRect().top - scrollerEl().getBoundingClientRect().top - before.offset
    expect(Math.abs(delta), 'the line the reader was on moved').toBeLessThanOrEqual(1)
  })
})

describe('the next call', () => {
  panelTest('opens at its latest line and follows, after the reader scrolled up on the last one', async () => {
    const s = server(200)
    s.all[CALL_B] = callBRows()
    const { emit, show } = mount(s)
    await screen.findByText(/#199 /)
    await wheel(-1500)
    await screen.findByRole('button', { name: i18n.t('transcript.jumpToLatest') })

    await show(CALL_B)
    await screen.findByText(/B119 /)
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
    await emit({ ...finalEvent(121, 'B121 live'), utteranceId: 'b-121' }, 'CALL_TRANSCRIPT', CALL_B)
    await expectLastRowInView()
  })

  panelTest('the same when the first call ended and showed ENDED before the next arrived', async () => {
    const s = server(200)
    s.all[CALL_B] = callBRows()
    const { emit, show } = mount(s)
    await screen.findByText(/#199 /)
    await wheel(-1500)
    await screen.findByRole('button', { name: i18n.t('transcript.jumpToLatest') })

    await show(undefined)
    await screen.findByText(i18n.t('transcript.status.ENDED'))
    expect(screen.getByText(/#199 /)).toBeTruthy()
    await show(CALL_B)
    await screen.findByText(/B119 /)
    await expectLastRowInView()
    expect(jumpButton()).toBeNull()
    await emit({ ...finalEvent(121, 'B121 live'), utteranceId: 'b-121' }, 'CALL_TRANSCRIPT', CALL_B)
    await expectLastRowInView()
  })
})

describe('the reader uses the keyboard', () => {
  /** A click in the log, as a reader does to give it the keys; no selection is left behind. */
  async function clickIn() {
    await userEvent.click(rowsIn().at(-2)!)
    document.getSelection()?.removeAllRanges()
  }

  /** Keyboard scrolling is animated: wait until the view has stopped moving. */
  async function stopped() {
    let last = -1
    for (let i = 0; i < 60; i++) {
      // oxlint-disable-next-line no-await-in-loop
      await frames(5)
      if (scrollerEl().scrollTop === last) return
      last = scrollerEl().scrollTop
    }
  }

  panelTest('PageUp leaves the end and End comes back, without a click on the button', async () => {
    const { emit } = mount(server(200))
    await screen.findByText(/#199 /)
    await clickIn()
    await userEvent.keyboard('{PageUp}')
    await stopped()
    await screen.findByRole('button', { name: i18n.t('transcript.jumpToLatest') })
    const before = topRow()
    for (let seq = 201; seq <= 204; seq++) {
      // oxlint-disable-next-line no-await-in-loop
      await emit(finalEvent(seq, `#${seq} ${EN_LONG}`))
    }
    await frames(3)
    expect(topRow()).toEqual(before)

    await userEvent.keyboard('{End}')
    await stopped()
    await waitFor(() => expect(jumpButton()).toBeNull())
    await emit(finalEvent(205, '#205 after End'))
    await expectLastRowInView()
  })
})

describe('Load earlier that prepends nothing', () => {
  panelTest('leaves no stale place behind: the window trimming the top does not unpin the end', async () => {
    // 450 rows: the hook shows the newest 400 and offers Load earlier, which
    // finds nothing older to fetch.
    const { emit } = mount(server(450))
    await screen.findByText(/#449 /)
    await wheel(-100000)
    scrollerEl().scrollTop = 0
    await frames(2)
    ;(await screen.findByRole('button', { name: i18n.t('transcript.loadEarlier') })).click()
    await frames(4)
    await wheel(100000)
    await waitFor(() => expect(jumpButton()).toBeNull())
    for (let seq = 451; seq <= 456; seq++) {
      // oxlint-disable-next-line no-await-in-loop
      await emit(finalEvent(seq, `#${seq} ${EN_LONG}`))
      // oxlint-disable-next-line no-await-in-loop
      await expectLastRowInView()
    }
    expect(jumpButton()).toBeNull()
  })
})
