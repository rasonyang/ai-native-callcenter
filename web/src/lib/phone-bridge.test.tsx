import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { usePhoneBridgeValue, type PhoneBridge } from '@/lib/phone-bridge'
import { installBackend, installFakeExtension, sipSessionFixture } from '@/test/harness'

/**
 * The page's half of the extension protocol.
 *
 * Two things are being pinned here. The envelope — same window, same origin,
 * our name, our version — because a page that accepts anything else has handed
 * a registration password to whoever asked. And that the credentials leave no
 * trace: they are posted and dropped, never held in state, in a cache or in
 * storage, so closing the tab is the whole of the cleanup.
 */

let bridge: PhoneBridge

const MY_EXTENSION = '1001'

function Host({
  enabled = true,
  myExtension,
}: {
  enabled?: boolean
  myExtension?: string
}) {
  bridge = usePhoneBridgeValue(enabled, myExtension)
  return null
}

/**
 * The bridge as the shell mounts it. `myExtension` is passed separately
 * because presence loads asynchronously in the real thing: `null` is a page
 * that does not know it yet, and `learnExtension` is that request landing.
 */
function renderBridge(enabled = true, myExtension: string | null = MY_EXTENSION) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const tree = (ext: string | undefined) => (
    <QueryClientProvider client={queryClient}>
      <Host enabled={enabled} myExtension={ext} />
    </QueryClientProvider>
  )
  const view = render(tree(myExtension ?? undefined))
  return {
    ...view,
    queryClient,
    learnExtension: (ext: string) => view.rerender(tree(ext)),
  }
}

/** One message the extension would send, with the envelope under test. */
function fromExtension(
  data: Record<string, unknown>,
  envelope: { origin?: string; source?: Window | null; protocolVersion?: unknown; from?: unknown } = {},
) {
  window.dispatchEvent(
    new MessageEvent('message', {
      data: {
        source: 'from' in envelope ? envelope.from : 'web-sip-phone',
        protocolVersion: 'protocolVersion' in envelope ? envelope.protocolVersion : 1,
        ...data,
      },
      origin: envelope.origin ?? window.location.origin,
      source: 'source' in envelope ? envelope.source : window,
    }),
  )
}

/** The nonce the page put on its last hello. */
function lastHelloNonce(postMessage: ReturnType<typeof vi.spyOn>): string {
  const hellos = postMessage.mock.calls.filter(
    (call) => (call[0] as { type?: string }).type === 'hello',
  )
  const last = hellos[hellos.length - 1]
  return (last[0] as { nonce: string }).nonce
}

/** How many sessions the page has asked the platform to mint. */
function mints(api: { requests: Array<{ path: string; method: string }> }): number {
  return api.requests.filter((r) => r.method === 'POST' && r.path === '/agent/sip-session').length
}

let postMessage: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  postMessage = vi.spyOn(window, 'postMessage')
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  delete document.documentElement.dataset.webSipPhone
})

describe('what the page posts', () => {
  it('says hello on mount, on this origin, under our name and version', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const [message, targetOrigin] = postMessage.mock.calls[0]
    expect(message).toMatchObject({ source: 'aicc', protocolVersion: 1, type: 'hello' })
    expect((message as { nonce: string }).nonce).toBeTruthy()
    expect(targetOrigin).toBe(window.location.origin)
  })

  it('says nothing at all for an account with no phone', async () => {
    installBackend()
    renderBridge(false)
    await act(async () => {})
    expect(postMessage).not.toHaveBeenCalled()
  })

  it('hands the credentials over and keeps nothing back', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const credentials = sipSessionFixture({ a1Hash: 'deadbeef' })
    act(() => bridge.provision(credentials))
    const [message, targetOrigin] = postMessage.mock.calls.at(-1)!
    expect(message).toMatchObject({
      source: 'aicc',
      protocolVersion: 1,
      type: 'provision',
      ...credentials,
    })
    expect(targetOrigin).toBe(window.location.origin)
  })

  it('takes the credentials back when asked', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => bridge.deprovision())
    expect(postMessage.mock.calls.at(-1)![0]).toMatchObject({
      source: 'aicc',
      protocolVersion: 1,
      type: 'deprovision',
    })
  })
})

