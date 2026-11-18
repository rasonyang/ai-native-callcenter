import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import type { Campaign, CampaignContact, CampaignLeadIntent } from '@/lib/campaigns'
import { parseCsv } from '@/lib/csv'
import type { Callback } from '@/lib/ledger'

import { CampaignLeads } from './campaign-leads'

const CAMPAIGN_ID = '00000000-0000-4000-8000-000000000001'
const CALL_A = '00000000-0000-4000-8000-00000000ca11'
const CALL_B = '00000000-0000-4000-8000-00000000cb11'
const CALL_D = '00000000-0000-4000-8000-00000000cd11'

const campaign = { id: CAMPAIGN_ID, name: 'Spring renewals' } as Campaign

function lead(over: Partial<CampaignContact>): CampaignContact {
  return {
    id: crypto.randomUUID(),
    campaignId: CAMPAIGN_ID,
    number: '+8613800138000',
    numberRaw: '13800138000',
    slots: {},
    status: 'DONE',
    attempts: 1,
    systemFailures: 0,
    lastOutcome: 'ANSWERED',
    leadIntent: 'HIGH',
    leadNotes: 'wants the offer',
    lastCallId: null,
    lastAttemptId: null,
    createdAt: '2026-10-01T00:00:00Z',
    ...over,
  }
}

function callback(over: Partial<Callback>): Callback {
  return {
    id: crypto.randomUUID(),
    phoneNumber: '+8613800138000',
    message: '',
    status: 'OPEN',
    createdAt: '2026-10-01T00:00:00Z',
    ...over,
  } as Callback
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

/**
 * Serves the leads and answers /callbacks?callId= from byCall. An unfiltered
 * /callbacks answers with a callback for every lead's number, which is what a
 * screen matching by phone would wrongly pick up.
 */
function stubBackend(leads: CampaignContact[], byCall: Record<string, Callback>) {
  const requests: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), 'http://test')
    const path = url.pathname.replace('/api/v1', '')
    requests.push(path + url.search)
    if (path === `/campaigns/${CAMPAIGN_ID}/contacts`) return json({ items: leads, total: leads.length })
    if (path === '/callbacks') {
      const callId = url.searchParams.get('callId')
      if (callId === null) return json({ items: leads.map((l) => callback({ phoneNumber: l.number })) })
      return json({ items: byCall[callId] ? [byCall[callId]] : [] })
    }
    return json({ items: [] })
  }))
  return requests
}

function renderLeads(shown: Campaign = campaign, intent: CampaignLeadIntent = 'HIGH') {
  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({
      getParentRoute: () => root, path: '/admin/campaigns/$campaignId',
      component: () => <CampaignLeads campaign={shown} intent={intent} page={0} onChange={() => {}} />,
    }),
    createRoute({ getParentRoute: () => root, path: '/admin/cdr/$callId', component: () => null }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: [`/admin/campaigns/${CAMPAIGN_ID}`] }),
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />
      </QueryClientProvider>
    </I18nextProvider>,
  )
}

const rowOf = (number: string) => screen.getByText(number).closest('tr') as HTMLElement

