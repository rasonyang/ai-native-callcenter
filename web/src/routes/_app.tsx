import { Outlet, createFileRoute, redirect, useRouterState } from '@tanstack/react-router'

import { AppShell } from '@/components/app-shell'
import { BreadcrumbDetailProvider } from '@/lib/breadcrumb'
import { PhoneOnboarding } from '@/components/phone-onboarding'
import { SoftphoneBar } from '@/components/softphone-bar'
import { usePresence } from '@/lib/agent'
import { ApiError, api } from '@/lib/api'
import { useSession } from '@/lib/session'
import { PhoneBridgeProvider, usePhoneBridgeValue } from '@/lib/phone-bridge'
import { EventStreamProvider, useEventStream } from '@/lib/use-event-stream'

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
  const { status, listeners } = useEventStream(Boolean(user))
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  const isAgent = user?.role === 'AGENT'
  const { data: presence } = usePresence(Boolean(isAgent))
  // Only an agent has a phone, so only an agent's page talks to the extension
  // or mints a SIP session. The context is still provided to everyone below,
  // because signing out goes through it whoever is doing it.
  const phone = usePhoneBridgeValue(isAgent, presence?.extensionNumber)

  if (!user) return null

  // Only an agent has a phone to be reachable on, and only once signed in.
  const deviceState =
    isAgent && presence && presence.state !== 'LOGGED_OUT'
      ? presence.availability === 'DEVICE_UNREACHABLE'
        ? ('unreachable' as const)
        : ('reachable' as const)
      : undefined

  return (
    // One EventSource feeds the whole application; this makes its tail and its
    // health reachable from any panel below, without a second connection.
    <EventStreamProvider value={{ status, listeners }}>
      <PhoneBridgeProvider value={phone}>
        {/* The shell draws the trail; the page below names what it is about. */}
        <BreadcrumbDetailProvider>
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
        </BreadcrumbDetailProvider>
        {/* Setup an agent has not finished blocks the cockpit, on whichever
            page they happen to have opened. */}
        {isAgent && <PhoneOnboarding />}
      </PhoneBridgeProvider>
    </EventStreamProvider>
  )
}
