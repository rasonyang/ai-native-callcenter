import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'

import { ApiError, agentApi, type ErrorCode, type SipSession } from './api'

/**
 * The page's half of the conversation with the web-sip-phone extension.
 *
 * The extension owns the microphone and the SIP registration; this page owns
 * the session that entitles the agent to one. The two meet over
 * `window.postMessage` on this origin only: the content script runs in the
 * page's own window, so there is no other window to address and no reason to
 * widen the target beyond it.
 *
 * Credentials pass through `provision()` and are gone the moment the message
 * leaves. They are never held in React state, in a query cache, in storage or
 * in a log — a1Hash is a registration password, and a page that keeps one has
 * turned a session-scoped secret into a durable one.
 */

/** Bumped only for a change the extension and the page cannot both survive. */
export const PHONE_PROTOCOL_VERSION = 1

/** Who each side says it is; a message from anyone else is not ours. */
const PAGE_SOURCE = 'aicc'
const EXTENSION_SOURCE = 'web-sip-phone'

/**
 * The published extension id, which does not exist yet. A deployment that
 * knows one sets `VITE_WEB_SIP_PHONE_ID` at build time.
 *
 * Neither is the first answer. An extension that has said hello announces its
 * own `chrome.runtime.id`, and that is the one a link is built from: an
 * unpacked build has an id nobody could have put in the bundle, and a page
 * that guessed instead sent the agent to
 * `chrome-extension://replace_with_web_store_id/…`, which Chrome blocks
 * outright (seen live).
 */
export const WEB_SIP_PHONE_EXTENSION_ID = 'REPLACE_WITH_WEB_STORE_ID'

/** A Chrome extension id: 32 letters from the first half of the alphabet. */
const EXTENSION_ID_PATTERN = /^[a-p]{32}$/

/** The id this build was given, or null when it is still the placeholder. */
function configuredExtensionId(): string | null {
  const configured = import.meta.env.VITE_WEB_SIP_PHONE_ID
  return configured && configured !== WEB_SIP_PHONE_EXTENSION_ID ? configured : null
}

/**
 * The id to address the extension by: what it announced about itself first,
 * what this build was told second, and nothing at all if neither exists. A
 * link that cannot be built is not rendered — a dead one teaches the agent
 * that the instructions are wrong.
 */
export function extensionIdFor(announced: string | null): string | null {
  return announced ?? configuredExtensionId()
}

/**
 * Where an agent installs it. This link exists for the case where nothing has
 * answered, so it can only ever use the build-time id.
 */
export function webStoreUrl(): string | null {
  const id = configuredExtensionId()
  return id ? `https://chromewebstore.google.com/detail/${id}` : null
}

/**
 * The extension's own options page.
 *
 * `site` opens the site list, which is what decides whether a content script
 * runs on this deployment at all; `microphone` opens the permission it holds
 * on the agent's behalf; `account` opens the manual account, which is where a
 * manual override is undone.
 */
export function optionsUrl(
  section: 'site' | 'microphone' | 'account',
  announced: string | null,
): string | null {
  const id = extensionIdFor(announced)
  if (!id) return null
  if (section === 'site') {
    return `chrome-extension://${id}/options.html?site=${window.location.hostname}`
  }
  return `chrome-extension://${id}/options.html#${section}`
}

/**
 * The marker a content script writes on the document. It proves an extension
 * injected something here; it does not prove which version, or that it is
 * listening yet — the hello handshake is what answers that.
 */
export const PRESENCE_MARKER = 'webSipPhone'

export type PhoneRegistration = 'UNREGISTERED' | 'REGISTERING' | 'REGISTERED' | 'FAILED'

/** Where the phone's credentials came from: nowhere, a person, or this page. */
export type CredentialSource = 'NONE' | 'MANUAL' | 'PROVISIONED'

export type MicrophonePermission = 'UNKNOWN' | 'GRANTED' | 'DENIED'

export type PhoneErrorCode = 'REGISTRATION_FAILED' | 'WSS_LOST' | 'MIC_UNAVAILABLE' | 'MEDIA_FAILED'

