import {
  hashKey, useMutation, useQuery, useQueryClient, type Query, type QueryClient, type QueryFilters,
} from '@tanstack/react-query'

import { ApiError, api, type Identity } from './api'
import { usePhoneBridge } from './phone-bridge'

const SESSION_KEY = ['session'] as const
const SESSION_HASH = hashKey(SESSION_KEY)

/**
 * Every query but the session itself: what belongs to the signed-in user, as
 * opposed to the record of who that is. Code that sweeps the whole cache
 * (ending a session, resyncing after the event stream opens) takes this filter
 * rather than the session's key, which stays private to this file.
 */
export const ALL_BUT_SESSION = {
  predicate: (query: Query) => query.queryHash !== SESSION_HASH,
} satisfies QueryFilters

/**
 * Asks the server who is signed in. Only a 401 means nobody is: it resolves to
 * null so the caller can send the reader to the sign-in page. Anything else (a
 * 5xx, a network error) is thrown, because a server that is restarting has not
 * signed anyone out.
 */
async function fetchSession(signal?: AbortSignal): Promise<Identity | null> {
  try {
    const { user } = await api.me(signal)
    return user
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) return null
    throw error
  }
}

/**
 * The current session. An expired or absent session resolves to null rather
 * than throwing, so the router can redirect instead of showing an error.
 */
export function useSession({ isEnabled = true }: { isEnabled?: boolean } = {}) {
  return useQuery({
    queryKey: SESSION_KEY,
    queryFn: () => fetchSession(),
    staleTime: 30_000,
    retry: false,
    // The layout stands the query down while it is showing a failed check: the
    // route has just asked and been refused, and the answer it is showing
    // would otherwise be asked for again the moment the layout mounted.
    enabled: isEnabled,
  })
}

/**
 * How long the route's session check waits for `/me`. Without a bound a server
 * that accepts the connection and never answers leaves the navigation pending
 * for good, with neither a page nor an error; the abort ends it as a failure
 * to reach the server, which the error state shows and "Try again" retries.
 */
const SESSION_CHECK_TIMEOUT_MS = 10_000

/**
 * The user this tab has proved signed in, if any, whatever its age. It lets
 * the layout keep its shell when a later check fails without a 401: the user
 * is still who the server last said, and nothing has said otherwise.
 */
export function cachedSession(queryClient: QueryClient): Identity | null {
  return queryClient.getQueryData<Identity | null>(SESSION_KEY) ?? null
}

/**
 * The session check a route makes before it loads, shared by the entry route
 * and the authenticated layout so the two cannot drift apart again (#324).
 *
 * A user the server confirms is kept as the session; a 401 ends the session,
 * the previous user's caches with it, and resolves to null; any other failure
 * is thrown, with the session left as it was.
 *
 * `reuseProvedWithinMs` lets a caller take a user proved that recently instead
 * of asking again. It is a hand-off, not a cache: past the window the server
 * is asked, so a session that expired or was revoked is still caught on the
 * next navigation. A cached null is never reused, since the server is the
 * only one who can say a session has started.
 */
export async function loadSession(
  queryClient: QueryClient,
  { reuseProvedWithinMs = 0 }: { reuseProvedWithinMs?: number } = {},
): Promise<Identity | null> {
  const cached = queryClient.getQueryState<Identity | null>(SESSION_KEY)
  if (cached?.data && Date.now() - cached.dataUpdatedAt < reuseProvedWithinMs) {
    return cached.data
  }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), SESSION_CHECK_TIMEOUT_MS)
  let user: Identity | null
  try {
    user = await fetchSession(controller.signal)
  } finally {
    clearTimeout(timer)
  }
  if (user) rememberSession(queryClient, user)
  else clearSession(queryClient)
  return user
}

/**
 * The user a request just proved signed in, kept as the session, so the
 * layout that mounts next reads it instead of asking `/me` a second time.
 */
export function rememberSession(queryClient: QueryClient, user: Identity) {
  queryClient.setQueryData(SESSION_KEY, user)
}

/**
 * The session is over: forget the user, and with them every cache that was
 * theirs, so whoever signs in next in this tab starts from nothing.
 *
 * The session stays in the cache as null, which is what tells a mounted layout
 * to stand down. Everything else is removed, not invalidated: an invalidated
 * query keeps its data, and the next user's first render showed the previous
 * user's presence, calls and callbacks until the refetch landed (#324). Nor is
 * it reset: `resetQueries` refetches every query still observed, and when the
 * session ends under an open page (signing out, a 401 on navigation) that is
 * a burst of requests the server can only refuse. A removed query is fetched
 * again only by a component that mounts after it, by then the next user's.
 */
export function clearSession(queryClient: QueryClient) {
  queryClient.setQueryData(SESSION_KEY, null)
  queryClient.removeQueries(ALL_BUT_SESSION)
}

export function useLogin() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }: { username: string; password: string }) =>
      api.login(username, password),
    onSuccess: ({ user }) => {
      rememberSession(queryClient, user)
    },
  })
}

/**
 * Signing out, phone included.
 *
 * The extension is told to drop the credentials, and then the session ends.
 * The bridge latches on that: from here it sends no hello and mints nothing,
 * since the bridge stays enabled until the session is cleared and a hello in
 * that window would bring the widget back onto the sign-in page. (A session
 * that ends any other way is deprovisioned by the bridge itself when it stops
 * being enabled or unmounts.)
 * Revoking the SIP session is the server's part of signing out: it signs the
 * agent out of presence first and only then revokes the session and flushes
 * the registration, so the phone disappearing is no longer news — an agent
 * who is signed out cannot be sent a call.
 *
 * The browser must not do that half itself. Revoking from here flushed the
 * registration while the agent was still READY, and the switch's
 * `sofia::unregister` arrived as a phone lost by an agent still on the queue:
 * a DEVICE_LOST in the record of every clean sign-out.
 */
export function useLogout() {
  const queryClient = useQueryClient()
  const phone = usePhoneBridge()
  return useMutation({
    mutationFn: async () => {
      phone.deprovision()
      await api.logout()
    },
    onSuccess: () => {
      clearSession(queryClient)
    },
  })
}
