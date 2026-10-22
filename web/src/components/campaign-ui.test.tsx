import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState, type ReactElement } from 'react'
import { I18nextProvider } from 'react-i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import { ApiError } from '@/lib/api'
import { Route as CampaignListRoute } from '@/routes/_app.admin.campaigns.index'
import { Route as DncRoute } from '@/routes/_app.admin.dnc'
import { applyToCache } from '@/lib/use-event-stream'
import {
  blankForm, createBodyOf, formFrom, type Campaign, type CampaignForm, type CampaignSettings,
} from '@/lib/campaigns'
import { renderWithProviders } from '@/test/harness'

import { CampaignFormView } from './campaign-form'
import { CampaignImportDialog } from './campaign-import'

const CAMPAIGN_ID = '00000000-0000-4000-8000-000000000001'

function campaignFixture(over: Partial<Campaign> = {}): Campaign {
  return {
    id: CAMPAIGN_ID,
    name: 'Spring renewals',
    didId: '00000000-0000-4000-8000-0000000000d1',
    didNumber: '95099',
    status: 'DRAFT',
    timeZone: 'Asia/Shanghai',
    callingWindow: { days: ['MON', 'TUE'], ranges: [{ start: '09:00', end: '18:00' }] },
    maxConcurrent: 5,
    ringTimeoutSec: 30,
    retryPolicy: {
      maxAttempts: 3,
      maxSystemRetries: 5,
      retryOutcomes: ['NO_ANSWER'],
      intervalSec: { NO_ANSWER: 7200, BUSY: 1800, REJECTED: 86400, UNKNOWN: 86400 },
      transientBackoff: { baseSec: 300, factor: 3, capSec: 14400 },
    },
    createdAt: '2026-10-01T00:00:00Z',
    contacts: { total: 4, active: 4, done: 0, exhausted: 0, invalidNumber: 0, dnc: 0 },
    ...over,
  }
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

afterEach(() => {
  vi.unstubAllGlobals()
  void i18n.changeLanguage('en')
})

// --- the form's locks ----------------------------------------------------------

const SETTINGS: CampaignSettings = {
  outboundHours: { start: '08:00', end: '21:00' },
  defaults: {
    maxConcurrent: 5,
    ringTimeoutSec: 30,
    retryPolicy: {
      maxAttempts: 3,
      maxSystemRetries: 5,
      retryOutcomes: ['NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN', 'TRANSIENT', 'ORIGINATE_FAILED'],
      intervalSec: { NO_ANSWER: 7200, BUSY: 1800, REJECTED: 86400, UNKNOWN: 86400 },
      transientBackoff: { baseSec: 300, factor: 3, capSec: 14400 },
    },
  },
  limits: { maxConcurrent: { min: 1, max: 100000 }, ringTimeoutSec: { min: 5, max: 59 } },
}

function FormHarness({
  campaign, error, settings, onSubmit = () => {},
}: { campaign?: Campaign; error?: unknown; settings?: CampaignSettings; onSubmit?: (form: CampaignForm) => void }) {
  const [form, setForm] = useState<CampaignForm>(() => (campaign ? formFrom(campaign) : blankForm('UTC')))
  return (
    <CampaignFormView
      value={form}
      onChange={setForm}
      onSubmit={() => onSubmit(form)}
      campaign={campaign}
      settings={settings}
      dids={[
        { id: campaign?.didId ?? 'did-1', number: '95099', description: '', isEnabled: true, allowOutbound: true, allowInbound: false, isDefaultOutbound: false, isRecordingEnabled: true, language: 'en', isCampaignReady: true },
        { id: 'did-in', number: '95001', description: '', isEnabled: true, allowOutbound: false, allowInbound: true, isDefaultOutbound: false, isRecordingEnabled: true, language: 'en', isCampaignReady: false },
      ]}
      queues={[]}
      error={error}
      submitLabel="Save"
    />
  )
}

describe('the campaign form', () => {
  it('opens every field for a draft', () => {
    renderWithProviders(<FormHarness campaign={campaignFixture()} />)
    for (const label of ['Name', 'Number to dial from', 'Time zone', 'Maximum concurrent calls', 'Ring timeout (seconds)']) {
      expect(screen.getByLabelText(label)).toBeEnabled()
    }
  })

  it('locks what the API locks while running, and says why', () => {
    renderWithProviders(<FormHarness campaign={campaignFixture({ status: 'RUNNING' })} />)
    expect(screen.getByLabelText('Name')).toBeDisabled()
    expect(screen.getByLabelText('Number to dial from')).toBeDisabled()
    expect(screen.getByLabelText('Time zone')).toBeDisabled()
    expect(screen.getByLabelText('Ring timeout (seconds)')).toBeDisabled()
    expect(screen.getByLabelText('Default country code')).toBeDisabled()
    expect(screen.getByLabelText('Follow-up queue')).toBeDisabled()
    // The three that may change.
    expect(screen.getByLabelText('Maximum concurrent calls')).toBeEnabled()
    expect(screen.getByLabelText('Maximum attempts')).toBeEnabled()
    expect(screen.getByLabelText('Range 1 start')).toBeEnabled()
    expect(screen.getAllByText(/Locked while the campaign is running or paused/).length).toBeGreaterThan(0)
  })

  it('locks the same fields while paused', () => {
    renderWithProviders(<FormHarness campaign={campaignFixture({ status: 'PAUSED', pauseReason: 'MANUAL' })} />)
    expect(screen.getByLabelText('Name')).toBeDisabled()
    expect(screen.getByLabelText('Maximum concurrent calls')).toBeEnabled()
  })

  it('makes a finished campaign read-only, with nothing to save', () => {
    renderWithProviders(<FormHarness campaign={campaignFixture({ status: 'COMPLETED' })} />)
    for (const label of ['Name', 'Maximum concurrent calls', 'Maximum attempts', 'Range 1 start']) {
      expect(screen.getByLabelText(label)).toBeDisabled()
    }
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
    // Once, at the top, not under every field.
    expect(screen.getAllByText('A finished campaign cannot be changed.')).toHaveLength(1)
  })

  it('drops a field’s refusal as soon as that field changes, and only that one', async () => {
    const refusal = new ApiError(422, { code: 'CAMPAIGN_WINDOW_OUTSIDE_LIMIT', message: 'x' })
    const { user } = renderWithProviders(<FormHarness campaign={campaignFixture()} error={refusal} />)
    expect(screen.getByText(/hours this deployment permits/)).toBeInTheDocument()
    await user.type(screen.getByLabelText('Name'), '!')
    expect(screen.getByText(/hours this deployment permits/)).toBeInTheDocument()
    await user.clear(screen.getByLabelText('Range 1 end'))
    expect(screen.queryByText(/hours this deployment permits/)).not.toBeInTheDocument()
  })

  it('lets only a campaign-ready number be chosen, and says why the others cannot', () => {
    renderWithProviders(<FormHarness />)
    const select = screen.getByLabelText('Number to dial from')
    expect(within(select).getByRole('option', { name: '95099' })).toBeEnabled()
    const notReady = within(select).getByRole('option', { name: /95001/ })
    expect(notReady).toBeDisabled()
    expect(notReady).toHaveTextContent('outbound not allowed')
  })

  it('keeps the stored number of a campaign readable after it stopped being ready', () => {
    const campaign = campaignFixture({ didId: 'did-in', didNumber: '95001' })
    renderWithProviders(<FormHarness campaign={campaign} />)
    const select = screen.getByLabelText('Number to dial from')
    expect(select).toHaveValue('did-in')
    expect(within(select).getByRole('option', { name: /95001/ })).toBeEnabled()
  })

  it('lets a new campaign take the default retry policy or edit it', async () => {
    const { user } = renderWithProviders(<FormHarness />)
    expect(screen.queryByLabelText('Maximum attempts')).not.toBeInTheDocument()
    await user.click(screen.getByLabelText("Use the deployment's default retry policy"))
    // Settings have not arrived: the starting point is not known yet, so editing waits.
    expect(screen.getByLabelText("Use the deployment's default retry policy")).toBeChecked()
  })

  it('starts the retry edit from the deployment’s own policy', async () => {
    const { user } = renderWithProviders(<FormHarness settings={SETTINGS} />)
    await user.click(screen.getByLabelText("Use the deployment's default retry policy"))
    expect(screen.getByLabelText('Maximum attempts')).toHaveValue(3)
    expect(screen.getByLabelText('Maximum system retries')).toHaveValue(5)
  })

  it('takes placeholders and bounds from the settings', () => {
    renderWithProviders(<FormHarness settings={SETTINGS} />)
    const concurrent = screen.getByLabelText('Maximum concurrent calls')
    expect(concurrent).toHaveAttribute('placeholder', '5')
    expect(concurrent).toHaveAttribute('max', '100000')
    const ring = screen.getByLabelText('Ring timeout (seconds)')
    expect(ring).toHaveAttribute('placeholder', '30')
    expect(ring).toHaveAttribute('min', '5')
    expect(ring).toHaveAttribute('max', '59')
  })

  it('works while the settings are loading or failed: nothing is pre-validated', async () => {
    const submitted = vi.fn()
    const { user } = renderWithProviders(
      <FormHarness onSubmit={submitted} />,
    )
    const ring = screen.getByLabelText('Ring timeout (seconds)')
    expect(ring).toHaveAttribute('placeholder', 'Deployment default')
    await user.type(ring, '99')
    await user.clear(screen.getByLabelText('Range 1 start'))
    await user.type(screen.getByLabelText('Range 1 start'), '03:00')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(submitted).toHaveBeenCalledTimes(1)
  })

  it('refuses a window range outside the calling hours before sending, naming the hours', async () => {
    const submitted = vi.fn()
    const { user } = renderWithProviders(<FormHarness settings={SETTINGS} onSubmit={submitted} />)
    await user.clear(screen.getByLabelText('Range 1 start'))
    await user.type(screen.getByLabelText('Range 1 start'), '07:00')
    expect(screen.getByText('07:00–18:00 is outside the allowed calling hours 08:00–21:00.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(submitted).not.toHaveBeenCalled()

    await user.clear(screen.getByLabelText('Range 1 start'))
    await user.type(screen.getByLabelText('Range 1 start'), '08:00')
    expect(screen.queryByText(/outside the allowed calling hours/)).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(submitted).toHaveBeenCalledTimes(1)
  })

  it('says the same in Chinese', async () => {
    await i18n.changeLanguage('zh')
    const { user } = renderWithProviders(<FormHarness settings={SETTINGS} />)
    await user.clear(screen.getByLabelText('第 1 段开始'))
    await user.type(screen.getByLabelText('第 1 段开始'), '07:00')
    expect(screen.getByText('07:00–18:00 超出允许的外呼时段 08:00–21:00。')).toBeInTheDocument()
  })

  it('refuses a number outside the limits the settings state', async () => {
    const submitted = vi.fn()
    const { user } = renderWithProviders(<FormHarness settings={SETTINGS} onSubmit={submitted} />)
    await user.type(screen.getByLabelText('Ring timeout (seconds)'), '60')
    expect(screen.getByText('Enter a whole number from 5 to 59.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(submitted).not.toHaveBeenCalled()
    await user.clear(screen.getByLabelText('Ring timeout (seconds)'))
    await user.type(screen.getByLabelText('Ring timeout (seconds)'), '59')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(submitted).toHaveBeenCalledTimes(1)
  })

  it('sends only the four required fields when nothing optional is touched', async () => {
    let sent: CampaignForm | undefined
    const { user } = renderWithProviders(
      <FormHarness settings={SETTINGS} onSubmit={(form) => { sent = form }} />,
    )
    await user.type(screen.getByLabelText('Name'), 'Q4')
    await user.selectOptions(screen.getByLabelText('Number to dial from'), 'did-1')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(Object.keys(createBodyOf(sent as CampaignForm)).toSorted()).toEqual([
      'callingWindow', 'didId', 'name', 'timeZone',
    ])
  })
})

// --- the list ---------------------------------------------------------------------

function renderList(client: QueryClient) {
  const root = createRootRoute({ component: Outlet })
  const Page = CampaignListRoute.options.component as () => ReactElement
  const tree = root.addChildren([
    createRoute({ getParentRoute: () => root, path: '/admin/campaigns', component: Page }),
    createRoute({ getParentRoute: () => root, path: '/admin/campaigns/new', component: () => null }),
    createRoute({ getParentRoute: () => root, path: '/admin/campaigns/$campaignId', component: () => null }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: ['/admin/campaigns'] }),
  })
  return render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />
      </QueryClientProvider>
    </I18nextProvider>,
  )
}

describe('the campaign list', () => {
  it('shows status, activity, counters and number, and moves with the event stream', async () => {
    const running = campaignFixture({
      status: 'RUNNING', activity: 'WAITING_WINDOW', nextDialAt: new Date(Date.now() + 36 * 3600_000).toISOString(),
      inFlight: 0,
    })
    const requests: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      requests.push(String(input))
      return json({ items: [running], total: 1 })
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    renderList(client)

    expect(await screen.findByText('Spring renewals')).toBeInTheDocument()
    expect(screen.getByText(/waiting for calling hours/)).toBeInTheDocument()
    expect(screen.getByText('95099')).toBeInTheDocument()
    expect(requests).toEqual(['/api/v1/campaigns?limit=50&offset=0'])

    // The dialer reports it is dialling: the row changes without a request.
    act(() => {
      applyToCache(client, {
        version: 1, seq: 1, type: 'CAMPAIGN_UPDATED', occurredAt: new Date().toISOString(),
        payload: {
          campaignId: CAMPAIGN_ID, status: 'RUNNING', activity: 'DIALING', inFlight: 7,
          contacts: { total: 4, active: 2, done: 2, exhausted: 0, invalidNumber: 0, dnc: 0 },
        },
      })
    })
    expect(await screen.findByText('· dialling')).toBeInTheDocument()
    expect(screen.queryByText(/waiting for calling hours/)).not.toBeInTheDocument()
    expect(screen.getByText('7')).toBeInTheDocument()
    expect(requests).toHaveLength(1)
  })

  it('shows a campaign created elsewhere when its first CAMPAIGN_UPDATED arrives', async () => {
    const fresh = campaignFixture({ id: 'fresh-1', name: 'Fresh draft' })
    let created = false
    vi.stubGlobal('fetch', vi.fn(async () =>
      json({ items: created ? [fresh] : [], total: created ? 1 : 0 }),
    ))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    renderList(client)
    await waitFor(() => expect(client.isFetching()).toBe(0))
    expect(screen.queryByText('Fresh draft')).not.toBeInTheDocument()

    created = true
    act(() => {
      applyToCache(client, {
        version: 1, seq: 2, type: 'CAMPAIGN_UPDATED', occurredAt: new Date().toISOString(),
        payload: {
          campaignId: 'fresh-1', status: 'DRAFT',
          contacts: { total: 0, active: 0, done: 0, exhausted: 0, invalidNumber: 0, dnc: 0 },
        },
      })
    })
    expect(await screen.findByText('Fresh draft')).toBeInTheDocument()
  })

  it('filters by status through the API', async () => {
    const requests: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      requests.push(String(input))
      return json({ items: [], total: 0 })
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    renderList(client)
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('Filter by status'), 'PAUSED')
    await waitFor(() => expect(requests).toContain('/api/v1/campaigns?status=PAUSED&limit=50&offset=0'))
  })

  it('reads the same row in Chinese', async () => {
    await i18n.changeLanguage('zh')
    vi.stubGlobal('fetch', vi.fn(async () =>
      json({ items: [campaignFixture({ status: 'PAUSED', pauseReason: 'PROVIDER_FAILURE' })], total: 1 }),
    ))
    renderList(new QueryClient({ defaultOptions: { queries: { retry: false } } }))
    await screen.findByText('Spring renewals')
    const row = screen.getByRole('row', { name: /Spring renewals/ })
    expect(within(row).getByText('已暂停')).toBeInTheDocument()
    expect(within(row).getByText(/语音服务商故障/)).toBeInTheDocument()
  })
})

// --- the import -----------------------------------------------------------------------

function csvFile(text: string, name = 'contacts.csv') {
  return new File([text], name, { type: 'text/csv' })
}

/**
 * A real workbook, out of the fixtures the unit tests read:
 * `scripts/make-xlsx-fixtures.mjs` writes them.
 */
function workbookFile(name = 'contacts.xlsx') {
  const bytes = readFileSync(path.join(import.meta.dirname, '..', 'test', 'fixtures', name))
  return new File([bytes], name, {
    type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  })
}

/**
 * Reading a workbook is heavier than reading text: the reader is a chunk of its
 * own, loaded on first use, and in a run where every test file is going at once
 * its first transform has taken over five seconds on this machine. The budget
 * is for that machinery, not for what the tests assert.
 */
const WORKBOOK_TIMEOUT = 15_000

describe('the contact import', () => {
  function stubImport(reject: (number: string) => string | undefined = () => undefined) {
    const bodies: Array<{ contacts: Array<{ number: string; timeZone?: string; slots?: Record<string, string> }> }> = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      expect(String(input)).toBe(`/api/v1/campaigns/${CAMPAIGN_ID}/contacts`)
      expect(init.method).toBe('POST')
      const body = JSON.parse(String(init.body))
      bodies.push(body)
      const seen = new Set<string>()
      const rows = body.contacts.map((c: { number: string }, index: number) => {
        const code = reject(c.number)
        if (code) return { index, status: 'REJECTED', code }
        if (seen.has(c.number)) return { index, status: 'DUPLICATE', number: `+${c.number}` }
        seen.add(c.number)
        return { index, status: 'IMPORTED', number: `+${c.number}` }
      })
      return json({
        imported: rows.filter((r: { status: string }) => r.status === 'IMPORTED').length,
        duplicates: rows.filter((r: { status: string }) => r.status === 'DUPLICATE').length,
        rejected: rows.filter((r: { status: string }) => r.status === 'REJECTED').length,
        rows,
      })
    }))
    return bodies
  }

  // A flow that reads no business data by default, which is the free-name path
  // these tests were written against; the ones about the flow's names pass
  // them in.
  function open(onImported = vi.fn(), flowFields: string[] = []) {
    const view = renderWithProviders(
      <CampaignImportDialog
        campaignId={CAMPAIGN_ID}
        flowFields={flowFields}
        open
        onOpenChange={() => {}}
        onImported={onImported}
      />,
    )
    return { ...view, onImported }
  }

  it('maps a messy Chinese file, sends it, and shows every result by its line', async () => {
    const bodies = stubImport((n) => (n === 'bad' ? 'NUMBER_INVALID' : undefined))
    const { user, onImported } = open()
    const text =
      '﻿手机号,姓名,备注\r\n' +
      '13800138000,王伟,"想了解, 套餐\r\n第二行"\r\n' +
      '\r\n' +
      'bad,Li,x\r\n' +
      '13700137000,Zhao\r\n' + // wrong column count
      '13600136000,😀,y\r\n'
    await user.upload(screen.getByLabelText('CSV or Excel file'), csvFile(text))

    expect(await screen.findByText('contacts.csv: 4 rows, 3 columns')).toBeInTheDocument()
    expect(screen.getByLabelText('What column 手机号 is')).toHaveValue('number')
    // Other columns start ignored; the operator opts the ones the flow reads in.
    expect(screen.getByLabelText('What column 姓名 is')).toHaveValue('ignore')
    await user.selectOptions(screen.getByLabelText('What column 姓名 is'), 'slot')
    await user.selectOptions(screen.getByLabelText('What column 备注 is'), 'slot')
    expect(screen.getByLabelText('Field name for column 姓名')).toHaveValue('姓名')
    expect(screen.getByText(/^1 row has a different number of columns/)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Import 4 rows' }))

    await screen.findByText('2 imported')
    expect(screen.getByText('0 duplicates')).toBeInTheDocument()
    expect(screen.getByText('2 rejected')).toBeInTheDocument()
    expect(bodies).toHaveLength(1)
    expect(bodies[0].contacts).toEqual([
      { number: '13800138000', slots: { 姓名: '王伟', 备注: '想了解, 套餐\r\n第二行' } },
      { number: 'bad', slots: { 姓名: 'Li', 备注: 'x' } },
      { number: '13600136000', slots: { 姓名: '😀', 备注: 'y' } },
    ])
    // The line is the one in the file, counting the multi-line value and the blank line.
    const table = screen.getByRole('table')
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => within(r).getAllByRole('cell').map((c) => c.textContent))).toEqual([
      ['5', 'Rejected', 'bad', 'Not a phone number the platform can read.'],
      ['6', 'Rejected', '13700137000', 'Wrong number of columns (found/expected 2/3); not sent.'],
    ])
    expect(onImported).toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Download rejected rows' })).toBeInTheDocument()
  })

  it("offers the flow's field names and preselects a column whose header names one", async () => {
    const bodies = stubImport()
    const { user } = open(vi.fn(), ['name', 'orderNo'])
    await user.upload(
      screen.getByLabelText('CSV or Excel file'),
      csvFile('Phone,NAME,OrderNo,订单号,notes\n13800138000,王伟,A-1,甲,keep\n'),
    )

    // A header that names a field is that field, whatever its case.
    expect(await screen.findByLabelText('What column NAME is')).toHaveValue('slot:name')
    expect(screen.getByLabelText('What column OrderNo is')).toHaveValue('slot:orderNo')
    // A column the flow reads nothing from is still ignored.
    expect(screen.getByLabelText('What column 订单号 is')).toHaveValue('ignore')
    expect(screen.getByLabelText('What column notes is')).toHaveValue('ignore')

    // The vocabulary is the flow's, so a name cannot be misspelt into a
    // SLOT_UNKNOWN the operator only discovers row by row.
    const offered = within(screen.getByLabelText('What column 订单号 is'))
      .getAllByRole('option')
      .map((option) => option.textContent)
    expect(offered).toEqual(['Number', 'Time zone', 'name', 'orderNo', 'Ignore'])
    expect(screen.queryByLabelText('Field name for column 订单号')).not.toBeInTheDocument()

    // Two columns on one field is refused, not silently undone.
    await user.selectOptions(screen.getByLabelText('What column 订单号 is'), 'slot:orderNo')
    expect(screen.getByText('Two columns have the same field name.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^Import/ })).toBeDisabled()

    await user.selectOptions(screen.getByLabelText('What column 订单号 is'), 'ignore')
    await user.click(screen.getByRole('button', { name: 'Import 1 row' }))

    await screen.findByText('1 imported')
    expect(bodies).toHaveLength(1)
    expect(bodies[0].contacts).toEqual([
      { number: '13800138000', slots: { name: '王伟', orderNo: 'A-1' } },
    ])
  })

  it('sends a file of several thousand rows in requests of at most 5000', async () => {
    const bodies = stubImport()
    const { user } = open()
    const lines = ['number,name']
    for (let i = 0; i < 12345; i++) lines.push(`13${String(i).padStart(9, '0')},客户${i}`)
    await user.upload(screen.getByLabelText('CSV or Excel file'), csvFile(lines.join('\n')))
    await user.click(await screen.findByRole('button', { name: 'Import 12345 rows' }))
    await screen.findByText('12345 imported', undefined, { timeout: 10000 })
    expect(bodies.map((b) => b.contacts.length)).toEqual([5000, 5000, 2345])
  })

  it('sends only the number when no other column is opted in', async () => {
    const bodies = stubImport()
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), csvFile('number,name\n1,a\n'))
    await user.click(await screen.findByRole('button', { name: 'Import 1 row' }))
    await screen.findByText('1 imported')
    expect(bodies[0].contacts).toEqual([{ number: '1' }])
  })

  it('will not import without a number column', async () => {
    const bodies = stubImport()
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), csvFile('number,name\n1,a\n'))
    await user.selectOptions(await screen.findByLabelText('What column number is'), 'ignore')
    expect(screen.getByText('Choose the number column.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^Import/ })).toBeDisabled()
    expect(bodies).toHaveLength(0)
  })

  it('imports an Excel workbook through the same mapping, request and report as a CSV', async () => {
    const bodies = stubImport()
    const { user, onImported } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), workbookFile())

    expect(await screen.findByText('contacts.xlsx: 3 rows, 4 columns')).toBeInTheDocument()
    expect(screen.getByLabelText('What column 手机号 is')).toHaveValue('number')
    await user.selectOptions(screen.getByLabelText('What column 姓名 is'), 'slot')
    await user.selectOptions(screen.getByLabelText('What column 时区 is'), 'timeZone')
    await user.click(screen.getByRole('button', { name: 'Import 3 rows' }))

    await screen.findByText('3 imported')
    // The number column holds numeric cells in the workbook; they still arrive
    // as the digits the spreadsheet shows.
    expect(bodies[0].contacts).toEqual([
      { number: '13800138000', timeZone: 'Asia/Shanghai', slots: { 姓名: '王伟' } },
      { number: '13900139000', timeZone: 'Asia/Shanghai', slots: { 姓名: '李静' } },
      { number: '13300133000', timeZone: 'Asia/Shanghai', slots: { 姓名: '陈涛' } },
    ])
    expect(onImported).toHaveBeenCalled()
  }, WORKBOOK_TIMEOUT)

  it('lets the operator choose a sheet when the workbook has more than one', async () => {
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), workbookFile('two-sheets.xlsx'))

    // Until the operator says otherwise, the first sheet is the one being mapped.
    expect(await screen.findByText('two-sheets.xlsx: 3 rows, 4 columns')).toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText('Sheet to import'), '1')

    expect(await screen.findByText('two-sheets.xlsx: 1 row, 2 columns')).toBeInTheDocument()
    expect(screen.getByLabelText('What column 手机号 is')).toHaveValue('number')
    // The second sheet is guessed on its own header, not the first one's answer.
    expect(screen.getByLabelText('What column 姓名 is')).toHaveValue('ignore')
  }, WORKBOOK_TIMEOUT)

  it('opens on the first sheet with something to map and keeps the picker for the empty one', async () => {
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), workbookFile('empty-first.xlsx'))

    // The first sheet is blank, so the dialog opens on the second.
    expect(await screen.findByText('empty-first.xlsx: 3 rows, 4 columns')).toBeInTheDocument()
    expect(screen.getByLabelText('What column 手机号 is')).toHaveValue('number')
    const picker = screen.getByLabelText('Sheet to import')
    expect(picker).toHaveValue('1')

    // Choosing the empty sheet says so, and the picker stays to get back.
    await user.selectOptions(picker, '0')
    expect(await screen.findByText('The file has no rows.')).toBeInTheDocument()
    expect(screen.queryByLabelText(/^What column/)).not.toBeInTheDocument()
    expect(screen.getByLabelText('Sheet to import')).toHaveValue('0')

    await user.selectOptions(screen.getByLabelText('Sheet to import'), '1')
    expect(await screen.findByText('empty-first.xlsx: 3 rows, 4 columns')).toBeInTheDocument()
    expect(screen.getByLabelText('What column 手机号 is')).toHaveValue('number')
  }, WORKBOOK_TIMEOUT)

  it('shows the file picked last when an earlier read finishes after it', async () => {
    // Hold the first file's read open; the second one is read at once.
    const held: Array<() => void> = []
    const real = FileReader.prototype.readAsText
    const spy = vi.spyOn(FileReader.prototype, 'readAsText').mockImplementation(function (
      this: FileReader,
      blob: Blob,
      encoding?: string,
    ) {
      if ((blob as File).name !== 'slow.csv') return real.call(this, blob, encoding)
      held.push(() => {
        Object.defineProperty(this, 'result', { value: '手机号\n13000000000\n13000000001\n13000000002\n' })
        this.dispatchEvent(new Event('load'))
      })
    })
    try {
      const { user } = open()
      const input = screen.getByLabelText('CSV or Excel file')
      await user.upload(input, csvFile('手机号\n13800138000\n', 'slow.csv'))
      await user.upload(input, csvFile('手机号\n13900139000\n13900139001\n', 'fast.csv'))
      expect(await screen.findByText('fast.csv: 2 rows, 1 columns')).toBeInTheDocument()

      // The slower read lands now, and has to be ignored.
      expect(held).toHaveLength(1)
      act(() => held[0]())
      await new Promise((resolve) => setTimeout(resolve, 20))
      expect(screen.getByText('fast.csv: 2 rows, 1 columns')).toBeInTheDocument()
      expect(screen.queryByText(/^slow\.csv:/)).not.toBeInTheDocument()
    } finally {
      spy.mockRestore()
    }
  })

  it('says so when the file cannot be read as a workbook', async () => {
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), workbookFile('broken.xlsx'))
    expect(
      await screen.findByText(/could not be read as an Excel workbook/),
    ).toBeInTheDocument()
    // No columns to map, and so nothing to import: the dialog stays as it was.
    expect(screen.queryByLabelText(/^What column/)).not.toBeInTheDocument()
    expect(screen.queryByText(/^broken\.xlsx:/)).not.toBeInTheDocument()
  }, WORKBOOK_TIMEOUT)

  it('stops on a failed request and offers to retry the rest', async () => {
    let calls = 0
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      calls++
      if (calls === 1) {
        return json({ error: { code: 'INTERNAL', message: 'boom' } }, 500)
      }
      const body = JSON.parse(String(init.body))
      return json({
        imported: body.contacts.length, duplicates: 0, rejected: 0,
        rows: body.contacts.map((_: unknown, index: number) => ({ index, status: 'IMPORTED' })),
      })
    }))
    const { user } = open()
    await user.upload(screen.getByLabelText('CSV or Excel file'), csvFile('number\n1\n2\n'))
    await user.click(await screen.findByRole('button', { name: 'Import 2 rows' }))
    expect(await screen.findByText(/The import stopped at row 1/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Retry the rest' }))
    await screen.findByText('2 imported')
    expect(screen.queryByText(/The import stopped/)).not.toBeInTheDocument()
  })
})

