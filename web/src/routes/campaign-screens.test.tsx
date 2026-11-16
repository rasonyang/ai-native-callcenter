import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import { withMaxOutboundChannels, type DID } from '@/lib/catalog'
import type { Identity, Role } from '@/lib/api'
import { renderWithProviders } from '@/test/harness'

import { Route as CampaignDetailRoute } from './_app.admin.campaigns.$campaignId'
import { Route as CampaignListRoute } from './_app.admin.campaigns.index'
import { Route as NewCampaignRoute } from './_app.admin.campaigns.new'
import { Route as DncRoute } from './_app.admin.dnc'
import { Route as NumbersRoute } from './_app.admin.numbers'
import { Route as TestNumbersRoute } from './_app.admin.test-numbers'

const as = (role: Role): Identity => ({
  userId: '00000000-0000-4000-8000-000000000001',
  username: role.toLowerCase(),
  displayName: role,
  role,
  locale: 'en',
})

/** A thrown redirect carries the page it sends the visitor to. */
function bounce(route: { options: { beforeLoad?: unknown } }, user: Identity | undefined) {
  try {
    ;(route.options.beforeLoad as (a: unknown) => void)({ context: { user } })
  } catch (thrown) {
    return (thrown as { options?: { to?: string } }).options?.to
  }
  return undefined
}

describe('who may open the campaign and do-not-call pages', () => {
  const routes = {
    list: CampaignListRoute,
    create: NewCampaignRoute,
    detail: CampaignDetailRoute,
    dnc: DncRoute,
    testNumbers: TestNumbersRoute,
  }

  it.each(Object.entries(routes))('turns an agent away from %s, to their own home', (_name, route) => {
    expect(bounce(route, as('AGENT'))).toBe('/agent')
  })

  it.each(Object.entries(routes))('sends a visitor with no session to sign in from %s', (_name, route) => {
    expect(bounce(route, undefined)).toBe('/login')
  })

  // The scope map grants campaigns:read, dnc:read and testnumbers:read from the supervisor
  // floor up, so both senior roles open the pages.
  it.each(Object.entries(routes))('lets a supervisor and an administrator into %s', (_name, route) => {
    expect(bounce(route, as('SUPERVISOR'))).toBeUndefined()
    expect(bounce(route, as('ADMIN'))).toBeUndefined()
  })
})

// --- the number form ---------------------------------------------------------

const DID_ID = '00000000-0000-4000-8000-0000000000d1'

function didFixture(over: Partial<DID> = {}): DID {
  return {
    id: DID_ID,
    number: '95099',
    language: 'en',
    isRecordingEnabled: true,
    description: 'Renewals',
    isEnabled: true,
    allowInbound: false,
    allowOutbound: true,
    isDefaultOutbound: false,
    maxOutboundChannels: 20,
    isCampaignReady: true,
    ...over,
  }
}

interface Recorded {
  method: string
  path: string
  body: unknown
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function stubDIDs(did: DID) {
  const requests: Recorded[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      const method = init.method ?? 'GET'
      requests.push({ method, path, body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined })
      if (path === '/dids' && method === 'GET') return json({ items: [did] })
      if (path.startsWith('/dids/') && method === 'PUT') return json(did)
      if (path === '/queues') return json({ items: [] })
      if (path === '/flows') return json({ items: [] })
      return json({ items: [] })
    }),
  )
  return requests
}

afterEach(() => {
  vi.unstubAllGlobals()
})

const Numbers = NumbersRoute.options.component as () => ReactElement

async function openEditor(did: DID) {
  const requests = stubDIDs(did)
  const view = renderWithProviders(<Numbers />)
  await screen.findByText('95099')
  await view.user.click(await screen.findByTitle('Edit'))
  const dialog = await screen.findByRole('dialog')
  return { requests, dialog, user: view.user }
}

const putOf = (requests: Recorded[]) => requests.find((r) => r.method === 'PUT')

describe('the number form and the outbound line limit', () => {
  it('shows the limit the number has', async () => {
    const { dialog } = await openEditor(didFixture())
    expect(within(dialog).getByLabelText('Outbound lines')).toHaveValue(20)
  })

  it('sends the stored limit back unchanged when the operator edits something else', async () => {
    const { requests, dialog, user } = await openEditor(didFixture())
    const description = within(dialog).getByLabelText('Description')
    await user.clear(description)
    await user.type(description, 'Renewals Q4')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(putOf(requests)).toBeDefined())
    expect(putOf(requests)?.body).toMatchObject({
      description: 'Renewals Q4',
      maxOutboundChannels: 20,
      allowOutbound: true,
    })
  })

  it('sends a new limit', async () => {
    const { requests, dialog, user } = await openEditor(didFixture())
    const field = within(dialog).getByLabelText('Outbound lines')
    await user.clear(field)
    await user.type(field, '35')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(putOf(requests)).toBeDefined())
    expect(putOf(requests)?.body).toMatchObject({ maxOutboundChannels: 35 })
  })

  it('sends no limit key at all when the field is emptied, which is "no limit"', async () => {
    const { requests, dialog, user } = await openEditor(didFixture())
    await user.clear(within(dialog).getByLabelText('Outbound lines'))
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(putOf(requests)).toBeDefined())
    expect(putOf(requests)?.body).not.toHaveProperty('maxOutboundChannels')
  })

  it('keeps a number that never had a limit without one', async () => {
    const { requests, dialog, user } = await openEditor(didFixture({ maxOutboundChannels: undefined }))
    expect(within(dialog).getByLabelText('Outbound lines')).toHaveValue(null)
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(putOf(requests)).toBeDefined())
    expect(putOf(requests)?.body).not.toHaveProperty('maxOutboundChannels')
  })

  it('does not offer the limit on a number that cannot dial out, and keeps its value', async () => {
    const { requests, dialog, user } = await openEditor(didFixture({ allowOutbound: false, allowInbound: true, flowId: DID_ID }))
    expect(within(dialog).queryByLabelText('Outbound lines')).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(putOf(requests)).toBeDefined())
    expect(putOf(requests)?.body).toMatchObject({ maxOutboundChannels: 20 })
  })
})

describe('reading the limit field', () => {
  it('ignores what is not a positive whole number', () => {
    const draft = didFixture()
    expect(withMaxOutboundChannels(draft, '0')).toBe(draft)
    expect(withMaxOutboundChannels(draft, '-3')).toBe(draft)
    expect(withMaxOutboundChannels(draft, '2.5')).toBe(draft)
    expect(withMaxOutboundChannels(draft, 'abc')).toBe(draft)
  })
  it('removes the key for an empty field and sets it for a number', () => {
    expect(withMaxOutboundChannels(didFixture(), '  ')).not.toHaveProperty('maxOutboundChannels')
    expect(withMaxOutboundChannels(didFixture(), '7').maxOutboundChannels).toBe(7)
  })
})