describe('the leads tab', () => {
  it('links a HIGH lead to its call and its callback by call id, never by number', async () => {
    const requests = stubBackend(
      [
        lead({ number: '+8613800138000', lastCallId: CALL_A, lastAttemptId: crypto.randomUUID() }),
        // Its number has callbacks in the queue, but its own call filed none.
        lead({ number: '+8613800138001', lastCallId: CALL_B, lastAttemptId: crypto.randomUUID() }),
        // No call named yet.
        lead({ number: '+8613800138002', lastOutcome: undefined }),
        // A call reconciled UNKNOWN: its callback is shown, no call page promised.
        lead({ number: '+8613800138003', lastOutcome: 'UNKNOWN', lastCallId: CALL_D, lastAttemptId: crypto.randomUUID() }),
      ],
      {
        [CALL_A]: callback({ callId: CALL_A, status: 'OPEN' }),
        [CALL_D]: callback({ callId: CALL_D, status: 'CLAIMED', phoneNumber: '+8613800138003' }),
      },
    )
    renderLeads()

    await screen.findByText('+8613800138000')
    await waitFor(() => expect(within(rowOf('+8613800138000')).getByText('Open')).toBeInTheDocument())
    const link = within(rowOf('+8613800138000')).getByRole('link', { name: 'Open call' })
    expect(link).toHaveAttribute('href', `/admin/cdr/${CALL_A}`)

    await waitFor(() => expect(within(rowOf('+8613800138003')).getByText('Claimed')).toBeInTheDocument())
    expect(within(rowOf('+8613800138003')).queryByRole('link')).toBeNull()

    // A lead whose call filed no callback shows a dash, not its number's.
    const noCallback = rowOf('+8613800138001')
    expect(within(noCallback).queryByText('Open')).toBeNull()
    expect(within(noCallback).getByRole('link', { name: 'Open call' })).toHaveAttribute('href', `/admin/cdr/${CALL_B}`)

    const unnamed = rowOf('+8613800138002')
    expect(within(unnamed).queryByRole('link')).toBeNull()
    expect(within(unnamed).queryByText('Open')).toBeNull()

    const callbackRequests = requests.filter((r) => r.startsWith('/callbacks'))
    expect(callbackRequests.toSorted()).toEqual(
      [CALL_A, CALL_B, CALL_D].map((id) => `/callbacks?callId=${id}`).toSorted(),
    )
  })

  it('asks for no callback for a lead that is not HIGH, and still links its call', async () => {
    const requests = stubBackend(
      [lead({ leadIntent: 'MEDIUM', lastCallId: CALL_A, lastAttemptId: crypto.randomUUID() })],
      { [CALL_A]: callback({ callId: CALL_A }) },
    )
    renderLeads()
    await screen.findByText('+8613800138000')
    const row = rowOf('+8613800138000')
    expect(within(row).getByRole('link', { name: 'Open call' })).toHaveAttribute('href', `/admin/cdr/${CALL_A}`)
    expect(requests.some((r) => r.startsWith('/callbacks'))).toBe(false)
  })
})

/** What the browser was handed: the file's name and its bytes. */
function captureDownloads() {
  const files: Array<{ name: string; blob: Blob }> = []
  const blobs = new Map<string, Blob>()
  // jsdom has no object URLs; the URL constructor itself stays as it is.
  vi.stubGlobal('URL', Object.assign(URL, {
    createObjectURL: (blob: Blob) => {
      const url = `blob:export-${blobs.size}`
      blobs.set(url, blob)
      return url
    },
    revokeObjectURL: () => {},
  }))
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function click(this: HTMLAnchorElement) {
    files.push({ name: this.download, blob: blobs.get(this.getAttribute('href') ?? '') as Blob })
  })
  return files
}

async function bytesOf(blob: Blob): Promise<Uint8Array> {
  return new Uint8Array(await blob.arrayBuffer())
}

const abortError = () => new DOMException('The operation was aborted.', 'AbortError')

/**
 * A campaign's leads served page by page, by offset and limit, as the server
 * pages them. `hold` keeps the page at that offset waiting until released (or
 * until the request is aborted); `fail` answers it with a server error.
 */
function stubPagedBackend(leads: CampaignContact[], options: { hold?: number; fail?: number } = {}) {
  const requests: string[] = []
  let release!: () => void
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://test')
    const path = url.pathname.replace('/api/v1', '')
    requests.push(path + url.search)
    if (path === '/callbacks') return json({ items: [] })
    const offset = Number(url.searchParams.get('offset') ?? 0)
    const limit = Number(url.searchParams.get('limit') ?? 50)
    const intent = url.searchParams.get('leadIntent')
    const matching = leads.filter((l) => !intent || l.leadIntent === intent)
    if (offset === options.fail) {
      return new Response(JSON.stringify({ error: { code: 'INTERNAL', message: 'boom' } }), { status: 500 })
    }
    if (offset === options.hold) {
      await new Promise<void>((resolve, reject) => {
        void held.then(resolve)
        init?.signal?.addEventListener('abort', () => reject(abortError()))
      })
    }
    return json({ items: matching.slice(offset, offset + limit), total: matching.length })
  }))
  return { requests, release }
}

