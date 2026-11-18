import { describe, expect, it, vi } from 'vitest'

import { buildRecords, guessRoles } from './campaign-import'
import {
  EXPORT_PAGE_SIZE, LEAD_COLUMNS, exportLeads, leadCells, leadsFileName,
  type ExportLeadsOptions, type ExportProgress, type FetchContactPage,
} from './campaign-export'
import type { CampaignContact, CampaignLeadIntent } from './campaigns'
import { parseCsv } from './csv'
import type { CallbackStatus } from './ledger'

const CAMPAIGN_ID = '00000000-0000-4000-8000-000000000001'
const id = (prefix: string, n: number) => `00000000-0000-4000-8000-${prefix}${String(n).padStart(12 - prefix.length, '0')}`

/** Synthetic leads only: 138 0000 xxxx mobiles, 010 6000 xxxx landlines. */
function lead(n: number, over: Partial<CampaignContact> = {}): CampaignContact {
  const intents: CampaignLeadIntent[] = ['HIGH', 'MEDIUM', 'LOW', 'NONE', 'DO_NOT_CALL']
  return {
    id: id('c', n),
    campaignId: CAMPAIGN_ID,
    number: n % 3 === 0 ? `+86106000${String(n).padStart(4, '0')}` : `+861380000${String(n).padStart(4, '0')}`,
    numberRaw: '',
    slots: {},
    status: 'DONE',
    attempts: 1,
    systemFailures: 0,
    lastOutcome: 'ANSWERED',
    leadIntent: intents[n % intents.length],
    leadNotes: `Lead ${n}`,
    lastCallId: id('ca', n),
    lastAttemptId: id('a', n),
    createdAt: '2026-11-01T00:00:00Z',
    ...over,
  }
}

/** A server holding `rows`, paged as the real one pages: by offset and limit. */
function server(rows: CampaignContact[], options: { maxPage?: number } = {}) {
  const calls: Array<{ offset: number; limit: number }> = []
  const fetchPage: FetchContactPage = async ({ offset, limit }) => {
    calls.push({ offset, limit })
    const size = Math.min(limit, options.maxPage ?? limit)
    return { items: rows.slice(offset, offset + size), total: rows.length }
  }
  return { fetchPage, calls }
}

const noCallbacks = async () => undefined

function run(over: Partial<ExportLeadsOptions> & Pick<ExportLeadsOptions, 'fetchPage'>) {
  const progress: ExportProgress[] = []
  const promise = exportLeads({
    fetchCallbackStatus: noCallbacks,
    countryCode: '86',
    signal: new AbortController().signal,
    onProgress: (p) => progress.push(p),
    ...over,
  })
  return { promise, progress }
}

const bodyRows = (text: string) => parseCsv(text).rows.map((r) => r.cells)

describe('the leads export', () => {
  it('asks for the largest page the server honours', async () => {
    const { fetchPage, calls } = server([lead(1)])
    await run({ fetchPage }).promise
    expect(calls).toEqual([{ offset: 0, limit: EXPORT_PAGE_SIZE }])
    expect(EXPORT_PAGE_SIZE).toBe(1000)
  })

  it('writes every row of several pages once, in the list order', async () => {
    const rows = Array.from({ length: 2345 }, (_, i) => lead(i + 1))
    const { fetchPage, calls } = server(rows)
    const { promise, progress } = run({ fetchPage })
    const text = await promise
    expect(calls.map((c) => c.offset)).toEqual([0, 1000, 2000])
    const ids = bodyRows(text).map((cells) => cells[LEAD_COLUMNS.indexOf('contactId')])
    expect(ids).toEqual(rows.map((r) => r.id))
    expect(progress.at(-1)).toEqual({ done: 2345, total: 2345 })
  })

  it('moves on by the rows that came back when the server answers smaller pages', async () => {
    const rows = Array.from({ length: 120 }, (_, i) => lead(i + 1))
    const { fetchPage, calls } = server(rows, { maxPage: 50 })
    const text = await run({ fetchPage }).promise
    expect(calls.map((c) => c.offset)).toEqual([0, 50, 100])
    expect(bodyRows(text)).toHaveLength(120)
  })

  it('writes a row once when the list moves under it between two pages', async () => {
    const rows = Array.from({ length: 5 }, (_, i) => lead(i + 1))
    let call = 0
    // A new lead lands before the second page: everything after it moves one on.
    const fetchPage: FetchContactPage = async ({ offset, limit }) => {
      call++
      const list = call === 1 ? rows : [lead(0), ...rows]
      return { items: list.slice(offset, offset + Math.min(limit, 3)), total: list.length }
    }
    const text = await run({ fetchPage }).promise
    const ids = bodyRows(text).map((cells) => cells[LEAD_COLUMNS.indexOf('contactId')])
    expect(ids).toEqual(rows.map((r) => r.id))
  })

  it('writes the header alone when nothing matches', async () => {
    const { fetchPage } = server([])
    const text = await run({ fetchPage }).promise
    expect(text).toBe(`﻿${LEAD_COLUMNS.join(',')}\r\n`)
  })

  it('fails without a file when a page fails mid-way, and asks for nothing after it', async () => {
    const rows = Array.from({ length: 2500 }, (_, i) => lead(i + 1))
    const { fetchPage: real } = server(rows)
    const failure = new Error('page 2 failed')
    const fetchPage = vi.fn<FetchContactPage>(async (page, signal) => {
      if (page.offset === 1000) throw failure
      return real(page, signal)
    })
    await expect(run({ fetchPage }).promise).rejects.toBe(failure)
    expect(fetchPage.mock.calls.map(([page]) => page.offset)).toEqual([0, 1000])
  })

  it('fails without a file when a callback lookup fails', async () => {
    const rows = [lead(5, { leadIntent: 'HIGH' }), lead(10, { leadIntent: 'HIGH' })]
    const { fetchPage } = server(rows)
    const failure = new Error('lookup failed')
    await expect(run({ fetchPage, fetchCallbackStatus: async () => { throw failure } }).promise).rejects.toBe(failure)
  })

  it('stops on cancel: no text, no further page', async () => {
    const rows = Array.from({ length: 3000 }, (_, i) => lead(i + 1))
    const controller = new AbortController()
    const { fetchPage: real } = server(rows)
    const fetchPage = vi.fn<FetchContactPage>(async (page, signal) => {
      const answer = await real(page, signal)
      if (page.offset === 1000) controller.abort()
      return answer
    })
    const { promise } = run({ fetchPage, signal: controller.signal })
    await expect(promise).rejects.toMatchObject({ name: 'AbortError' })
    expect(fetchPage.mock.calls.map(([page]) => page.offset)).toEqual([0, 1000])
  })

  it('looks a callback up by call id for a HIGH lead only, and writes its status', async () => {
    const rows = [
      lead(5, { leadIntent: 'HIGH' }),
      lead(6, { leadIntent: 'MEDIUM' }),
      lead(10, { leadIntent: 'HIGH', lastCallId: null }),
      lead(15, { leadIntent: 'HIGH' }),
    ]
    const { fetchPage } = server(rows)
    const byCall: Record<string, CallbackStatus> = { [id('ca', 5)]: 'CLAIMED' }
    const asked: string[] = []
    const text = await run({
      fetchPage,
      fetchCallbackStatus: async (callId) => {
        asked.push(callId)
        return byCall[callId]
      },
    }).promise
    expect(asked.toSorted()).toEqual([id('ca', 5), id('ca', 15)].toSorted())
    const status = bodyRows(text).map((cells) => cells[LEAD_COLUMNS.indexOf('callbackStatus')])
    expect(status).toEqual(['CLAIMED', '', '', ''])
  })

  it('keeps no more callback lookups in flight than its limit', async () => {
    const rows = Array.from({ length: 20 }, (_, i) => lead(i * 5, { leadIntent: 'HIGH' }))
    const { fetchPage } = server(rows)
    let inFlight = 0
    let most = 0
    await run({
      fetchPage,
      concurrency: 3,
      fetchCallbackStatus: async () => {
        inFlight++
        most = Math.max(most, inFlight)
        await new Promise((resolve) => setTimeout(resolve, 1))
        inFlight--
        return 'OPEN'
      },
    }).promise
    expect(most).toBe(3)
  })
})

