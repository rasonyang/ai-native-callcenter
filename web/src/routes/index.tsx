import { createFileRoute, redirect } from '@tanstack/react-router'

import { api } from '@/lib/api'
import { roleHomeFor } from '@/lib/nav'

/** Entry point: send each role to the landing page it actually works from. */
export const Route = createFileRoute('/')({
  beforeLoad: async () => {
    try {
      const { user } = await api.me()
      throw redirect({ to: roleHomeFor(user.role) })
    } catch (error) {
      if (error && typeof error === 'object' && 'to' in error) throw error
      throw redirect({ to: '/login' })
    }
  },
})
