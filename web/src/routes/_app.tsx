import { Outlet, createFileRoute, redirect, useMatches } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { AppShell } from '@/components/app-shell'
import { ApiError, api } from '@/lib/api'
import { useSession } from '@/lib/session'
import { useEventStream } from '@/lib/use-event-stream'

/**
 * Authenticated layout. Everything below this route requires a session, so the
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
  const { t } = useTranslation()
  const { data: user } = useSession()
  const { status } = useEventStream(Boolean(user))
  const matches = useMatches()

  if (!user) return null

  // The breadcrumb reflects the matched route chain, and its links are fixed
  // targets so a click always lands on the same page.
  const title = matches.at(-1)?.pathname ?? '/'

  return (
    <AppShell
      user={user}
      streamStatus={status}
      breadcrumb={<span className="text-muted-foreground">{t(breadcrumbKey(title))}</span>}
    >
      <Outlet />
    </AppShell>
  )
}

function breadcrumbKey(pathname: string): string {
  if (pathname.startsWith('/agent')) return 'nav.dashboard'
  if (pathname.startsWith('/supervisor')) return 'nav.wallboard'
  if (pathname.startsWith('/admin')) return 'nav.overview'
  return 'app.name'
}