describe('what the page accepts', () => {
  it('takes a hello reply carrying a nonce it sent', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const nonce = lastHelloNonce(postMessage)
    act(() => fromExtension({ type: 'hello', nonce, extensionVersion: '1.4.0' }))
    await waitFor(() => expect(bridge.detected).toBe(true))
    expect(bridge.extensionVersion).toBe('1.4.0')
  })

  it.each([
    ['another origin', { origin: 'https://phish.example' }],
    ['another window', { source: null }],
    ['another sender', { from: 'not-the-phone' }],
    ['another protocol version', { protocolVersion: 2 }],
  ])('ignores a hello from %s', async (_case, envelope) => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const nonce = lastHelloNonce(postMessage)
    act(() => fromExtension({ type: 'hello', nonce, extensionVersion: '9.9.9' }, envelope))
    await act(async () => {})
    expect(bridge.detected).toBe(false)
  })

  it('ignores a hello nobody asked for', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => fromExtension({ type: 'hello', nonce: 'made-up', extensionVersion: '9.9.9' }))
    await act(async () => {})
    expect(bridge.detected).toBe(false)
  })

  it('says hello again when a content script appears after the page loaded', async () => {
    installBackend()
    const extension = installFakeExtension()
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    const before = extension.messagesOfType('hello').length
    await act(async () => {
      extension.mark()
    })
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBeGreaterThan(before))
    extension.uninstall()
  })
})

/**
 * When a session is minted, and — more to the point — when it is not.
 *
 * Minting replaces the agent's session and flushes the registration the
 * previous one held, and a registration lost while the agent is READY drops
 * them to NOT_READY/DEVICE_LOST. The extension keeps provisioned credentials
 * across a page reload, so a page that mints because it just loaded knocks a
 * working agent out of the queue for pressing F5. That shipped and was caught
 * on a live call.
 */
