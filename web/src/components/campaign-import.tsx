import { Download, Upload } from 'lucide-react'
import { Dialog } from 'radix-ui'
import { useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  buildRecords, emptySummary, guessRoles, mappingProblem, mergeSummaries, rejectedCsv, roleFromValue,
  roleValue, runImport, type ColumnRole, type ImportSummary,
} from '@/lib/campaign-import'
import { IMPORT_CHUNK_SIZE, campaignApi } from '@/lib/campaigns'
import { parseCsv } from '@/lib/csv'
import { describeError } from '@/lib/errors'
import { readXlsxWorkbook, type XlsxSheet } from '@/lib/xlsx'

const PREVIEW_ROWS = 5
const SHOWN_PROBLEMS = 200

/** The file's text. FileReader decodes as UTF-8, which is what the importer asks for. */
function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener('load', () => resolve(String(reader.result ?? '')))
    reader.addEventListener('error', () => reject(reader.error))
    reader.readAsText(file, 'utf-8')
  })
}

/**
 * Whether the file is an Excel workbook rather than one of the text formats.
 * The extension is what the operator chose; the browser's idea of an `.xlsx`
 * content type is not reliable enough to decide on.
 */
function isWorkbook(file: File): boolean {
  return /\.xlsx$/i.test(file.name)
}

/** Hands the operator a file made in the browser. */
function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/csv;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}

/**
 * What the operator chose, parsed. A text file is one sheet; a workbook is one
 * per worksheet, in the file's own order.
 */
interface PickedFile {
  name: string
  sheets: XlsxSheet[]
  /** Which sheet is being mapped. Always the first one for a text file. */
  sheetAt: number
  /** The bytes are not UTF-8, so some text may be garbled (text files only). */
  badEncoding: boolean
  /** A quote that never closes, so rows after it may be one row (text files only). */
  unterminatedQuote: boolean
}

/**
 * Contact import. The file is read and parsed here — a text file by the CSV
 * reader, a workbook by the Excel one — the columns are mapped by the
 * operator, and the rows go to the import operation in requests of at most
 * 5000. What comes back, row by row, is shown with the line each row had in
 * the file, and the refused ones can be downloaded to fix and send again.
 */
