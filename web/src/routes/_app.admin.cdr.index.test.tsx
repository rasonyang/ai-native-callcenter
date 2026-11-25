import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import {
  Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter,
} from '@tanstack/react-router'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import i18n from '@/lib/i18n'
import type { CDR, CDRList } from '@/lib/ledger'
import { Route } from '@/routes/_app.admin.cdr.index'
import { installBackend, renderWithProviders } from '@/test/harness'

/**
 * The Call records screen (#711): calls dated in the deployment's time zone,
 * whatever zone the browser is in; a date filter of whole days in that zone;
 * the caller, called-number and billed-duration filters beside the number box
 * of either end (#714, tested in `_app.admin.cdr.test.tsx`); a totals line over
 * the whole filtered set; and an export of that set, which the server makes.
 *
 * The browser runs in Los Angeles here and the deployment in Shanghai, the
 * issue's own case, so a date that came out of the browser's zone would show.
 */

const CDRExplorer = Route.options.component as () => ReactElement

const BROWSER_ZONE = 'America/Los_Angeles'
const DEPLOYMENT_ZONE = 'Asia/Shanghai'

const previousTZ = process.env.TZ
beforeAll(() => {
  process.env.TZ = BROWSER_ZONE
})
afterAll(() => {
  if (previousTZ === undefined) delete process.env.TZ
  else process.env.TZ = previousTZ
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

const id = (n: number) => `0199f3a2-7c41-7d22-9b1e-${String(n).padStart(12, '0')}`

function cdr(n: number, overrides: Partial<CDR> = {}): CDR {
  return {
    callId: id(n),
    startedAt: '2026-12-01T01:30:00Z',
    endedAt: '2026-12-01T01:35:00Z',
    callType: 'OUTBOUND',
    fromNumber: '02155550101',
    toNumber: '13800000001',
    ringSec: 3,
    botSec: 40,
    queueWaitSec: 0,
    talkSec: 60,
    billSec: 60,
    totalSec: 63,
    status: 'ANSWERED',
    isContained: false,
    hasRecording: false,
    legs: [{ kind: 'BOT', label: 'collections', durationSec: 40 }],
    ...overrides,
  }
}

/** The issue's test calls, by its numbering, as instants (the table gives Shanghai times). */
const CALLS = {
  1: cdr(1, { startedAt: '2026-11-30T15:59:50Z', billSec: 12 }),
  2: cdr(2, { startedAt: '2026-11-30T16:00:05Z', toNumber: '13800000002', billSec: 60 }),
  7: cdr(7, { startedAt: '2026-12-31T15:59:59Z', billSec: 3725 }),
  8: cdr(8, { startedAt: '2026-12-31T16:00:00Z', billSec: 61 }),
}

function list(items: CDR[], overrides: Partial<CDRList> = {}): CDRList {
  return {
    items,
    total: items.length,
    billSecTotal: items.reduce((sum, row) => sum + row.billSec, 0),
    timeZone: DEPLOYMENT_ZONE,
    ...overrides,
  }
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })

const CSV_NAME = 'calls-20261201-20261201.csv'

/**
 * The screen under a router (its rows link to a call's page), against a
 * backend that answers the listing with `answer` and the export with `file`.
 */
function render(options: {
  answer?: (query: URLSearchParams) => unknown
  file?: () => Response
} = {}) {
  const answer = options.answer ?? (() => list(Object.values(CALLS)))
  const file = options.file ?? (() => new Response('﻿Call ID\r\n', {
    headers: {
      'content-type': 'text/csv; charset=utf-8',
      'content-disposition': `attachment; filename="${CSV_NAME}"`,
    },
  }))
  const api = installBackend({
    handle: ({ path, method }) => {
      if (method !== 'GET') return undefined
      if (path.startsWith('/cdrs/export')) return file()
      if (path.startsWith('/cdrs?')) return answer(queryOf(path))
      return undefined
    },
  })

  const root = createRootRoute({ component: Outlet })
  const tree = root.addChildren([
    createRoute({ getParentRoute: () => root, path: '/admin/cdr', component: CDRExplorer }),
    createRoute({ getParentRoute: () => root, path: '/admin/cdr/$callId', component: () => null }),
  ])
  const router = createRouter({
    routeTree: tree,
    history: createMemoryHistory({ initialEntries: ['/admin/cdr'] }),
  })
  const view = renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
  )
  return { api, ...view }
}

const queryOf = (path: string) => new URLSearchParams(path.split('?')[1] ?? '')

const listings = (api: { requests: Array<{ path: string }> }) =>
  api.requests.filter((r) => r.path.startsWith('/cdrs?')).map((r) => queryOf(r.path))

const exports = (api: { requests: Array<{ path: string }> }) =>
  api.requests.filter((r) => r.path.startsWith('/cdrs/export')).map((r) => queryOf(r.path))

/** The first cell of every data row: each call's start as the table shows it. */
function startCells(): string[] {
  return screen
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[0].textContent ?? '')
}

/** What the browser was handed: each file's name. */
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

