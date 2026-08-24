import {
  Activity, BarChart3, Bot, BookUser, FileClock, Headphones, LayoutDashboard,
  ListChecks, Phone, PhoneCall, PhoneForwarded, ScrollText, ShieldCheck, Users,
} from 'lucide-react'


import type { Role } from './api'

export interface NavItem {
  to: string
  labelKey: string
  icon: typeof LayoutDashboard
  /**
   * Exactly the roles this page belongs to — a set, not a floor.
   *
   * A rank ("SUPERVISOR and above") gave every administrator the agent
   * cockpit and the wallboard as well, so the sidebar grew with seniority
   * instead of describing a job. An administrator configures the platform; a
   * supervisor watches the floor; an agent takes calls. The two pages both
   * senior roles genuinely share — the call ledger and the reports — say so by
   * naming both.
   */
  roles: Role[]
  /** Built pages only. Anything unbuilt stays out of the sidebar. */
  isReady: boolean
}

export interface NavGroup {
  labelKey: string
  /** The role index this group's items sit under, for the active-link rule. */
  roleHome: string
  items: NavItem[]
}

/** Where a role starts, and where it is sent back to. */
export function roleHomeFor(role: Role): string {
  if (role === 'AGENT') return '/agent'
  if (role === 'SUPERVISOR') return '/supervisor'
  return '/admin'
}

/**
 * The sidebar, and the source the breadcrumb resolves against: every page must
 * be reachable from a nav item so "Role / Section" can be derived rather than
 * hand-written (web/CLAUDE.md).
 */
export const NAV: NavGroup[] = [
  {
    labelKey: 'nav.workspace',
    roleHome: '/agent',
    items: [
      { to: '/agent', labelKey: 'nav.dashboard', icon: Headphones, roles: ['AGENT'], isReady: true },
      { to: '/agent/calls', labelKey: 'nav.myCalls', icon: Phone, roles: ['AGENT'], isReady: true },
      { to: '/agent/contacts', labelKey: 'nav.contacts', icon: BookUser, roles: ['AGENT'], isReady: true },
      { to: '/agent/callbacks', labelKey: 'nav.callbacks', icon: PhoneForwarded, roles: ['AGENT'], isReady: true },
    ],
  },
  {
    labelKey: 'nav.supervise',
    roleHome: '/supervisor',
    items: [
      { to: '/supervisor', labelKey: 'nav.wallboard', icon: LayoutDashboard, roles: ['SUPERVISOR'], isReady: true },
      { to: '/supervisor/agents', labelKey: 'nav.agents', icon: Users, roles: ['SUPERVISOR'], isReady: true },
      { to: '/supervisor/queues', labelKey: 'nav.queues', icon: ListChecks, roles: ['SUPERVISOR'], isReady: true },
      { to: '/supervisor/quality', labelKey: 'nav.quality', icon: ShieldCheck, roles: ['SUPERVISOR'], isReady: false },
    ],
  },
  {
    labelKey: 'nav.manage',
    roleHome: '/admin',
    items: [
      { to: '/admin', labelKey: 'nav.overview', icon: Activity, roles: ['ADMIN'], isReady: true },
      { to: '/admin/users', labelKey: 'nav.users', icon: Users, roles: ['ADMIN'], isReady: false },
      { to: '/admin/agents', labelKey: 'nav.agentIdentities', icon: Headphones, roles: ['ADMIN'], isReady: true },
      { to: '/admin/extensions', labelKey: 'nav.extensions', icon: PhoneCall, roles: ['ADMIN'], isReady: true },
      { to: '/admin/routing', labelKey: 'nav.routing', icon: ListChecks, roles: ['ADMIN'], isReady: true },
      { to: '/admin/numbers', labelKey: 'nav.numbers', icon: ScrollText, roles: ['ADMIN'], isReady: true },
      { to: '/admin/bots', labelKey: 'nav.bots', icon: Bot, roles: ['ADMIN'], isReady: true },
    ],
  },
  {
    labelKey: 'nav.system',
    roleHome: '/admin',
    items: [
      // Finished calls and the reports over them are the one place the two
      // senior roles look at the same screen: a supervisor reviews the floor's
      // day, an administrator answers for the platform's.
      { to: '/admin/cdr', labelKey: 'nav.cdr', icon: FileClock, roles: ['ADMIN', 'SUPERVISOR'], isReady: true },
      { to: '/admin/reports', labelKey: 'nav.reports', icon: BarChart3, roles: ['ADMIN', 'SUPERVISOR'], isReady: true },
      // The trail names accounts and carries what their requests contained,
      // which is administration rather than supervision.
      { to: '/admin/audit', labelKey: 'nav.audit', icon: ScrollText, roles: ['ADMIN'], isReady: true },
    ],
  },
]

/** Whether this role is one of the ones a page belongs to. */
export function visibleTo(role: Role, item: NavItem): boolean {
  return item.roles.includes(role)
}

export interface Crumb {
  /** A translation key, for the segments that come from the nav config. */
  labelKey?: string
  /** Literal text, for a segment naming one record. */
  label?: string
  /** Absent on the last segment: it is where the reader already is. */
  to?: string
}

/** Where a group starts for this viewer — the first page in it they may open. */
function groupHomeFor(group: NavGroup, role: Role): string | undefined {
  return group.items.find((item) => visibleTo(role, item) && item.isReady)?.to
}

/**
 * Resolves "Group / Section / Detail" for a path. Every segment but the last
 * links to a fixed target, never to browser history, so any level above the
 * current one is one click away.
 *
 * The first segment is the sidebar group — Manage, System, Supervise,
 * Workspace — not the reader's role. The group is what the page actually sits
 * under, and it is the same word they just clicked in the sidebar; a role name
 * there claimed something else, and on a page two roles share (the ledger) it
 * claimed it wrongly.
 */
export function breadcrumbFor(pathname: string, role: Role, detail?: string): Crumb[] {
  let best: { group: NavGroup; item: NavItem } | undefined
  for (const group of NAV) {
    for (const item of group.items) {
      if (pathname === item.to || pathname.startsWith(item.to + '/')) {
        if (!best || item.to.length > best.item.to.length) best = { group, item }
      }
    }
  }
  if (!best) return [{ labelKey: 'app.name' }]

  const trail: Crumb[] = [
    { labelKey: best.group.labelKey, to: groupHomeFor(best.group, role) },
    { labelKey: best.item.labelKey, to: best.item.to },
  ]
  if (detail) trail.push({ label: detail })

  // The reader is standing on the last one, so it is text rather than a link.
  delete trail[trail.length - 1].to
  return trail
}
