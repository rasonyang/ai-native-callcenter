import { RouterProvider, createMemoryHistory } from '@tanstack/react-router'
import { QueryClient } from '@tanstack/react-query'
import { act, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError, type Identity } from '@/lib/api'
import i18n from '@/lib/i18n'
import type * as ContactsModule_ from '@/lib/contacts'
import { createAppRouter } from '@/router'
import {
  cdrFixture, identityFixture, installBackend, presenceFixture, renderWithProviders,
} from '@/test/harness'

/**
 * Opening the site root, and the session check it shares with the layout.
 *
 * `/` has no page of its own: its whole job is to send a signed-in user to the
 * one they work from, and that page sits under the `_app` layout, which checks
 * the session too. The two checks used to be written twice and disagreed: `/`
 * asked `/me`, forgot the answer and let the layout ask again; a 401 there left
 * the previous user's caches behind; a server that did not answer sent the
 * reader to the sign-in form at `/` and showed an error at `/agent` (#324).
 *
 * These tests run the generated route tree, the real layout included, with
 * one query client shared by the router and the page, so what a `beforeLoad`
 * writes to the cache is what the page reads. Every `/auth/me` is counted.
 */

type ContactsModule = typeof ContactsModule_

// One page is made to fail while it renders, to see what the layout does with
// an error that is not a failed request.
const throwingPage = vi.hoisted(() => ({ isThrowing: false }))
vi.mock('@/lib/contacts', async (importOriginal) => {
  const original = await importOriginal<ContactsModule>()
  return {
    ...original,
    useContacts: (...args: Parameters<typeof original.useContacts>) => {
      // The way a page usually breaks: a TypeError, the same type fetch
      // rejects with when the network fails, so the copy must not mistake it.
      if (throwingPage.isThrowing) throw new TypeError("Cannot read properties of undefined (reading 'map')")
      return original.useContacts(...args)
    },
  }
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const SESSION_KEY = ['session']
/** A valid UUIDv7, for the deep link. */
const CDR_ID = '019a0000-0000-7000-8000-000000000221'
/** `loadSession`'s bound on `/me`, which the test drives past. */
const SESSION_CHECK_TIMEOUT = 10_000
/** The error state's heading in each language. */
const TITLES = {
  en: "Can't reach the server",
  zh: '无法连接服务器',
  unexpected: "This page couldn't be loaded",
}
/** A query that belongs to whoever is signed in. */
const THEIR_PRESENCE_KEY = ['agent', 'presence']

/** How `/auth/me` answers. */
type MeAnswer = 'SESSION' | 'UNAVAILABLE' | 'BAD_GATEWAY_HTML' | 'UNREACHABLE' | 'HANGS'

/**
 * The backend, with `/auth/me` counted and able to fail the ways a server
 * that is restarting fails. With `me` at SESSION it answers from
 * `backend.session`: the user, or a 401 when that is null.
 */
function installServer(session: Identity | null) {
  const backend = installBackend({ session })
  const answer = globalThis.fetch
  const server = {
    backend,
    me: 'SESSION' as MeAnswer,
    meRequests: 0,
    signsIn: null as Identity | null,
  }
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      // Signing in is whoever `server.signsIn` names, the way the real
      // endpoint answers with the account the credentials belong to.
      if (String(input) === '/api/v1/auth/login' && server.signsIn) {
        backend.session = server.signsIn
        return Promise.resolve(
          new Response(JSON.stringify({ user: server.signsIn }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
        )
      }
      // Signing out ends the session on the server too.
      if (String(input) === '/api/v1/auth/logout') backend.session = null
      if (String(input) === '/api/v1/auth/me') {
        server.meRequests++
        if (server.me === 'UNREACHABLE') return Promise.reject(new TypeError('Failed to fetch'))
        // A server that accepted the connection and never answers: only the
        // caller's abort ends it, as it does in a browser.
        if (server.me === 'HANGS') {
          return new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () =>
              reject(new DOMException('The operation was aborted.', 'AbortError')),
            )
          })
        }
        // What a proxy in front of a restarting server answers.
        if (server.me === 'BAD_GATEWAY_HTML') {
          return Promise.resolve(
            new Response('<html><body><h1>502 Bad Gateway</h1></body></html>', {
              status: 502,
              statusText: 'Bad Gateway',
              headers: { 'content-type': 'text/html' },
            }),
          )
        }
        if (server.me === 'UNAVAILABLE') {
          return Promise.resolve(
            new Response(JSON.stringify({ error: { code: 'INTERNAL', message: 'restarting' } }), {
              status: 503,
              headers: { 'content-type': 'application/json' },
            }),
          )
        }
      }
      return answer(input, init)
    }),
  )
  return server
}