describe('provisioning', () => {
  it('mints nothing for a phone already holding our session', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('mints for a phone that reports holding nothing', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    await waitFor(() => expect(extension.messagesOfType('provision')).toHaveLength(1))
    const { expiresAt, ...credentials } = sipSessionFixture()
    expect(expiresAt).toBeTruthy()
    expect(extension.messagesOfType('provision')[0]).toMatchObject(credentials)
    extension.uninstall()
  })

  it('does not mint twice for the same thing being wrong twice', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    await act(async () => {
      extension.report({ credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' })
    })
    await act(async () => {})
    expect(mints(api)).toBe(1)
    extension.uninstall()
  })

  it('replaces an account somebody typed in by hand', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'MANUAL', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('replaces a session provisioned for somebody else', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: '1002', registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  /**
   * The content script posts its state as soon as it attaches, which can be
   * before this page knows which extension the agent is bound to — presence
   * is a request. A state judged against an unknown extension must be judged
   * again when it is known, not dropped.
   */
  it('judges a state that arrived before presence did, once presence lands', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: '1002', registration: 'REGISTERED' },
    })
    const { learnExtension } = renderBridge(true, null)
    await waitFor(() => expect(bridge.state?.account).toBe('1002'))
    // Whose account 1002 is cannot be known yet, so nothing is done about it.
    expect(mints(api)).toBe(0)
    await act(async () => {
      learnExtension(MY_EXTENSION)
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('mints once, not twice, for a phone with nothing when presence lands late', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    const { learnExtension } = renderBridge(true, null)
    await waitFor(() => expect(mints(api)).toBe(1))
    await act(async () => {
      learnExtension(MY_EXTENSION)
    })
    await act(async () => {})
    expect(mints(api)).toBe(1)
    extension.uninstall()
  })

  // Reloading the page re-runs everything above with an extension that kept
  // its credentials. Nothing may be minted, or the agent loses their phone
  // for pressing F5.
  it('mints nothing when the tab comes back and the phone says hello again', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    const hellos = extension.messagesOfType('hello').length
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBeGreaterThan(hellos))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    extension.uninstall()
  })

  // The registration the switch holds is not ours any more: a newer session
  // in another browser took it. Taking it back is the agent's decision.
  it('leaves a phone displaced by another browser alone until asked', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {
      extension.report({ registration: 'FAILED', credentialSource: 'PROVISIONED' })
    })
    await act(async () => {})
    expect(mints(api)).toBe(0)
    await act(async () => {
      bridge.reprovision()
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  /**
   * A manual account saved in the extension's Options while a provisioned one
   * is held. The manual account is what the phone uses and ours is kept
   * dormant, so a provision posted from here would be accepted and not
   * applied — while the mint behind it still flushed, server-side, the
   * registration the manual account is holding.
   */
  it('mints nothing for a phone whose provision has been overridden', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: {
        credentialSource: 'MANUAL',
        account: MY_EXTENSION,
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    renderBridge()
    await waitFor(() => expect(bridge.state?.provisionStatus).toBe('OVERRIDDEN'))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('mints once the override is cleared and the phone is left with nothing', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: {
        credentialSource: 'MANUAL',
        account: MY_EXTENSION,
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    renderBridge()
    await waitFor(() => expect(bridge.state?.provisionStatus).toBe('OVERRIDDEN'))
    expect(mints(api)).toBe(0)
    // Signing out in the extension's Options leaves it holding nothing.
    await act(async () => {
      extension.report({
        credentialSource: 'NONE',
        account: null,
        registration: 'UNREGISTERED',
        provisionStatus: 'NONE',
      })
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('reads a state from a build that does not know the field', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'MANUAL', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    // No status at all, so the old rules stand and a manual account is
    // replaced by a provisioned one.
    await waitFor(() => expect(mints(api)).toBe(1))
    expect(bridge.state?.provisionStatus).toBeUndefined()
    extension.uninstall()
  })

  it('ignores a status it does not recognise', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: {
        credentialSource: 'MANUAL',
        account: MY_EXTENSION,
        registration: 'REGISTERED',
        provisionStatus: 'SOMETHING_ELSE' as never,
      },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    expect(bridge.state?.provisionStatus).toBeUndefined()
    extension.uninstall()
  })

  it('stops asking once the server says no extension is bound', async () => {
    const api = installBackend({ sipSession: null })
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.provisionErrorCode).toBe('CONFLICT'))
    const attempts = mints(api)
    // A different condition, which would otherwise earn a fresh attempt.
    await act(async () => {
      extension.report({ credentialSource: 'MANUAL', account: MY_EXTENSION })
    })
    await act(async () => {})
    expect(mints(api)).toBe(attempts)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('asks again when the agent presses re-provision', async () => {
    const api = installBackend({ sipSession: null })
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.provisionErrorCode).toBe('CONFLICT'))
    const attempts = mints(api)
    await act(async () => {
      bridge.reprovision()
    })
    await waitFor(() => expect(mints(api)).toBe(attempts + 1))
    extension.uninstall()
  })
})

describe('where the credentials do not go', () => {
  it('writes no part of them to storage or to the query cache', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    const { queryClient } = renderBridge()
    await waitFor(() => expect(extension.messagesOfType('provision')).not.toHaveLength(0))

    // The extension was given the hash, so this is not a test of nothing.
    expect(extension.messagesOfType('provision')[0].a1Hash).toBe(sipSessionFixture().a1Hash)

    const written = setItem.mock.calls.map(([, value]) => String(value)).join('\n')
    expect(written).not.toContain('a1Hash')
    expect(written).not.toContain(sipSessionFixture().a1Hash)

    const cached = JSON.stringify(
      queryClient.getQueryCache().getAll().map((query) => ({ key: query.queryKey, data: query.state.data })),
    )
    expect(cached).not.toContain('a1Hash')
    expect(cached).not.toContain(sipSessionFixture().a1Hash)
    extension.uninstall()
  })
})
