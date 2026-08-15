import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { SoftphoneBar } from '@/components/softphone-bar'
import {
  CALL_ID,
  CALLER,
  callFixture,
  installBackend,
  presenceFixture,
  renderWithProviders,
  type Backend,
} from '@/test/harness'

/**
 * The topbar softphone. Every assertion below is about the request that leaves
 * the browser, because that is the contract the switch sees — a control that
 * renders but no longer calls its endpoint must fail here.
 */

afterEach(() => {
  vi.unstubAllGlobals()
})

async function renderBar(backend: Partial<Backend> = {}) {
  const api = installBackend(backend)
  const view = renderWithProviders(<SoftphoneBar />)
  await screen.findByRole('button', { name: /ready|break|login|on call|sign in/i })
  return { api, ...view }
}

/** Opens the presence menu and picks one item. */
async function pickPresence(
  user: ReturnType<typeof renderWithProviders>['user'],
  itemName: RegExp,
) {
  const trigger = screen.getAllByRole('button')[0]
  await user.click(trigger)
  await user.click(await screen.findByRole('menuitem', { name: itemName }))
}

describe('presence', () => {
  it('signs in when signed out', async () => {
    const { api, user } = await renderBar({
      presence: presenceFixture({ state: 'LOGGED_OUT', availability: 'LOGGED_OUT' }),
    })
    await user.click(screen.getByRole('button', { name: /sign in/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/login' }),
      ),
    )
  })

  it('goes ready from the menu', async () => {
    const { api, user } = await renderBar({
      presence: presenceFixture({ state: 'NOT_READY', availability: 'NOT_READY', reason: 'BREAK' }),
    })
    await pickPresence(user, /go ready/i)
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/ready' }),
      ),
    )
  })

  it('goes not-ready with the chosen reason', async () => {
    const { api, user } = await renderBar()
    await pickPresence(user, /^break$/i)
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: '/agent/not-ready',
          body: { reason: 'BREAK' },
        }),
      ),
    )
  })

  it('signs out from the menu', async () => {
    const { api, user } = await renderBar()
    await pickPresence(user, /sign out/i)
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/logout' }),
      ),
    )
  })

  it('puts the state colour on the dot, not on the label', async () => {
    await renderBar()
    const trigger = screen.getAllByRole('button')[0]
    const dot = trigger.querySelector('span[style*="background-color"]')
    expect(dot).not.toBeNull()
    // No descendant may paint the label text with a state colour.
    expect(trigger.querySelector('span[style*="color:"]:not([style*="background-color"])')).toBeNull()
  })
})

describe('call controls', () => {
  it('answers a ringing call', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('RINGING')] })
    await user.click(await screen.findByRole('button', { name: /^answer$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/answer` }),
      ),
    )
  })

  it('holds an established call', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('TALKING')] })
    await user.click(await screen.findByRole('button', { name: /^hold$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/hold` }),
      ),
    )
  })

  it('retrieves a held call', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('HELD')] })
    await user.click(await screen.findByRole('button', { name: /resume/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/retrieve` }),
      ),
    )
  })

  it('transfers to a typed destination', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('TALKING')] })
    await user.click(await screen.findByRole('button', { name: /^transfer$/i }))
    const menu = await screen.findByRole('menu')
    await user.type(within(menu).getByLabelText(/extension/i), '3001')
    await user.click(within(menu).getByRole('button', { name: /^transfer$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: `/calls/${CALL_ID}/transfer`,
          body: { destination: '3001' },
        }),
      ),
    )
  })

  it('hangs up', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('TALKING')] })
    await user.click(await screen.findByRole('button', { name: /hang up/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/hangup` }),
      ),
    )
  })

  it('shows the caller and the direction', async () => {
    await renderBar({ calls: [callFixture('TALKING')] })
    expect(await screen.findByText(CALLER)).toBeInTheDocument()
  })

  it('mutes the agent', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('TALKING')] })
    await user.click(await screen.findByRole('button', { name: /^mute$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/mute` }),
      ),
    )
  })

  it('offers unmute once the switch reports the leg muted', async () => {
    const muted = callFixture('TALKING')
    muted.parties[1].isMuted = true
    const { api, user } = await renderBar({ calls: [muted] })
    await user.click(await screen.findByRole('button', { name: /^unmute$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/unmute` }),
      ),
    )
  })

  it('cannot mute with no call to mute', async () => {
    await renderBar()
    expect(await screen.findByRole('button', { name: /^mute$/i })).toBeDisabled()
  })

  it('hangup is inert with no call', async () => {
    await renderBar()
    expect(await screen.findByRole('button', { name: /hang up/i })).toBeDisabled()
  })
})

describe('dialler', () => {
  it('composes a number on the keypad and dials it', async () => {
    const { api, user } = await renderBar()
    await user.click(await screen.findByRole('button', { name: /keypad/i }))
    const pad = await screen.findByRole('dialog')
    const press = (digit: string) =>
      user.click(within(pad).getByRole('button', { name: digit }))
    await press('9')
    await press('5')
    await press('0')
    await press('1')
    await press('1')
    expect(within(pad).getByLabelText(/customer number/i)).toHaveValue('95011')
    await user.click(within(pad).getByRole('button', { name: /^dial$/i }))
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

  it('sends tones instead of dialling while a call is up', async () => {
    const { api, user } = await renderBar({ calls: [callFixture('TALKING')] })
    await user.click(await screen.findByRole('button', { name: /keypad/i }))
    const pad = await screen.findByRole('dialog')
    await user.click(within(pad).getByRole('button', { name: '5' }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          path: `/calls/${CALL_ID}/dtmf`,
          body: { digits: '5' },
        }),
      ),
    )
    // No second call may be placed from inside a live one.
    expect(within(pad).queryByRole('button', { name: /^dial$/i })).toBeNull()
    expect(api.commands.some((c) => c.path === '/calls/dial')).toBe(false)
  })
})
