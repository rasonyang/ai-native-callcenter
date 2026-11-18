import type { Campaign, CampaignContact, CampaignLeadIntent, ContactQuery } from './campaigns'
import { asText, guardFormula, toCsv } from './csv'
import type { CallbackStatus } from './ledger'
import { nationalNumber } from './phonenum'

/**
 * A campaign's leads as a CSV file, built in the browser from the list
 * operation the Leads tab reads (GET /campaigns/{campaignId}/contacts), with
 * the tab's own filter, every page of it. The screen gets nothing an
 * integration could not page itself.
 *
 * The file is the same in every language: the header is the wire field names,
 * enum values are written as the API sends them (HIGH), and an empty cell is a
 * value the API left out. Phone numbers are in national form when they are of
 * the campaign's default country code (nationalNumber), and written as text
 * so that a spreadsheet keeps a landline's leading 0; the file reads back
 * through the contact import as the same numbers.
 */

/**
 * Rows asked for per request. The contract allows a larger limit, but the
 * server answers at most 1000 contacts per page (a larger limit is served as
 * its default page of 50), so 1000 is the largest page that is honoured. The
 * paging below never relies on it: it moves on by the rows that came back.
 */
export const EXPORT_PAGE_SIZE = 1000

/** Callback lookups in flight at once, one request per HIGH lead. */
export const CALLBACK_CONCURRENCY = 6

/** The columns, in the order of the Leads table, then the contact's id. */
export const LEAD_COLUMNS = [
  'number', 'leadIntent', 'leadNotes', 'callbackStatus', 'callId', 'contactId',
] as const

export interface ExportProgress {
  /** Rows written so far. */
  done: number
  /** Rows the filter matches, as the latest page said. */
  total: number
}

export interface ContactPage {
  items: CampaignContact[]
  total: number
}

/** One page of the list, with the tab's filter already applied. */
export type FetchContactPage = (
  page: { offset: number; limit: number },
  signal: AbortSignal,
) => Promise<ContactPage>

/** The status of the callback a call filed, or undefined when it filed none. */
export type FetchCallbackStatus = (callId: string, signal: AbortSignal) => Promise<CallbackStatus | undefined>

/**
 * Whether a lead's row carries a callback status. The Leads table shows one
 * for a HIGH lead whose call is known, found by call id, never by number.
 */
export function hasCallbackColumn(contact: CampaignContact): contact is CampaignContact & { lastCallId: string } {
  return contact.leadIntent === 'HIGH' && Boolean(contact.lastCallId)
}

/** One lead as the cells of its row, in LEAD_COLUMNS order. */
export function leadCells(
  contact: CampaignContact,
  callbackStatus: CallbackStatus | undefined,
  countryCode: string | undefined,
): string[] {
  return [
    asText(nationalNumber(contact.number, countryCode)),
    guardFormula(contact.leadIntent ?? ''),
    guardFormula(contact.leadNotes ?? ''),
    guardFormula(callbackStatus ?? ''),
    guardFormula(contact.lastCallId ?? ''),
    guardFormula(contact.id),
  ]
}

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw signal.reason ?? new DOMException('The export was cancelled.', 'AbortError')
}

/**
 * Runs fn over items with at most `limit` in flight, results in item order.
 * The first failure rejects, and nothing further is started after it.
 */
async function mapLimit<T, R>(items: T[], limit: number, fn: (item: T) => Promise<R>): Promise<R[]> {
  const out: R[] = []
  let next = 0
  let isStopped = false
  const worker = async () => {
    while (!isStopped && next < items.length) {
      const index = next++
      try {
        // eslint-disable-next-line no-await-in-loop
        out[index] = await fn(items[index])
      } catch (error) {
        isStopped = true
        throw error
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker))
  return out
}

export interface ExportLeadsOptions {
  fetchPage: FetchContactPage
  fetchCallbackStatus: FetchCallbackStatus
  /** The campaign's defaultCountryCode: numbers of it are written nationally. */
  countryCode: string | undefined
  signal: AbortSignal
  onProgress?: (progress: ExportProgress) => void
  pageSize?: number
  concurrency?: number
}

/**
 * Every lead the filter behind fetchPage matches, as CSV text (BOM, CRLF).
 *
 * Pages are read one after another, each from where the rows already read
 * end, until the total the server reports is reached or a page comes back
 * empty. Rows keep the list's order, and a row seen on an earlier page (the
 * list moved under the export: a call made a new lead before it) is written
 * once. Any failed request rejects, and so does a cancel through `signal`:
 * the caller gets text only when every page and every lookup has succeeded,
 * so a partial file is never written. No leads is a file with the header
 * alone.
 */
export async function exportLeads(options: ExportLeadsOptions): Promise<string> {
  const { fetchPage, fetchCallbackStatus, countryCode, signal, onProgress } = options
  const pageSize = options.pageSize ?? EXPORT_PAGE_SIZE
  const concurrency = options.concurrency ?? CALLBACK_CONCURRENCY

  const rows: string[][] = []
  const seen = new Set<string>()
  let offset = 0
  let done = 0
  for (;;) {
    throwIfAborted(signal)
    // One page at a time: the next offset is where this page ends.
    // eslint-disable-next-line no-await-in-loop
    const page = await fetchPage({ offset, limit: pageSize }, signal)
    throwIfAborted(signal)
    const { total } = page
    if (page.items.length === 0) break
    offset += page.items.length

    const fresh = page.items.filter((contact) => {
      if (seen.has(contact.id)) return false
      seen.add(contact.id)
      return true
    })
    done += fresh.filter((contact) => !hasCallbackColumn(contact)).length
    onProgress?.({ done, total })
    // eslint-disable-next-line no-await-in-loop
    const statuses = await mapLimit(fresh, concurrency, async (contact) => {
      if (!hasCallbackColumn(contact)) return undefined
      const status = await fetchCallbackStatus(contact.lastCallId, signal)
      throwIfAborted(signal)
      done++
      onProgress?.({ done, total })
      return status
    })
    fresh.forEach((contact, i) => rows.push(leadCells(contact, statuses[i], countryCode)))
    if (offset >= total) break
  }
  throwIfAborted(signal)
  return toCsv([[...LEAD_COLUMNS], ...rows])
}

const twoDigits = (n: number) => String(n).padStart(2, '0')

/** A campaign's name as a file name part: letters and digits of any script, joined by dashes. */
function fileNamePart(name: string): string {
  return name
    .normalize('NFKC')
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60)
    .replace(/-+$/, '')
}

/**
 * campaign-<name>-leads-<intent>-<YYYYMMDD>.csv, the date the browser's own.
 * A name with no letters or digits gives way to the campaign's id. The name
 * is not translated: the file it names is the same in every language.
 */
export function leadsFileName(
  campaign: Pick<Campaign, 'id' | 'name'>,
  query: ContactQuery & { leadIntent: CampaignLeadIntent },
  now: Date = new Date(),
): string {
  const day = `${now.getFullYear()}${twoDigits(now.getMonth() + 1)}${twoDigits(now.getDate())}`
  const intent = query.leadIntent.toLowerCase().replaceAll('_', '-')
  return `campaign-${fileNamePart(campaign.name) || campaign.id}-leads-${intent}-${day}.csv`
}
