import { cleanup, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import type { CDR, RecordingRow } from '@/lib/ledger'
import { renderApp, waitForRows } from '@/test/app'
import { removeClipboard, stubClipboard, stubExecCommand } from '@/test/clipboard'
import { cdrFixture } from '@/test/harness'

/**
 * Getting at a call's id on the two Call records screens (#714): copying it
 * from the call's page and from its row, and the way back, a pasted id opening
 * the call. The two ids differ only in their last character, so a copy that is
 * truncated, or taken from the wrong row, cannot pass.
 */

const A = '019ac2e4-7b10-7c3e-9a51-0f6d2b8e4a01'
const B = '019ac2e4-7b10-7c3e-9a51-0f6d2b8e4a02'
/** Well formed, and no call has it. */
const MISSING = '019ac2e4-7b10-7c3e-9a51-0f6d2b8e4aff'

const rowA = cdrFixture({
  callId: A,
  startedAt: '2026-11-25T02:10:00Z',
  endedAt: '2026-11-25T02:15:00Z',
  fromNumber: '13800000001',
  toNumber: '95001',
  did: '95001',
  hasRecording: true,
})
const rowB = cdrFixture({
  callId: B,
  startedAt: '2026-11-25T02:00:00Z',
  endedAt: '2026-11-25T02:04:00Z',
  fromNumber: '13900000002',
  toNumber: '95001',
  did: '95001',
  hasRecording: true,
})

const recording = (callId: string): RecordingRow => ({
  id: '019ac2e4-7b10-7c3e-9a51-00000000aa01',
  callId,
  backend: 's3',
  objectKey: 'recordings/2026/11/25/call.wav',
  sizeBytes: 5_242_880,
  durationSec: 300,
  format: 'wav',
  createdAt: '2026-11-25T02:15:00Z',
})

const notFound = () =>
  new Response(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'cdr not found' } }), {
    status: 404,
    headers: { 'content-type': 'application/json' },
  })

/**
 * The ledger as the server answers it: the list matched by number and status
 * the way `listCDRs` does, and one call by id, 404 for an id it does not hold.
 */
function ledger(rows: CDR[]) {
  return ({ path, method }: { path: string; method: string }) => {
    if (method !== 'GET') return undefined
    if (path.startsWith('/cdrs?')) {
      const query = new URLSearchParams(path.split('?')[1])
      const number = query.get('number') ?? ''
      const status = query.get('status') ?? ''
      const items = rows.filter(
        (row) =>
          (!number || row.fromNumber.includes(number) || row.toNumber.includes(number)) &&
          (!status || row.status === status),
      )
      return { items, total: items.length }
    }
    const one = /^\/cdrs\/([^/?]+)$/.exec(path)
    if (one) {
      const row = rows.find((r) => r.callId === one[1])
      return row ? { cdr: row, transcript: [], recordings: [recording(row.callId)] } : notFound()
    }
    return undefined
  }
}

function render(path: string) {
  return renderApp(path, { handle: ledger([rowA, rowB]), recordings: [recording(A)] })
}

/** The row whose caller is this number. */
const rowOf = async (number: string) => (await screen.findByText(number)).closest('tr') as HTMLElement

/** The list requests sent so far, as their query strings. */
const listQueries = (requests: Array<{ path: string; method: string }>) =>
  requests
    .filter((r) => r.method === 'GET' && r.path.startsWith('/cdrs?'))
    .map((r) => new URLSearchParams(r.path.split('?')[1]))

beforeEach(() => {
  // The router restores the scroll position on every navigation; jsdom has none.
  vi.stubGlobal('scrollTo', () => {})
  // jsdom has no media playback; the list pauses its player when it unmounts.
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {})
})

