import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'

import '@/lib/i18n'
import '@/index.css'
import { installDevFixture } from '@/lib/dev-fixture'
import { createAppRouter } from './router'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Live data arrives over the event stream, so polling is unnecessary.
      refetchOnWindowFocus: false,
      retry: 1,
      staleTime: 10_000,
    },
  },
})

installDevFixture(queryClient)

const router = createAppRouter({ queryClient })

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
