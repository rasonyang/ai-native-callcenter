import { describe, expect, it, vi } from 'vitest'

import {
  buildRecords, guessRoles, mappingProblem, mergeSummaries, rejectedCsv, roleFromValue, roleValue,
  runImport, type ColumnRole, type ImportRecord,
} from './campaign-import'
import type { CampaignContactRow, CampaignImportResult } from './campaigns'
import { parseCsv } from './csv'

describe('guessing what each column is', () => {
  it('finds a number and a time zone column by their headers, and ignores the rest', () => {
    // A column the flow does not read would fail every row, so none is sent
    // unless the operator opts it in.
    expect(guessRoles(['name', 'phone', 'tz', 'plan'])).toEqual([
      { kind: 'ignore' },
      { kind: 'number' },
      { kind: 'timeZone' },
      { kind: 'ignore' },
    ])
    expect(guessRoles(['姓名', '手机号', '时区', '套餐'])).toEqual([
      { kind: 'ignore' },
      { kind: 'number' },
      { kind: 'timeZone' },
      { kind: 'ignore' },
    ])
  })

  it('takes the first column for the number when no header says so', () => {
    expect(guessRoles(['a', 'b'])[0]).toEqual({ kind: 'number' })
  })

  it('ignores a column with no header', () => {
    expect(guessRoles(['phone', ''])[1]).toEqual({ kind: 'ignore' })
  })

  it('takes a header that names a field the flow reads, in any case', () => {
    // The case is whatever the spreadsheet's author typed; the name is the
    // flow's, so the mapping carries the flow's spelling, not the header's.
    expect(guessRoles(['Phone', 'NAME', 'OrderNo', '订单号', 'notes'], ['name', 'orderNo'])).toEqual([
      { kind: 'number' },
      { kind: 'slot', name: 'name' },
      { kind: 'slot', name: 'orderNo' },
      { kind: 'ignore' },
      { kind: 'ignore' },
    ])
  })

  it('gives a field to the first of two headers that name it, and ignores the later ones', () => {
    // `Name` and `name` are one field however the case differs; offering it to
    // both would open the dialog already refused as DUPLICATE_SLOT.
    const roles = guessRoles(['phone', 'Name', 'name', 'NAME'], ['name'])
    expect(roles).toEqual([
      { kind: 'number' },
      { kind: 'slot', name: 'name' },
      { kind: 'ignore' },
      { kind: 'ignore' },
    ])
    expect(mappingProblem(roles)).toBeUndefined()
  })

  it('falls back to the first column that is not a flow field when no header reads like a number', () => {
    // The first column is not always the number: here it is a field the flow reads.
    expect(guessRoles(['Name', 'plan', 'contact_no'], ['name', 'plan'])).toEqual([
      { kind: 'slot', name: 'name' },
      { kind: 'slot', name: 'plan' },
      { kind: 'number' },
    ])
    // When every column names a field, column 0 is still the number.
    expect(guessRoles(['name', 'plan'], ['name', 'plan'])).toEqual([
      { kind: 'number' },
      { kind: 'slot', name: 'plan' },
    ])
    // A header that reads like a number still wins over the fallback.
    expect(guessRoles(['a', 'phone'], ['x'])[1]).toEqual({ kind: 'number' })
  })

  it('prefers the number and the zone over a field of the same name', () => {
    // "phone" and "tz" are not fields the flow can be given; the roles that
    // decide how a row is dialled win.
    expect(guessRoles(['phone', 'tz'], ['phone', 'tz'])).toEqual([
      { kind: 'number' },
      { kind: 'timeZone' },
    ])
  })
})

describe('the value a column dropdown carries', () => {
  it('round-trips every role', () => {
    const roles: ColumnRole[] = [
      { kind: 'number' }, { kind: 'timeZone' }, { kind: 'slot', name: 'orderNo' }, { kind: 'ignore' },
    ]
    for (const role of roles) expect(roleFromValue(roleValue(role))).toEqual(role)
  })

  it('reads the bare slot choice as a field with no name yet', () => {
    expect(roleFromValue('slot')).toEqual({ kind: 'slot', name: '' })
  })
})

describe('checking a mapping', () => {
  const number: ColumnRole = { kind: 'number' }
  it('needs exactly one number column', () => {
    expect(mappingProblem([{ kind: 'ignore' }])).toBe('NO_NUMBER')
    expect(mappingProblem([number, number])).toBe('MANY_NUMBERS')
    expect(mappingProblem([number])).toBeUndefined()
  })
  it('allows one time zone column', () => {
    expect(mappingProblem([number, { kind: 'timeZone' }, { kind: 'timeZone' }])).toBe('MANY_TIME_ZONES')
  })
  it('wants every flow field named, and named once', () => {
    expect(mappingProblem([number, { kind: 'slot', name: ' ' }])).toBe('EMPTY_SLOT_NAME')
    expect(
      mappingProblem([number, { kind: 'slot', name: 'plan' }, { kind: 'slot', name: 'plan' }]),
    ).toBe('DUPLICATE_SLOT')
  })
})

