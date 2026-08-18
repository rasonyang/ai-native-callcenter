import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.index'
import {
  CALL_ID,
  CALLER,
  callFixture,
  installBackend,
  presenceFixture,
  renderPage,
  emitEvent,
  type Backend,
} from '@/test/harness'

/**
 * The agent cockpit page. The control grid must keep six cells: the three the
 * platform can execute stay wired, the three it cannot stay visible and
 * disabled — neither silently dropped nor faked.
 */

const Cockpit = Route.options.component as () => ReactElement

afterEach(() => {
  vi.unstubAllGlobals()
})

async function renderCockpit(backend: Partial<Backend> = {}) {
  const api = installBackend(backend)
  const view = renderPage(Cockpit)
  return { api, ...view }
}

const onCall = { calls: [callFixture('TALKING')], presence: presenceFixture({ availability: 'ON_CALL' }) }

describe('control grid', () => {
  it('shows all six controls', async () => {
    await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    for (const name of [/^mute$/i, /^hold$/i, /^transfer$/i, /^conference$/i, /^keypad$/i, /hang up/i]) {
      expect(within(grid).getByRole('button', { name })).toBeVisible()
    }
    expect(within(grid).getAllByRole('button')).toHaveLength(6)
  })

  it('disables only conference, the one with no endpoint, and says why', async () => {
    await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    const disabled = within(grid)
      .getAllByRole('button')
      .filter((b) => (b as HTMLButtonElement).disabled)
      .map((b) => b.getAttribute('aria-label'))
    expect(disabled).toEqual(['Conference'])
    expect(within(grid).getByRole('button', { name: /^conference$/i })).toHaveAttribute(
      'title',
      expect.stringMatching(/no endpoint/i),
    )
  })

  it('mutes and unmutes the agent', async () => {
    const { api, user } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /^mute$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/mute` }),
      ),
    )
  })

  it('offers unmute once the switch reports the leg muted', async () => {
    const muted = callFixture('TALKING')
    muted.parties[1].isMuted = true
    const { api, user } = await renderCockpit({ calls: [muted] })
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /^unmute$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/unmute` }),
      ),
    )
  })

  it('sends a tone the moment a key is pressed', async () => {
    const { api, user } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /keypad/i }))
    const pad = await screen.findByRole('dialog')
    await user.click(within(pad).getByRole('button', { name: '3' }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: `/calls/${CALL_ID}/dtmf`,
          body: { digits: '3' },
        }),
      ),
    )
    // One request per key: batching them would defeat an IVR that listens for
    // one digit at a time.
    await user.click(within(pad).getByRole('button', { name: '#' }))
    await waitFor(() =>
      expect(api.commands.filter((c) => c.path.endsWith('/dtmf'))).toEqual([
        expect.objectContaining({ body: { digits: '3' } }),
        expect.objectContaining({ body: { digits: '#' } }),
      ]),
    )
  })

  it('holds', async () => {
    const { api, user } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /^hold$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/hold` }),
      ),
    )
  })

  it('resumes a held call', async () => {
    const { api, user } = await renderCockpit({ calls: [callFixture('HELD')] })
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /resume/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/retrieve` }),
      ),
    )
  })

  it('transfers', async () => {
    const { api, user } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /^transfer$/i }))
    const pop = await screen.findByRole('dialog')
    await user.type(within(pop).getByLabelText(/extension/i), '3002')
    await user.click(within(pop).getByRole('button', { name: /^transfer$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: `/calls/${CALL_ID}/transfer`,
          body: { destination: '3002' },
        }),
      ),
    )
  })

  it('hangs up', async () => {
    const { api, user } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    await user.click(within(grid).getByRole('button', { name: /hang up/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/hangup` }),
      ),
    )
  })
})

describe('ringing', () => {
  it('accepts', async () => {
    const { api, user } = await renderCockpit({ calls: [callFixture('RINGING')] })
    await user.click(await screen.findByRole('button', { name: /accept/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/answer` }),
      ),
    )
  })

  it('declines', async () => {
    const { api, user } = await renderCockpit({ calls: [callFixture('RINGING')] })
    await user.click(await screen.findByRole('button', { name: /decline/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/hangup` }),
      ),
    )
  })
})

describe('dial out', () => {
  it('dials a typed number', async () => {
    const { api, user } = await renderCockpit()
    const input = await screen.findByPlaceholderText(/customer number/i)
    await user.type(input, '95011')
    await user.click(screen.getByRole('button', { name: /^dial$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/calls/dial',
          body: { destination: '95011' },
        }),
      ),
    )
  })

  it('composes a number on the keypad', async () => {
    const { user } = await renderCockpit()
    await user.click(await screen.findByRole('button', { name: /keypad/i }))
    const pad = await screen.findByRole('dialog')
    await user.click(within(pad).getByRole('button', { name: '4' }))
    await user.click(within(pad).getByRole('button', { name: '2' }))
    expect(screen.getByPlaceholderText(/customer number/i)).toHaveValue('42')
  })
})

describe('the rest of the cockpit', () => {
  it('shows the caller, the queue and the business data', async () => {
    await renderCockpit(onCall)
    expect(await screen.findAllByText(CALLER)).not.toHaveLength(0)
    expect(screen.getAllByText('support-zh').length).toBeGreaterThan(0)
    expect(screen.getByText('ORD-10391')).toBeInTheDocument()
  })

  it('lists every leg of the call', async () => {
    await renderCockpit(onCall)
    const legs = await screen.findByRole('list', { name: /call legs/i })
    expect(within(legs).getAllByRole('listitem')).toHaveLength(2)
  })

  it('completes wrap-up', async () => {
    const { api, user } = await renderCockpit({
      presence: presenceFixture({
        state: 'NOT_READY',
        availability: 'WRAP_UP',
        reason: 'AFTER_CALL_WORK',
        wrapUpEndsAt: new Date(Date.now() + 30_000).toISOString(),
      }),
    })
    await user.click(await screen.findByRole('button', { name: /ready for the next call/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/ready' }),
      ),
    )
  })

  it('reads presence from the server', async () => {
    await renderCockpit({ presence: presenceFixture({ extensionNumber: '1001' }) })
    // Once in the card's title slot, once in the detail row.
    expect(await screen.findAllByText('1001')).toHaveLength(2)
  })
})

/**
 * The cockpit lost its softphone once to an agentic whole-file rewrite. These
 * assert that adding the transcript panel took nothing with it: the control
 * grid is intact and still issues the same request.
 */
describe('the transcript panel does not disturb the cockpit', () => {
  it('keeps all six controls and the transcript on the same screen', async () => {
    await renderCockpit(onCall)

    expect(await screen.findByText('Live transcript')).toBeInTheDocument()

    const grid = await screen.findByRole('group', { name: /call controls/i })
    expect(within(grid).getAllByRole('button')).toHaveLength(6)
    for (const name of [/^mute$/i, /^hold$/i, /^transfer$/i, /^conference$/i, /^keypad$/i, /hang up/i]) {
      expect(within(grid).getByRole('button', { name })).toBeVisible()
    }
  })

  it('still hangs up through the same endpoint with the panel mounted', async () => {
    const { api } = await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })

    await userEvent.click(within(grid).getByRole('button', { name: /hang up/i }))
    await waitFor(() =>
      expect(api.commands.map((c) => c.path)).toContain(`/calls/${CALL_ID}/hangup`),
    )
  })

  it('asks for no transcript when there is no call', async () => {
    const { api } = await renderCockpit({ calls: [] })

    await screen.findByText('Live transcript')
    expect(api.requests.filter((r) => r.path.includes('/transcript'))).toHaveLength(0)
  })
})

/**
 * The live transcript, from the cockpit rather than from the panel in
 * isolation.
 *
 * live-transcript.test.tsx renders LiveTranscript with a call id handed to it.
 * That proves the panel works; it cannot prove the cockpit gives it the right
 * call id, or that the app shell's event stream reaches it. Every other defect
 * this week lived in exactly that gap — a component that worked, wired to
 * nothing, with no test naming the connection.
 */
describe('live transcript', () => {
  it('shows the call transcript the agent is on', async () => {
    await renderCockpit({
      ...onCall,
      transcript: [
        {
          seq: 1, occurredAt: new Date().toISOString(), speaker: 'BOT', kind: 'TEXT',
          content: { text: 'Thanks for calling NovaNet' }, offsetMs: 1000, source: 'MODEL',
          utteranceId: 'u-1',
        },
      ],
    })
    expect(await screen.findByText(/thanks for calling novanet/i)).toBeInTheDocument()
  })

  it('renders a line that arrives on the event stream', async () => {
    await renderCockpit(onCall)
    // Wait until the cockpit actually has the call, not merely the panel
    // title: the panel renders before /calls/mine resolves.
    await screen.findByRole('group', { name: /call controls/i })

    emitEvent('CALL_TRANSCRIPT', {
      type: 'CALL_TRANSCRIPT',
      callId: CALL_ID,
      payload: {
        utteranceId: 'u-9', speaker: 'CUSTOMER', kind: 'TEXT', isFinal: true,
        seq: 9, text: 'my account number is 4471', source: 'ASR',
      },
    })

    expect(await screen.findByText(/my account number is 4471/i)).toBeInTheDocument()
  })

  it('shows the state the server reports, not one it invents', async () => {
    await renderCockpit({ ...onCall, transcriptState: 'LIVE' })
    expect(await screen.findByText(/transcribing…/i)).toBeInTheDocument()

    emitEvent('CALL_TRANSCRIPTION_STATE', {
      type: 'CALL_TRANSCRIPTION_STATE',
      callId: CALL_ID,
      payload: { state: 'DEGRADED', reason: 'ASR_SESSION_FAILED' },
    })
    expect(await screen.findByText(/transcribing one side/i)).toBeInTheDocument()
  })

  it('keeps the state the stream reported when a staler snapshot arrives', async () => {
    // The snapshot is a past answer: it is requested when the call id appears
    // and lands a round trip later. On a live call the server published
    // CONNECTING at tap attach and LIVE 199ms after; the snapshot was taken
    // inside that window and, on arrival, overwrote LIVE. The panel then sat
    // on "Connecting…" for the whole call with the transcript running fine
    // underneath it — which is exactly what the agent reported seeing.
    await renderCockpit({ ...onCall, transcriptState: 'CONNECTING', slowSnapshotMs: 150 })
    await screen.findByRole('group', { name: /call controls/i })

    emitEvent('CALL_TRANSCRIPTION_STATE', {
      type: 'CALL_TRANSCRIPTION_STATE',
      callId: CALL_ID,
      payload: { state: 'LIVE' },
    })
    expect(await screen.findByText(/transcribing…/i)).toBeInTheDocument()

    // The stale snapshot lands after the event and must not move it back.
    await new Promise((resolve) => setTimeout(resolve, 300))
    expect(screen.getByText(/transcribing…/i)).toBeInTheDocument()
    expect(screen.queryByText(/connecting…/i)).not.toBeInTheDocument()
  })
})