export function CampaignImportDialog({
  campaignId,
  flowFields,
  open,
  onOpenChange,
  onImported,
}: {
  campaignId: string
  /**
   * The business-data names the campaign's flow reads (the campaign's
   * flowFields). Undefined means the server could not say, which is not the same
   * as an empty list: the field name is then typed, as it was before the server
   * offered any.
   */
  flowFields: string[] | undefined
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called whenever rows were accepted, so counters can be refreshed. */
  onImported: () => void
}) {
  const { t } = useTranslation()
  const fileInput = useRef<HTMLInputElement>(null)
  const [picked, setPicked] = useState<PickedFile | null>(null)
  const [isReading, setIsReading] = useState(false)
  const [unreadable, setUnreadable] = useState(false)
  const [roles, setRoles] = useState<ColumnRole[]>([])
  const [isRunning, setIsRunning] = useState(false)
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null)
  const [summary, setSummary] = useState<ImportSummary | null>(null)

  const fileName = picked?.name ?? ''
  const table = picked ? picked.sheets[picked.sheetAt].table : null
  const problem = mappingProblem(roles)
  // Every column is the number, the zone, one of the names the flow reads, or
  // ignored. Three cases for the names: known and non-empty, the dropdown of
  // them; known and empty, none, since any name typed would be SLOT_UNKNOWN on
  // every row; unknown (the server could not read the flow), the free-typed
  // "Flow field" as before, with the server's per-row SLOT_UNKNOWN as the check.
  const isFieldNameTyped = flowFields === undefined
  const hasNoFlowFields = flowFields !== undefined && flowFields.length === 0
  const roleOptions = useMemo(
    () => [
      { value: 'number', label: t('campaign.import.roles.number') },
      { value: 'timeZone', label: t('campaign.import.roles.timeZone') },
      ...(flowFields ?? []).map((field) => ({ value: `slot:${field}`, label: field })),
      ...(flowFields === undefined
        ? [{ value: 'slot', label: t('campaign.import.roles.slot') }]
        : []),
      { value: 'ignore', label: t('campaign.import.roles.ignore') },
    ],
    [flowFields, t],
  )
  const records = useMemo(() => (table ? buildRecords(table, roles) : []), [table, roles])
  const badShape = records.filter((r) => r.localCode).length

  // Which read is the current one. Reading a workbook takes long enough for the
  // operator to choose another file (the input is hidden but still reachable
  // from the keyboard) or to close the dialog, and a slower read that finishes
  // late must not put its file back on screen.
  const readSeq = useRef(0)

  const reset = () => {
    readSeq.current++
    setPicked(null)
    setIsReading(false)
    setUnreadable(false)
    setRoles([])
    setSummary(null)
    setProgress(null)
  }

  const pick = async (file: File | undefined) => {
    if (!file) return
    reset()
    const mine = readSeq.current
    if (isWorkbook(file)) {
      setIsReading(true)
      try {
        const sheets = await readXlsxWorkbook(file)
        if (readSeq.current !== mine) return
        // A cover sheet or a blank first tab is common; open on the first
        // sheet that has something to map. The picker reaches the others.
        const sheetAt = Math.max(0, sheets.findIndex((sheet) => sheet.table.header.length > 0))
        setPicked({ name: file.name, sheets, sheetAt, badEncoding: false, unterminatedQuote: false })
        setRoles(guessRoles(sheets[sheetAt]?.table.header ?? [], flowFields ?? []))
      } catch {
        // A workbook this reader cannot open: a corrupt archive, a renamed
        // `.xls`, or a `.xlsx` whose bytes are something else entirely.
        if (readSeq.current === mine) setUnreadable(true)
      } finally {
        if (readSeq.current === mine) setIsReading(false)
      }
      return
    }
    const text = await readText(file)
    if (readSeq.current !== mine) return
    const parsed = parseCsv(text)
    setPicked({
      name: file.name,
      sheets: [{ name: file.name, table: parsed }],
      sheetAt: 0,
      badEncoding: text.includes('�'),
      unterminatedQuote: parsed.hasUnterminatedQuote,
    })
    setRoles(guessRoles(parsed.header, flowFields ?? []))
  }

  /** Another worksheet of the same workbook: its own header, its own guess. */
  const chooseSheet = (at: number) => {
    if (!picked) return
    setPicked({ ...picked, sheetAt: at })
    setRoles(guessRoles(picked.sheets[at]?.table.header ?? [], flowFields ?? []))
  }

  const run = async (from: number, previous: ImportSummary) => {
    setIsRunning(true)
    const result = await runImport(
      records.slice(from),
      (rows) => campaignApi.importContacts(campaignId, rows),
      { onProgress: (done) => setProgress({ done: from + done, total: records.length }) },
    )
    // The run counted from its own first record; the file's index is `from` further on.
    if (result.failure) result.failure.firstUnsent += from
    const merged = mergeSummaries(previous, result)
    setSummary(merged)
    setIsRunning(false)
    if (merged.imported > 0) onImported()
  }

  const problems = (summary?.entries ?? []).filter((e) => e.status !== 'IMPORTED')

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (isRunning) return
        if (!next) reset()
        onOpenChange(next)
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex max-h-[85vh] w-[820px] max-w-[95vw] -translate-x-1/2 -translate-y-1/2 flex-col rounded-md border bg-card p-4 shadow-md">
          <Dialog.Title className="text-base font-medium">{t('campaign.import.title')}</Dialog.Title>
          <Dialog.Description className="mt-1 text-xs text-muted-foreground">
            {t('campaign.import.hint', { max: IMPORT_CHUNK_SIZE })}
          </Dialog.Description>

          <div className="mt-4 min-h-0 flex-1 space-y-4 overflow-y-auto">
            <div className="flex items-center gap-3">
              <input
                ref={fileInput}
                type="file"
                accept=".csv,.tsv,.txt,.xlsx,text/csv,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
                className="sr-only"
                aria-label={t('campaign.import.file')}
                onChange={(e) => void pick(e.target.files?.[0])}
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={isRunning || isReading}
                onClick={() => fileInput.current?.click()}
              >
                <Upload />
                {t('campaign.import.choose')}
              </Button>
              <span className="text-xs text-muted-foreground">
                {isReading
                  ? t('campaign.import.reading')
                  : table
                    ? t('campaign.import.parsed', {
                        name: fileName,
                        count: table.rows.length,
                        columns: table.header.length,
                      })
                    : t('campaign.import.noFile')}
              </span>
            </div>

            {unreadable && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                {t('campaign.import.xlsxFailed')}
              </p>
            )}
            {picked && picked.sheets.length > 1 && !summary && (
              <div className="flex items-center gap-2">
                <span className="text-xs text-muted-foreground">{t('campaign.import.sheet')}</span>
                <div className="w-56">
                  <Select
                    ariaLabel={t('campaign.import.sheetOf')}
                    value={String(picked.sheetAt)}
                    onChange={(value) => chooseSheet(Number(value))}
                    options={picked.sheets.map((sheet, index) => ({
                      value: String(index),
                      label: sheet.name,
                    }))}
                  />
                </div>
              </div>
            )}
            {table && table.header.length === 0 && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                {t('campaign.import.empty')}
              </p>
            )}
            {picked?.badEncoding && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                {t('campaign.import.badEncoding')}
              </p>
            )}
            {picked?.unterminatedQuote && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                {t('campaign.import.unterminatedQuote')}
              </p>
            )}

            {table && table.header.length > 0 && !summary && (
              <>
                <div>
                  <h3 className="mb-1 text-sm font-medium">{t('campaign.import.mapping')}</h3>
                  <p className="mb-2 text-xs text-muted-foreground">{t('campaign.import.mappingHint')}</p>
                  {hasNoFlowFields && (
                    <p className="mb-2 text-xs text-muted-foreground">
                      {t('campaign.import.noFlowFields')}
                    </p>
                  )}
                  <DataTable>
                    <THead>
                      {table.header.map((name, index) => (
                        <Th key={index} className="min-w-36 align-top normal-case">
                          <div className="space-y-1 py-1.5">
                            <div className="truncate text-foreground" title={name}>
                              {name || t('campaign.import.unnamed', { n: index + 1 })}
                            </div>
                            <Select
                              ariaLabel={t('campaign.import.roleOf', { name })}
                              // A name typed below is not one of the offered options,
                              // so the control keeps saying "Flow field" for it.
                              value={
                                roles[index]?.kind === 'slot' && isFieldNameTyped
                                  ? 'slot'
                                  : roleValue(roles[index])
                              }
                              onChange={(value) =>
                                setRoles(
                                  roles.map((r, i): ColumnRole => {
                                    if (i === index) {
                                      const role = roleFromValue(value)
                                      // The free-name choice starts from the header,
                                      // which is usually the field's own name.
                                      return role.kind === 'slot' && role.name === ''
                                        ? { kind: 'slot', name: name.trim() }
                                        : role
                                    }
                                    // Only one column is the number, and only one the
                                    // zone. Two columns with one field name are
                                    // refused by mappingProblem, not undone here.
                                    return (value === 'number' || value === 'timeZone') &&
                                      r.kind === value
                                      ? { kind: 'ignore' }
                                      : r
                                  }),
                                )
                              }
                              options={roleOptions}
                            />
                            {isFieldNameTyped && roles[index]?.kind === 'slot' && (
                              <Input
                                aria-label={t('campaign.import.slotNameOf', { name })}
                                className="h-7 text-xs"
                                value={(roles[index] as { name: string }).name}
                                onChange={(e) =>
                                  setRoles(
                                    roles.map((r, i): ColumnRole =>
                                      i === index ? { kind: 'slot', name: e.target.value } : r,
                                    ),
                                  )
                                }
                              />
                            )}
                          </div>
                        </Th>
                      ))}
                    </THead>
                    <TBody>
                      {table.rows.slice(0, PREVIEW_ROWS).map((row) => (
                        <Tr key={row.line}>
                          {table.header.map((_, i) => (
                            <Td key={i} className="max-w-48 truncate text-xs">
                              {row.cells[i] ?? ''}
                            </Td>
                          ))}
                        </Tr>
                      ))}
                    </TBody>
                  </DataTable>
                  <p className="mt-1 text-xs text-muted-foreground">
                    {t('campaign.import.previewNote', {
                      shown: Math.min(PREVIEW_ROWS, table.rows.length),
                      count: table.rows.length,
                    })}
                  </p>
                </div>
                {problem && (
                  <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                    {t(`campaign.import.problems.${problem}`)}
                  </p>
                )}
                {badShape > 0 && (
                  <p className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                    {t('campaign.import.badShape', { count: badShape })}
                  </p>
                )}
              </>
            )}

            {isRunning && progress && (
              <p className="tabular text-sm" role="status">
                {t('campaign.import.progress', progress)}
              </p>
            )}

            {summary && (
              <div className="space-y-3" role="status">
                <div className="tabular flex gap-6 text-sm">
                  <span>{t('campaign.import.imported', { count: summary.imported })}</span>
                  <span>{t('campaign.import.duplicates', { count: summary.duplicates })}</span>
                  <span>{t('campaign.import.rejected', { count: summary.rejected })}</span>
                </div>
                {summary.failure && (
                  <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                    {t('campaign.import.failed', {
                      row: summary.failure.firstUnsent + 1,
                      reason: describeError(summary.failure.error, t),
                    })}
                  </p>
                )}
                {problems.length > 0 && (
                  <>
                    <DataTable>
                      <THead>
                        <Th>{t('campaign.import.line')}</Th>
                        <Th>{t('campaign.import.result')}</Th>
                        <Th>{t('campaign.import.number')}</Th>
                        <Th>{t('campaign.import.why')}</Th>
                      </THead>
                      <TBody>
                        {problems.slice(0, SHOWN_PROBLEMS).map((entry) => (
                          <Tr key={entry.line}>
                            <Td className="tabular text-xs">{entry.line}</Td>
                            <Td className="text-xs">{t(`campaign.import.statuses.${entry.status}`)}</Td>
                            <Td className="tabular text-xs">{entry.number ?? entry.cells[0] ?? ''}</Td>
                            <Td className="text-xs text-muted-foreground">
                              {entry.code
                                ? t(`campaign.import.codes.${entry.code}`, {
                                    detail: entry.detail ?? '',
                                    defaultValue: entry.code,
                                  })
                                : t('campaign.import.duplicateNote')}
                            </Td>
                          </Tr>
                        ))}
                      </TBody>
                    </DataTable>
                    {problems.length > SHOWN_PROBLEMS && (
                      <p className="text-xs text-muted-foreground">
                        {t('campaign.import.moreProblems', { count: problems.length - SHOWN_PROBLEMS })}
                      </p>
                    )}
                  </>
                )}
                {summary.rejected > 0 && table && (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() =>
                      download(
                        t('campaign.import.rejectedFile', { name: fileName.replace(/\.[^.]+$/, '') }),
                        rejectedCsv(table.header, summary.entries),
                      )
                    }
                  >
                    <Download />
                    {t('campaign.import.downloadRejected')}
                  </Button>
                )}
              </div>
            )}
          </div>

          <div className="flex justify-end gap-2 pt-4">
            <Dialog.Close asChild>
              <Button type="button" variant="ghost" size="sm" disabled={isRunning}>
                {summary && !summary.failure ? t('common.close') : t('common.cancel')}
              </Button>
            </Dialog.Close>
            {summary?.failure ? (
              <Button
                type="button"
                size="sm"
                disabled={isRunning}
                onClick={() => void run(summary.failure?.firstUnsent ?? 0, { ...summary, failure: undefined })}
              >
                {t('campaign.import.retry')}
              </Button>
            ) : (
              !summary && (
                <Button
                  type="button"
                  size="sm"
                  disabled={isRunning || !table || records.length === 0 || problem !== undefined}
                  onClick={() => void run(0, emptySummary())}
                >
                  {t('campaign.import.start', { count: records.length })}
                </Button>
              )
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