/**
 * jsdom has no EventSource. This one never connects by itself; a test opens it,
 * or sends it an event, the way the server would.
 */
class FakeEventSource {
  static opened: FakeEventSource[] = []
  private listeners = new Map<string, ((event: MessageEvent) => void)[]>()
  isClosed = false

  constructor() {
    FakeEventSource.opened.push(this)
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener])
  }

  close() {
    this.isClosed = true
  }

  emit(type: string, data?: unknown) {
    const event = new MessageEvent(type, { data: data === undefined ? undefined : JSON.stringify(data) })
    for (const listener of this.listeners.get(type) ?? []) listener(event)
  }
}

/** The stream the layout has open now. */
function liveStream() {
  const stream = FakeEventSource.opened.findLast((source) => !source.isClosed)
  if (!stream) throw new Error('no event stream is open')
  return stream
}

/**
 * The query client the application builds, less the retry: one failed `/me`
 * is the case under test, and a retry would only double it.
 */
function appQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

/**
 * Seeds the cache as a tab that has been open a while would hold it: someone
 * proved signed in a minute ago, with a query of theirs alongside.
 */
function seedEarlierSession(queryClient: QueryClient, user: Identity) {
  const aMinuteAgo = Date.now() - 60_000
  queryClient.setQueryData(SESSION_KEY, user, { updatedAt: aMinuteAgo })
  queryClient.setQueryData(THEIR_PRESENCE_KEY, { agentId: user.userId }, { updatedAt: aMinuteAgo })
}