async function pickDays(first: string, last: string) {
  const from = await screen.findByLabelText('First day')
  await waitFor(() => expect(from).toBeEnabled())
  fireEvent.change(from, { target: { value: first } })
  fireEvent.change(screen.getByLabelText('Last day'), { target: { value: last } })
}

it('runs in a browser zone that is not the deployment’s', () => {
  expect(Intl.DateTimeFormat().resolvedOptions().timeZone).toBe(BROWSER_ZONE)
})

describe('the start of each call', () => {
  it('is dated in the deployment’s zone, with the year, whatever the browser’s zone', async () => {
    render()
    await screen.findByRole('link', { name: '2026-11-30 23:59:50' })
    expect(startCells()).toEqual([
      '2026-11-30 23:59:50',
      '2026-12-01 00:00:05',
      '2026-12-31 23:59:59',
      '2027-01-01 00:00:00',
    ])
  })

  it('names the zone once, by the date filter', async () => {
    render()
    expect(await screen.findByRole('group', { name: 'Start date (Asia/Shanghai)' })).toBeInTheDocument()
    expect(screen.getAllByText(/Asia\/Shanghai/)).toHaveLength(1)
  })

  it('stands beside the billed duration, which shows billSec as recorded', async () => {
    render()
    await screen.findByRole('link', { name: '2026-12-31 23:59:59' })
    const headers = screen.getAllByRole('columnheader').map((th) => th.textContent)
    expect(headers.indexOf('Billed')).toBe(headers.indexOf('Duration') + 1)
    const row7 = screen.getByRole('link', { name: '2026-12-31 23:59:59' }).closest('tr') as HTMLElement
    const cells = within(row7).getAllByRole('cell')
    expect(cells[headers.indexOf('Billed')]).toHaveTextContent('1:02:05')
  })
})

describe('the totals line', () => {
  it('counts every call the filter selects and the seconds they were billed, not the page', async () => {
    render({ answer: () => list([CALLS[2], CALLS[7]], { total: 6, billSecTotal: 3785 }) })
    expect(await screen.findByText('6 calls · 1:03:05 billed (3,785 s)')).toBeInTheDocument()
  })

  it('follows a filter change', async () => {
    render({
      answer: (query) =>
        query.get('billSecMin') === '60'
          ? list([CALLS[2], CALLS[8]])
          : list(Object.values(CALLS), { total: 8, billSecTotal: 3858 }),
    })
    await screen.findByText('8 calls · 1:04:18 billed (3,858 s)')
    fireEvent.change(screen.getByLabelText('Billed at least, in seconds'), { target: { value: '60' } })
    expect(await screen.findByText('2 calls · 02:01 billed (121 s)')).toBeInTheDocument()
  })
})

describe('the date filter', () => {
  it('waits for the deployment’s zone before a day can be picked', async () => {
    let release!: () => void
    const answered = new Promise<void>((resolve) => {
      release = resolve
    })
    render({ answer: async () => (await answered, list([CALLS[1]])) })
    const from = await screen.findByLabelText('First day')
    expect(from).toBeDisabled()
    expect(screen.getByLabelText('Last day')).toBeDisabled()
    release()
    await waitFor(() => expect(from).toBeEnabled())
  })

  it('sends whole days in the deployment’s zone: the first day’s midnight to the next midnight after the last', async () => {
    const { api } = render()
    await pickDays('2026-12-01', '2026-12-01')
    await waitFor(() => expect(listings(api).at(-1)?.get('to')).toBe('2026-12-01T16:00:00Z'))
    const query = listings(api).at(-1) as URLSearchParams
    expect(query.get('from')).toBe('2026-11-30T16:00:00Z')
    expect(query.get('offset')).toBe('0')
  })
})

describe('the number and billed-duration filters', () => {
  it('send the caller, the called number and both billed bounds, and no either-end number', async () => {
    const { api } = render()
    await screen.findByRole('link', { name: '2026-11-30 23:59:50' })
    fireEvent.change(screen.getByPlaceholderText('From number…'), { target: { value: '0215555' } })
    fireEvent.change(screen.getByPlaceholderText('To number…'), { target: { value: '13800000001' } })
    fireEvent.change(screen.getByLabelText('Billed at least, in seconds'), { target: { value: '60' } })
    fireEvent.change(screen.getByLabelText('Billed at most, in seconds'), { target: { value: '61' } })
    await waitFor(() => expect(listings(api).at(-1)?.get('billSecMax')).toBe('61'))
    const query = listings(api).at(-1) as URLSearchParams
    expect(query.get('fromNumber')).toBe('0215555')
    expect(query.get('toNumber')).toBe('13800000001')
    expect(query.get('billSecMin')).toBe('60')
    // The number box (#714) is left empty: it adds nothing of its own.
    expect(query.has('number')).toBe(false)
  })

  it('send the number box as either end’s number, beside the caller and the called number', async () => {
    const { api } = render()
    await screen.findByRole('link', { name: '2026-11-30 23:59:50' })
    fireEvent.change(screen.getByPlaceholderText('Number…'), { target: { value: ' 5555 ' } })
    fireEvent.change(screen.getByPlaceholderText('From number…'), { target: { value: '021' } })
    fireEvent.change(screen.getByPlaceholderText('To number…'), { target: { value: '138' } })
    await waitFor(() => expect(listings(api).at(-1)?.get('toNumber')).toBe('138'))
    const query = listings(api).at(-1) as URLSearchParams
    expect(query.get('number')).toBe('5555')
    expect(query.get('fromNumber')).toBe('021')
    expect(query.get('offset')).toBe('0')
  })

  it('sends a status with the others', async () => {
    const { api } = render()
    await screen.findByRole('link', { name: '2026-11-30 23:59:50' })
    fireEvent.change(screen.getByLabelText('Status'), { target: { value: 'ANSWERED' } })
    fireEvent.change(screen.getByPlaceholderText('To number…'), { target: { value: '138' } })
    await waitFor(() => expect(listings(api).at(-1)?.get('toNumber')).toBe('138'))
    expect(listings(api).at(-1)?.get('status')).toBe('ANSWERED')
  })
})

