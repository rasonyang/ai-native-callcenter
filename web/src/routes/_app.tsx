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

/** Longest match wins, so a section's own page never borrows its parent's name. */
const BREADCRUMBS: Array<[string, string]> = [
  ['/supervisor/agents', 'nav.agents'],
  ['/supervisor/queues', 'nav.queues'],
  ['/supervisor/quality', 'nav.quality'],
  ['/supervisor', 'nav.wallboard'],
  ['/admin/users', 'nav.users'],
  ['/admin/routing', 'nav.routing'],
  ['/admin/bots', 'nav.bots'],
  ['/admin/trunks', 'nav.trunks'],
  ['/admin/cdr', 'nav.cdr'],
  ['/admin/reports', 'nav.reports'],
  ['/admin/audit', 'nav.audit'],
  ['/admin', 'nav.overview'],
  ['/agent', 'nav.dashboard'],
]

function breadcrumbKey(pathname: string): string {
  for (const [prefix, key] of BREADCRUMBS) {
    if (pathname.startsWith(prefix)) return key
  }
  return 'app.name'
}