function renderApp(path: string, queryClient = appQueryClient()) {
  FakeEventSource.opened = []
  vi.stubGlobal('EventSource', FakeEventSource)
  const router = createAppRouter({
    queryClient,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  // Every location the reader was shown, in order. A redirect that happens in
  // a `beforeLoad` shows nothing, so a flash of the sign-in form on the way to
  // the cockpit appears here even when the reader ends up in the right place.
  const shown: string[] = []
  router.subscribe('onRendered', (event) => shown.push(event.toLocation.pathname))
  // The same from the page's side: the sign-in form's password field, and
  // every piece of text put on the page, so a value that was rendered for a
  // moment and then replaced is still on record.
  const signInForms: Element[] = []
  const written: string[] = []
  const observer = new MutationObserver((records) => {
    for (const record of records) {
      if (record.type === 'characterData') written.push(record.target.textContent ?? '')
      for (const node of record.addedNodes) {
        written.push(node.textContent ?? '')
        if (!(node instanceof Element)) continue
        const field = node.matches('input[type="password"]')
          ? node
          : node.querySelector('input[type="password"]')
        if (field) signInForms.push(field)
      }
    }
  })
  observer.observe(document.body, { childList: true, subtree: true, characterData: true })
  const view = renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
    { queryClient },
  )
  const settled = () => waitFor(() => expect(router.state.status).toBe('idle'))
  return {
    ...view,
    router,
    queryClient,
    shown,
    /** Whether the sign-in form was put on the page at any point. */
    sawSignInForm: () => signInForms.length > 0,
    /** Forgets what was written so far, so a test can watch from a moment on. */
    forgetWritten: () => void (written.length = 0),
    /** Whether `text` was put on the page since the last `forgetWritten`. */
    wrote: (text: string) => written.some((chunk) => chunk.includes(text)),
    settled,
    stop: () => observer.disconnect(),
  }
}

describe('the site root', () => {
  it.each([
    ['AGENT', '/agent'],
    ['SUPERVISOR', '/supervisor'],
    ['ADMIN', '/admin'],
  ] as const)('sends a signed-in %s to their own home, asking /me once', async (role, home) => {
    const server = installServer(identityFixture({ role }))
    const app = renderApp('/')

    await waitFor(() => expect(app.router.state.location.pathname).toBe(home))
    await app.settled()
    // The layout rendered, its sidebar on the role's home.
    expect(await screen.findAllByRole('link', { current: 'page' })).not.toHaveLength(0)
    // `/` proved the session and the layout took its word for it.
    expect(server.meRequests).toBe(1)
    expect(app.queryClient.getQueryData(SESSION_KEY)).toMatchObject({ role })
    // The form is not shown on the way there, not even for one frame.
    expect(app.sawSignInForm()).toBe(false)
    expect(app.shown).not.toContain('/login')
    app.stop()
  })

  it('ends the previous session on a 401 and shows nothing but the sign-in page', async () => {
    const server = installServer(null)
    const queryClient = appQueryClient()
    seedEarlierSession(queryClient, identityFixture({ username: 'bob', displayName: 'Bob Li' }))
    const app = renderApp('/', queryClient)

    expect(await screen.findByLabelText('Password')).toBeVisible()
    await app.settled()
    expect(app.router.state.location.pathname).toBe('/login')
    expect(server.meRequests).toBe(1)
    // Bob's session is over, and so is everything that was his.
    expect(queryClient.getQueryData(SESSION_KEY)).toBeNull()
    expect(queryClient.getQueryData(THEIR_PRESENCE_KEY)).toBeUndefined()
    expect(screen.queryByText('Bob Li')).not.toBeInTheDocument()
    expect(app.shown.every((path) => path === '/login')).toBe(true)
    app.stop()
  })

  it('catches a session that expired between two visits', async () => {
    const server = installServer(identityFixture())
    const app = renderApp('/')
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    await app.settled()

    // Revoked in another tab, moments later. `/` always asks, so the session
    // the layout was handed a moment ago does not answer for it.
    server.backend.session = null
    await act(() => app.router.navigate({ to: '/' }))

    await waitFor(() => expect(app.router.state.location.pathname).toBe('/login'))
    expect(await screen.findByLabelText('Password')).toBeVisible()
    expect(app.queryClient.getQueryData(SESSION_KEY)).toBeNull()
    app.stop()
  })
})

describe('a /me that fails without a 401', () => {
  // A server that is restarting has signed nobody out. Every way in shows the
  // designed error state and keeps the session, so the reader recovers with
  // "Try again", not by signing in again.
  const FAILURES = [
    ['a 503', 'UNAVAILABLE'],
    ['a 502 with an HTML body', 'BAD_GATEWAY_HTML'],
    ['a network error', 'UNREACHABLE'],
    ['no answer at all', 'HANGS'],
  ] as const
  const PATHS = [
    ['/', 'AGENT', false],
    ['/agent', 'AGENT', true],
    ['/supervisor', 'SUPERVISOR', true],
    ['/admin', 'ADMIN', true],
    [`/admin/cdr/${CDR_ID}`, 'ADMIN', true],
  ] as const
  /** What a reader must never see: the developer strings behind the failure. */
  const RAW = ['restarting', 'Failed to fetch', 'Bad Gateway', 'aborted', '502']

  /** Drives the check past its timeout when the server never answers. */
  async function failUntilShown(
    meRequests: () => number, answer: MeAnswer, language: 'en' | 'zh', hasShell: boolean,
  ) {
    const title = TITLES[language]
    if (answer === 'HANGS') {
      // The check's timer starts once it has asked.
      await waitFor(() => expect(meRequests()).toBe(1))
      await vi.advanceTimersByTimeAsync(SESSION_CHECK_TIMEOUT)
    }
    expect(await screen.findByRole('heading', { name: title })).toBeVisible()
    expect(Boolean(document.querySelector('aside'))).toBe(hasShell)
  }

  describe.each([
    ['en', 'Try again'],
    ['zh', '重试'],
  ] as const)('in %s', (language, retry) => {
    beforeEach(async () => {
      await i18n.changeLanguage(language)
    })
    afterEach(async () => {
      await i18n.changeLanguage('en')
    })

    it.each(
      FAILURES.flatMap(([failure, answer]) =>
        PATHS.flatMap(([path, role, isUnderApp]) =>
          [false, true].map(
            (hasEarlierSession) =>
              [
                `${failure} at ${path}${hasEarlierSession ? ' with an earlier session' : ' on a fresh tab'}`,
                answer, path, role, isUnderApp, hasEarlierSession,
              ] as const,
          ),
        ),
      ).filter(([, answer, path, , , hasEarlierSession]) =>
        // The full cross product in English; zh covers every failure kind, both
        // variants and both kinds of path (the strings are the same in all).
        language === 'en' || path === '/' || path === '/admin' || (answer === 'UNAVAILABLE' && hasEarlierSession),
      ),
    )('shows %s', async (_label, answer, path, role, isUnderApp, hasEarlierSession) => {
      // shouldAdvanceTime: Testing Library's own waits settle on a
      // setTimeout(0), which a frozen clock would never fire.
      if (answer === 'HANGS') vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true })
      const warn = vi.spyOn(console, 'warn')
      const server = installServer(identityFixture({ role }))
      server.me = answer
      const queryClient = appQueryClient()
      const user = identityFixture({ role })
      if (hasEarlierSession) seedEarlierSession(queryClient, user)
      const app = renderApp(path, queryClient)

      // Under `_app` with a user proved earlier the shell stays; without one
      // (and at `/`, which has none) the error draws its own frame.
      await failUntilShown(() => server.meRequests, answer, language, isUnderApp && hasEarlierSession)
      expect(screen.queryByRole('heading', { name: TITLES[language === 'en' ? 'zh' : 'en'] })).not.toBeInTheDocument()
      const text = document.body.textContent ?? ''
      for (const raw of RAW) expect(text).not.toContain(raw)
      expect(screen.getByRole('button', { name: retry })).toBeVisible()
      expect(server.meRequests).toBe(1)
      expect(app.sawSignInForm()).toBe(false)
      expect(app.router.state.location.pathname).toBe(path)
      expect(queryClient.getQueryData(SESSION_KEY) ?? null).toEqual(hasEarlierSession ? user : null)
      expect(warn.mock.calls.flat().join(' ')).not.toContain("wasn't caught by any route")
      app.stop()
    })
  })

  it('logs the error to the console, for whoever is diagnosing it', async () => {
    const log = vi.spyOn(console, 'error').mockImplementation(() => {})
    const server = installServer(identityFixture())
    server.me = 'UNAVAILABLE'
    const app = renderApp('/agent')
    expect(await screen.findByRole('heading', { name: TITLES.en })).toBeVisible()
    expect(log.mock.calls.some((call) => call.some((arg) => arg instanceof ApiError))).toBe(true)
    app.stop()
  })

  it('offers the way back to the start only for an unexpected error', async () => {
    const server = installServer(identityFixture())
    server.me = 'UNAVAILABLE'
    const app = renderApp('/agent')
    expect(await screen.findByRole('heading', { name: TITLES.en })).toBeVisible()
    expect(screen.queryByRole('link', { name: 'Back to start' })).not.toBeInTheDocument()
    app.stop()
  })

  it('shows a /me answered 401 as before: the sign-in page, the cache cleared', async () => {
    installServer(null)
    const queryClient = appQueryClient()
    seedEarlierSession(queryClient, identityFixture())
    const app = renderApp('/admin/cdr/' + CDR_ID, queryClient)
    expect(await screen.findByLabelText('Password')).toBeVisible()
    expect(queryClient.getQueryData(SESSION_KEY)).toBeNull()
    expect(screen.queryByRole('heading', { name: TITLES.en })).not.toBeInTheDocument()
    app.stop()
  })

  describe('"Try again"', () => {
    it.each([
      ['/', false],
      ['/agent', true],
    ] as const)('while the server is still down asks once more and stops, at %s', async (path, hasEarlierSession) => {
      const server = installServer(identityFixture())
      server.me = 'UNAVAILABLE'
      const queryClient = appQueryClient()
      if (hasEarlierSession) seedEarlierSession(queryClient, identityFixture())
      const app = renderApp(path, queryClient)
      await screen.findByRole('heading', { name: TITLES.en })
      expect(server.meRequests).toBe(1)

      await app.user.click(screen.getByRole('button', { name: 'Try again' }))
      await waitFor(() => expect(server.meRequests).toBe(2))
      expect(await screen.findByRole('heading', { name: TITLES.en })).toBeVisible()
      await new Promise((resolve) => setTimeout(resolve, 300))
      expect(server.meRequests).toBe(2)
      app.stop()
    })

    it.each([
      ['/', false, '/agent'],
      ['/agent/calls', true, '/agent/calls'],
      ['/agent/calls', false, '/agent/calls'],
    ] as const)('once the server is back lands on the page asked for, from %s (earlier session: %s)', async (path, hasEarlierSession, lands) => {
      const server = installServer(identityFixture())
      server.me = 'UNAVAILABLE'
      const queryClient = appQueryClient()
      if (hasEarlierSession) seedEarlierSession(queryClient, identityFixture())
      const app = renderApp(path, queryClient)
      await screen.findByRole('heading', { name: TITLES.en })

      server.me = 'SESSION'
      await app.user.click(screen.getByRole('button', { name: 'Try again' }))

      await waitFor(() => expect(app.router.state.location.pathname).toBe(lands))
      await app.settled()
      if (lands === '/agent/calls') expect(await screen.findByRole('heading', { name: 'My Calls' })).toBeVisible()
      expect(screen.queryByRole('heading', { name: TITLES.en })).not.toBeInTheDocument()
      expect(app.sawSignInForm()).toBe(false)
      expect(app.shown).not.toContain('/login')
      expect(server.meRequests).toBe(2)
      app.stop()
    })
  })

  describe('navigating within the app', () => {
    it('keeps the shell and the softphone when the check fails, and recovers with "Try again"', async () => {
      vi.useFakeTimers({ toFake: ['Date'] })
      const server = installServer(identityFixture())
      const app = renderApp('/')
      await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
      await app.settled()
      const sidebar = document.querySelector('aside')
      const header = document.querySelector('header')
      const bar = await waitFor(() => {
        const found = document.querySelector('[data-softphone-bar]')
        if (!found) throw new Error('the softphone bar is not on screen yet')
        return found
      })
      expect(sidebar).not.toBeNull()
      expect(screen.getByRole('button', { name: 'AC' })).toBeVisible()

      vi.setSystemTime(Date.now() + 60_000)
      server.me = 'UNAVAILABLE'
      const log = vi.spyOn(console, 'error').mockImplementation(() => {})
      await act(() => app.router.navigate({ to: '/agent/calls' }))

      expect(await screen.findByRole('heading', { name: TITLES.en })).toBeVisible()
      // Nothing was thrown, so the layout is the one to log it, once.
      await waitFor(() =>
        expect(log.mock.calls.filter((call) => call.some((arg) => arg instanceof ApiError))).toHaveLength(1),
      )
      expect(document.querySelector('aside')).toBe(sidebar)
      expect(document.querySelector('header')).toBe(header)
      expect(document.querySelector('[data-softphone-bar]')).toBe(bar)
      expect(screen.queryByRole('heading', { name: 'My Calls' })).not.toBeInTheDocument()
      expect(app.sawSignInForm()).toBe(false)
      const meBefore = server.meRequests

      server.me = 'SESSION'
      await app.user.click(screen.getByRole('button', { name: 'Try again' }))
      expect(await screen.findByRole('heading', { name: 'My Calls' })).toBeVisible()
      expect(document.querySelector('aside')).toBe(sidebar)
      expect(server.meRequests).toBe(meBefore + 1)
      expect(screen.queryByRole('heading', { name: TITLES.en })).not.toBeInTheDocument()
      app.stop()
    })
  })

  describe('a page that fails to render', () => {
    it('keeps the shell and says the page could not be loaded, with a way back', async () => {
      const log = vi.spyOn(console, 'error').mockImplementation(() => {})
      installServer(identityFixture())
      throwingPage.isThrowing = true
      const app = renderApp('/agent/contacts')

      expect(await screen.findByRole('heading', { name: TITLES.unexpected })).toBeVisible()
      expect(document.querySelector('aside')).not.toBeNull()
      await waitFor(() => expect(document.querySelector('[data-softphone-bar]')).not.toBeNull())
      expect(screen.queryByRole('heading', { name: TITLES.en })).not.toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Back to start' })).toHaveAttribute('href', '/')
      expect(document.body.textContent).not.toContain('Cannot read properties')
      expect(log.mock.calls.some((call) => call.some((arg) => arg instanceof TypeError && arg.message.startsWith('Cannot read properties')))).toBe(true)
      throwingPage.isThrowing = false
      app.stop()
    })
  })
})

