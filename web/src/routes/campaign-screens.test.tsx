import { QueryClient } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter, type RouterHistory,
} from '@tanstack/react-router'
import { act, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import type { Campaign, CampaignContact } from '@/lib/campaigns'
import { withMaxOutboundChannels, type DID } from '@/lib/catalog'
import type { Identity, Role } from '@/lib/api'
import i18n from '@/lib/i18n'
import { installBackend, renderWithProviders, type Backend } from '@/test/harness'

import { Route as CampaignDetailRoute, campaignDetailSearch } from './_app.admin.campaigns.$campaignId'
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

// --- the campaign page's address ----------------------------------------------

const CAMPAIGN_ID = '0199f3a2-7c41-7d22-9b1e-5a2f4c8d1e07'
const CALL_ID = '0199f3a2-7c41-7d22-9b1e-000000002001'
const DETAIL = `/admin/campaigns/${CAMPAIGN_ID}`

const campaignFixture: Campaign = {
  id: CAMPAIGN_ID,
  name: 'October renewal reminders',
  didId: DID_ID,
  didNumber: '95099',
  status: 'RUNNING',
  activity: 'WAITING_WINDOW',
  nextDialAt: '2026-10-30T01:00:00Z',
  inFlight: 5,
  timeZone: 'Asia/Shanghai',
  callingWindow: { days: ['MON', 'TUE', 'WED', 'THU', 'FRI'], ranges: [{ start: '09:00', end: '18:00' }] },
  maxConcurrent: 5,
  ringTimeoutSec: 30,
  retryPolicy: {
    maxAttempts: 3,
    maxSystemRetries: 5,
    retryOutcomes: ['NO_ANSWER', 'BUSY'],
    intervalSec: { NO_ANSWER: 7200, BUSY: 1800 },
    transientBackoff: { baseSec: 300, factor: 3, capSec: 14400 },
  },
  createdAt: '2026-10-20T09:12:00Z',
  contacts: { total: 12000, active: 8300, done: 3100, exhausted: 400, invalidNumber: 120, dnc: 80 },
}

/** A lead whose call took place, so its row links to the call record. */
const leadFixture: CampaignContact = {
  id: '0199f3a2-7c41-7d22-9b1e-000000001001',
  campaignId: CAMPAIGN_ID,
  number: '+8613800000000',
  numberRaw: '13800000000',
  slots: {},
  status: 'DONE',
  attempts: 1,
  systemFailures: 0,
  lastOutcome: 'ANSWERED',
  leadIntent: 'MEDIUM',
  leadNotes: 'Asked to be called after the holiday.',
  lastCallId: CALL_ID,
  lastAttemptId: '0199f3a2-7c41-7d22-9b1e-000000003001',
  createdAt: '2026-10-20T09:12:00Z',
}

const CampaignDetail = CampaignDetailRoute.options.component as () => ReactElement

/**
 * What the server makes of a searched number, for the one country these tests
 * dial in (86): the lead's number, no number, or a refusal (anything that is
 * not "+", digits, spaces and dashes with seven digits or more).
 */
function searchedFor(raw: string): 'lead' | 'none' | 'refused' {
  if (!/^\+?[\d -]+$/.test(raw) || raw.replace(/\D/g, '').length < 7) return 'refused'
  const digits = raw.replace(/\D/g, '')
  const e164 = raw.startsWith('+') ? `+${digits}` : `+86${digits.replace(/^0/, '')}`
  return e164 === leadFixture.number ? 'lead' : 'none'
}

const refusedNumber = () =>
  json({
    error: {
      code: 'VALIDATION_FAILED',
      message: 'phoneNumber is not a phone number',
      params: { field: 'phoneNumber', rule: 'NUMBER_INVALID' },
    },
  }, 400)

/**
 * The campaign page under a memory router that starts at the last of
 * `entries`, with the route's own search validation. Every list answers 120
 * rows in total, three pages; a list searched by number answers the lead when
 * it is the lead's number, nothing else, and refuses what is not a number.
 */
function renderCampaignAt(entries: string[], options: { queryClient?: QueryClient } = {}) {
  const backend = installBackend({
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      const [route, params] = path.split('?')
      const query = new URLSearchParams(params)
      const number = query.get('phoneNumber')
      const found = number === null ? 'all' : searchedFor(number)
      if (found === 'refused') return refusedNumber()
      if (route === `/campaigns/${CAMPAIGN_ID}`) return campaignFixture
      if (route === `/campaigns/${CAMPAIGN_ID}/contacts`) {
        const intent = query.get('leadIntent')
        const items = [leadFixture].filter((row) => !intent || row.leadIntent === intent)
        if (found === 'all') return { items, total: 120 }
        return found === 'lead' ? { items, total: items.length } : { items: [], total: 0 }
      }
      if (route === `/campaigns/${CAMPAIGN_ID}/attempts`) return { items: [], total: found === 'all' ? 120 : 0 }
      if (route === '/callbacks') return { items: [] }
      return undefined
    },
  })
  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({ getParentRoute: () => root, path: '/admin/campaigns', component: () => null }),
    createRoute({
      getParentRoute: () => root,
      path: '/admin/campaigns/$campaignId',
      validateSearch: CampaignDetailRoute.options.validateSearch,
      component: CampaignDetail,
    }),
    createRoute({ getParentRoute: () => root, path: '/admin/cdr/$callId', component: () => <h1>call record</h1> }),
  ])
  const history = createMemoryHistory({ initialEntries: entries })
  const router = createRouter({ routeTree: tree, history })
  const view = renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
    options,
  )
  return { ...view, backend, history }
}

