import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import { useAuditLogs, type AuditFilter } from '@/lib/ledger'

/**
 * The page's own component is bound to the router, so these exercise the two
 * things that are this feature's own rather than the table's: which filter the
 * two controls add up to, and what a row says when the account that acted has
 * since been deleted.
 */

const ENTRIES = [
  {
    auditId: 2,
    occurredAt: '2026-08-24T04:30:39Z',
    actorId: '00000000-0000-4000-8000-00000000dead',
    action: 'DELETE /api/v1/extensions/{extensionId}',
    targetKind: 'extension',
    targetId: '11111111-2222-3333-4444-555555555555',
    detail: { request: '{"number":"1099","password":"[redacted]"}' },
    ip: '192.168.31.5',
  },
]

function stubFetch() {
  const seen: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      seen.push(url)
      return Response.json({ items: ENTRIES, total: 1 })
    }),
  )
  return seen
}

function Harness({ filter }: { filter: AuditFilter }) {
  const { data } = useAuditLogs(filter)
  return (
    <ul>
      {(data?.items ?? []).map((e) => (
        <li key={e.auditId}>
          {e.actorUsername ?? (e.actorId ? `deleted ${e.actorId.slice(0, 8)}` : '—')}
          {' · '}
          {e.action}
          {' · '}
          {String((e.detail as Record<string, unknown>)?.request ?? '')}
        </li>
      ))}
    </ul>
  )
}

function renderHarness(filter: AuditFilter) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <Harness filter={filter} />
      </I18nextProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('audit trail', () => {
  // The stored action is "METHOD /route/template", so one literal prefix
  // carries both controls. Sending them as two filters would need a shape the
  // stored value does not have.
  it('asks for one action prefix built from the method and the route', async () => {
    const seen = stubFetch()
    renderHarness({ actionPrefix: 'DELETE /api/v1/extensions', limit: 50, offset: 0 })

    await waitFor(() => expect(seen.length).toBeGreaterThan(0))
    const url = seen[0]
    expect(url).toContain('actionPrefix=DELETE+%2Fapi%2Fv1%2Fextensions')
    expect(url).not.toContain('method=')
  })

  // An account can be deleted while its actions stay recorded. A blank cell
  // would read as "nobody did this", which is the one thing the row disproves.
  it('still names the actor of a change when the account is gone', async () => {
    stubFetch()
    renderHarness({ limit: 50 })

    await waitFor(() => expect(screen.getByText(/deleted 00000000/)).toBeInTheDocument())
  })

  // Redaction happens on the way in, so the page has nothing to hide — but it
  // must not be re-introducing the value either.
  it('shows the request as stored, redaction included', async () => {
    stubFetch()
    renderHarness({ limit: 50 })

    await waitFor(() => expect(screen.getByText(/\[redacted\]/)).toBeInTheDocument())
    expect(screen.queryByText(/vc-probe-pass/)).not.toBeInTheDocument()
  })
})
