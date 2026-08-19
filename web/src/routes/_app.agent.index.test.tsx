import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.index'
import {
  CALL_ID,
  CALLER,
  callFixture,
  contactFixture,
  dispositionsFixture,
  todayFixture,
  installBackend,
  presenceFixture,
  renderPage,
  emitEvent,
  waitingFixture,
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

  it('completes wrap-up', async () => {
    const { api, user } = await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
    })
    await user.selectOptions(await screen.findByLabelText(/^disposition$/i), 'RESOLVED')
    await user.click(screen.getByRole('button', { name: /^done$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/wrap-up' }),
      ),
    )
  })

  it('shows the agent their own day', async () => {
    await renderCockpit({
      today: todayFixture({ callsHandled: 23, avgHandleSec: 276, avgWrapUpSec: 42, occupancyPct: 78 }),
    })
    expect(await screen.findByText('Today')).toBeInTheDocument()
    expect(await screen.findByText('23')).toBeInTheDocument()
    // Durations read as a supervisor's report would write them, unpadded.
    expect(screen.getByText('4:36')).toBeInTheDocument()
    expect(screen.getByText('0:42')).toBeInTheDocument()
    expect(screen.getByText('78%')).toBeInTheDocument()
  })
})

/** An agent in after-call work for the call they just finished. */
function inWrapUp(overrides: Partial<ReturnType<typeof presenceFixture>> = {}) {
  return presenceFixture({
    state: 'NOT_READY',
    availability: 'WRAP_UP',
    reason: 'AFTER_CALL_WORK',
    wrapUpCallId: CALL_ID,
    ...overrides,
  })
}

/**
 * After-call work is where a call becomes reportable. The form files against
 * the call the *server* says it was for — nothing in the request names one —
 * and it stays available after the timer has run out, because an agent still
 * typing has not forfeited what they typed.
 */
describe('after-call work', () => {
  it('files the disposition and the note, naming no call', async () => {
    const { api, user } = await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
    })

    await user.selectOptions(await screen.findByLabelText(/^disposition$/i), 'FOLLOW_UP_REQUIRED')
    await user.type(screen.getByLabelText(/wrap-up note/i), 'calling them back tomorrow')
    await user.click(screen.getByRole('button', { name: /^done$/i }))

    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/wrap-up',
          body: { dispositionCode: 'FOLLOW_UP_REQUIRED', note: 'calling them back tomorrow' },
        }),
      ),
    )
  })

  // The four the platform offers, in its order, with nothing invented here.
  it('offers the vocabulary the server serves', async () => {
    await renderCockpit({ presence: inWrapUp(), dispositions: dispositionsFixture() })

    const codes = await screen.findByLabelText(/^disposition$/i)
    const labels = within(codes)
      .getAllByRole('option')
      .map((option) => option.textContent)
    expect(labels).toEqual([
      expect.stringMatching(/disposition…/i),
      'Resolved',
      'Follow-up Required',
      'No Answer',
      'Other',
    ])
  })

  // The disposition is required, so there is nothing to press until one is
  // chosen — the server refuses without it either way.
  it('will not finish until a disposition is chosen', async () => {
    const { api, user } = await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
    })

    const done = await screen.findByRole('button', { name: /^done$/i })
    expect(done).toBeDisabled()

    await user.type(screen.getByLabelText(/wrap-up note/i), 'a note alone is not a filing')
    expect(screen.getByRole('button', { name: /^done$/i })).toBeDisabled()
    expect(api.commands).toHaveLength(0)

    await user.selectOptions(screen.getByLabelText(/^disposition$/i), 'OTHER')
    expect(screen.getByRole('button', { name: /^done$/i })).toBeEnabled()
  })

  // The timer counts up from when the call ended, because nothing is going to
  // take the decision off the agent — there is no deadline to count down to.
  it('counts the time in after-call work upwards', async () => {
    await renderCockpit({
      presence: inWrapUp({ enteredAt: new Date(Date.now() - 42_000).toISOString() }),
      dispositions: dispositionsFixture(),
    })
    expect(await screen.findByText('00:42')).toBeInTheDocument()
  })

  it('still accepts a filing after the agent has moved on', async () => {
    // Something took them out of after-call work — their own choice, a
    // supervisor — while they were still typing. The call stays theirs to file
    // against until the next one is wrapped.
    const { api, user } = await renderCockpit({
      presence: presenceFixture({ availability: 'READY', wrapUpCallId: CALL_ID }),
      dispositions: dispositionsFixture(),
    })

    await user.selectOptions(await screen.findByLabelText(/^disposition$/i), 'RESOLVED')
    await user.click(screen.getByRole('button', { name: /^done$/i }))

    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/wrap-up',
          body: { dispositionCode: 'RESOLVED' },
        }),
      ),
    )
  })

  it('offers nothing to file when no call has been handled', async () => {
    await renderCockpit({ presence: presenceFixture({ availability: 'READY' }) })
    expect(await screen.findByText(/wrap-up starts when a call ends/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^done$/i })).toBeNull()
  })
})

/**
 * My queue: the line this agent is working. The platform decides which queues
 * those are — the request carries none — and the wait turns red against the
 * queue's own promise rather than a number invented in the browser.
 */
describe('my queue', () => {
  it('lists who is waiting, longest wait first, with their queue', async () => {
    await renderCockpit({
      waiting: [
        waitingFixture({ fromNumber: '+8613700990011', queueDisplayName: 'Billing' }),
        waitingFixture({
          callId: '00000000-0000-4000-8000-0000000000w2',
          fromNumber: '+14085550166',
          queueDisplayName: 'Support EN',
          joinedAt: new Date(Date.now() - 47_000).toISOString(),
        }),
      ],
    })

    const list = await screen.findByRole('list', { name: /my queue/i })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('+8613700990011')
    expect(rows[0]).toHaveTextContent('Billing')
    expect(screen.getByText('2 waiting')).toBeInTheDocument()
  })

  it('marks a wait past the queue’s own target', async () => {
    await renderCockpit({
      waiting: [
        waitingFixture({
          slaThresholdSec: 20,
          joinedAt: new Date(Date.now() - 137_000).toISOString(),
        }),
      ],
    })
    const list = await screen.findByRole('list', { name: /my queue/i })
    const wait = within(list).getByText('02:17')
    expect(wait).toHaveStyle({ color: 'var(--state-breach)' })
  })

  it('says so when nobody is waiting', async () => {
    await renderCockpit({ waiting: [] })
    expect(await screen.findByText(/nobody is waiting in your queues/i)).toBeInTheDocument()
  })
})

/**
 * The caller card names the person where the book knows them. The lookup is by
 * exact number: greeting a customer by somebody else's name is worse than
 * greeting an unknown number.
 */
describe('the caller card', () => {
  it('shows the contact behind the number', async () => {
    await renderCockpit({ ...onCall, contacts: [contactFixture()] })

    expect(await screen.findByText('Zhang Wei')).toBeInTheDocument()
    expect(screen.getByText(/novanet/i)).toBeInTheDocument()
    expect(screen.getByText('VIP')).toBeInTheDocument()
    expect(screen.getByText(/prefers callbacks after 16:00/i)).toBeInTheDocument()
  })

  it('falls back to the number for a caller nobody has recorded', async () => {
    await renderCockpit({ ...onCall, contacts: [contactFixture({ phoneNumber: '+10000000000' })] })

    expect(await screen.findAllByText(CALLER)).not.toHaveLength(0)
    expect(screen.queryByText('Zhang Wei')).toBeNull()
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
