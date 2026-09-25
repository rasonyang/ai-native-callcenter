import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { PhoneOnboarding } from '@/components/phone-onboarding'
import {
  PHONE_SEEN_STORAGE_KEY, PhoneBridgeProvider, usePhoneBridgeValue, type PhoneBridge,
} from '@/lib/phone-bridge'
import {
  installBackend, installFakeExtension, renderWithProviders, type FakeExtension,
} from '@/test/harness'

/**
 * The setup card.
 *
 * Every row reads the extension's own state, so nothing here is a checkbox
 * the agent ticks: installing it completes step one, and the extension
 * answering at all completes step two, because a content script only runs on
 * a site its owner allowed. The card leaves when the last row completes and
 * nothing else closes it.
 */

afterEach(() => {
  vi.unstubAllGlobals()
  vi.unstubAllEnvs()
  delete document.documentElement.dataset.webSipPhone
  // Having met the extension once is remembered per browser, which is the
  // point of the latch and would otherwise be remembered per test run too.
  window.localStorage.removeItem(PHONE_SEEN_STORAGE_KEY)
})

/** The bridge behind the card, so a test can ask for the card itself. */
let bridge: PhoneBridge

function Card() {
  const phone = usePhoneBridgeValue(true)
  bridge = phone
  return (
    <PhoneBridgeProvider value={phone}>
      <PhoneOnboarding />
    </PhoneBridgeProvider>
  )
}

function renderCard() {
  installBackend()
  return renderWithProviders(<Card />)
}

/** The status word beside each step title, in order. */
function steps() {
  return screen.getAllByRole('listitem').map((item) => ({
    title: item.textContent ?? '',
    isDone: (item.textContent ?? '').includes('Done'),
  }))
}

describe('an agent with no extension', () => {
  // Nothing has been seen here to lose contact with, so it is told to install.
  it('is shown the card, blocking, with nothing done', async () => {
    renderCard()
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps().map((s) => s.isDone)).toEqual([false, false, false])
    expect(screen.queryByText(/lost contact with this page/i)).toBeNull()
  })

  /**
   * A link into an extension is only as good as the id in it. Chrome blocks
   * `chrome-extension://replace_with_web_store_id/…` outright — that shipped
   * and an agent hit ERR_BLOCKED_BY_CLIENT — so with no id to address, the
   * card says how to get there by hand instead of offering a dead button.
   */
  it('addresses the published extension when the build was told nothing', async () => {
    // `web/.env.local` may say otherwise on a developer's machine, so the
    // test states the case it is about: no override, the published id.
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', '')
    renderCard()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('link', { name: /web store/i })).toHaveAttribute(
      'href',
      'https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp',
    )
    expect(document.body.innerHTML).toContain('chrome-extension://dkhaojcfjdcdpldokeokajkmambkbacp/')
  })

  it('uses the id this build was given, when it was given one', async () => {
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', 'abcdefghijklmnopabcdefghijklmnop')
    renderCard()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('link', { name: /web store/i })).toHaveAttribute(
      'href',
      'https://chromewebstore.google.com/detail/abcdefghijklmnopabcdefghijklmnop',
    )
    expect(screen.getByRole('link', { name: /extension options/i })).toHaveAttribute(
      'href',
      `chrome-extension://abcdefghijklmnopabcdefghijklmnop/options.html?site=${window.location.hostname}`,
    )
    expect(screen.getByRole('link', { name: /microphone settings/i })).toHaveAttribute(
      'href',
      'chrome-extension://abcdefghijklmnopabcdefghijklmnop/options.html#microphone',
    )
    // A page the agent opens themselves, in their own tab.
    const store = screen.getByRole('link', { name: /web store/i })
    expect(store).toHaveAttribute('target', '_blank')
    expect(store).toHaveAttribute('rel', expect.stringContaining('noopener'))
  })

  it('cannot dismiss it with Escape', async () => {
    renderCard()
    await screen.findByText(/set up your phone/i)
    await userEvent.keyboard('{Escape}')
    expect(screen.getByText(/set up your phone/i)).toBeInTheDocument()
  })
})

