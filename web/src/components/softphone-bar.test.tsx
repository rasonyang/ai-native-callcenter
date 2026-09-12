import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi, type MockInstance } from 'vitest'

import { SoftphoneBar } from '@/components/softphone-bar'
import { usePresence } from '@/lib/agent'
import { PhoneBridgeProvider, usePhoneBridgeValue } from '@/lib/phone-bridge'
import { useLogout, useSession } from '@/lib/session'
import type { ExtensionState } from '@/lib/phone-bridge'
import {
  AGENT_ID,
  CALL_ID,
  CALLER,
  callFixture,
  installBackend,
  identityFixture,
  installFakeExtension,
  presenceFixture,
  renderWithProviders,
  type Backend,
  type FakeExtension,
} from '@/test/harness'

/**
 * The topbar softphone. Every assertion below is about the request that leaves
 * the browser, because that is the contract the switch sees — a control that
 * renders but no longer calls its endpoint must fail here.
 */

/** When a spied call happened, relative to every other spied call. */
function orderOf(spy: MockInstance, matches: (call: unknown[]) => boolean): number | null {
  const index = spy.mock.calls.findIndex((call) => matches(call as unknown[]))
  return index === -1 ? null : spy.mock.invocationCallOrder[index]
}

let extension: FakeExtension | undefined

afterEach(() => {
  extension?.uninstall()
  extension = undefined
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  delete document.documentElement.dataset.webSipPhone
})

/** The bar as it is mounted in the app shell: below the phone bridge. */
function Bar() {
  const { data: presence } = usePresence(true)
  const phone = usePhoneBridgeValue(true, presence?.extensionNumber)
  return (
    <PhoneBridgeProvider value={phone}>
      <SoftphoneBar />
    </PhoneBridgeProvider>
  )
}

/**
 * Renders the bar into the ordinary world: an agent whose phone extension is
 * installed, provisioned and registered at their own extension. A test about
 * a phone that is not there passes `extension: false` or a state of its own.
 */
async function renderBar(
  backend: Partial<Backend> & {
    extension?: false | Partial<ExtensionState>
    /** The id the extension announces, when the test is about a link. */
    extensionId?: string
  } = {},
) {
  const { extension: phone, extensionId, ...rest } = backend
  const api = installBackend(rest)
  if (phone !== false) {
    extension = installFakeExtension({ state: phone ?? {}, extensionId })
  }
  const view = renderWithProviders(<Bar />)
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

/**
 * A call this agent placed themselves, and an internal call where the other
 * party is an agent too. The bar used to read the first party carrying an
 * agentId as "me", and to treat DIALING as ringing — together that put an
 * Answer button on the screen of whoever had just dialled, and showed the
 * person being rung their caller's leg instead of their own.
 */
const OTHER_AGENT_ID = '00000000-0000-4000-8000-0000000000a2'

function placedByThisAgent() {
  const call = callFixture('DIALING')
  return {
    ...call,
    callType: 'INTERNAL' as const,
    parties: [{ ...call.parties[1], role: 'ORIGINATOR' as const, number: '1008', otherNumber: '1002' }],
  }
}

function rungByAnotherAgent() {
  const call = callFixture('RINGING')
  const at = new Date().toISOString()
  return {
    ...call,
    callType: 'INTERNAL' as const,
    parties: [
      // The placer first, which is what made the naive lookup pick them.
      { partyId: 'pa', channelId: 'placer', role: 'ORIGINATOR' as const, state: 'DIALING' as const,
        number: '1002', otherNumber: '1008', agentId: OTHER_AGENT_ID, createdAt: at },
      { partyId: 'pb', channelId: 'callee', role: 'TARGET' as const, state: 'RINGING' as const,
        number: '1008', otherNumber: '1002', agentId: AGENT_ID, createdAt: at },
    ],
  }
}

describe('who the bar thinks it belongs to', () => {
  it('offers nothing to answer on a call this agent placed', async () => {
    await renderBar({ calls: [placedByThisAgent()], presence: presenceFixture({ availability: 'ON_CALL' }) })
    await screen.findByText(/1002/)
    expect(screen.queryByRole('button', { name: /^answer$/i })).not.toBeInTheDocument()
  })

  it('answers on behalf of the agent being rung, not the one dialling', async () => {
    const { api, user } = await renderBar({
      calls: [rungByAnotherAgent()],
      presence: presenceFixture({ availability: 'ON_CALL' }),
    })
    await user.click(await screen.findByRole('button', { name: /^answer$/i }))
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: `/calls/${CALL_ID}/answer` }),
      ),
    )
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
          path: '/calls',
          body: { kind: 'AGENT_OUTBOUND', to: '95011' },
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
    expect(api.commands.some((c) => c.method === 'POST' && c.path === '/calls')).toBe(false)
  })

  // On a call the agent placed, the number they dialled is on their own leg;
  // reading only "the other party" left the bar saying Unknown number for a
  // call whose panels named it correctly (seen live).
  it('names the number on a call the agent placed', async () => {
    const call = callFixture('TALKING')
    await renderBar({
      calls: [
        {
          ...call,
          callType: 'INTERNAL',
          parties: [
            {
              ...call.parties[1],
              role: 'ORIGINATOR',
              number: '1008',
              otherNumber: '1007',
              agentId: AGENT_ID,
            },
          ],
        },
      ],
    })
    expect(await screen.findByText('1007')).toBeInTheDocument()
    expect(screen.queryByText(/unknown number/i)).toBeNull()
  })
})