describe('the layout between pages', () => {
  // The layout takes a user proved a moment ago (the hand-off from `/` or the
  // sign-in form), and asks again on any navigation after that: it is the only
  // place a session that ended under an open tab is noticed.
  it('asks /me again once the hand-off is over', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    const server = installServer(identityFixture())
    const app = renderApp('/')
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    await app.settled()
    expect(server.meRequests).toBe(1)

    vi.setSystemTime(Date.now() + 60_000)
    await act(() => app.router.navigate({ to: '/agent/calls' }))
    await app.settled()

    expect(app.router.state.location.pathname).toBe('/agent/calls')
    expect(server.meRequests).toBe(2)
    app.stop()
  })

  it('sends a reader whose session ended to the sign-in page on their next navigation', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    const server = installServer(identityFixture())
    const app = renderApp('/')
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    await app.settled()

    server.backend.session = null
    vi.setSystemTime(Date.now() + 60_000)
    await act(() => app.router.navigate({ to: '/agent/calls' }))

    await waitFor(() => expect(app.router.state.location.pathname).toBe('/login'))
    expect(await screen.findByLabelText('Password')).toBeVisible()
    expect(app.queryClient.getQueryData(SESSION_KEY)).toBeNull()
    expect(app.shown).not.toContain('/agent/calls')
    app.stop()
  })
})

