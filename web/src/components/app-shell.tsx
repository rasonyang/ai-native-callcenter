import { Link, useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { ChevronDown, LogOut, Radio } from 'lucide-react'
import { DropdownMenu } from 'radix-ui'
import type { ReactNode } from 'react'

import { LanguageSwitch } from '@/components/language-switch'
import { Button } from '@/components/ui/button'
import type { Identity } from '@/lib/api'
import { NAV, allowed, breadcrumbFor } from '@/lib/nav'
import { useLogout } from '@/lib/session'
import type { StreamStatus } from '@/lib/use-event-stream'
import { cn } from '@/lib/utils'

/**
 * Application shell: 220px sidebar, 48px topbar carrying the breadcrumb and,
 * for an agent, their softphone controls.
 */
export function AppShell({
  user,
  streamStatus,
  pathname,
  softphone,
  deviceState,
  children,
}: {
  user: Identity
  streamStatus: StreamStatus
  pathname: string
  /** The agent's call controls, shown inline in the topbar. */
  softphone?: ReactNode
  /**
   * Whether this account's phone is reachable, shown as a dot on the avatar.
   * Only an agent has a phone, so it is absent for everyone else.
   */
  deviceState?: DeviceState
  children: ReactNode
}) {
  const { t } = useTranslation()

  return (
    <div className="flex h-screen">
      <aside className="flex w-[220px] shrink-0 flex-col border-r bg-card">
        <div className="flex h-12 shrink-0 items-center px-4 text-sm font-semibold">
          {t('app.name')}
        </div>
        <nav className="flex-1 overflow-y-auto px-2 pb-4">
          {NAV.map((group) => {
            const items = group.items.filter(
              (item) => allowed(user.role, item.minRole) && item.isReady,
            )
            if (items.length === 0) return null
            return (
              <div key={group.labelKey + group.roleHome} className="mb-3">
                <div className="px-2 py-1 text-xs uppercase tracking-wide text-muted-foreground">
                  {t(group.labelKey)}
                </div>
                {items.map((item) => (
                  <Link
                    key={item.to}
                    to={item.to}
                    className="flex h-8 items-center gap-2 rounded-md px-2 text-sm text-foreground hover:bg-muted"
                    // A role index (/agent, /supervisor, /admin) is a prefix of
                    // every page under it, so it only lights up on itself.
                    activeOptions={{ exact: item.to === group.roleHome }}
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
        <header className="flex h-12 shrink-0 items-center gap-4 border-b bg-card px-6">
          <Breadcrumb pathname={pathname} />
          {softphone}
          <div className="ml-auto flex items-center gap-2">
            <StreamIndicator status={streamStatus} />
            <LanguageSwitch />
            <UserMenu user={user} deviceState={deviceState} />
          </div>
        </header>

        <main className="min-w-0 flex-1 overflow-y-auto p-6">{children}</main>
      </div>
    </div>
  )
}

/**
 * Role / Section, derived from the nav config. Clickable segments are
 * secondary and turn accent on hover; the current one is primary and inert.
 */
function Breadcrumb({ pathname }: { pathname: string }) {
  const { t } = useTranslation()
  const crumbs = breadcrumbFor(pathname)

  return (
    <div className="flex shrink-0 items-center gap-1.5 text-sm">
      {crumbs.map((crumb, index) => (
        <span key={crumb.labelKey + index} className="flex items-center gap-1.5">
          {index > 0 && <span className="text-muted-foreground/50">/</span>}
          {crumb.to ? (
            <Link to={crumb.to} className="text-muted-foreground hover:text-primary">
              {t(crumb.labelKey)}
            </Link>
          ) : (
            <span className="font-medium">{t(crumb.labelKey)}</span>
          )}
        </span>
      ))}
    </div>
  )
}

/** Reachable phone, unreachable phone, or no phone at all. */
export type DeviceState = 'reachable' | 'unreachable'

const DEVICE_DOT_COLOR: Record<DeviceState, string> = {
  reachable: 'var(--state-available)',
  unreachable: 'var(--state-offline)',
}

function UserMenu({ user, deviceState }: { user: Identity; deviceState?: DeviceState }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const logout = useLogout()

  const initials = user.displayName
    .split(/\s+/)
    .map((part) => part[0])
    .join('')
    .slice(0, 2)
    .toUpperCase()

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button variant="ghost" className="px-1.5">
          <span className="relative">
            <span className="flex size-6 items-center justify-center rounded-full bg-primary/8 text-xs font-medium text-primary">
              {initials}
            </span>
            {deviceState && (
              <span
                className="absolute -right-0.5 -top-0.5 size-2 rounded-full border border-card"
                style={{ backgroundColor: DEVICE_DOT_COLOR[deviceState] }}
                title={t(`device.${deviceState}`)}
              />
            )}
          </span>
          <ChevronDown className="size-4 text-muted-foreground" />
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={4}
          className="z-50 min-w-44 rounded-md border bg-popover p-1 text-sm shadow-md"
        >
          <div className="px-2 py-1.5">
            <div className="font-medium">{user.displayName}</div>
            <div className="text-xs text-muted-foreground">{user.username}</div>
          </div>
          <div className="my-1 h-px bg-border" />
          <DropdownMenu.Item
            className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 outline-none data-[highlighted]:bg-muted"
            onSelect={() =>
              logout.mutate(undefined, { onSuccess: () => void navigate({ to: '/login' }) })
            }
          >
            <LogOut className="size-4" />
            {t('common.signOut')}
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}

/**
 * Health of the data stream between this browser and the server.
 *
 * Deliberately not a green status dot: the softphone extension shows its own
 * registration dot in the same corner, and two adjacent green dots meaning
 * different things is worse than no indicator. Monochrome at rest, amber only
 * when the stream is degraded.
 */
function StreamIndicator({ status }: { status: StreamStatus }) {
  const { t } = useTranslation()
  if (status === 'connected') {
    return (
      <div
        className="flex items-center gap-1 text-xs text-muted-foreground"
        title={t('stream.connectedHint')}
      >
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
