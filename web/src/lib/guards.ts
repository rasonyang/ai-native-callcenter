import { redirect } from '@tanstack/react-router'

import type { Identity, Role } from './api'
import { roleHomeFor } from './nav'

/**
 * Keeps a page out of the hands of a role it does not belong to.
 *
 * The roles are a set, not a floor, and they are the same set the sidebar
 * filters on: a page hidden from the menu but reachable by typing its URL is
 * half a rule, and the half that is missing is the one anybody would notice.
 *
 * The server refuses the data regardless, so this is not the security
 * boundary; it exists so nobody is shown a page whose every request will fail,
 * and so an administrator does not land in the agent cockpit.
 */
export function requireRole(user: Identity | undefined, ...roles: Role[]) {
  if (!user) throw redirect({ to: '/login' })
  if (!roles.includes(user.role)) {
    throw redirect({ to: roleHomeFor(user.role) })
  }
}