describe('the event stream', () => {
  // The stream resyncs every snapshot when it opens and when the server resets
  // it, but the session is not a snapshot: the stream was let in on the same
  // cookie, so it says nothing new about who is signed in.
  it('resyncs the page on open and on reset without asking /me again', async () => {
    const server = installServer(identityFixture())
    const app = renderApp('/')
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    await app.settled()
    const presenceReads = () =>
      server.backend.requests.filter((request) => request.path === '/agent/presence').length
    const before = presenceReads()

    act(() => liveStream().emit('open'))
    await waitFor(() => expect(presenceReads()).toBe(before + 1))

    act(() =>
      liveStream().emit('SYSTEM_RESET', {
        version: 1,
        seq: 1,
        type: 'SYSTEM_RESET',
        occurredAt: new Date().toISOString(),
      }),
    )
    await waitFor(() => expect(presenceReads()).toBe(before + 2))

    expect(server.meRequests).toBe(1)
    app.stop()
  })
})

describe('the next reader on the same tab', () => {
  const alice = identityFixture()
  const ben = identityFixture({
    userId: '00000000-0000-4000-8000-0000000000b2',
    username: 'ben',
    displayName: 'Ben Zhao',
  })
  /** The caller on Alice's last call, which her cockpit shows. */
  const ALICES_CALLER = '+8613911112222'

  type App = ReturnType<typeof renderApp>
  type Server = ReturnType<typeof installServer>

  // Both ways a session ends under an open page: the layout is still mounted,
  // its queries still observed, when the cache is emptied.
  it.each([
    [
      'her session expires',
      async (app: App, server: Server) => {
        server.backend.session = null
        await act(() => app.router.navigate({ to: '/' }))
      },
    ],
    [
      'she signs out',
      async (app: App) => {
        await app.user.click(screen.getByRole('button', { name: 'AC' }))
        await app.user.click(await screen.findByRole('menuitem', { name: 'Sign out' }))
      },
    ],
  ] as const)("is shown none of Alice's data after %s", async (_, endSession) => {
    const server = installServer(alice)
    server.backend.myCDRs = [cdrFixture({ fromNumber: ALICES_CALLER })]
    const app = renderApp('/')
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    expect(await screen.findByText(ALICES_CALLER)).toBeVisible()

    const sent = server.backend.requests.length
    await endSession(app, server)
    await waitFor(() => expect(app.router.state.location.pathname).toBe('/login'))
    expect(await screen.findByLabelText('Password')).toBeVisible()
    await app.settled()

    // Nothing of Alice's is left: the session, ended, is the only data the
    // cache holds. (A hook still mounted when it was emptied may have put its
    // key back, with nothing in it.)
    const holdingData = app.queryClient
      .getQueryCache()
      .getAll()
      .filter((query) => query.state.data !== undefined)
    expect(holdingData.map((query) => query.queryKey)).toEqual([SESSION_KEY])
    expect(app.queryClient.getQueryData(SESSION_KEY)).toBeNull()
    // Emptying the cache under the open page fetched nothing: the only
    // requests are the one that found the session over and the one that ended it.
    const since = server.backend.requests.slice(sent).map((r) => `${r.method} ${r.path}`)
    expect(since.filter((r) => r !== 'GET /auth/me' && r !== 'POST /auth/logout')).toEqual([])

    // Ben signs in on the same tab, to a cockpit of his own.
    server.backend.myCDRs = []
    server.backend.presence = presenceFixture({ agentId: ben.userId, extensionNumber: '1002' })
    server.signsIn = ben
    const meBefore = server.meRequests
    app.forgetWritten()
    await app.user.type(screen.getByLabelText('Username'), 'ben')
    await app.user.type(screen.getByLabelText('Password'), 'aicc@123')
    await app.user.click(screen.getByRole('button', { name: 'Sign in' }))

    await waitFor(() => expect(app.router.state.location.pathname).toBe('/agent'))
    expect(await screen.findByRole('button', { name: 'BZ' })).toBeVisible()
    await waitFor(() =>
      expect(server.backend.requests.slice(sent).map((r) => r.path)).toContain('/cdrs/mine?limit=1'),
    )
    await app.settled()
    // Alice's last call was never put on Ben's page, not even before his own
    // data arrived.
    expect(app.wrote(ALICES_CALLER)).toBe(false)
    // The sign-in proved Ben's session; the layout took it from there.
    expect(server.meRequests).toBe(meBefore)
    app.stop()
  })
})
