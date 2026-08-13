import { Link, useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import {
  Activity, BarChart3, Bot, FileClock, Headphones, LayoutDashboard,
  ListChecks, LogOut, PhoneCall, Radio, ShieldCheck, Users,
} from 'lucide-react'
import type { ReactNode } from 'react'

import { LanguageSwitch } from '@/components/language-switch'
import { Button } from '@/components/ui/button'
import type { Identity, Role } from '@/lib/api'
import { useLogout } from '@/lib/session'
import type { StreamStatus } from '@/lib/use-event-stream'
import { cn } from '@/lib/utils'

interface NavItem {
  to: string
  labelKey: string
  icon: typeof LayoutDashboard
  minRole: Role
}

interface NavGroup {
  labelKey: string
  items: NavItem[]
}

/** Navigation is role-scoped: an agent never sees supervisor or admin groups. */
const NAV: NavGroup[] = [
  {
    labelKey: 'nav.workspace',
    items: [{ to: '/agent', labelKey: 'nav.dashboard', icon: Headphones, minRole: 'AGENT' }],
  },
  {
    labelKey: 'nav.supervise',
    items: [
      { to: '/supervisor', labelKey: 'nav.wallboard', icon: LayoutDashboard, minRole: 'SUPERVISOR' },
      { to: '/supervisor/agents', labelKey: 'nav.agents', icon: Users, minRole: 'SUPERVISOR' },
      { to: '/supervisor/queues', labelKey: 'nav.queues', icon: ListChecks, minRole: 'SUPERVISOR' },
      { to: '/supervisor/quality', labelKey: 'nav.quality', icon: ShieldCheck, minRole: 'SUPERVISOR' },
    ],
  },
  {
    labelKey: 'nav.manage',
    items: [
      { to: '/admin', labelKey: 'nav.overview', icon: Activity, minRole: 'ADMIN' },
      { to: '/admin/users', labelKey: 'nav.users', icon: Users, minRole: 'ADMIN' },
      { to: '/admin/routing', labelKey: 'nav.routing', icon: ListChecks, minRole: 'ADMIN' },
      { to: '/admin/bots', labelKey: 'nav.bots', icon: Bot, minRole: 'ADMIN' },
      { to: '/admin/trunks', labelKey: 'nav.trunks', icon: PhoneCall, minRole: 'ADMIN' },
    ],
  },
  {
    labelKey: 'nav.system',
    items: [
      { to: '/admin/cdr', labelKey: 'nav.cdr', icon: FileClock, minRole: 'SUPERVISOR' },
      { to: '/admin/reports', labelKey: 'nav.reports', icon: BarChart3, minRole: 'SUPERVISOR' },
      { to: '/admin/audit', labelKey: 'nav.audit', icon: FileClock, minRole: 'ADMIN' },
    ],
  },
]

const ROLE_RANK: Record<Role, number> = { AGENT: 1, SUPERVISOR: 2, ADMIN: 3 }

function allowed(role: Role, min: Role) {
  return ROLE_RANK[role] >= ROLE_RANK[min]
}

/**
 * Application shell: a fixed 220px sidebar and a 48px breadcrumb topbar.
 * Breadcrumb links point at fixed targets — never browser history.
 */
export function AppShell({
  user,
  streamStatus,
  breadcrumb,
  children,
}: {
  user: Identity
  streamStatus: StreamStatus
  breadcrumb: ReactNode
  children: ReactNode
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const logout = useLogout()

  return (
    <div className="flex min-h-screen">
      <aside className="w-[220px] shrink-0 border-r bg-card">
        <div className="flex h-12 items-center px-4 text-sm font-semibold">{t('app.name')}</div>
        <nav className="px-2 pb-4">
          {NAV.map((group) => {
            const items = group.items.filter((item) => allowed(user.role, item.minRole))
            if (items.length === 0) return null
            return (
              <div key={group.labelKey} className="mb-3">
                <div className="px-2 py-1 text-xs uppercase tracking-wide text-muted-foreground">
                  {t(group.labelKey)}
                </div>
                {items.map((item) => (
                  <Link
                    key={item.to}
                    to={item.to}
                    className="flex h-8 items-center gap-2 rounded-md px-2 text-sm text-foreground hover:bg-muted"
                    activeProps={{ className: 'bg-primary/6 text-primary' }}
                  >
                    <item.icon className="size-4" />
                    {t(item.labelKey)}
                  </Link>
                ))}
              </div>
            )
          })}
        </nav>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 items-center justify-between border-b bg-card px-4">
          <div className="flex items-center gap-2 text-sm">{breadcrumb}</div>
          <div className="flex items-center gap-2">
            <StreamIndicator status={streamStatus} />
            <LanguageSwitch />
            <div className="text-xs text-muted-foreground">
              {user.displayName} · {t(`roles.${user.role}`)}
            </div>
            <Button
              variant="ghost"
              size="icon"
              title={t('common.signOut')}
              aria-label={t('common.signOut')}
              onClick={() =>
                logout.mutate(undefined, { onSuccess: () => void navigate({ to: '/login' }) })
              }
            >
              <LogOut />
            </Button>
          </div>
        </header>

        <main className="min-w-0 flex-1 p-6">{children}</main>
      </div>
    </div>
  )
}

/**
 * Health of the data stream between this browser and the server.
 *
 * Deliberately not a green status dot: the softphone extension shows its own
 * registration dot in the same corner of the screen, and two adjacent green
 * dots meaning different things is worse than no indicator at all. This one
 * stays monochrome and names what it is, turning amber only when the stream is
 * actually degraded.
 */
function StreamIndicator({ status }: { status: StreamStatus }) {
  const { t } = useTranslation()
  if (status === 'connected') {
    return (
      <div className="flex items-center gap-1 text-xs text-muted-foreground" title={t('stream.connectedHint')}>
        <Radio className="size-3" />
        {t('stream.connected')}
      </div>
    )
  }
  return (
    <div
      className="flex items-center gap-1 text-xs"
      style={{ color: 'var(--state-ringing)' }}
      title={t('stream.degradedHint')}
    >
      <Radio className={cn('size-3', status === 'reconnecting' && 'animate-pulse')} />
      {t(status === 'reconnecting' ? 'stream.reconnecting' : 'stream.offline')}
    </div>
  )
}