/**
 * What has become of the session this page provisioned.
 *
 * `ACTIVE` is the ordinary case, the held provision being the one in use.
 * `OVERRIDDEN` is a manual account saved in the extension's Options while a
 * provisioned session is still held: the manual one is applied and ours is
 * kept but dormant. `NONE` is nothing held at all.
 *
 * Absent on builds older than this field, which is why every reading of it
 * has to work with `undefined`.
 */
export type ProvisionStatus = 'ACTIVE' | 'OVERRIDDEN' | 'NONE'

const PROVISION_STATUSES: ProvisionStatus[] = ['ACTIVE', 'OVERRIDDEN', 'NONE']

/** The field as reported, or undefined for an older build or a value we do
 *  not know. An unreadable status is the same as no status. */
function readProvisionStatus(value: unknown): ProvisionStatus | undefined {
  return PROVISION_STATUSES.find((status) => status === value)
}

/** Everything the extension reports about itself, in one message. */
export interface ExtensionState {
  registration: PhoneRegistration
  account: string | null
  sipDomain: string | null
  credentialSource: CredentialSource
  microphone: MicrophonePermission
  error: PhoneErrorCode | null
  /** Absent on an extension older than the field; see `ProvisionStatus`. */
  provisionStatus?: ProvisionStatus
}

export interface PhoneBridge {
  /** A hello reply carrying one of our own nonces has arrived. */
  detected: boolean
  extensionVersion: string | null
  /** The id the extension announced for itself, if it announced one. */
  extensionId: string | null
  /** The last state the extension reported, or null while it has said nothing. */
  state: ExtensionState | null
  sendHello: () => void
  provision: (credentials: SipSession) => void
  deprovision: () => void
  /** Mints a fresh session and hands it over again, after a refusal or a move. */
  reprovision: () => void
  /** The server's refusal of the last attempt to mint a session, if any. */
  provisionErrorCode: ErrorCode | null
  /**
   * The setup card. The platform opens it while setup is incomplete; this is
   * the agent asking for it themselves, which is the only case they may also
   * close it.
   */
  isOnboardingForced: boolean
  openOnboarding: () => void
  closeOnboarding: () => void
}

/**
 * Outside the provider the bridge is inert rather than absent: a screen that
 * does not belong to an agent still renders the components that ask for it,
 * and signing out must not depend on a phone being there.
 */
const INERT: PhoneBridge = {
  detected: false,
  extensionVersion: null,
  extensionId: null,
  state: null,
  sendHello: () => {},
  provision: () => {},
  deprovision: () => {},
  reprovision: () => {},
  provisionErrorCode: null,
  isOnboardingForced: false,
  openOnboarding: () => {},
  closeOnboarding: () => {},
}

const PhoneBridgeContext = createContext<PhoneBridge | null>(null)

export const PhoneBridgeProvider = PhoneBridgeContext.Provider

export function usePhoneBridge(): PhoneBridge {
  return useContext(PhoneBridgeContext) ?? INERT
}

function newNonce(): string {
  return globalThis.crypto?.randomUUID?.() ?? `n${Math.random().toString(36).slice(2)}`
}

/** The shape of an extension message we are willing to read. */
type ExtensionMessage =
  | {
      type: 'hello'
      nonce: string
      extensionVersion: string
      protocolVersion: number
      /** Its own `chrome.runtime.id`, so the page can link into its options. */
      extensionId?: string
    }
  | ({ type: 'state' } & ExtensionState)

function readExtensionMessage(event: MessageEvent): ExtensionMessage | null {
  // Same window, same origin, our protocol, their name. A page-scripted
  // message from anywhere else is not the extension, whatever it claims.
  if (event.source !== window) return null
  if (event.origin !== window.location.origin) return null
  const data = event.data as Partial<ExtensionMessage> & {
    source?: unknown
    protocolVersion?: unknown
  }
  if (!data || typeof data !== 'object') return null
  if (data.source !== EXTENSION_SOURCE) return null
  if (data.protocolVersion !== PHONE_PROTOCOL_VERSION) return null
  if (data.type !== 'hello' && data.type !== 'state') return null
  return data as ExtensionMessage
}

/**
 * The bridge itself, computed once below the session gate.
 *
 * `enabled` is the agent test: only an agent has a phone to provision, and
 * only an agent's session can mint one. `myExtension` is the number that
 * agent is bound to, which is what decides whether the account the phone
 * reports is theirs.
 */