/** The address bar's query, as plain strings. */
const addressOf = (history: RouterHistory) => Object.fromEntries(new URLSearchParams(history.location.search))

/** The query of each request for a campaign list, oldest first. */
const listQueries = (backend: Backend, list: 'contacts' | 'attempts') =>
  backend.requests
    .filter((r) => r.method === 'GET' && r.path.split('?')[0] === `/campaigns/${CAMPAIGN_ID}/${list}`)
    .map((r) => Object.fromEntries(new URLSearchParams(r.path.split('?')[1])))

const tab = (key: string) => ({ name: i18n.t(`campaign.tabs.${key}`) })

describe('the campaign page keeps its tab, filter and page in the address', () => {
  // jsdom has no scrolling for the router to reset on the first render.
  beforeEach(() => {
    vi.stubGlobal('scrollTo', () => {})
  })

  it('opens the tab the address names', async () => {
    const { backend } = renderCampaignAt([`${DETAIL}?tab=leads`])
    expect(await screen.findByRole('tab', { ...tab('leads'), selected: true })).toBeInTheDocument()
    expect(within(screen.getByRole('tabpanel')).getByText(i18n.t('campaign.leadNotes'))).toBeInTheDocument()
    await waitFor(() => expect(listQueries(backend, 'contacts')).toEqual([
      { leadIntent: 'HIGH', limit: '50', offset: '0' },
    ]))
  })

  it('opens Contacts, unfiltered on its first page, for an address it does not understand', async () => {
    const { backend } = renderCampaignAt([`${DETAIL}?tab=reports&status=GONE&intent=SOME&page=-2`])
    expect(await screen.findByRole('tab', { ...tab('contacts'), selected: true })).toBeInTheDocument()
    await waitFor(() => expect(listQueries(backend, 'contacts')).toEqual([{ limit: '50', offset: '0' }]))
  })

  it('hands the filter and the page in the address to the query', async () => {
    const { backend } = renderCampaignAt([`${DETAIL}?tab=attempts&outcome=NO_ANSWER&page=2`])
    await waitFor(() => expect(listQueries(backend, 'attempts')).toEqual([
      { outcome: 'NO_ANSWER', limit: '50', offset: '50' },
    ]))
    expect(screen.getByRole('combobox', { name: i18n.t('campaign.outcome') })).toHaveValue('NO_ANSWER')
    expect(await screen.findByText(i18n.t('common.pageOf', { page: 2, pages: 3, total: 120 }))).toBeInTheDocument()
  })

  it('switches tab in place of the entry, so one Back still leaves the campaign', async () => {
    const { user, history } = renderCampaignAt(['/admin/campaigns', `${DETAIL}?status=DONE&page=2`])
    await user.click(await screen.findByRole('tab', tab('attempts')))
    // The new tab opens unfiltered on its first page.
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'attempts' }))
    expect(history.length).toBe(2)
    expect(await screen.findByRole('tab', { ...tab('attempts'), selected: true })).toBeInTheDocument()
    act(() => history.back())
    await waitFor(() => expect(history.location.pathname).toBe('/admin/campaigns'))
  })

  it('starts a changed filter on its first page', async () => {
    const { user, history, backend } = renderCampaignAt([`${DETAIL}?intent=LOW&page=3`])
    await waitFor(() => expect(listQueries(backend, 'contacts').at(-1)).toEqual({
      leadIntent: 'LOW', limit: '50', offset: '100',
    }))
    await user.selectOptions(screen.getByRole('combobox', { name: i18n.t('campaign.contactStatus') }), 'DONE')
    await waitFor(() => expect(addressOf(history)).toEqual({ status: 'DONE', intent: 'LOW' }))
    await waitFor(() => expect(listQueries(backend, 'contacts').at(-1)).toEqual({
      status: 'DONE', leadIntent: 'LOW', limit: '50', offset: '0',
    }))
  })

  it("comes Back from a lead's call record to the same tab, intent and page", async () => {
    const { user, history } = renderCampaignAt(['/admin/campaigns', DETAIL])
    await user.click(await screen.findByRole('tab', tab('leads')))
    await user.selectOptions(await screen.findByRole('combobox', { name: i18n.t('campaign.leadIntent') }), 'MEDIUM')
    await user.click(await screen.findByRole('button', { name: i18n.t('common.next') }))
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'leads', intent: 'MEDIUM', page: '2' }))

    await user.click(await screen.findByRole('link', { name: i18n.t('campaign.openCall') }))
    await screen.findByRole('heading', { name: 'call record' })
    act(() => history.back())

    expect(await screen.findByRole('tab', { ...tab('leads'), selected: true })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: i18n.t('campaign.leadIntent') })).toHaveValue('MEDIUM')
    expect(await screen.findByText(i18n.t('common.pageOf', { page: 2, pages: 3, total: 120 }))).toBeInTheDocument()
    // Every move on the page replaced its entry: the next Back leaves it.
    act(() => history.back())
    await waitFor(() => expect(history.location.pathname).toBe('/admin/campaigns'))
  })
})