/** 1500 synthetic HIGH leads (138 0000 xxxx), with no call yet, so no callback lookups. */
const manyLeads = () =>
  Array.from({ length: 1500 }, (_, i) =>
    lead({ id: `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`, number: `+861380000${String(i).padStart(4, '0')}` }))

describe('exporting the leads', () => {
  it('downloads every lead of the filter, all pages, as a CSV with a BOM', async () => {
    const files = captureDownloads()
    const leads = [
      ...manyLeads(),
      lead({ leadIntent: 'MEDIUM', number: '+8613900000001' }),
    ]
    const { requests, release } = stubPagedBackend(leads, { hold: 1000 })
    renderLeads({ ...campaign, defaultCountryCode: '86' })
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Export' }))
    // The first page is in; the second is held, so the progress shows.
    expect(await screen.findByRole('status')).toHaveTextContent('Exporting 1000 of 1500')
    release()
    await waitFor(() => expect(files).toHaveLength(1))
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.getByRole('button', { name: 'Export' })).toBeInTheDocument()

    const exportRequests = requests.filter((r) => r.includes('limit=1000'))
    expect(exportRequests).toEqual([
      `/campaigns/${CAMPAIGN_ID}/contacts?leadIntent=HIGH&limit=1000&offset=0`,
      `/campaigns/${CAMPAIGN_ID}/contacts?leadIntent=HIGH&limit=1000&offset=1000`,
    ])
    expect(files[0].name).toMatch(/^campaign-spring-renewals-leads-high-\d{8}\.csv$/)
    const bytes = await bytesOf(files[0].blob)
    expect(Array.from(bytes.subarray(0, 3))).toEqual([0xef, 0xbb, 0xbf])
    const table = parseCsv(new TextDecoder().decode(bytes))
    expect(table.header).toEqual(['number', 'leadIntent', 'leadNotes', 'callbackStatus', 'callId', 'contactId'])
    expect(table.rows).toHaveLength(1500)
    expect(table.rows[0].cells.slice(0, 2)).toEqual(['\t13800000000', 'HIGH'])
  })

  it('writes no file and says so when a page fails', async () => {
    const files = captureDownloads()
    stubPagedBackend(manyLeads(), { fail: 1000 })
    renderLeads()
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Export' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/^The export stopped and no file was saved: /)
    expect(files).toEqual([])
    expect(screen.getByRole('button', { name: 'Export' })).toBeInTheDocument()
  })

  it('writes no file when cancelled, and shows no error', async () => {
    const files = captureDownloads()
    const { requests } = stubPagedBackend(manyLeads(), { hold: 1000 })
    renderLeads()
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Export' }))
    await screen.findByText('Exporting 1000 of 1500')
    await user.click(screen.getByRole('button', { name: 'Cancel export' }))

    expect(await screen.findByRole('button', { name: 'Export' })).toBeInTheDocument()
    expect(screen.queryByRole('status')).toBeNull()
    // Nothing arrives later either: the held page was aborted, not answered.
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(files).toEqual([])
    expect(screen.queryByRole('alert')).toBeNull()
    expect(requests.filter((r) => r.includes('limit=1000'))).toHaveLength(2)
  })

  it('exports the intent the tab shows', async () => {
    const files = captureDownloads()
    const { requests } = stubPagedBackend([
      lead({ leadIntent: 'LOW', number: '+861060000000', leadNotes: '=1+1' }),
      lead({ leadIntent: 'HIGH' }),
    ])
    renderLeads({ ...campaign, defaultCountryCode: '86' }, 'LOW')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Export' }))
    await waitFor(() => expect(files).toHaveLength(1))
    expect(requests).toContain(`/campaigns/${CAMPAIGN_ID}/contacts?leadIntent=LOW&limit=1000&offset=0`)
    const rows = parseCsv(new TextDecoder().decode(await bytesOf(files[0].blob))).rows
    expect(rows.map((r) => r.cells.slice(0, 3))).toEqual([['\t01060000000', 'LOW', "'=1+1"]])
    expect(files[0].name).toMatch(/-leads-low-\d{8}\.csv$/)
  })
})
