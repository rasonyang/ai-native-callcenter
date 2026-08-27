import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.index'
import {
  CALL_ID,
  CALLER,
  callFixture,
  cdrFixture,
  contactFixture,
  dispositionsFixture,
  openWrapUpFixture,
  todayFixture,
  installBackend,
  presenceFixture,
  renderPage,
  emitEvent,
  waitingFixture,
  AGENT_ID,
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
  it('shows the five controls, every one of them wired', async () => {
    await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    for (const name of [/^mute$/i, /^hold$/i, /^transfer$/i, /^keypad$/i, /hang up/i]) {
      expect(within(grid).getByRole('button', { name })).toBeVisible()
    }
    expect(within(grid).getAllByRole('button')).toHaveLength(5)
  })

  // The grid used to carry a sixth, disabled key for conferencing. The product
  // does not want conferencing, so it is not a gap being tracked — it is a
  // control that should never have been drawn.
  it('offers nothing the agent cannot use', async () => {
    await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    const disabled = within(grid)
      .getAllByRole('button')
      .filter((b) => (b as HTMLButtonElement).disabled)
    expect(disabled).toEqual([])
    expect(within(grid).queryByRole('button', { name: /conference/i })).toBeNull()
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

  // One extension calling another is two people on a line, not a call being
  // handled: nowhere to pass it to and no queue to put it back into. The
  // server refuses these, so the keys must not offer them.
  it('darkens hold and transfer on a call between two extensions', async () => {
    const internal = callFixture('TALKING')
    internal.callType = 'INTERNAL'
    await renderCockpit({ calls: [internal] })
    const grid = await screen.findByRole('group', { name: /call controls/i })

    expect(within(grid).getByRole('button', { name: /^hold$/i })).toBeDisabled()
    expect(within(grid).getByRole('button', { name: /^transfer$/i })).toBeDisabled()
    // Muting yourself and hanging up still make sense on any call.
    expect(within(grid).getByRole('button', { name: /^mute$/i })).toBeEnabled()
  })

  it('leaves hold and transfer available on a call the agent is handling', async () => {
    await renderCockpit(onCall)
    const grid = await screen.findByRole('group', { name: /call controls/i })
    expect(within(grid).getByRole('button', { name: /^hold$/i })).toBeEnabled()
    expect(within(grid).getByRole('button', { name: /^transfer$/i })).toBeEnabled()
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

  // A call the agent placed is not a call to answer: their own leg is DIALING
  // while the other side rings, and offering Accept there asked them to pick
  // up their own outgoing call (seen live).
  it('offers no answer on a call the agent placed', async () => {
    await renderCockpit({ calls: [placedCall()] })
    expect(await screen.findByText(/calling out/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /accept/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /hang up/i })).toBeInTheDocument()
  })

  // Until the far end is raised the call has one leg — the agent's — and the
  // number being called is on it. Reading only the other party showed the
  // agent "Unknown number" for a number they had just typed (seen live).
  it('names the number being called before the far end exists', async () => {
    await renderCockpit({ calls: [placedCall()] })
    // Both the call panel and the customer panel name it, and neither falls
    // back to "unknown".
    expect(await screen.findAllByText('1007')).toHaveLength(2)
    expect(screen.queryByText(/unknown number/i)).not.toBeInTheDocument()
  })
})

/** A call the agent placed, still dialling: only their own leg exists. */
function placedCall() {
  const call = callFixture('DIALING')
  return {
    ...call,
    callType: 'INTERNAL' as const,
    parties: [
      {
        ...call.parties[1],
        role: 'ORIGINATOR' as const,
        number: '1008',
        otherNumber: '1007',
        agentId: AGENT_ID,
      },
    ],
  }
}

/**
 * An internal call between two agents. Both parties carry an agentId, which is
 * the case the cockpit used to get wrong: it took the first party that had one
 * as "me", and the placer is listed first, so the person being rung was shown
 * the caller's leg — Calling out, Dialling, and an Answer button that answered
 * nothing. Who "I" am has to come from the session.
 */
const OTHER_AGENT_ID = '00000000-0000-4000-8000-0000000000a2'

function agentToAgentCall() {
  const call = callFixture('RINGING')
  const at = new Date().toISOString()
  return {
    ...call,
    callType: 'INTERNAL' as const,
    queue: undefined,
    parties: [
      // The one who dialled, deliberately first in the list.
      {
        partyId: '00000000-0000-4000-8000-0000000000pa',
        channelId: 'placer',
        role: 'ORIGINATOR' as const,
        state: 'DIALING' as const,
        number: '1002',
        otherNumber: '1008',
        agentId: OTHER_AGENT_ID,
        createdAt: at,
      },
      // The one being rung — the owner of the screen under test.
      {
        partyId: '00000000-0000-4000-8000-0000000000pb',
        channelId: 'callee',
        role: 'TARGET' as const,
        state: 'RINGING' as const,
        number: '1008',
        otherNumber: '1002',
        agentId: AGENT_ID,
        createdAt: at,
      },
    ],
  }
}

describe('an internal call seen from each side', () => {
  it('shows the agent being rung their own leg, not the placer\'s', async () => {
    await renderCockpit({
      calls: [agentToAgentCall()],
      presence: presenceFixture({ agentId: AGENT_ID, availability: 'ON_CALL' }),
    })
    expect(await screen.findByText(/incoming call/i)).toBeVisible()
    expect(screen.getByText(/^ringing$/i)).toBeVisible()
    expect(screen.queryByText(/^dialling$/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/calling out/i)).not.toBeInTheDocument()
  })

  it('lets the one being rung accept, and answers their own call', async () => {
    const { api, user } = await renderCockpit({
      calls: [agentToAgentCall()],
      presence: presenceFixture({ agentId: AGENT_ID, availability: 'ON_CALL' }),
    })
    await user.click(await screen.findByRole('button', { name: /accept/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/answer` }),
      ),
    )
  })

  it('shows the placer that they are calling out, with nothing to answer', async () => {
    await renderCockpit({
      calls: [agentToAgentCall()],
      presence: presenceFixture({ agentId: OTHER_AGENT_ID, availability: 'ON_CALL' }),
    })
    expect(await screen.findByText(/calling out/i)).toBeVisible()
    expect(screen.getByText(/^dialling$/i)).toBeVisible()
    expect(screen.queryByRole('button', { name: /accept/i })).not.toBeInTheDocument()
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
          path: '/calls',
          body: { kind: 'AGENT_OUTBOUND', to: '95011' },
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
      currentWrapUp: openWrapUpFixture(),
    })
    await user.click(await screen.findByRole('button', { name: /^done$/i }))
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
    expect(screen.getByText('91%')).toBeInTheDocument()
    expect(screen.getByText('21/23')).toBeInTheDocument()
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
      currentWrapUp: openWrapUpFixture(),
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
    await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
      currentWrapUp: openWrapUpFixture(),
    })

    const codes = await screen.findByLabelText(/^disposition$/i)
    const labels = within(codes)
      .getAllByRole('option')
      .map((option) => option.textContent)
    // No empty first option: the record already carries a disposition, and
    // offering one would invite the agent to unset it.
    expect(labels).toEqual(['Resolved', 'Follow-up Required', 'No Answer', 'Other'])
  })

  // The record is opened for the agent with the ordinary outcome already
  // chosen, so the common call is one press. Nothing is required of them.
  it('arrives already filled in and takes one press', async () => {
    const { api, user } = await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
      currentWrapUp: openWrapUpFixture(),
    })

    expect(await screen.findByLabelText(/^disposition$/i)).toHaveValue('RESOLVED')
    const done = screen.getByRole('button', { name: /^done$/i })
    expect(done).toBeEnabled()

    await user.click(done)
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/wrap-up',
          body: { dispositionCode: 'RESOLVED', note: '' },
        }),
      ),
    )
  })

  // The record is the server's, so a reopened cockpit finds the work waiting
  // with whatever was already written on it.
  it('restores the open record after a reload', async () => {
    await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
      currentWrapUp: openWrapUpFixture({
        dispositionCode: 'NO_ANSWER',
        dispositionLabel: 'No Answer',
        note: 'typed before the page was refreshed',
      }),
    })

    expect(await screen.findByLabelText(/^disposition$/i)).toHaveValue('NO_ANSWER')
    expect(screen.getByLabelText(/wrap-up note/i)).toHaveValue('typed before the page was refreshed')
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
      currentWrapUp: openWrapUpFixture(),
    })

    await user.selectOptions(await screen.findByLabelText(/^disposition$/i), 'RESOLVED')
    await user.click(screen.getByRole('button', { name: /^done$/i }))

    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/wrap-up',
          body: { dispositionCode: 'RESOLVED', note: '' },
        }),
      ),
    )
  })

  // The block is guidance, and it lives on the server's answer rather than on
  // a screen's memory: reloading the page is not a way past it.
  it('holds the agent out of the next call until the record is confirmed', async () => {
    const { user } = await renderCockpit({
      presence: presenceFixture({ availability: 'READY', wrapUpCallId: CALL_ID }),
      dispositions: dispositionsFixture(),
      currentWrapUp: openWrapUpFixture(),
    })

    // Dialling out would start another conversation with this one unwritten.
    const number = await screen.findByPlaceholderText(/customer number/i)
    expect(number).toBeDisabled()
    expect(screen.getAllByText(/finish the wrap-up first/i).length).toBeGreaterThan(0)

    await user.click(screen.getByRole('button', { name: /^done$/i }))
    await waitFor(() =>
      expect(screen.getByPlaceholderText(/customer number/i)).toBeEnabled(),
    )
    expect(screen.queryByText(/finish the wrap-up first/i)).toBeNull()
  })

  it('leaves the dialler alone when nothing is waiting', async () => {
    await renderCockpit({
      presence: presenceFixture({ availability: 'READY' }),
      currentWrapUp: null,
    })
    expect(await screen.findByPlaceholderText(/customer number/i)).toBeEnabled()
    expect(screen.queryByText(/finish the wrap-up first/i)).toBeNull()
  })

  it('offers nothing to file when no call has been handled', async () => {
    await renderCockpit({ presence: presenceFixture({ availability: 'READY' }) })
    expect(await screen.findByText(/wrap-up starts when a call ends/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^done$/i })).toBeNull()
  })

  // The note is written while the agent is still talking — that is when they
  // know what to write — and it is the same note when the call ends, because
  // it is about the same conversation.
  it('takes the note during the call and keeps it when the call ends', async () => {
    const { api, user, refetch } = await renderCockpit({
      ...onCall,
      dispositions: dispositionsFixture(),
    })

    const note = await screen.findByLabelText(/wrap-up note/i)
    await user.type(note, 'promised to email the invoice')
    // The outcome belongs to a finished call, so it is not offered yet.
    expect(screen.getByLabelText(/^disposition$/i)).toBeDisabled()
    expect(screen.getByRole('button', { name: /^done$/i })).toBeDisabled()

    // The caller hangs up. The two facts do not land together on a live
    // system: for about a second the call is gone and presence has not yet
    // said "after-call work", and the note must survive that gap.
    api.calls = []
    await refetch()
    expect(screen.getByLabelText(/wrap-up note/i)).toHaveValue('promised to email the invoice')

    api.presence = inWrapUp()
    await refetch()

    await waitFor(() => expect(screen.getByLabelText(/^disposition$/i)).toBeEnabled())
    expect(screen.getByLabelText(/wrap-up note/i)).toHaveValue('promised to email the invoice')

    await user.selectOptions(screen.getByLabelText(/^disposition$/i), 'RESOLVED')
    await user.click(screen.getByRole('button', { name: /^done$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/wrap-up',
          body: { dispositionCode: 'RESOLVED', note: 'promised to email the invoice' },
        }),
      ),
    )
  })

  // An agent can be on a new call while the last one is still unfiled: a
  // direct call reaches them in after-call work. Filing then would put this
  // conversation's note on the previous conversation.
  it('will not file the previous call while a new one is in progress', async () => {
    await renderCockpit({
      calls: [callFixture('TALKING')],
      presence: presenceFixture({
        availability: 'ON_CALL',
        wrapUpCallId: '00000000-0000-4000-8000-0000000000c9',
      }),
      dispositions: dispositionsFixture(),
    })

    // The snapshots do not land together: presence knows about the unfiled
    // call before /calls/mine reports the live one.
    await waitFor(() => expect(screen.getByLabelText(/^disposition$/i)).toBeDisabled())
    expect(screen.getByRole('button', { name: /^done$/i })).toBeDisabled()
  })

  // Once confirmed, the record itself says so, and the card reads it back.
  it('shows what was confirmed as soon as the record says so', async () => {
    const { user } = await renderCockpit({
      presence: inWrapUp(),
      dispositions: dispositionsFixture(),
      currentWrapUp: openWrapUpFixture(),
      myCDRs: [],
    })

    await user.selectOptions(await screen.findByLabelText(/^disposition$/i), 'NO_ANSWER')
    await user.type(screen.getByLabelText(/wrap-up note/i), 'nobody there')
    await user.click(screen.getByRole('button', { name: /^done$/i }))

    expect(await screen.findByText('No Answer')).toBeInTheDocument()
    expect(screen.getByText('nobody there')).toBeInTheDocument()
    expect(screen.queryByLabelText(/wrap-up note/i)).toBeNull()
  })

  // Finished work is a record, not a form: what was filed is shown back and
  // the next call gives the agent a fresh sheet.
  it('shows the last filing read-only once the work is done', async () => {
    await renderCockpit({
      presence: presenceFixture({ availability: 'READY' }),
      myCDRs: [
        cdrFixture({
          wrapUp: {
            agentId: '00000000-0000-4000-8000-0000000000a1',
            dispositionCode: 'FOLLOW_UP_REQUIRED',
            dispositionLabel: 'Follow-up Required',
            note: 'calling them back tomorrow',
            isConfirmed: true,
            createdAt: new Date().toISOString(),
          },
        }),
      ],
    })

    expect(await screen.findByText('Follow-up Required')).toBeInTheDocument()
    expect(screen.getByText('calling them back tomorrow')).toBeInTheDocument()
    expect(screen.queryByLabelText(/wrap-up note/i)).toBeNull()
    expect(screen.queryByRole('button', { name: /^done$/i })).toBeNull()
  })
})

/**
 * My queue: the line this agent is working. The platform decides which queues
 * those are — the request carries none — and the wait turns red against the
 * queue's own promise rather than a number invented in the browser.
 */
describe('my queue', () => {
  // The queues are the rows, not the callers. An agent staffing a quiet line
  // and an agent staffing none at all both have nobody waiting; listing only
  // the waiting callers showed them the same empty card, and the second only
  // found out they were on no queue when a call never arrived.
  it('lists the queues it works, with how many are waiting in each', async () => {
    await renderCockpit({
      waiting: [
        waitingFixture({ fromNumber: '+8613700990011', queueDisplayName: 'Billing' }),
        waitingFixture({
          callId: '00000000-0000-4000-8000-0000000000w2',
          fromNumber: '+14085550166',
          queueId: '00000000-0000-4000-8000-0000000000q2',
          queueName: 'support-en',
          queueDisplayName: 'Support EN',
          joinedAt: new Date(Date.now() - 47_000).toISOString(),
        }),
      ],
    })

    const list = await screen.findByRole('list', { name: /my queue/i })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('Billing')
    expect(rows[0]).toHaveTextContent('1')
    expect(rows[1]).toHaveTextContent('Support EN')
    expect(screen.getByText('2 waiting')).toBeInTheDocument()
  })

  it('shows a queue nobody is waiting in as zero, not as an absence', async () => {
    await renderCockpit({
      waiting: [],
      staffedQueues: [
        {
          queueId: '00000000-0000-4000-8000-0000000000q1',
          name: 'support-zh',
          displayName: 'Billing',
          slaThresholdSec: 20,
        },
      ],
    })

    const list = await screen.findByRole('list', { name: /my queue/i })
    const row = within(list).getByRole('listitem')
    expect(row).toHaveTextContent('Billing')
    expect(row).toHaveTextContent('0')
    expect(screen.getByText('0 waiting')).toBeInTheDocument()
  })

  it('marks a wait past the queue\u2019s own target', async () => {
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

  it('says so when the agent is on no queue at all', async () => {
    await renderCockpit({ waiting: [], staffedQueues: [] })
    expect(await screen.findByText(/not on any queue/i)).toBeInTheDocument()
  })
})

/**
 * The caller card names the person where the book knows them. The lookup is by
 * exact number: greeting a customer by somebody else's name is worse than
 * greeting an unknown number.
 */
describe('the caller card', () => {
  // The customer stays on the card after they hang up: the agent is still
  // working that call, and a card that emptied itself at the hangup would take
  // the person away mid-sentence.
  it('keeps the caller after the call ends, with when they were last spoken to', async () => {
    const contact = contactFixture({
      phoneNumber: '+8613700990011',
      lastCallAt: new Date('2026-07-30T09:12:00Z').toISOString(),
    })
    await renderCockpit({
      calls: [],
      contacts: [contact],
      myCDRs: [cdrFixture({ fromNumber: '+8613700990011' })],
    })

    expect(await screen.findByText('Zhang Wei')).toBeInTheDocument()
    expect(screen.getByText(/last contact/i)).toBeInTheDocument()
    expect(screen.getByText(/jul 30, 2026/i)).toBeInTheDocument()
  })

  it('says so when there is nobody to show at all', async () => {
    await renderCockpit({ calls: [], contacts: [], myCDRs: [] })
    expect(await screen.findByText(/no caller identified yet/i)).toBeInTheDocument()
  })


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
    expect(within(grid).getAllByRole('button')).toHaveLength(5)
    for (const name of [/^mute$/i, /^hold$/i, /^transfer$/i, /^keypad$/i, /hang up/i]) {
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
