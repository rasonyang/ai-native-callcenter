import { QueryClient } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { waitFor } from '@testing-library/react'
import { vi } from 'vitest'

import { routeTree } from '@/routeTree.gen'

import {
  identityFixture, installBackend, installFakeExtension, renderWithProviders,
  type Backend, type FakeExtension,
} from './harness'

/**
 * Renders a screen inside the real application: the generated route tree, so
 * the `_app` layout (220px sidebar, top bar, the padded `main`) wraps the page
 * exactly as it does in production. A layout test that drew a page full-bleed
 * would measure a width no user ever sees; this one cannot, because there is no
 * second shell to drift from the first.
 *
 * Browser tests only (`*.layout.test.tsx`): it pulls in every route.
 */

/** An EventSource that never connects. The shell opens one for the live feed. */
class IdleEventSource {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  readyState = IdleEventSource.CONNECTING
  onopen = null
  onmessage = null
  onerror = null
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

/** The previous test's stand-in phone, removed so it stops answering hellos. */
let previousExtension: FakeExtension | undefined

export function renderApp(
  path: string,
  backend: Partial<Backend> = {},
  options: {
    /**
     * Who is signed in. ADMIN by default, the first customers being the admin
     * screens. An AGENT gets what production draws around an agent's page: the
     * softphone bar in the top bar, the presence query, and a phone extension
     * that answers, so the setup card does not cover the screen.
     */
    role?: 'ADMIN' | 'AGENT' | 'SUPERVISOR'
    /** What the agent's stand-in phone reports; the default is a phone that is registered. */
    extension?: Parameters<typeof installFakeExtension>[0]
  } = {},
) {
  const role = options.role ?? 'ADMIN'
  previousExtension?.uninstall()
  previousExtension = role === 'AGENT' ? installFakeExtension(options.extension) : undefined
  const installed = installBackend({
    session:
      role === 'AGENT'
        ? identityFixture({ role })
        : identityFixture({ role, displayName: role === 'ADMIN' ? 'Admin' : 'Supervisor' }),
    ...backend,
  })
  vi.stubGlobal('EventSource', IdleEventSource)

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  const rendered = renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
    { queryClient },
  )
  return { ...rendered, backend: installed, router }
}

/**
 * Waits until at least `min` data rows are on screen. A table draws its
 * "Loading" message first and a route draws nothing until its module is
 * loaded, so "no loading row" would pass before anything arrived; counting the
 * rows themselves (those with a cell that is not a full-width message) cannot.
 */
export async function waitForRows(min: number) {
  await waitFor(() => {
    const rows = [...document.querySelectorAll('tbody tr')]
      .filter((row) => row.querySelector('td:not([colspan])'))
    if (rows.length < min) throw new Error(`${rows.length} of ${min} rows are on screen`)
  })
}
