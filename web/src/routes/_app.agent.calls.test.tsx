import { screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { Route } from '@/routes/_app.agent.calls'
import { cdrFixture, installBackend, renderWithProviders, type Backend } from '@/test/harness'

/**
 * My Calls. The agent is the session, never a parameter: there is no way to
 * ask this page for somebody else's calls, and the wrap-up on each row is the
 * agent's own filing rather than a colleague's.
 */

const MyCalls = Route.options.component as () => ReactElement

afterEach(() => {
  vi.unstubAllGlobals()
})

async function renderMyCalls(backend: Partial<Backend> = {}) {
  const api = installBackend(backend)
  return { api, ...renderWithProviders(<MyCalls />) }
}

it('asks for the caller’s own calls and names no agent', async () => {
  const { api } = await renderMyCalls({ myCDRs: [cdrFixture()] })

  await waitFor(() => expect(api.requests.some((r) => r.path.startsWith('/cdrs/mine'))).toBe(true))
  for (const request of api.requests) {
    expect(request.path).not.toMatch(/agentId/)
  }
  // Never the supervisor's ledger listing, which this account may not read.
  expect(api.requests.some((r) => r.path.startsWith('/cdrs?'))).toBe(false)
})

it('shows what the agent filed about each call', async () => {
  await renderMyCalls({
    myCDRs: [
      cdrFixture({
        wrapUp: {
          agentId: '00000000-0000-4000-8000-0000000000a1',
          dispositionCode: 'RESOLVED',
          dispositionLabel: 'Resolved',
          note: 'replaced the router',
          createdAt: new Date().toISOString(),
        },
      }),
    ],
  })

  expect(await screen.findByText('Resolved')).toBeInTheDocument()
  expect(screen.getByText('replaced the router')).toBeInTheDocument()
  expect(screen.getByText('support-zh')).toBeInTheDocument()
})

it('says plainly when a call was never wrapped up', async () => {
  await renderMyCalls({ myCDRs: [cdrFixture()] })
  expect(await screen.findByText(/not filed/i)).toBeInTheDocument()
})

// On the agent's own list the interesting number is always the customer's,
// whichever side of the call placed it.
it('shows the customer, not the agent, on an outbound call', async () => {
  await renderMyCalls({
    myCDRs: [cdrFixture({ callType: 'OUTBOUND', fromNumber: '95001', toNumber: '+8613912340001' })],
  })

  expect(await screen.findByText('+8613912340001')).toBeInTheDocument()
})

it('sends the status filter the agent chose', async () => {
  const { api, user } = await renderMyCalls({ myCDRs: [cdrFixture()] })

  await user.selectOptions(await screen.findByLabelText(/status/i), 'NO_ANSWER')
  await waitFor(() =>
    expect(api.requests.some((r) => r.path.includes('status=NO_ANSWER'))).toBe(true),
  )
})

describe('empty', () => {
  it('invites the agent to take a call rather than showing an empty grid', async () => {
    await renderMyCalls({ myCDRs: [] })
    expect(await screen.findByText(/no calls yet/i)).toBeInTheDocument()
  })
})