afterEach(() => {
  // Unmounted while the stubs above are still in place.
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('the call page', () => {
  it('shows the whole id as its own item, out of the description line', async () => {
    render(`/admin/cdr/${A}`)
    const heading = await screen.findByRole('heading', { level: 1, name: '13800000001 → 95001' })
    const main = screen.getByRole('main')

    // The element holds the id and nothing else: no start time, no spaces.
    expect(within(main).getByText(A).textContent).toBe(A)
    expect(within(main).getByText(i18n.t('agent.callId'))).toBeInTheDocument()
    // The description line under the title is the start time alone.
    const description = heading.nextElementSibling as HTMLElement
    expect(description.textContent).not.toContain(A)
    expect(description.textContent).not.toContain('·')
    expect(description.textContent?.trim()).not.toBe('')
  })

  it('copies exactly the id and says so', async () => {
    const { user } = render(`/admin/cdr/${A}`)
    await screen.findByRole('heading', { level: 1 })
    const writeText = stubClipboard(async () => {})
    const copy = within(screen.getByRole('main')).getByRole('button', { name: i18n.t('admin.copy') })

    await user.click(copy)

    expect(writeText.mock.calls).toEqual([[A]])
    await waitFor(() => expect(copy).toHaveTextContent(i18n.t('admin.copied')))
  })

  it('copies exactly the id over plain HTTP too', async () => {
    const { user } = render(`/admin/cdr/${A}`)
    await screen.findByRole('heading', { level: 1 })
    removeClipboard()
    const commands = stubExecCommand(true)
    const copy = within(screen.getByRole('main')).getByRole('button', { name: i18n.t('admin.copy') })

    await user.click(copy)

    expect(commands).toEqual([expect.objectContaining({ command: 'copy', selected: A })])
    await waitFor(() => expect(copy).toHaveTextContent(i18n.t('admin.copied')))
  })
})

describe('the list', () => {
  it("copies row B's id from row B's button, and stays on the list", async () => {
    const { user, router, backend } = render('/admin/cdr')
    await waitForRows(2)
    const writeText = stubClipboard(async () => {})
    const copyA = within(await rowOf('13800000001')).getByRole('button', { name: i18n.t('agent.copyCallId') })
    const copyB = within(await rowOf('13900000002')).getByRole('button', { name: i18n.t('agent.copyCallId') })
    expect(copyB).toHaveAttribute('title', i18n.t('agent.copyCallId'))

    await user.click(copyB)

    expect(writeText.mock.calls).toEqual([[B]])
    await waitFor(() => expect(copyB).toHaveAccessibleName(i18n.t('agent.callIdCopied')))
    // Only the row copied says so.
    expect(copyA).toHaveAccessibleName(i18n.t('agent.copyCallId'))
    expect(router.state.location.pathname).toBe('/admin/cdr')
    expect(backend.requests.some((r) => r.path === `/cdrs/${B}`)).toBe(false)
    expect(screen.getAllByRole('row')).toHaveLength(3)
  })

  it('says a refused copy was refused, never that it copied', async () => {
    const { user } = render('/admin/cdr')
    await waitForRows(2)
    removeClipboard()
    stubExecCommand(false)
    const copyB = within(await rowOf('13900000002')).getByRole('button', { name: i18n.t('agent.copyCallId') })

    await user.click(copyB)

    await waitFor(() => expect(copyB).toHaveAccessibleName(i18n.t('cdr.callIdCopyFailed')))
  })

  it('still plays a recording from the same cell', async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    const { user, router, backend } = render('/admin/cdr')
    await waitForRows(2)
    const row = await rowOf('13800000001')

    await user.click(within(row).getByRole('button', { name: i18n.t('cdr.play') }))

    await waitFor(() => expect(play).toHaveBeenCalledTimes(1))
    expect(backend.requests.some((r) => r.path === `/calls/${A}/recordings`)).toBe(true)
    expect(within(row).getByRole('button', { name: i18n.t('cdr.stop') })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/admin/cdr')
  })
})

describe('the filter box', () => {
  it('opens the call of a pasted id, spaces and line break around it', async () => {
    const { user, router, backend } = render('/admin/cdr')
    await waitForRows(2)

    await user.click(screen.getByPlaceholderText(i18n.t('cdr.filterNumber')))
    await user.paste(` ${A}\n`)

    await waitFor(() => expect(router.state.location.pathname).toBe(`/admin/cdr/${A}`))
    expect(await screen.findByRole('heading', { level: 1, name: '13800000001 → 95001' })).toBeInTheDocument()
    // Looked up as an id, never sent as a number.
    expect(backend.requests.some((r) => r.path === `/cdrs/${A}`)).toBe(true)
    expect(listQueries(backend.requests).some((q) => q.has('number'))).toBe(false)
  })

  it('accepts an id in upper case', async () => {
    const { user, router } = render('/admin/cdr')
    await waitForRows(2)

    await user.click(screen.getByPlaceholderText(i18n.t('cdr.filterNumber')))
    await user.paste(A.toUpperCase())

    await waitFor(() => expect(router.state.location.pathname).toBe(`/admin/cdr/${A}`))
  })

  it('says a well-formed id names no call, and leaves the list and its filters alone', async () => {
    const { user, router, backend } = render('/admin/cdr')
    await waitForRows(2)
    await user.selectOptions(screen.getByRole('combobox'), 'ANSWERED')
    await waitFor(() => expect(listQueries(backend.requests).at(-1)?.get('status')).toBe('ANSWERED'))
    await waitForRows(2)
    const listed = backend.requests.length

    await user.click(screen.getByPlaceholderText(i18n.t('cdr.filterNumber')))
    await user.paste(MISSING)

    expect(await screen.findByRole('alert')).toHaveTextContent(i18n.t('cdr.callNotFound', { callId: MISSING }))
    expect(router.state.location.pathname).toBe('/admin/cdr')
    expect(screen.getByRole('combobox')).toHaveValue('ANSWERED')
    // The same two rows, and no list request since: not an empty table.
    expect(await rowOf('13800000001')).toBeInTheDocument()
    expect(await rowOf('13900000002')).toBeInTheDocument()
    expect(screen.queryByText(i18n.t('cdr.noCalls'))).not.toBeInTheDocument()
    expect(backend.requests.slice(listed).map((r) => r.path)).toEqual([`/cdrs/${MISSING}`])
  })

  it.each([
    ['a number', '13800000001'],
    ['a fragment of an id', '019ac2e4'],
  ])('filters %s by number, as before', async (_name, value) => {
    const { user, router, backend } = render('/admin/cdr')
    await waitForRows(2)

    await user.click(screen.getByPlaceholderText(i18n.t('cdr.filterNumber')))
    await user.paste(value)

    await waitFor(() => expect(listQueries(backend.requests).at(-1)?.get('number')).toBe(value))
    expect(backend.requests.some((r) => r.path.startsWith('/cdrs/'))).toBe(false)
    expect(router.state.location.pathname).toBe('/admin/cdr')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