describe('reading the campaign page address', () => {
  it.each([
    [{}, {}],
    [{ tab: 'contacts', page: 1 }, {}],
    [{ tab: 'contacts', status: 'DONE', intent: 'LOW', page: 3 }, { status: 'DONE', intent: 'LOW', page: 3 }],
    // HIGH is what Leads shows when the address names no intent.
    [{ tab: 'leads', intent: 'HIGH' }, { tab: 'leads' }],
    [{ tab: 'leads', intent: 'LOW', page: 4 }, { tab: 'leads', intent: 'LOW', page: 4 }],
    // A filter the open tab does not have is dropped.
    [{ tab: 'attempts', status: 'DONE', intent: 'LOW', outcome: 'BUSY' }, { tab: 'attempts', outcome: 'BUSY' }],
    [{ tab: 'outcomes', outcome: 'BUSY', page: 3 }, { tab: 'outcomes' }],
    [{ tab: 'settings' }, { tab: 'settings' }],
    [{ tab: 'Leads', status: 'done', intent: 7, outcome: null }, {}],
    [{ page: 0 }, {}],
    [{ page: -2 }, {}],
    [{ page: 2.5 }, {}],
    [{ page: 'two' }, {}],
    [{ page: 1e300 }, {}],
    // A searched number stays as it was typed, trimmed, on the three listings.
    [{ number: ' 138 0013 8000 ' }, { number: '138 0013 8000' }],
    [{ tab: 'leads', intent: 'LOW', number: '+86 138-0013-8000' }, { tab: 'leads', intent: 'LOW', number: '+86 138-0013-8000' }],
    [{ tab: 'attempts', number: '021 6555 0101', page: 2 }, { tab: 'attempts', number: '021 6555 0101', page: 2 }],
    // Digits alone come back from the router as a JSON number.
    [{ number: 13800138000 }, { number: '13800138000' }],
    [{ tab: 'outcomes', number: '13800138000' }, { tab: 'outcomes' }],
    [{ tab: 'settings', number: '13800138000' }, { tab: 'settings' }],
    [{ number: '   ' }, {}],
    [{ number: 'x'.repeat(65) }, {}],
    [{ number: -5 }, {}],
    [{ number: 1.5 }, {}],
    [{ number: ['13800138000'] }, {}],
  ])('reads %o as %o', (raw, expected) => {
    expect(campaignDetailSearch(raw)).toEqual(expected)
  })

  // The router lays the result over the raw query, so a key missing from it
  // would hand the raw value to the page.
  it('names every key, so nothing raw gets through', () => {
    expect(Object.keys(campaignDetailSearch({ tab: 'reports', status: 'GONE' })).toSorted()).toEqual(
      ['intent', 'number', 'outcome', 'page', 'status', 'tab'],
    )
  })
})