// --- the do-not-call page ---------------------------------------------------------------

describe('the do-not-call page', () => {
  const Page = DncRoute.options.component as () => ReactElement

  it('adds a number in any form and shows the canonical one the server answers with', async () => {
    const posted: unknown[] = []
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      if (init.method === 'POST') {
        posted.push(JSON.parse(String(init.body)))
        return json({ number: '+8613800138000', source: 'MANUAL', createdAt: '2026-10-12T00:00:00Z' }, 201)
      }
      return json({ items: [], total: 0 })
    }))
    const { user } = renderWithProviders(<Page />)
    await user.type(await screen.findByLabelText('Number to add'), '0086 138 0013 8000')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText('+8613800138000 is on the list.')).toBeInTheDocument()
    expect(posted).toEqual([{ number: '0086 138 0013 8000', source: 'MANUAL' }])
  })

  it.each([
    ['NUMBER_EMPTY', 'Enter a number.'],
    ['NUMBER_INVALID', 'That is not a phone number the platform can read.'],
  ])('shows %s on the number field', async (rule, text) => {
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      if (init.method === 'POST') {
        return json({ error: { code: 'VALIDATION_FAILED', message: 'x', params: { field: 'number', rule } } }, 422)
      }
      return json({ items: [], total: 0 })
    }))
    const { user } = renderWithProviders(<Page />)
    await user.type(await screen.findByLabelText('Number to add'), 'abc')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText(text)).toBeInTheDocument()
  })

  it('searches by prefix with the plus sign encoded, and removes after a confirm', async () => {
    const seen: Array<{ method: string; url: string }> = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      seen.push({ method: init.method ?? 'GET', url: String(input) })
      if (init.method === 'DELETE') return new Response(null, { status: 204 })
      return json({
        items: [{ number: '+8613800138000', source: 'IMPORT', createdAt: '2026-10-12T00:00:00Z' }],
        total: 1,
      })
    }))
    const { user } = renderWithProviders(<Page />)
    expect(await screen.findByText('+8613800138000')).toBeInTheDocument()
    await user.type(screen.getByLabelText('Search by prefix'), '+86 138')
    await waitFor(() => expect(seen.some((s) => s.url.includes('prefix=%2B86138'))).toBe(true))

    await user.click(screen.getByTitle('Delete'))
    expect(seen.some((s) => s.method === 'DELETE')).toBe(false)
    await user.click(within(await screen.findByText(/Remove \+8613800138000\?/).then((n) => n.parentElement as HTMLElement)).getByRole('button', { name: 'Delete' }))
    await waitFor(() =>
      expect(seen).toContainEqual({ method: 'DELETE', url: '/api/v1/dnc-numbers/%2B8613800138000' }),
    )
  })
})

// --- a call with no record ----------------------------------------------------------

describe('a call page for a call with no record', () => {
  it('says so at once instead of loading', async () => {
    const { Route } = await import('@/routes/_app.admin.cdr.$callId')
    vi.spyOn(Route, 'useParams').mockReturnValue({ callId: 'c-1' } as never)
    const started = Date.now()
    vi.stubGlobal('fetch', vi.fn(async () =>
      json({ error: { code: 'NOT_FOUND', message: 'no such call' } }, 404),
    ))
    const Page = Route.options.component as () => ReactElement
    renderWithProviders(<Page />)
    expect(await screen.findByText('No call record')).toBeInTheDocument()
    expect(Date.now() - started).toBeLessThan(900)
    expect(fetch).toHaveBeenCalledTimes(1)
  })
})