describe('building the rows to send', () => {
  const table = parseCsv('手机号,时区,姓名,备注,忽略\n13800138000,Asia/Shanghai,王伟,"想了解, 套餐",x\n13900139000,,Li,,y\n')
  const roles: ColumnRole[] = [
    { kind: 'number' },
    { kind: 'timeZone' },
    { kind: 'slot', name: 'name' },
    { kind: 'slot', name: 'note' },
    { kind: 'ignore' },
  ]

  it('sends the number, the zone and the other columns as flow fields', () => {
    const records = buildRecords(table, roles)
    expect(records[0].row).toEqual({
      number: '13800138000',
      timeZone: 'Asia/Shanghai',
      slots: { name: '王伟', note: '想了解, 套餐' },
    })
  })

  it('leaves out an empty zone and empty fields instead of sending empty strings', () => {
    expect(buildRecords(table, roles)[1].row).toEqual({ number: '13900139000', slots: { name: 'Li' } })
  })

  it('sends no slots at all when none is mapped', () => {
    const only = buildRecords(table, [{ kind: 'number' }, ...roles.slice(1).map((): ColumnRole => ({ kind: 'ignore' }))])
    expect(only[0].row).toEqual({ number: '13800138000' })
  })

  it('keeps a row of the wrong width at home, with its line', () => {
    const bad = parseCsv('number,name\n1,a\n2\n3,c,extra\n')
    const records = buildRecords(bad, [{ kind: 'number' }, { kind: 'slot', name: 'name' }])
    expect(records.map((r) => r.localCode)).toEqual([undefined, 'ROW_SHAPE', 'ROW_SHAPE'])
    expect(records[1]).toMatchObject({ line: 3, localDetail: '1/2' })
    expect(records[1].row).toBeUndefined()
  })
})

/** A fake import operation that answers like the server: one result per row, in order. */
function server(opts: { reject?: (row: CampaignContactRow) => string | undefined; seen?: Set<string> } = {}) {
  const seen = opts.seen ?? new Set<string>()
  const calls: CampaignContactRow[][] = []
  const send = vi.fn(async (rows: CampaignContactRow[]): Promise<CampaignImportResult> => {
    calls.push(rows)
    const out: CampaignImportResult = { imported: 0, duplicates: 0, rejected: 0, rows: [] }
    rows.forEach((row, index) => {
      const code = opts.reject?.(row)
      if (code) {
        out.rejected++
        out.rows.push({ index, status: 'REJECTED', code: code as 'NUMBER_INVALID' })
      } else if (seen.has(row.number)) {
        out.duplicates++
        out.rows.push({ index, status: 'DUPLICATE', number: `+${row.number}` })
      } else {
        seen.add(row.number)
        out.imported++
        out.rows.push({ index, status: 'IMPORTED', number: `+${row.number}` })
      }
    })
    return out
  })
  return { send, calls }
}

const recordsOf = (n: number, from = 0): ImportRecord[] =>
  Array.from({ length: n }, (_, i) => ({
    line: from + i + 2,
    cells: [String(1000 + from + i)],
    row: { number: String(1000 + from + i) },
  }))