const searchBox = () => screen.getByRole('textbox', { name: i18n.t('campaign.search.label') })

/** The queries of the list's requests that searched a number, oldest first. */
const searched = (backend: Backend, list: 'contacts' | 'attempts') =>
  listQueries(backend, list).filter((q) => 'phoneNumber' in q)

describe('searching a campaign by number', () => {
  beforeEach(() => {
    vi.stubGlobal('scrollTo', () => {})
  })

  it('asks nothing while a number is typed, and searches it on Enter, in the address', async () => {
    const { user, history, backend } = renderCampaignAt([`${DETAIL}?tab=leads&intent=MEDIUM`])
    await screen.findByRole('tab', { ...tab('leads'), selected: true })
    await user.type(searchBox(), '138 0000 0000')
    expect(searched(backend, 'contacts')).toEqual([])
    expect(addressOf(history)).toEqual({ tab: 'leads', intent: 'MEDIUM' })

    await user.keyboard('{Enter}')
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'leads', intent: 'MEDIUM', number: '138 0000 0000' }))
    await waitFor(() => expect(searched(backend, 'contacts')).toEqual([
      { leadIntent: 'MEDIUM', phoneNumber: '138 0000 0000', limit: '50', offset: '0' },
    ]))
    expect(await screen.findByText(leadFixture.leadNotes!)).toBeInTheDocument()
    // The box keeps the number as typed: the page adds no country code to it.
    expect(searchBox()).toHaveValue('138 0000 0000')
  })

  it('narrows with the filters, each kept when the other changes, and starts on the first page', async () => {
    const { user, history, backend } = renderCampaignAt([`${DETAIL}?tab=attempts&outcome=NO_ANSWER&page=3`])
    await waitFor(() => expect(listQueries(backend, 'attempts').at(-1)).toEqual({
      outcome: 'NO_ANSWER', limit: '50', offset: '100',
    }))
    await user.type(searchBox(), '021-6555-0101{Enter}')
    await waitFor(() => expect(listQueries(backend, 'attempts').at(-1)).toEqual({
      outcome: 'NO_ANSWER', phoneNumber: '021-6555-0101', limit: '50', offset: '0',
    }))
    expect(await screen.findByText(i18n.t('campaign.search.noAttempts'))).toBeInTheDocument()
    await user.selectOptions(screen.getByRole('combobox', { name: i18n.t('campaign.outcome') }), 'BUSY')
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'attempts', outcome: 'BUSY', number: '021-6555-0101' }))
    await waitFor(() => expect(listQueries(backend, 'attempts').at(-1)).toEqual({
      outcome: 'BUSY', phoneNumber: '021-6555-0101', limit: '50', offset: '0',
    }))
  })

  it('opens with the number a reload or a link names, digits alone included', async () => {
    const { backend } = renderCampaignAt([`${DETAIL}?status=DONE&number=13800000000`])
    await waitFor(() => expect(searched(backend, 'contacts')).toEqual([
      { status: 'DONE', phoneNumber: '13800000000', limit: '50', offset: '0' },
    ]))
    expect(searchBox()).toHaveValue('13800000000')
    expect(await screen.findByText(leadFixture.number)).toBeInTheDocument()
  })

  it('clears with one click, keeping the other filters', async () => {
    const { user, history, backend } = renderCampaignAt([`${DETAIL}?status=DONE&number=${encodeURIComponent('138 0000 0000')}&page=2`])
    await waitFor(() => expect(searched(backend, 'contacts')).toHaveLength(1))
    await user.click(screen.getByRole('button', { name: i18n.t('campaign.search.clear') }))
    await waitFor(() => expect(addressOf(history)).toEqual({ status: 'DONE' }))
    expect(searchBox()).toHaveValue('')
    await waitFor(() => expect(listQueries(backend, 'contacts').at(-1)).toEqual({
      status: 'DONE', limit: '50', offset: '0',
    }))
    expect(screen.queryByRole('button', { name: i18n.t('campaign.search.clear') })).toBeNull()
  })

  it('drops the search as soon as the box is emptied', async () => {
    const { user, history } = renderCampaignAt([`${DETAIL}?tab=leads&number=${encodeURIComponent('138 0000 0000')}`])
    await waitFor(() => expect(searchBox()).toHaveValue('138 0000 0000'))
    await user.clear(searchBox())
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'leads' }))
  })

  it('says under the box that a value is not a number, once, and not while it is being changed', async () => {
    const { user, backend } = renderCampaignAt([DETAIL])
    await screen.findByRole('tab', { ...tab('contacts'), selected: true })
    await user.type(searchBox(), '138 abc{Enter}')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(i18n.t('errors.rules.NUMBER_INVALID'))
    expect(searchBox()).toHaveAttribute('aria-invalid', 'true')
    // The table says there is no such contact, not that something failed.
    expect(screen.getByText(i18n.t('campaign.search.noContacts'))).toBeInTheDocument()
    expect(screen.queryByText(i18n.t('errors.VALIDATION_FAILED'))).toBeNull()
    // Refused once: not asked again.
    expect(searched(backend, 'contacts')).toHaveLength(1)

    await user.type(searchBox(), '{Backspace}')
    expect(screen.queryByRole('alert')).toBeNull()
    expect(searchBox()).toHaveAttribute('aria-invalid', 'false')
  })

  // The application retries a failed query once; a refused number would only
  // be refused again, and the box would wait a second for nothing.
  it('does not ask again for a number the server refused', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 1, retryDelay: 0, gcTime: 0 } } })
    const { user, backend } = renderCampaignAt([DETAIL], { queryClient })
    await screen.findByRole('tab', { ...tab('contacts'), selected: true })
    await user.type(searchBox(), '12{Enter}')
    expect(await screen.findByRole('alert')).toHaveTextContent(i18n.t('errors.rules.NUMBER_INVALID'))
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(searched(backend, 'contacts')).toEqual([{ phoneNumber: '12', limit: '50', offset: '0' }])
  })

  it('comes Back from a call record to the searched list', async () => {
    const { user, history } = renderCampaignAt(['/admin/campaigns', DETAIL])
    await user.click(await screen.findByRole('tab', tab('leads')))
    await user.selectOptions(await screen.findByRole('combobox', { name: i18n.t('campaign.leadIntent') }), 'MEDIUM')
    await user.type(searchBox(), '138-0000-0000{Enter}')
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'leads', intent: 'MEDIUM', number: '138-0000-0000' }))

    await user.click(await screen.findByRole('link', { name: i18n.t('campaign.openCall') }))
    await screen.findByRole('heading', { name: 'call record' })
    act(() => history.back())

    expect(await screen.findByRole('tab', { ...tab('leads'), selected: true })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: i18n.t('campaign.leadIntent') })).toHaveValue('MEDIUM')
    expect(searchBox()).toHaveValue('138-0000-0000')
    expect(await screen.findByText(leadFixture.leadNotes!)).toBeInTheDocument()
    // The search replaced the entry, as every move on the page does.
    act(() => history.back())
    await waitFor(() => expect(history.location.pathname).toBe('/admin/campaigns'))
  })

  // Like the tabs' other filters: a new tab opens unfiltered.
  it('is on Contacts, Leads and Attempts, and a new tab opens without it', async () => {
    const { user, history, backend } = renderCampaignAt([`${DETAIL}?number=${encodeURIComponent('138 0000 0000')}`])
    await waitFor(() => expect(searchBox()).toHaveValue('138 0000 0000'))
    await user.click(screen.getByRole('tab', tab('leads')))
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'leads' }))
    expect(searchBox()).toHaveValue('')
    await user.click(screen.getByRole('tab', tab('attempts')))
    await waitFor(() => expect(addressOf(history)).toEqual({ tab: 'attempts' }))
    expect(searchBox()).toHaveValue('')
    await user.type(searchBox(), '138 0000 0000{Enter}')
    await waitFor(() => expect(searched(backend, 'attempts')).toEqual([
      { phoneNumber: '138 0000 0000', limit: '50', offset: '0' },
    ]))
  })
})
