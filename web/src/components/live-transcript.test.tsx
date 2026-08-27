import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LiveTranscript } from '@/components/live-transcript'
import i18n from '@/lib/i18n'
import { mergeLine, statusFor, type Line } from '@/lib/transcript'
import { EventStreamProvider } from '@/lib/use-event-stream'
import type { AiccEvent, EventType } from '@/lib/events'

import { AGENT_ID, CALL_ID } from '@/test/harness'

type Listener = (event: AiccEvent) => void

/**
 * Renders the panel with a controllable stream, so the tests can deliver
 * lines in the order the design says must be tolerated rather than the order
 * a happy path would produce.
 */
function renderPanel(options: { items?: unknown[]; failSnapshot?: boolean } = {}) {
  const listeners = new Map<EventType | '*', Listener[]>()

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/transcript')) {
        if (options.failSnapshot) return new Response('nope', { status: 500 })
        return new Response(
          JSON.stringify({
            items: options.items ?? [],
            nextSinceSeq: 0,
            isLive: true,
            state: 'LIVE',
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        )
      }
      return new Response(JSON.stringify({ items: [] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    }),
  )

  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  queryClientRef.current = client
  const view = render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <EventStreamProvider value={{ status: 'connected', listeners }}>
          <LiveTranscript callId={CALL_ID} myAgentId={AGENT_ID} streamStatus="connected" />
        </EventStreamProvider>
      </I18nextProvider>
    </QueryClientProvider>,
  )

  const emit = (payload: Record<string, unknown>, type: EventType = 'CALL_TRANSCRIPT') => {
    const event = { type, callId: CALL_ID, payload } as unknown as AiccEvent
    for (const listener of listeners.get(type) ?? []) listener(event)
  }
  return { ...view, emit, options }
}

/** Holds the harness's query client so a test can force a refetch. */
const queryClientRef: { current: QueryClient | null } = { current: null }

/** One persisted row, as the snapshot serves it. */
const row = (seq: number, text: string) => ({
  seq,
  occurredAt: new Date().toISOString(),
  speaker: 'CUSTOMER',
  kind: 'TEXT',
  content: { text },
  offsetMs: seq * 1000,
  source: 'ASR',
  utteranceId: `u-${seq}`,
})

const final = (seq: number, text: string, extra: Record<string, unknown> = {}) => ({
  utteranceId: `u-${seq}`,
  speaker: 'CUSTOMER',
  kind: 'TEXT',
  isFinal: true,
  seq,
  text,
  source: 'ASR',
  ...extra,
})

afterEach(() => vi.unstubAllGlobals())

describe('mergeLine', () => {
  const line = (over: Partial<Line>): Line => ({
    utteranceId: 'u', speaker: 'CUSTOMER', kind: 'TEXT', text: '', isFinal: true, ...over,
  })

  it('orders by seq however the lines arrive', () => {
    let lines: Line[] = []
    for (const seq of [3, 1, 2]) {
      lines = mergeLine(lines, line({ utteranceId: `u-${seq}`, seq, text: String(seq) }))
    }
    expect(lines.map((l) => l.text)).toEqual(['1', '2', '3'])
  })

  it('replaces an utterance in place instead of appending a second copy', () => {
    let lines = mergeLine([], line({ utteranceId: 'u-1', text: 'I need', isFinal: false }))
    lines = mergeLine(lines, line({ utteranceId: 'u-1', text: 'I need help', seq: 1 }))
    expect(lines).toHaveLength(1)
    expect(lines[0].text).toBe('I need help')
    expect(lines[0].isFinal).toBe(true)
  })

  it('keeps a partial after every numbered line, because it has no place yet', () => {
    let lines = mergeLine([], line({ utteranceId: 'u-1', seq: 1, text: 'first' }))
    lines = mergeLine(lines, line({ utteranceId: 'u-p', text: 'still talking', isFinal: false }))
    lines = mergeLine(lines, line({ utteranceId: 'u-2', seq: 2, text: 'second' }))
    expect(lines.map((l) => l.text)).toEqual(['first', 'second', 'still talking'])
  })

  it('does not deduplicate by text: repeated words are legitimate', () => {
    let lines = mergeLine([], line({ utteranceId: 'u-1', seq: 1, text: 'yes' }))
    lines = mergeLine(lines, line({ utteranceId: 'u-2', seq: 2, text: 'yes' }))
    expect(lines).toHaveLength(2)
  })
})