describe('as the agent works through it', () => {
  let extension: FakeExtension | undefined

  afterEach(async () => {
    // An uninstall removes the marker, which the mounted card reacts to.
    await act(async () => extension?.uninstall())
    extension = undefined
  })

  // Step two has no signal of its own: being answered is the signal. The
  // microphone step waits for a report, so an extension that has not sent one
  // yet leaves the card up.
  it.each([
    ['reports no microphone', { microphone: 'DENIED' as const }],
    ['has not reported yet', null],
  ])('completes install and site, and stays, for an extension that %s', async (_case, state) => {
    extension = installFakeExtension({ state })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
    expect(screen.getByText(/set up your phone/i)).toBeInTheDocument()
  })

  it('completes the microphone when the extension reports it granted, and leaves', async () => {
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    extension = fake
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    await act(async () => {
      fake.report({ microphone: 'GRANTED' })
    })
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
  })

  // The id an unpacked build has is one no bundle could have carried, so the
  // extension announcing its own is the only way to link into it.
  it('links into the extension by the id it announced for itself', async () => {
    // It outranks the build-time id: an unpacked install has an id of its own
    // and the bundle's is at best the last machine's.
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', 'abcdefghijklmnopabcdefghijklmnop')
    extension = installFakeExtension({
      extensionId: 'ponmlkjihgfedcbaponmlkjihgfedcba',
      state: { microphone: 'DENIED' },
    })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    await waitFor(() =>
      expect(screen.getByRole('link', { name: /microphone settings/i })).toHaveAttribute(
        'href',
        'chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/options.html#microphone',
      ),
    )
  })

  it('ignores an announced id that is not one and falls back to the published id', async () => {
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', '')
    extension = installFakeExtension({ extensionId: 'nope', state: { microphone: 'DENIED' } })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(document.body.innerHTML).not.toContain('chrome-extension://nope/')
    expect(document.body.innerHTML).toContain('chrome-extension://dkhaojcfjdcdpldokeokajkmambkbacp/')
  })

  it('offers a reload on the site step for builds that do not inject into open tabs', async () => {
    renderCard()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('button', { name: /reload after allowing/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /reload after installing/i })).toBeInTheDocument()
  })

  it('ticks and leaves for an extension injected into the open tab, without a reload', async () => {
    renderCard()
    await screen.findByText(/set up your phone/i)
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    extension = fake
    await act(async () => {
      fake.mark()
    })
    await waitFor(() => expect(steps()[1].isDone).toBe(true))
    expect(steps()[0].isDone).toBe(true)
    await act(async () => {
      fake.report({ microphone: 'GRANTED' })
    })
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
  })

  /**
   * The extension holds the registration in a worker that survives the
   * machine sleeping; the content script in the sleeping tab does not, and it
   * takes its marker with it. The phone is installed, allowed and registered
   * — three undone steps would be three lies, and the agent would go looking
   * for a Web Store page to reinstall what they already have. The one true
   * thing is that this page cannot reach it, and the one cure is a reload.
   */
  it('says it lost contact, not that nothing is installed, when the marker goes', async () => {
    const fake = installFakeExtension({ state: { microphone: 'GRANTED' } })
    extension = fake
    fake.mark()
    renderCard()
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
    await act(async () => {
      fake.uninstall()
    })
    expect(await screen.findByText(/lost contact with this page/i)).toBeInTheDocument()
    expect(screen.queryByText(/set up your phone/i)).toBeNull()
    expect(screen.queryByRole('listitem')).toBeNull()

    const reload = vi.fn()
    vi.stubGlobal('location', { ...window.location, reload })
    await userEvent.click(screen.getByRole('button', { name: /reload the page/i }))
    expect(reload).toHaveBeenCalledTimes(1)
  })

  // Asking for the card is asking for the steps: an agent who opens it
  // themselves wants the links into the extension, whatever the page can
  // currently reach.
  it('still shows the three steps when the agent opens the card themselves', async () => {
    const fake = installFakeExtension({ state: { microphone: 'GRANTED' } })
    extension = fake
    fake.mark()
    renderCard()
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
    await act(async () => {
      fake.uninstall()
    })
    await screen.findByText(/lost contact with this page/i)
    await act(async () => {
      bridge.openOnboarding()
    })
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps()).toHaveLength(3)
  })
})