describe('importing in chunks', () => {
  it('splits a long list into requests of at most 5000 rows, in order', async () => {
    const { send, calls } = server()
    const summary = await runImport(recordsOf(12001), send)
    expect(calls.map((c) => c.length)).toEqual([5000, 5000, 2001])
    expect(calls[1][0].number).toBe('6000')
    expect(summary.imported).toBe(12001)
    expect(summary.entries).toHaveLength(12001)
  })

  it('never lets a caller raise the chunk size above what the API takes', async () => {
    const { send, calls } = server()
    await runImport(recordsOf(5001), send, { chunkSize: 100000 })
    expect(calls.map((c) => c.length)).toEqual([5000, 1])
  })

  it('adds up imported, duplicate and rejected rows across chunks', async () => {
    const { send } = server({
      seen: new Set(['1001']),
      reject: (row) => (row.number === '1003' ? 'NUMBER_INVALID' : undefined),
    })
    const summary = await runImport(recordsOf(6), send, { chunkSize: 2 })
    expect(summary).toMatchObject({ imported: 4, duplicates: 1, rejected: 1 })
    expect(summary.entries.map((e) => e.status)).toEqual([
      'IMPORTED', 'DUPLICATE', 'IMPORTED', 'REJECTED', 'IMPORTED', 'IMPORTED',
    ])
  })

  it('reports each result against the line of the file the row came from', async () => {
    const { send } = server({ reject: (row) => (row.number === '1002' ? 'NUMBER_INVALID' : undefined) })
    const summary = await runImport(recordsOf(4), send, { chunkSize: 2 })
    const rejected = summary.entries.filter((e) => e.status === 'REJECTED')
    expect(rejected).toEqual([
      expect.objectContaining({ line: 4, code: 'NUMBER_INVALID', cells: ['1002'] }),
    ])
  })

  it('counts rows refused in the browser as rejected, in file order, without sending them', async () => {
    const records: ImportRecord[] = [
      { line: 2, cells: ['1'], row: { number: '1' } },
      { line: 3, cells: ['2', 'x', 'y'], localCode: 'ROW_SHAPE', localDetail: '3/1' },
      { line: 4, cells: ['3'], row: { number: '3' } },
    ]
    const { send, calls } = server()
    const summary = await runImport(records, send)
    expect(calls).toEqual([[{ number: '1' }, { number: '3' }]])
    expect(summary).toMatchObject({ imported: 2, rejected: 1 })
    expect(summary.entries.map((e) => [e.line, e.status, e.code])).toEqual([
      [2, 'IMPORTED', undefined],
      [3, 'REJECTED', 'ROW_SHAPE'],
      [4, 'IMPORTED', undefined],
    ])
  })

  it('sends nothing for a chunk that holds only refused rows', async () => {
    const { send } = server()
    const summary = await runImport(
      [{ line: 2, cells: ['x'], localCode: 'ROW_SHAPE' }],
      send,
    )
    expect(send).not.toHaveBeenCalled()
    expect(summary.rejected).toBe(1)
  })

  it('reports progress after every chunk', async () => {
    const { send } = server()
    const progress: Array<[number, number]> = []
    await runImport(recordsOf(5), send, { chunkSize: 2, onProgress: (d, t) => progress.push([d, t]) })
    expect(progress).toEqual([[2, 5], [4, 5], [5, 5]])
  })

  it('stops at a failed request and says where, keeping what was done', async () => {
    const good = server()
    let n = 0
    const send = vi.fn(async (rows: CampaignContactRow[]) => {
      if (++n === 2) throw new Error('boom')
      return good.send(rows)
    })
    const summary = await runImport(recordsOf(6), send, { chunkSize: 2 })
    expect(summary.imported).toBe(2)
    expect(summary.failure?.firstUnsent).toBe(2)
    expect(send).toHaveBeenCalledTimes(2)
  })

  it('a retry from the failure point resumes without double counting', async () => {
    const seen = new Set<string>()
    const good = server({ seen })
    let fail = true
    const send = async (rows: CampaignContactRow[]) => {
      if (fail && rows[0].number === '1002') throw new Error('network')
      return good.send(rows)
    }
    const records = recordsOf(4)
    const first = await runImport(records, send, { chunkSize: 2 })
    expect(first.failure?.firstUnsent).toBe(2)
    fail = false
    const second = await runImport(records.slice(first.failure?.firstUnsent), send, { chunkSize: 2 })
    const merged = mergeSummaries({ ...first, failure: undefined }, second)
    expect(merged).toMatchObject({ imported: 4, duplicates: 0, rejected: 0 })
    expect(merged.entries.map((e) => e.line)).toEqual([2, 3, 4, 5])
    expect(merged.failure).toBeUndefined()
  })

  it('handles the size of a real file', async () => {
    const { send } = server()
    const summary = await runImport(recordsOf(30000), send)
    expect(summary.imported).toBe(30000)
  })
})

describe('the rejected rows file', () => {
  it('holds only the refused rows, with where and why in front of the original cells', async () => {
    const table = parseCsv('号码,姓名\n138,"王, 伟"\nbad,Li\n')
    const records = buildRecords(table, [{ kind: 'number' }, { kind: 'slot', name: 'name' }])
    const { send } = server({ reject: (row) => (row.number === 'bad' ? 'NUMBER_INVALID' : undefined) })
    const summary = await runImport(records, send)
    const out = parseCsv(rejectedCsv(table.header, summary.entries))
    expect(out.header).toEqual(['line', 'code', 'detail', '号码', '姓名'])
    expect(out.rows.map((r) => r.cells)).toEqual([['3', 'NUMBER_INVALID', '', 'bad', 'Li']])
  })
})