describe('statusFor', () => {
  it('reports ERROR when the stream cannot be heard, whatever the server last said', () => {
    expect(statusFor(true, 'reconnecting', 'LIVE', true)).toBe('ERROR')
    expect(statusFor(true, 'offline', 'LIVE', true)).toBe('ERROR')
  })

  it('reports exactly what the server said, inventing nothing', () => {
    // It used to fall back to CONNECTING when the server had said nothing,
    // which made a call nobody is transcribing indistinguishable from one whose
    // stream is on its way — and left the panel at "Connecting…" for the life
    // of a call that was never going to connect.
    expect(statusFor(true, 'connected', 'IDLE', true)).toBe('IDLE')
    expect(statusFor(true, 'connected', 'CONNECTING', true)).toBe('CONNECTING')
    expect(statusFor(true, 'connected', 'DEGRADED', true)).toBe('DEGRADED')
  })

  it('freezes at ENDED after a call rather than falling back to IDLE', () => {
    expect(statusFor(false, 'connected', 'LIVE', true)).toBe('ENDED')
    expect(statusFor(false, 'connected', 'IDLE', false)).toBe('IDLE')
  })
})

describe('LiveTranscript', () => {
  it('renders the snapshot, then merges the live tail in seq order', async () => {
    const { emit } = renderPanel({ items: [
      { seq: 1, occurredAt: new Date().toISOString(), speaker: 'BOT', kind: 'TEXT',
        content: { text: 'thanks for calling' }, offsetMs: 0, source: 'MODEL', utteranceId: 'u-1' },
    ] })

    expect(await screen.findByText('thanks for calling')).toBeInTheDocument()
    emit(final(3, 'third'))
    emit(final(2, 'second'))

    await waitFor(() => expect(screen.getByText('third')).toBeInTheDocument())
    const rows = screen.getAllByRole('listitem').map((li) => li.textContent)
    expect(rows[1]).toContain('second')
    expect(rows[2]).toContain('third')
  })

  // A tool line's content holds the tool's name, not text. Reading only text
  // rendered the row as "requested " — the sentence with the one word that
  // carried its meaning missing, twice per transfer.
  it('names the tool a bot line called, from the snapshot and from the stream', async () => {
    const { emit } = renderPanel({ items: [
      { seq: 1, occurredAt: new Date().toISOString(), speaker: 'BOT', kind: 'TOOL_CALL',
        content: { name: 'transfer_to_agent', args: '{}' }, offsetMs: 0,
        source: 'MODEL', utteranceId: 'u-1' },
    ] })

    expect(await screen.findByText(/requested transfer_to_agent/)).toBeInTheDocument()

    emit(final(2, 'transfer_to_agent', { speaker: 'BOT', kind: 'TOOL_RESULT' }))
    await waitFor(() =>
      expect(screen.getByText(/transfer_to_agent answered/)).toBeInTheDocument())
  })

  it('drops buffered lines the snapshot already carried, and keeps the ones it did not', async () => {
    // A line that arrives before the snapshot resolves and is also in it must
    // appear once; subscribing first is only safe because this holds.
    const { emit } = renderPanel({ items: [
      { seq: 1, occurredAt: new Date().toISOString(), speaker: 'CUSTOMER', kind: 'TEXT',
        content: { text: 'hello' }, offsetMs: 0, source: 'ASR', utteranceId: 'u-1' },
    ] })
    emit(final(1, 'hello', { utteranceId: 'u-1' }))
    emit(final(2, 'after the snapshot'))

    expect(await screen.findByText('after the snapshot')).toBeInTheDocument()
    expect(screen.getAllByText('hello')).toHaveLength(1)
  })

  it('replaces a partial in place without growing the row count', async () => {
    const { emit, container } = renderPanel()
    emit({ utteranceId: 'p1', speaker: 'CUSTOMER', kind: 'TEXT', isFinal: false, text: 'I need' })
    expect(await screen.findByText('I need')).toBeInTheDocument()
    // Counted in the DOM, not by role: a partial is aria-hidden by design, so
    // it has no listitem role to find.
    expect(container.querySelectorAll('li')).toHaveLength(1)

    emit(final(1, 'I need help', { utteranceId: 'p1' }))
    await waitFor(() => expect(screen.getByText('I need help')).toBeInTheDocument())
    expect(container.querySelectorAll('li')).toHaveLength(1)
  })

  it('hides a partial from assistive technology and reveals it once final', async () => {
    const { emit } = renderPanel()
    emit({ utteranceId: 'p1', speaker: 'CUSTOMER', kind: 'TEXT', isFinal: false, text: 'guessing' })
    const partial = await screen.findByText('guessing')
    expect(partial.closest('li')).toHaveAttribute('aria-hidden', 'true')

    emit(final(1, 'guessed right', { utteranceId: 'p1' }))
    await waitFor(() => expect(screen.getByText('guessed right')).toBeInTheDocument())
    expect(screen.getByText('guessed right').closest('li')).not.toHaveAttribute('aria-hidden')
  })

  it('labels the agent’s own line You and the bot Bot', async () => {
    const { emit } = renderPanel()
    emit(final(1, 'how can I help', { speaker: 'HUMAN_AGENT', agentId: AGENT_ID }))
    emit(final(2, 'thanks for calling', { speaker: 'BOT' }))

    expect(await screen.findByText('You')).toBeInTheDocument()
    expect(screen.getByText('Bot')).toBeInTheDocument()
    // Customer and You carry no icon; only Bot does.
    expect(screen.getByText('Bot').closest('li')?.querySelector('svg')).toBeTruthy()
    expect(screen.getByText('You').closest('li')?.querySelector('svg')).toBeNull()
  })

  it('ignores lines belonging to another call', async () => {
    const { emit } = renderPanel()
    const stray = { type: 'CALL_TRANSCRIPT', callId: 'someone-else', payload: final(1, 'not mine') }
    for (const listener of [] as Listener[]) listener(stray as unknown as AiccEvent)
    emit(final(1, 'mine'))
    expect(await screen.findByText('mine')).toBeInTheDocument()
    expect(screen.queryByText('not mine')).toBeNull()
  })

  it('shows the empty state rather than an error when the snapshot fails', async () => {
    renderPanel({ failSnapshot: true })
    // The hook retries once before reporting failure, which outlasts the
    // default 1s wait.
    expect(
      await screen.findByText('Transcript unavailable.', {}, { timeout: 5000 }),
    ).toBeInTheDocument()
  })

  it('offers a way back to the tail once the reader scrolls away', async () => {
    const { emit } = renderPanel()
    for (let seq = 1; seq <= 5; seq++) emit(final(seq, `line ${seq}`))
    await screen.findByText('line 5')

    const log = screen.getByRole('log')
    Object.defineProperty(log, 'scrollHeight', { value: 1000, configurable: true })
    Object.defineProperty(log, 'clientHeight', { value: 100, configurable: true })
    log.scrollTop = 0
    log.dispatchEvent(new Event('scroll'))

    const jump = await screen.findByRole('button', { name: 'Jump to latest' })
    await userEvent.click(jump)
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull())
  })
})