describe('a lead row', () => {
  it('is the Leads table columns with wire names, then the ids', () => {
    expect(LEAD_COLUMNS).toEqual(['number', 'leadIntent', 'leadNotes', 'callbackStatus', 'callId', 'contactId'])
  })

  it('writes the national number as text, enum values as on the wire and an absent value as empty', () => {
    const contact = lead(3, {
      number: '+861060000000', leadIntent: 'DO_NOT_CALL', leadNotes: undefined, lastCallId: null,
    })
    expect(leadCells(contact, undefined, '86')).toEqual([
      '\t01060000000', 'DO_NOT_CALL', '', '', '', contact.id,
    ])
  })

  it('keeps a number of another country international, still as text', () => {
    expect(leadCells(lead(1, { number: '+447700900123' }), 'OPEN', '86')[0]).toBe('\t+447700900123')
  })

  it('guards notes a spreadsheet would run, and keeps every other character', () => {
    const notes = ['=1+1', '@SUM(A1)', '-5 discount', '+86 asked twice', '王伟, "VIP"\nline two']
    const cells = notes.map((n) => leadCells(lead(1, { leadNotes: n }), undefined, '86')[2])
    expect(cells).toEqual(["'=1+1", "'@SUM(A1)", "'-5 discount", "'+86 asked twice", '王伟, "VIP"\nline two'])
  })
})

describe('the exported file read back by the contact import', () => {
  it('maps the number column by itself and sends the numbers in national form', async () => {
    const rows = [
      lead(1, { number: '+8613800000001', leadNotes: '张伟, "A" =x' }),
      lead(2, { number: '+861060000000', leadNotes: '=HYPERLINK("x")' }),
      lead(3, { number: '+447700900123', leadNotes: 'line\nbreak' }),
    ]
    const { fetchPage } = server(rows)
    const text = await run({ fetchPage }).promise
    const table = parseCsv(text)
    const roles = guessRoles(table.header)
    expect(roles[0]).toEqual({ kind: 'number' })
    const sent = buildRecords(table, roles).map((r) => r.row?.number)
    expect(sent).toEqual(['13800000001', '01060000000', '+447700900123'])
  })
})

describe('the file name', () => {
  const day = new Date(2026, 10, 18, 9, 30)

  it('names the campaign, the intent and the day', () => {
    expect(leadsFileName({ id: CAMPAIGN_ID, name: 'Spring Renewals 2026' }, { leadIntent: 'HIGH' }, day))
      .toBe('campaign-spring-renewals-2026-leads-high-20261118.csv')
    expect(leadsFileName({ id: CAMPAIGN_ID, name: '春季续费 / 北京' }, { leadIntent: 'DO_NOT_CALL' }, day))
      .toBe('campaign-春季续费-北京-leads-do-not-call-20261118.csv')
  })

  it('falls back to the id for a name with no letters or digits', () => {
    expect(leadsFileName({ id: CAMPAIGN_ID, name: '***' }, { leadIntent: 'LOW' }, day))
      .toBe(`campaign-${CAMPAIGN_ID}-leads-low-20261118.csv`)
  })
})
