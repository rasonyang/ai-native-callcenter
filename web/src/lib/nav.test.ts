import { describe, expect, it } from 'vitest'

import type { Identity, Role } from '@/lib/api'
import { requireRole } from '@/lib/guards'
import { NAV, breadcrumbFor, roleHomeFor, visibleTo } from '@/lib/nav'

/**
 * The sidebar is a job description, so the three lists are pinned literally.
 *
 * They used to be a rank — "SUPERVISOR and above" — which handed every
 * administrator the agent cockpit and the wallboard as well. Nothing failed;
 * the menu simply grew with seniority instead of describing what the person
 * in front of it does. A rank cannot express the one real overlap either: the
 * call ledger and the reports belong to both senior roles and to neither
 * junior one.
 */
function menuFor(role: Role): string[] {
  return NAV.flatMap((group) =>
    group.items.filter((item) => visibleTo(role, item) && item.isReady).map((item) => item.to),
  )
}

describe('the sidebar each role gets', () => {
  it('gives an administrator the platform, and nothing operational', () => {
    expect(menuFor('ADMIN')).toEqual([
      '/admin',
      '/admin/users',
      '/admin/extensions',
      '/admin/routing',
      '/admin/numbers',
      '/admin/bots',
      '/admin/cdr',
      '/admin/reports',
      '/admin/audit',
      '/admin/webhooks',
    ])
  })

  it('gives a supervisor the floor, plus the ledger and the reports', () => {
    expect(menuFor('SUPERVISOR')).toEqual([
      '/supervisor',
      '/supervisor/agents',
      '/supervisor/queues',
      '/admin/cdr',
      '/admin/reports',
    ])
  })

  it('gives an agent their own calls and nobody else’s', () => {
    expect(menuFor('AGENT')).toEqual([
      '/agent',
      '/agent/calls',
      '/agent/contacts',
      '/agent/callbacks',
    ])
  })

  // The two overlaps are deliberate and small; everything else is exclusive.
  it('overlaps only on the ledger and the reports', () => {
    const admin = new Set(menuFor('ADMIN'))
    const shared = menuFor('SUPERVISOR').filter((to) => admin.has(to))
    expect(shared).toEqual(['/admin/cdr', '/admin/reports'])
    expect(menuFor('AGENT').filter((to) => admin.has(to))).toEqual([])
  })
})

const as = (role: Role): Identity => ({
  userId: '00000000-0000-4000-8000-000000000001',
  username: role.toLowerCase(),
  displayName: role,
  role,
  locale: null,
})

/** A thrown redirect is a Response carrying the navigation it wants. */
function bounce(fn: () => void): string | undefined {
  try {
    fn()
  } catch (thrown) {
    return (thrown as { options?: { to?: string } }).options?.to
  }
  return undefined
}

describe('the route guard', () => {
  it('lets a page through for a role it belongs to', () => {
    expect(bounce(() => requireRole(as('ADMIN'), 'ADMIN'))).toBeUndefined()
    expect(bounce(() => requireRole(as('SUPERVISOR'), 'ADMIN', 'SUPERVISOR'))).toBeUndefined()
  })

  // Seniority is not access here: an administrator configures the platform, and the
  // wallboard is not theirs. Hiding it from the menu while leaving the URL
  // open would be half a rule.
  it('turns a senior role away from a page that is not theirs', () => {
    // Refused, and sent to their own home rather than into the section that
    // refused them — a bounce towards the closed door is the loop.
    expect(bounce(() => requireRole(as('ADMIN'), 'SUPERVISOR'))).toBe('/admin')
    expect(bounce(() => requireRole(as('ADMIN'), 'AGENT'))).toBe('/admin')
    expect(bounce(() => requireRole(as('SUPERVISOR'), 'ADMIN'))).toBe('/supervisor')
    expect(bounce(() => requireRole(as('AGENT'), 'ADMIN', 'SUPERVISOR'))).toBe('/agent')
  })

  it('sends the turned-away visitor to their own home, not to a loop', () => {
    for (const role of ['AGENT', 'SUPERVISOR', 'ADMIN'] as Role[]) {
      const home = roleHomeFor(role)
      // Whatever page refused them, the landing place is a page they can open.
      expect(bounce(() => requireRole(as(role), 'NOBODY' as Role))).toBe(home)
    }
  })

  it('sends a visitor with no session to the login page', () => {
    expect(bounce(() => requireRole(undefined, 'ADMIN'))).toBe('/login')
  })
})

describe('the breadcrumb', () => {
  it('starts at the sidebar group the page sits under', () => {
    // Not the reader's role: the ledger is one screen two roles share, and
    // "Administrator" would be a claim about the reader that this page is in
    // no position to make.
    expect(breadcrumbFor('/admin/cdr', 'SUPERVISOR')).toEqual([
      { labelKey: 'nav.system', to: '/admin/cdr' },
      { labelKey: 'nav.cdr' },
    ])
    expect(breadcrumbFor('/admin/cdr', 'ADMIN')).toEqual([
      { labelKey: 'nav.system', to: '/admin/cdr' },
      { labelKey: 'nav.cdr' },
    ])
  })

  // A group has no page of its own, so it links to where it starts for this
  // reader — which is a different page for a supervisor than for an
  // administrator, and never one they cannot open.
  it('links a group to the first page in it this reader may open', () => {
    expect(breadcrumbFor('/admin/bots', 'ADMIN')[0]).toEqual({
      labelKey: 'nav.manage',
      to: '/admin',
    })
    expect(breadcrumbFor('/supervisor/queues', 'SUPERVISOR')[0]).toEqual({
      labelKey: 'nav.supervise',
      to: '/supervisor',
    })
  })

  it('ends in the record the page is about, and links the section above it', () => {
    expect(breadcrumbFor('/admin/bots/019ffd60', 'ADMIN', 'novanet_support')).toEqual([
      { labelKey: 'nav.manage', to: '/admin' },
      { labelKey: 'nav.bots', to: '/admin/bots' },
      { label: 'novanet_support' },
    ])
  })

  // Every level above the current one is one click away, and the current one
  // is never a link — the reader is already standing on it.
  it('links every segment but the last', () => {
    for (const trail of [
      breadcrumbFor('/admin/bots', 'ADMIN'),
      breadcrumbFor('/admin/bots/019ffd60', 'ADMIN', 'novanet_support'),
      breadcrumbFor('/agent/calls', 'AGENT'),
    ]) {
      expect(trail.slice(0, -1).every((crumb) => Boolean(crumb.to))).toBe(true)
      expect(trail[trail.length - 1].to).toBeUndefined()
    }
  })

  // Until the page knows what it is looking at there is nothing to add, and a
  // gap would read as a record with no name.
  it('stops at the section while the record is still loading', () => {
    expect(breadcrumbFor('/admin/bots/019ffd60', 'ADMIN')).toEqual([
      { labelKey: 'nav.manage', to: '/admin' },
      { labelKey: 'nav.bots' },
    ])
  })
})