// Every state the server can publish must actually reach the screen.
//
// Each was reachable in code and none had ever been seen rendered, which is the
// same standard of evidence as "it compiles". A status line that silently shows
// nothing for DEGRADED is worse than none: the agent reads the absence as fine.
describe('every transcription state renders', () => {
  const cases: Array<[string, string]> = [
    ['IDLE', 'Not transcribing'],
    ['CONNECTING', 'Connecting…'],
    ['LIVE', 'Transcribing…'],
    ['DEGRADED', 'Transcribing one side'],
    ['ERROR', 'Transcription unavailable'],
    ['STOPPED', 'Transcription ended'],
  ]

  for (const [state, label] of cases) {
    it(`shows ${label} for ${state}`, async () => {
      const { emit } = renderPanel()
      emit({ state }, 'CALL_TRANSCRIPTION_STATE')
      expect(await screen.findByText(label)).toBeInTheDocument()
    })
  }

  // The reason a state carries is a stable code, never display text: the panel
  // renders the state, and the code is for whoever reads the event.
  it('renders the state and not the reason code', async () => {
    const { emit } = renderPanel()
    emit({ state: 'ERROR', reason: 'STREAM_NEVER_CONNECTED' }, 'CALL_TRANSCRIPTION_STATE')
    expect(await screen.findByText('Transcription unavailable')).toBeInTheDocument()
    expect(screen.queryByText(/STREAM_NEVER_CONNECTED/)).toBeNull()
  })
})

