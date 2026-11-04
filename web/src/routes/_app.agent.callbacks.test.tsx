import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.callbacks'
import { installBackend, renderWithProviders } from '@/test/harness'

/** The agent's queue pages like the team's: every callback is reachable. */

const MyCallbacks = Route.options.component as () => ReactElement

afterEach(() => vi.unstubAllGlobals())

it('pages the queue and starts over when the filter changes', async () => {
  const api = installBackend({
    handle: ({ path, method }) =>
      method === 'GET' && path.startsWith('/callbacks')
        ? {
            items: [{
              id: '0199f3a2-7c41-7d22-9b1e-000000000001', phoneNumber: '+8613800000001',
              message: 'a row', status: 'OPEN', createdAt: '2026-10-20T09:12:00Z',
            }],
            total: 120,
          }
        : undefined,
  })
  const { user } = renderWithProviders(<MyCallbacks />)
  const last = () => api.requests.findLast((r) => r.path.startsWith('/callbacks'))?.path

  await screen.findByText('a row')
  expect(last()).toBe('/callbacks?limit=50&offset=0')
  await user.click(screen.getByRole('button', { name: 'Next' }))
  await waitFor(() => expect(last()).toBe('/callbacks?limit=50&offset=50'))

  fireEvent.change(screen.getByRole('combobox'), { target: { value: 'DONE' } })
  await waitFor(() => expect(last()).toBe('/callbacks?status=DONE&limit=50&offset=0'))
})
