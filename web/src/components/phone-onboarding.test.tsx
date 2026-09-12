import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { PhoneOnboarding } from '@/components/phone-onboarding'
import { PhoneBridgeProvider, usePhoneBridgeValue } from '@/lib/phone-bridge'
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
})

function Card() {
  const phone = usePhoneBridgeValue(true)
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
  it('is shown the card, blocking, with nothing done', async () => {
    renderCard()
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps().map((s) => s.isDone)).toEqual([false, false, false])
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

  afterEach(() => {
    extension?.uninstall()
    extension = undefined
  })

  it('completes install and site the moment the extension answers', async () => {
    extension = installFakeExtension({ state: { microphone: 'DENIED' } })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    // Step two has no signal of its own: being answered is the signal.
    expect(steps()[1].isDone).toBe(true)
    expect(steps()[2].isDone).toBe(false)
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

  it('stays while an extension that said hello has not reported yet', async () => {
    extension = installFakeExtension({ state: null })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps()[2].isDone).toBe(false)
    expect(screen.getByText(/set up your phone/i)).toBeInTheDocument()
  })
})