export function usePhoneBridgeValue(enabled: boolean, myExtension?: string): PhoneBridge {
  const [detected, setDetected] = useState(false)
  const [extensionVersion, setExtensionVersion] = useState<string | null>(null)
  const [extensionId, setExtensionId] = useState<string | null>(null)
  const [state, setState] = useState<ExtensionState | null>(null)
  const [isOnboardingForced, setOnboardingForced] = useState(false)
  const nonces = useRef(new Set<string>())

  const post = useCallback((message: Record<string, unknown>) => {
    window.postMessage(
      { source: PAGE_SOURCE, protocolVersion: PHONE_PROTOCOL_VERSION, ...message },
      window.location.origin,
    )
  }, [])

  const sendHello = useCallback(() => {
    const nonce = newNonce()
    nonces.current.add(nonce)
    post({ type: 'hello', nonce })
  }, [post])

  const provision = useCallback(
    (credentials: SipSession) => {
      // Posted and dropped. Nothing above this line remembers the hash.
      post({ type: 'provision', nonce: newNonce(), ...credentials })
    },
    [post],
  )

  const deprovision = useCallback(() => post({ type: 'deprovision' }), [post])

  useEffect(() => {
    const onMessage = (event: MessageEvent) => {
      const message = readExtensionMessage(event)
      if (!message) return
      if (message.type === 'hello') {
        // A reply to a hello we sent, and to no other page's.
        if (!nonces.current.has(message.nonce)) return
        nonces.current.delete(message.nonce)
        // A hello answers "is it there", and nothing else. It is sent again
        // on every tab switch and every late injection, so anything that
        // hangs off it happens on every tab switch too — and minting a
        // session flushes the registration the last one was holding.
        setDetected(true)
        setExtensionVersion(message.extensionVersion)
        // Optional, and only believed when it looks like an id at all. An
        // older extension announces none and the build-time id answers for it.
        if (message.extensionId && EXTENSION_ID_PATTERN.test(message.extensionId)) {
          setExtensionId(message.extensionId)
        }
        return
      }
      // Only the state's own fields: the envelope is how the message got
      // here, not something a consumer should be able to read back off it.
      setState({
        registration: message.registration,
        account: message.account,
        sipDomain: message.sipDomain,
        credentialSource: message.credentialSource,
        microphone: message.microphone,
        error: message.error,
        provisionStatus: readProvisionStatus(message.provisionStatus),
      })
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
  }, [])

  useEffect(() => {
    if (!enabled) return
    sendHello()

    // The content script may be injected after this page mounted — a fresh
    // install, or a tab that was open through one. The marker landing on the
    // document is the only warning of that, so it is watched rather than
    // polled.
    const observer = new MutationObserver(() => {
      if (document.documentElement.dataset[PRESENCE_MARKER] !== undefined) sendHello()
    })
    observer.observe(document.documentElement, { attributes: true })

    // Coming back to the tab is the other moment an extension may have
    // changed underneath us: installed, disabled, or updated in place.
    const onVisible = () => {
      if (document.visibilityState === 'visible') sendHello()
    }
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      observer.disconnect()
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [enabled, sendHello])

  const openOnboarding = useCallback(() => setOnboardingForced(true), [])
  const closeOnboarding = useCallback(() => setOnboardingForced(false), [])
  const provisioning = usePhoneProvisioning(enabled, state, myExtension, provision)

  return {
    detected,
    extensionVersion,
    extensionId,
    state,
    sendHello,
    provision,
    deprovision,
    isOnboardingForced,
    openOnboarding,
    closeOnboarding,
    ...provisioning,
  }
}

/**
 * What a phone that holds nothing usable looks like, named so it can be
 * compared with the last one that was acted on.
 *
 * Three readings are the page's to fix. A phone with no credentials at all
 * has just been installed or emptied. A manually configured one is the thing
 * this feature retires — an agent typing an extension and a password into a
 * handset is what zero-config replaces, so a provisioned session always wins.
 * And a provisioned session for somebody else's extension is a phone that was
 * rebound while this page was not looking.
 *
 * What is deliberately *not* here: a provisioned session at this agent's own
 * extension that is FAILED or UNREGISTERED. That is the phone having been
 * taken over by a newer session in another browser, and taking it back is the
 * agent's decision — the Re-provision button — not something a page does on
 * its own the moment it reads the state. Nor is a manual override, which is
 * somebody having said, in the extension's own Options, which account this
 * browser is to use.
 */
function provisioningTrigger(state: ExtensionState, myExtension?: string): string | null {
  // A manual account saved over a held provision. Ours is still there and
  // deliberately not in use, so there is nothing to replace — and minting
  // anyway would flush the registration the manual account is holding, for a
  // credential the extension has already been told to keep dormant.
  if (state.provisionStatus === 'OVERRIDDEN') return null
  if (state.credentialSource === 'NONE') return 'NONE'
  if (state.credentialSource === 'MANUAL') return `MANUAL:${state.account ?? ''}`
  // An account we cannot judge is not an account we act on: without knowing
  // this agent's own extension, a mismatch is unknowable rather than wrong.
  if (myExtension && state.account && state.account !== myExtension) {
    return `PROVISIONED:${state.account}`
  }
  return null
}

/**
 * Mints a SIP session only when the phone has none it can use.
 *
 * Minting is destructive. The server replaces the agent's session and flushes
 * the registration the previous one was holding; the switch reports
 * `sofia::unregister`, and a registration lost while the agent is READY takes
 * them out of the queue with reason DEVICE_LOST. So the question is never
 * "has something happened that might mean the phone needs credentials" but
 * "has the phone said it has none".
 *
 * That is why nothing hangs off the page's own lifecycle. Signing in is not a
 * reason: the extension keeps provisioned credentials in `chrome.storage.session`
 * across reloads, so pressing F5 while READY found a phone already registered,
 * replaced its session anyway and dropped the agent to DEVICE_LOST before the
 * new hash re-registered (seen live). Neither is the hello handshake, which
 * goes out again on every tab switch.
 *
 * So it mints on exactly two things: a `state` that says the phone holds
 * nothing usable, and the agent asking. The last condition acted on is
 * remembered, so a phone that ignores a provision and keeps reporting the
 * same thing is asked once, not forever — and the last state is kept, so a
 * state that arrived before this agent's own extension was known (the content
 * script posts its state as soon as it attaches; presence loads async) is
 * judged again when it is.
 */
function usePhoneProvisioning(
  enabled: boolean,
  state: ExtensionState | null,
  myExtension: string | undefined,
  provision: (credentials: SipSession) => void,
): Pick<PhoneBridge, 'reprovision' | 'provisionErrorCode'> {
  const [provisionErrorCode, setProvisionErrorCode] = useState<ErrorCode | null>(null)
  const inFlight = useRef(false)
  /** The last condition a mint was fired for, so the same one fires once. */
  const lastTrigger = useRef<string | null>(null)
  // A CONFLICT means no extension is bound to this account. No amount of
  // retrying binds one, so the refusal stands until something changes.
  const isRefused = useRef(false)

  const mint = useCallback(async () => {
    if (!enabled || inFlight.current || isRefused.current) return
    inFlight.current = true
    try {
      const credentials = await agentApi.createSipSession()
      provision(credentials)
      setProvisionErrorCode(null)
    } catch (error) {
      if (error instanceof ApiError) {
        setProvisionErrorCode(error.code)
        if (error.code === 'CONFLICT') isRefused.current = true
      } else {
        setProvisionErrorCode('INTERNAL')
      }
    } finally {
      inFlight.current = false
    }
  }, [enabled, provision])

  useEffect(() => {
    if (!enabled) {
      // Signing out clears the refusal with everything else it clears.
      lastTrigger.current = null
      isRefused.current = false
      setProvisionErrorCode(null)
      return
    }

    if (!state) return
    const trigger = provisioningTrigger(state, myExtension)
    if (!trigger) {
      // The phone is holding something usable. Whatever was wrong before has
      // been answered, so the next time it goes wrong is a new occasion.
      lastTrigger.current = null
      return
    }
    if (trigger === lastTrigger.current) return
    lastTrigger.current = trigger
    void mint()
  }, [enabled, state, myExtension, mint])

  const reprovision = useCallback(() => {
    isRefused.current = false
    lastTrigger.current = null
    void mint()
  }, [mint])

  return { reprovision, provisionErrorCode }
}
