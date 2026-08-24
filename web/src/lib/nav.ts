import {
  Activity, BarChart3, Bot, BookUser, FileClock, Headphones, LayoutDashboard,
  ListChecks, Phone, PhoneCall, PhoneForwarded, ScrollText, ShieldCheck, Users,
} from 'lucide-react'


import type { Role } from './api'

export interface NavItem {
  to: string
  labelKey: string
  icon: typeof LayoutDashboard
  minRole: Role
  /** Built pages only. Anything unbuilt stays out of the sidebar. */
  isReady: boolean
}

export interface NavGroup {
  labelKey: string
  /** The role index this group belongs to, used by the breadcrumb. */
  roleKey: string
  roleHome: string
  items: NavItem[]
}

/**
 * The sidebar, and the source the breadcrumb resolves against: every page must
 * be reachable from a nav item so "Role / Section" can be derived rather than
 * hand-written (web/CLAUDE.md).
 */
export const NAV: NavGroup[] = [
  {
    labelKey: 'nav.workspace',
    roleKey: 'roles.AGENT',
    roleHome: '/agent',
    items: [
      { to: '/agent', labelKey: 'nav.dashboard', icon: Headphones, minRole: 'AGENT', isReady: true },
      { to: '/agent/calls', labelKey: 'nav.myCalls', icon: Phone, minRole: 'AGENT', isReady: true },
      { to: '/agent/contacts', labelKey: 'nav.contacts', icon: BookUser, minRole: 'AGENT', isReady: true },
      { to: '/agent/callbacks', labelKey: 'nav.callbacks', icon: PhoneForwarded, minRole: 'AGENT', isReady: true },
    ],
  },
  {
    labelKey: 'nav.supervise',
    roleKey: 'roles.SUPERVISOR',
    roleHome: '/supervisor',
    items: [
      { to: '/supervisor', labelKey: 'nav.wallboard', icon: LayoutDashboard, minRole: 'SUPERVISOR', isReady: true },
      { to: '/supervisor/agents', labelKey: 'nav.agents', icon: Users, minRole: 'SUPERVISOR', isReady: true },
      { to: '/supervisor/queues', labelKey: 'nav.queues', icon: ListChecks, minRole: 'SUPERVISOR', isReady: true },
      { to: '/supervisor/quality', labelKey: 'nav.quality', icon: ShieldCheck, minRole: 'SUPERVISOR', isReady: false },
    ],
  },
  {
    labelKey: 'nav.manage',
    roleKey: 'roles.ADMIN',
    roleHome: '/admin',
    items: [
      { to: '/admin', labelKey: 'nav.overview', icon: Activity, minRole: 'ADMIN', isReady: true },
      { to: '/admin/users', labelKey: 'nav.users', icon: Users, minRole: 'ADMIN', isReady: false },
      { to: '/admin/agents', labelKey: 'nav.agentIdentities', icon: Headphones, minRole: 'ADMIN', isReady: true },
      { to: '/admin/extensions', labelKey: 'nav.extensions', icon: PhoneCall, minRole: 'ADMIN', isReady: true },
      { to: '/admin/routing', labelKey: 'nav.routing', icon: ListChecks, minRole: 'ADMIN', isReady: true },
      { to: '/admin/numbers', labelKey: 'nav.numbers', icon: ScrollText, minRole: 'ADMIN', isReady: true },
      { to: '/admin/bots', labelKey: 'nav.bots', icon: Bot, minRole: 'ADMIN', isReady: true },
    ],
  },
  {
    labelKey: 'nav.system',
    roleKey: 'roles.ADMIN',
    roleHome: '/admin',
    items: [
      { to: '/admin/cdr', labelKey: 'nav.cdr', icon: FileClock, minRole: 'SUPERVISOR', isReady: true },
      { to: '/admin/reports', labelKey: 'nav.reports', icon: BarChart3, minRole: 'SUPERVISOR', isReady: true },
      { to: '/admin/audit', labelKey: 'nav.audit', icon: ScrollText, minRole: 'ADMIN', isReady: true },
    ],
  },
]

const ROLE_RANK: Record<Role, number> = { AGENT: 1, SUPERVISOR: 2, ADMIN: 3 }

export function allowed(role: Role, min: Role): boolean {
  return ROLE_RANK[role] >= ROLE_RANK[min]
}

export interface Crumb {
  labelKey: string
  to?: string
}

/**
 * Resolves "Role / Section" for a path. Every segment but the last links to a
 * fixed target, never to browser history.
 */
export function breadcrumbFor(pathname: string): Crumb[] {
  let best: { group: NavGroup; item: NavItem } | undefined
  for (const group of NAV) {
    for (const item of group.items) {
      if (pathname === item.to || pathname.startsWith(item.to + '/')) {
        if (!best || item.to.length > best.item.to.length) best = { group, item }
      }
    }
  }
  if (!best) return [{ labelKey: 'app.name' }]

  const crumbs: Crumb[] = [{ labelKey: best.group.roleKey, to: best.group.roleHome }]
  if (best.item.to !== best.group.roleHome) {
    crumbs.push({ labelKey: best.item.labelKey })
  } else {
    crumbs.push({ labelKey: best.item.labelKey })
  }
  return crumbs
}
