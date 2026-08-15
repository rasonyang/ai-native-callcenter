import { Outlet, createFileRoute, redirect, useRouterState } from '@tanstack/react-router'

import { AppShell } from '@/components/app-shell'
import { SoftphoneBar } from '@/components/softphone-bar'
import { usePresence } from '@/lib/agent'
import { ApiError, api } from '@/lib/api'
import { useSession } from '@/lib/session'
import { useEventStream } from '@/lib/use-event-stream'

/**
 * Authenticated layout. Everything below this route needs a session, so the
 * check happens once here rather than in every page.
 */
export const Route = createFileRoute('/_app')({
  beforeLoad: async () => {
    try {
      const { user } = await api.me()
      return { user }
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        throw redirect({ to: '/login' })
      }
      throw error
    }
  },
  component: AppLayout,
})

function AppLayout() {
  const { data: user } = useSession()
  const { status } = useEventStream(Boolean(user))
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  const isAgent = user?.role === 'AGENT'
  const { data: presence } = usePresence(Boolean(isAgent))

  if (!user) return null

  // Only an agent has a phone to be reachable on, and only once signed in.
  const deviceState =
    isAgent && presence && presence.state !== 'LOGGED_OUT'
      ? presence.availability === 'DEVICE_UNREACHABLE'
        ? ('unreachable' as const)
        : ('reachable' as const)
      : undefined

  return (
    <AppShell
      user={user}
      streamStatus={status}
      pathname={pathname}
      deviceState={deviceState}
      // An agent carries their call controls with them on every page.
      softphone={isAgent ? <SoftphoneBar /> : undefined}
    >
      <Outlet />
    </AppShell>
  )
}