/**
 * The phone the bar reports on.
 *
 * An agent types no credentials anywhere: signing in mints a SIP session and
 * hands it to the extension, signing out takes it back, and the chip says
 * whether the two ever met. The switch's own answer is what gates READY —
 * a queue cannot offer a call to a phone that is not registered.
 */
describe('the phone', () => {
  /**
   * A phone holding nothing is what earns a session — not the page loading.
   * The extension keeps provisioned credentials across a reload, and minting
   * flushes the registration it already has, which would drop a READY agent
   * to DEVICE_LOST for pressing F5.
   */
  it('provisions a phone that is holding nothing', async () => {
    const postMessage = vi.spyOn(window, 'postMessage')
    const { api } = await renderBar({
      extension: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    await waitFor(() =>
      expect(api.commands).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/agent/sip-session' }),
      ),
    )
    await waitFor(() =>
      expect(
        postMessage.mock.calls.some(
          ([message]) => (message as { type?: string }).type === 'provision',
        ),
      ).toBe(true),
    )
    const [provision, targetOrigin] = postMessage.mock.calls.find(
      ([message]) => (message as { type?: string }).type === 'provision',
    )!
    expect(provision).toMatchObject({ source: 'aicc', protocolVersion: 1, account: '1001' })
    expect(targetOrigin).toBe(window.location.origin)
  })

  it('provisions nothing for a phone that reloaded with its credentials', async () => {
    const { api } = await renderBar()
    await screen.findByText(/phone ready/i)
    expect(api.commands.some((r) => r.path === '/agent/sip-session')).toBe(false)
  })

  it('names the extension it is registered at', async () => {
    await renderBar()
    const chip = await screen.findByText(/phone ready/i)
    expect(chip.closest('span')).toHaveTextContent('Phone ready · 1001')
  })

  it('offers setup instead when no extension answers', async () => {
    await renderBar({ extension: false })
    expect(await screen.findByRole('button', { name: /set up phone/i })).toBeInTheDocument()
  })

  it('will not let an agent go ready with no registration on the switch', async () => {
    const { user } = await renderBar({
      presence: presenceFixture({
        state: 'NOT_READY',
        availability: 'NOT_READY',
        reason: 'BREAK',
        isDeviceRegistered: false,
        deviceAccount: null,
      }),
      extension: { registration: 'UNREGISTERED', credentialSource: 'NONE', account: null },
    })
    await user.click(
      screen.getAllByRole('button').find((b) => b.getAttribute('aria-haspopup') === 'menu')!,
    )
    const goReady = await screen.findByRole('menuitem', { name: /go ready/i })
    expect(goReady).toHaveAttribute('data-disabled')
  })

  it('lets an agent whose phone is registered go ready', async () => {
    const { user } = await renderBar({
      presence: presenceFixture({ state: 'NOT_READY', availability: 'NOT_READY', reason: 'BREAK' }),
    })
    await user.click(
      screen.getAllByRole('button').find((b) => b.getAttribute('aria-haspopup') === 'menu')!,
    )
    const goReady = await screen.findByRole('menuitem', { name: /go ready/i })
    expect(goReady).not.toHaveAttribute('data-disabled')
  })

  /**
   * Somebody saved a manual account in the extension's Options while a
   * provisioned session was held. The phone works; it is just not using what
   * this page gave it, and the way back is that same Options page.
   */
  it('says when the extension is running on a manual override', async () => {
    await renderBar({
      extensionId: 'ponmlkjihgfedcbaponmlkjihgfedcba',
      extension: {
        credentialSource: 'MANUAL',
        account: '1001',
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    expect(await screen.findByText(/manual override/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /extension options/i })).toHaveAttribute(
      'href',
      'chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/options.html#account',
    )
    // Replacing the credential is exactly what must not be offered here.
    expect(screen.queryByRole('button', { name: /re-provision|retry/i })).toBeNull()
  })

  it('still lets an overridden phone at this agent’s own extension go ready', async () => {
    const { user } = await renderBar({
      presence: presenceFixture({ state: 'NOT_READY', availability: 'NOT_READY', reason: 'BREAK' }),
      extension: {
        credentialSource: 'MANUAL',
        account: '1001',
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    await user.click(
      screen.getAllByRole('button').find((b) => b.getAttribute('aria-haspopup') === 'menu')!,
    )
    expect(await screen.findByRole('menuitem', { name: /go ready/i })).not.toHaveAttribute(
      'data-disabled',
    )
  })

  it('keeps an override onto another extension out of the queue', async () => {
    const { user } = await renderBar({
      presence: presenceFixture({ state: 'NOT_READY', availability: 'NOT_READY', reason: 'BREAK' }),
      extension: {
        credentialSource: 'MANUAL',
        account: '1002',
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    await user.click(
      screen.getAllByRole('button').find((b) => b.getAttribute('aria-haspopup') === 'menu')!,
    )
    expect(await screen.findByRole('menuitem', { name: /go ready/i })).toHaveAttribute(
      'data-disabled',
    )
  })

  // The platform takes an agent out of the queue when their phone goes away,
  // and the reason reads like every other reason it sets.
  it('names the reason when a lost phone is what made them not-ready', async () => {
    installBackend({
      presence: presenceFixture({
        state: 'NOT_READY',
        availability: 'NOT_READY',
        reason: 'DEVICE_LOST',
        isDeviceRegistered: false,
        deviceAccount: null,
      }),
    })
    extension = installFakeExtension({
      state: { registration: 'UNREGISTERED', account: null, credentialSource: 'NONE' },
    })
    renderWithProviders(<Bar />)
    expect(await screen.findByRole('button', { name: /phone lost/i })).toBeInTheDocument()
  })

  it('says so when the account has no extension bound to it', async () => {
    await renderBar({
      sipSession: null,
      presence: presenceFixture({ isDeviceRegistered: false, deviceAccount: null }),
      extension: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    expect(await screen.findByText(/no extension is bound/i)).toBeInTheDocument()
  })
})

// The shell's own wiring: only an agent's page talks to a phone, and the
// context is provided to everybody below either way.
function SignOutProbe() {
  const { data: user } = useSession()
  const phone = usePhoneBridgeValue(user?.role === 'AGENT')
  return (
    <PhoneBridgeProvider value={phone}>
      <LogoutButton />
    </PhoneBridgeProvider>
  )
}

function LogoutButton() {
  const { data: user } = useSession()
  const logout = useLogout()
  return (
    <button type="button" disabled={!user} onClick={() => logout.mutate()}>
      sign out
    </button>
  )
}


/**
 * Signing out of the web session signs the phone out with it: the extension is
 * told to drop the credentials, and the session ends.
 *
 * Revoking the SIP session is the server's half, done after the agent has been
 * signed out of presence. The browser did it itself once, and flushing the
 * registration while the agent was still READY put a DEVICE_LOST in the record
 * of every clean sign-out.
 */
describe('signing out of everything', () => {
  it('deprovisions the phone before it ends the web session', async () => {
    const api = installBackend()
    extension = installFakeExtension()
    const postMessage = vi.spyOn(window, 'postMessage')
    const { user } = renderWithProviders(<SignOutProbe />)
    await waitFor(() => expect(screen.getByRole('button')).toBeEnabled())

    await user.click(screen.getByRole('button'))
    await waitFor(() =>
      expect(api.requests).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/auth/logout' }),
      ),
    )

    const deprovisionAt = orderOf(postMessage, (call) =>
      (call[0] as { type?: string }).type === 'deprovision',
    )
    expect(deprovisionAt).not.toBeNull()
    const logoutAt = orderOf(globalThis.fetch as unknown as MockInstance, (call) =>
      String(call[0]).endsWith('/auth/logout'),
    )
    expect(deprovisionAt!).toBeLessThan(logoutAt!)
  })

  // The registration is the server's to flush, and only after it has signed
  // the agent out. A DELETE from here reported a phone lost by an agent who
  // was still READY.
  it('revokes no SIP session itself', async () => {
    const api = installBackend()
    extension = installFakeExtension()
    const { user } = renderWithProviders(<SignOutProbe />)
    await waitFor(() => expect(screen.getByRole('button')).toBeEnabled())
    await user.click(screen.getByRole('button'))
    await waitFor(() =>
      expect(api.requests).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/auth/logout' }),
      ),
    )
    expect(api.requests.some((r) => r.method === 'DELETE')).toBe(false)
  })

  it('signs out an account that has no phone at all', async () => {
    const api = installBackend({ session: identityFixture({ role: 'SUPERVISOR' }) })
    const { user } = renderWithProviders(<SignOutProbe />)
    await waitFor(() => expect(screen.getByRole('button')).toBeEnabled())
    await user.click(screen.getByRole('button'))
    await waitFor(() =>
      expect(api.requests).toContainEqual(
        expect.objectContaining({ method: 'POST', path: '/auth/logout' }),
      ),
    )
    expect(api.requests.some((r) => r.path === '/agent/sip-session')).toBe(false)
  })
})
