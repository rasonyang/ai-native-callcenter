import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { QueuePerformance } from '@/components/queue-performance'
import i18n from '@/lib/i18n'

/**
 * One queue, taken from live data on 2026-08-24: nineteen calls offered, ten
 * abandoned, nine answered and every one of those inside the threshold.
 *
 * Dividing by answered calls rates that queue 100%. More than half its callers
 * gave up waiting.
 */
const REPORT = {
  items: [
    {
      queueId: '019ffaae-9bf6-7e48-8fc7-47ce0998250f',
      totalCalls: 19,
      answeredCalls: 9,
      abandonedCalls: 10,
      answeredWithinSla: 9,
      avgWaitSec: 40,
      maxWaitSec: 207,
      avgTalkSec: 19,
    },
  ],
}

function renderTable() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/reports/queues')) return Response.json(REPORT)
      if (url.includes('/queues')) return Response.json({ items: [] })
      return new Response('{}', { status: 200 })
    }),
  )
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <QueuePerformance />
      </I18nextProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('queue service level', () => {
  // C27: the supervisor's wallboard divided by answered calls and the admin
  // report by total calls, both labelled only "SLA". A supervisor saw 100%
  // where an administrator saw 47%, on one queue on one day. Worse than a
  // disagreement: the fewer calls a queue answers, the smaller the denominator
  // gets with them, so the worse it does the better it looks.
  it('is measured over every call offered, not over the ones that were answered', async () => {
    renderTable()

    await waitFor(() => expect(screen.getByText('47%')).toBeInTheDocument())
    expect(screen.queryByText('100%')).not.toBeInTheDocument()
  })

  it('says in the header which question the number answers', async () => {
    renderTable()

    await waitFor(() => expect(screen.getByText(i18n.t('supervisor.sla'))).toBeInTheDocument())
    expect(screen.getByText(i18n.t('supervisor.sla'))).toHaveAttribute(
      'title',
      i18n.t('supervisor.slaHint'),
    )
  })
})
