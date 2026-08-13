import { redirect } from '@tanstack/react-router'

import type { Identity, Role } from './api'

const ROLE_RANK: Record<Role, number> = { AGENT: 1, SUPERVISOR: 2, ADMIN: 3 }

/**
 * Keeps a page out of the hands of a role that cannot use it.
 *
 * The server refuses the data regardless, so this is not the security
 * boundary; it exists so nobody is shown a page whose every request will fail.
 */
export function requireRole(user: Identity | undefined, min: Role) {
  if (!user) throw redirect({ to: '/login' })
  if (ROLE_RANK[user.role] < ROLE_RANK[min]) {
    throw redirect({ to: user.role === 'AGENT' ? '/agent' : '/supervisor' })
  }
}