describe('the export', () => {
  it('is disabled, and says why, until a date range is chosen', async () => {
    render()
    await screen.findByRole('link', { name: '2026-11-30 23:59:50' })
    const button = screen.getByRole('button', { name: 'Export' })
    expect(button).toBeDisabled()
    expect(button).toHaveAccessibleDescription('Choose a start date range to export.')
    fireEvent.change(screen.getByLabelText('First day'), { target: { value: '2026-12-01' } })
    expect(button).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Last day'), { target: { value: '2026-12-01' } })
    expect(button).toBeEnabled()
  })

  it('asks the server for every call the filters select, without paging, and saves the file it names', async () => {
    const files = captureDownloads()
    const { api, user } = render()
    await pickDays('2026-12-01', '2026-12-01')
    fireEvent.change(screen.getByPlaceholderText('To number…'), { target: { value: '13800000001' } })
    fireEvent.change(screen.getByLabelText('Billed at least, in seconds'), { target: { value: '1' } })
    await user.click(screen.getByRole('button', { name: 'Export' }))

    await waitFor(() => expect(files).toHaveLength(1))
    expect(files[0].name).toBe(CSV_NAME)
    const [query] = exports(api)
    expect(Object.fromEntries(query)).toEqual({
      from: '2026-11-30T16:00:00Z',
      to: '2026-12-01T16:00:00Z',
      toNumber: '13800000001',
      billSecMin: '1',
      language: 'en',
    })
  })

  it('selects what the list selects, the number box’s number of either end included', async () => {
    const files = captureDownloads()
    const { api, user } = render()
    await pickDays('2026-12-01', '2026-12-01')
    fireEvent.change(screen.getByPlaceholderText('Number…'), { target: { value: '13800000001' } })
    await waitFor(() => expect(listings(api).at(-1)?.get('number')).toBe('13800000001'))
    await user.click(screen.getByRole('button', { name: 'Export' }))

    await waitFor(() => expect(files).toHaveLength(1))
    const listed = listings(api).at(-1) as URLSearchParams
    const [exported] = exports(api)
    listed.delete('limit')
    listed.delete('offset')
    exported.delete('language')
    expect(Object.fromEntries(exported)).toEqual(Object.fromEntries(listed))
    expect(exported.get('number')).toBe('13800000001')
  })

  it('asks for the header row in the screen’s language', async () => {
    captureDownloads()
    await i18n.changeLanguage('zh')
    const { api, user, unmount } = render()
    try {
      const from = await screen.findByLabelText('起始日')
      await waitFor(() => expect(from).toBeEnabled())
      fireEvent.change(from, { target: { value: '2026-12-01' } })
      fireEvent.change(screen.getByLabelText('截止日'), { target: { value: '2026-12-31' } })
      await user.click(screen.getByRole('button', { name: '导出' }))
      await waitFor(() => expect(exports(api)).toHaveLength(1))
      expect(exports(api)[0].get('language')).toBe('zh')
      expect(exports(api)[0].get('to')).toBe('2026-12-31T16:00:00Z')
    } finally {
      // Unmounted first: the language change would otherwise re-render a
      // screen nobody is looking at, outside the test's act().
      unmount()
      await i18n.changeLanguage('en')
    }
  })

  it('shows the server’s refusal of a window too long, and saves nothing', async () => {
    const files = captureDownloads()
    const { user } = render({
      file: () =>
        json(
          {
            error: {
              code: 'VALIDATION_FAILED',
              message: 'an export covers at most two calendar months',
              params: { field: 'to', rule: 'EXPORT_WINDOW_TOO_LONG', maxMonths: 2 },
            },
          },
          400,
        ),
    })
    await pickDays('2026-12-01', '2027-02-01')
    await user.click(screen.getByRole('button', { name: 'Export' }))

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Export failed: An export covers at most 2 calendar months; narrow the date range.',
    )
    expect(files).toHaveLength(0)

    // The refusal was about that window; changing it clears it.
    fireEvent.change(screen.getByLabelText('Last day'), { target: { value: '2027-01-31' } })
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
  })
})
