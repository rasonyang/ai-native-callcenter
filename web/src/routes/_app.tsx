import { Outlet, createFileRoute, redirect, useRouterState } from '@tanstack/react-router'

import { AppShell } from '@/components/app-shell'
import { SoftphoneBar } from '@/components/softphone-bar'
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

  if (!user) return null

  return (
    <AppShell
      user={user}
      streamStatus={status}
      pathname={pathname}
      // An agent carries their call controls with them on every page.
      softphone={user.role === 'AGENT' ? <SoftphoneBar /> : undefined}
    >
      <Outlet />
    </AppShell>
  )
}
