import type { QueryClient } from '@tanstack/react-query'
import { createRouter, type RouterHistory } from '@tanstack/react-router'

import { RouteError, markCaught } from '@/components/route-error'
import { routeTree } from './routeTree.gen'

/**
 * The one place the application's router is built, so the tests run the same
 * configuration the browser does. Every route match gets the designed error
 * state; without a default the router falls back to its stock component and
 * warns that nothing caught the error (#327).
 */
export function createAppRouter({ queryClient, history }: { queryClient: QueryClient; history?: RouterHistory }) {
  return createRouter({
    routeTree,
    context: { queryClient },
    history,
    defaultPreload: 'intent',
    defaultErrorComponent: ({ error }) => <RouteError error={error} />,
    // React logs what a boundary catches; this only marks the error so the
    // component does not log it a second time.
    defaultOnCatch: (error) => markCaught(error),
  })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