// A call ending clears it from the agent's roster, so callId goes to
// undefined — but the agent still wants to read what was just said. Only a
// new call, with a callId of its own, may take the panel away from them.
describe('a call ending', () => {
  const SECOND_CALL_ID = '00000000-0000-4000-8000-0000000000c2'

  it('keeps the transcript on screen until the next call replaces it', async () => {
    const listeners = new Map<EventType | '*', Listener[]>()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.includes(`/calls/${CALL_ID}/transcript`)) {
          return new Response(
            JSON.stringify({
              items: [
                { seq: 1, occurredAt: new Date().toISOString(), speaker: 'CUSTOMER',
                  kind: 'TEXT', content: { text: 'the last thing said' }, offsetMs: 0,
                  source: 'ASR', utteranceId: 'u-1' },
              ],
              nextSinceSeq: 0, isLive: true, state: 'STOPPED',
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          )
        }
        return new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        })
      }),
    )

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={client}>
        <I18nextProvider i18n={i18n}>
          <EventStreamProvider value={{ status: 'connected', listeners }}>
            <LiveTranscript
              callId={CALL_ID}
              myAgentId={AGENT_ID}
              streamStatus="connected"
            />
          </EventStreamProvider>
        </I18nextProvider>
      </QueryClientProvider>,
    )

    expect(await screen.findByText('the last thing said')).toBeInTheDocument()

    // The call ends: the cockpit drops it from calls/mine and passes no callId.
    view.rerender(
      <QueryClientProvider client={client}>
        <I18nextProvider i18n={i18n}>
          <EventStreamProvider value={{ status: 'connected', listeners }}>
            <LiveTranscript callId={undefined} myAgentId={AGENT_ID} streamStatus="connected" />
          </EventStreamProvider>
        </I18nextProvider>
      </QueryClientProvider>,
    )

    expect(await screen.findByText('Call ended')).toBeInTheDocument()
    expect(screen.getByText('the last thing said')).toBeInTheDocument()

    // A new call rings in with its own callId — only now does the panel reset.
    view.rerender(
      <QueryClientProvider client={client}>
        <I18nextProvider i18n={i18n}>
          <EventStreamProvider value={{ status: 'connected', listeners }}>
            <LiveTranscript
              callId={SECOND_CALL_ID}
              myAgentId={AGENT_ID}
              streamStatus="connected"
            />
          </EventStreamProvider>
        </I18nextProvider>
      </QueryClientProvider>,
    )

    await waitFor(() => expect(screen.queryByText('the last thing said')).toBeNull())
  })
})

// The snapshot may seed and may never overwrite. It is a past answer, and the
// shell refetches every query on each stream reconnect, so a mid-call refetch
// landing on top of streamed lines is the ordinary path rather than an edge
// case. Before this, that refetch dropped every partial and every final the
// server had not yet persisted.
describe('a refetched snapshot', () => {
  it('merges into what the stream delivered instead of replacing it', async () => {
    const { emit, options } = renderPanel({ items: [row(1, 'from the bot phase')] })
    expect(await screen.findByText(/from the bot phase/i)).toBeInTheDocument()

    emit(final(2, 'said live on the stream'))
    expect(await screen.findByText(/said live on the stream/i)).toBeInTheDocument()

    // The stream reconnects, the shell invalidates every query, and the server
    // answers with a page that has since grown a row — and that does not yet
    // carry the streamed line, because it was not persisted when the page was
    // taken. Replacing the store with it would drop the line the agent is
    // reading.
    options.items = [row(1, 'from the bot phase'), row(3, 'persisted later')]
    await act(async () => {
      await queryClientRef.current?.invalidateQueries()
    })
    expect(await screen.findByText(/persisted later/i)).toBeInTheDocument()

    expect(screen.getByText(/said live on the stream/i)).toBeInTheDocument()
    expect(screen.getByText(/from the bot phase/i)).toBeInTheDocument()
  })
})
