import { Link, createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { Check, Copy, CopyX, Download, Loader2, Play, Square } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ApiError, type ServerFile } from '@/lib/api'
import { copyText } from '@/lib/clipboard'
import { downloadBlob } from '@/lib/download'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  cdrQuery, formatDuration, ledgerApi, recordingAudioUrl, useCDRs,
  type CDRStatus, type MissedReason,
} from '@/lib/ledger'
import { dayWindowIn, formatInZone } from '@/lib/zoned-day'

/**
 * The finished-call ledger, filterable, newest first.
 *
 * Calls are dated in the deployment's time zone (`CDRList.timeZone`), never
 * the browser's, and a day in the date filter is a day in that zone: the
 * export writes the same zone, so a filtered day and the file exported from it
 * hold the same calls. The zone is known from the first answer, so the date
 * filter waits for it rather than guessing.
 *
 * The totals line and the export cover every call the filters select, not the
 * page on screen; the export is the server's file (exportCDRs), which needs a
 * date range.
 *
 * The number box matches either end of a call and opens a call whose whole id
 * is pasted into it (#714); the caller and called boxes each match one end.
 */
export const Route = createFileRoute('/_app/admin/cdr/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: CDRExplorer,
})

const PAGE_SIZE = 50
const COLUMNS = 8
const STATUSES: Array<CDRStatus | ''> = ['', 'ANSWERED', 'NO_ANSWER', 'BUSY', 'FAILED']

/** What the operator typed or picked. Days are `YYYY-MM-DD`, as a date input holds them. */
interface Filters {
  firstDay: string
  lastDay: string
  /** Either end's number, from the number box; never a call id, which filters nothing. */
  number: string
  fromNumber: string
  toNumber: string
  status: string
  billSecMin: string
  billSecMax: string
}

const NO_FILTERS: Filters = {
  firstDay: '',
  lastDay: '',
  number: '',
  fromNumber: '',
  toNumber: '',
  status: '',
  billSecMin: '',
  billSecMax: '',
}

/** The largest value the server's integer filter takes; no call is billed for longer. */
const MAX_SECONDS = 2_147_483_647

/** A billed-seconds bound as the contract takes it: a whole number of seconds, or none. */
function secondsOf(value: string): number | undefined {
  const trimmed = value.trim()
  if (!/^\d+$/.test(trimmed)) return undefined
  return Math.min(Number(trimmed), MAX_SECONDS)
}

function CDRExplorer() {
  const { t, i18n } = useTranslation()
  const [filters, setFilters] = useState<Filters>(NO_FILTERS)
  // What the number box holds. It differs from filters.number only while the
  // box holds a call id: that opens the call and filters nothing.
  const [box, setBox] = useState('')
  const [page, setPage] = useState(0)
  const [zone, setZone] = useState<string>()
  const exporter = useServerExport()
  const lookup = useCallLookup()
  const copier = useRowCopy()

  // The calendar days become instants only in the deployment's zone, which
  // the first answer names; until then no day can be picked.
  const range = zone ? dayWindowIn(filters.firstDay, filters.lastDay, zone) : {}
  // The list, the totals and the export all read this one selection.
  const selection = {
    status: filters.status || undefined,
    number: filters.number.trim() || undefined,
    fromNumber: filters.fromNumber.trim() || undefined,
    toNumber: filters.toNumber.trim() || undefined,
    billSecMin: secondsOf(filters.billSecMin),
    billSecMax: secondsOf(filters.billSecMax),
    from: range.from,
    to: range.to,
  }

  const { data, isPending, isError, error } = useCDRs({
    ...selection,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })

  // Kept once known, so a failed answer later does not drop the date filter.
  if (data?.timeZone && data.timeZone !== zone) setZone(data.timeZone)

  const update = (patch: Partial<Filters>) => {
    setFilters((current) => ({ ...current, ...patch }))
    setPage(0)
    exporter.dismiss()
  }

  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const player = useRowPlayer()
  const count = new Intl.NumberFormat(i18n.language)

  const { from, to } = selection
  // Both bounds are required by the export; a range that ends before it
  // starts holds no call. (Same-format UTC instants compare as strings.)
  const canExport = Boolean(from && to && from < to)
  const needsRangeId = useId()
  const isExporting = exporter.state.phase === 'running'
  const startExport = () => {
    if (!from || !to) return
    const language = i18n.resolvedLanguage?.startsWith('zh') ? 'zh' : 'en'
    // The server names the file; this is only for a server that did not.
    const fallbackName = `calls-${filters.firstDay.replaceAll('-', '')}-${filters.lastDay.replaceAll('-', '')}.csv`
    void exporter.start(
      (signal) => ledgerApi.exportCDRs({ ...selection, from, to, language }, signal),
      fallbackName,
    )
  }

  const dateLabel = zone ? t('cdr.startDateIn', { zone }) : t('cdr.startDate')

  return (
    <>
      <PageHeader
        title={t('nav.cdr')}
        description={t('cdr.hint')}
        actions={
          <>
            {!canExport && (
              <span id={needsRangeId} className="text-xs text-muted-foreground">
                {t('cdr.export.needsRange')}
              </span>
            )}
            <Button
              variant="outline"
              disabled={!canExport || isExporting}
              title={t('cdr.export.hint')}
              aria-describedby={canExport ? undefined : needsRangeId}
              onClick={startExport}
            >
              {isExporting ? <Loader2 className="animate-spin" /> : <Download />}
              {isExporting ? t('cdr.export.running') : t('cdr.export.start')}
            </Button>
          </>
        }
      />

      <div className="mb-3 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div role="group" aria-label={dateLabel} className="flex items-center gap-1.5">
          <span className="text-xs whitespace-nowrap text-muted-foreground">{dateLabel}</span>
          <Input
            type="date"
            className="h-8 w-36 tabular"
            aria-label={t('cdr.firstDay')}
            disabled={!zone}
            max={filters.lastDay || undefined}
            value={filters.firstDay}
            onChange={(e) => update({ firstDay: e.target.value })}
          />
          <span className="text-muted-foreground">–</span>
          <Input
            type="date"
            className="h-8 w-36 tabular"
            aria-label={t('cdr.lastDay')}
            disabled={!zone}
            min={filters.firstDay || undefined}
            value={filters.lastDay}
            onChange={(e) => update({ lastDay: e.target.value })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Input
            className="h-8 w-36"
            placeholder={t('cdr.filterNumber')}
            aria-label={t('cdr.filterNumber')}
            title={t('cdr.filterNumberHint')}
            value={box}
            onChange={(e) => {
              const value = e.target.value
              setBox(value)
              const callId = callIdIn(value)
              if (callId) {
                lookup.open(callId)
                return
              }
              lookup.reset()
              update({ number: value })
            }}
            onKeyDown={(e) => {
              // Asks again for the id already in the box, after a failure.
              const callId = e.key === 'Enter' ? callIdIn(box) : undefined
              if (callId) lookup.open(callId)
            }}
          />
          <Input
            className="h-8 w-36"
            placeholder={t('cdr.filterFrom')}
            aria-label={t('cdr.filterFrom')}
            value={filters.fromNumber}
            onChange={(e) => update({ fromNumber: e.target.value })}
          />
          <Input
            className="h-8 w-36"
            placeholder={t('cdr.filterTo')}
            aria-label={t('cdr.filterTo')}
            value={filters.toNumber}
            onChange={(e) => update({ toNumber: e.target.value })}
          />
        </div>
        <div className="w-36">
          <Select
            value={filters.status}
            ariaLabel={t('cdr.status')}
            onChange={(next) => update({ status: next })}
            options={STATUSES.map((s) => ({
              value: s,
              label: s === '' ? t('cdr.allStatuses') : t(`cdr.statuses.${s}`),
            }))}
          />
        </div>
        <div className="flex items-center gap-1.5">
          <span className="text-xs whitespace-nowrap text-muted-foreground">{t('cdr.billedFilter')}</span>
          <Input
            type="number"
            inputMode="numeric"
            min={0}
            step={1}
            className="h-8 w-20 tabular"
            placeholder={t('cdr.min')}
            aria-label={t('cdr.billedMin')}
            value={filters.billSecMin}
            onChange={(e) => update({ billSecMin: e.target.value })}
          />
          <span className="text-muted-foreground">–</span>
          <Input
            type="number"
            inputMode="numeric"
            min={0}
            step={1}
            className="h-8 w-20 tabular"
            placeholder={t('cdr.max')}
            aria-label={t('cdr.billedMax')}
            value={filters.billSecMax}
            onChange={(e) => update({ billSecMax: e.target.value })}
          />
        </div>
      </div>

      {lookup.failure && (
        <p role="alert" className="mb-3 text-xs wrap-anywhere" style={{ color: 'var(--state-breach)' }}>
          {lookup.failure.error instanceof ApiError && lookup.failure.error.status === 404
            ? t('cdr.callNotFound', { callId: lookup.failure.callId })
            : t('cdr.callLookupFailed', {
                callId: lookup.failure.callId,
                reason: describeError(lookup.failure.error, t),
              })}
        </p>
      )}

      {exporter.state.phase === 'failed' && (
        <p role="alert" className="mb-3 text-xs" style={{ color: 'var(--state-breach)' }}>
          {t('cdr.export.failed', { reason: exportRefusal(exporter.state.error, t) })}
        </p>
      )}

      {data && (
        <p className="mb-2 text-xs text-muted-foreground tabular">
          {t('cdr.totals', {
            count: data.total,
            calls: count.format(data.total),
            billed: formatDuration(data.billSecTotal),
            seconds: count.format(data.billSecTotal),
          })}
        </p>
      )}

      <DataTable>
        <THead>
          <Th>{t('cdr.startedAt')}</Th>
          <Th>{t('cdr.from')}</Th>
          <Th>{t('cdr.to')}</Th>
          {/* The path a call took is on its own page too; below 1280 the
              row keeps the start, both numbers and both durations instead. */}
          <Th className="hidden xl:table-cell">{t('cdr.journey')}</Th>
          <Th>{t('cdr.status')}</Th>
          <Th align="right">{t('cdr.duration')}</Th>
          <Th align="right" className="whitespace-nowrap">{t('cdr.billed')}</Th>
          <Th align="right">{t('cdr.playColumn')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={COLUMNS}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={COLUMNS}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={COLUMNS}>{t('cdr.noCalls')}</TableMessage>
          )}
          {data &&
            rows.map((row) => (
              <Tr key={row.callId}>
                <Td className="tabular">
                  {/* One line from 1280 up, where the card has room for it;
                      a floor of the value's own width, never more. */}
                  <Link
                    to="/admin/cdr/$callId"
                    params={{ callId: row.callId }}
                    className="block text-foreground hover:text-primary xl:min-w-upto-40"
                  >
                    <StartedAt value={formatInZone(row.startedAt, data.timeZone)} />
                  </Link>
                </Td>
                <Td className="tabular">
                  <CellText maxWidth="max-w-40" minWidth="min-w-upto-29">
                    {row.fromNumber || '—'}
                  </CellText>
                </Td>
                <Td className="tabular">
                  {/* 104px at 1024 holds a national mobile number with room to
                      spare; the card has room for E.164's 112 from 1280 up. */}
                  <CellText maxWidth="max-w-40" minWidth="min-w-upto-26 xl:min-w-upto-28">
                    {row.toNumber || '—'}
                  </CellText>
                </Td>
                <Td className="hidden text-xs text-muted-foreground xl:table-cell">
                  {row.legs.map((leg) => t(`cdr.legs.${leg.kind}`)).join(' → ') || '—'}
                </Td>
                <Td>
                  <StatusCell
                    status={row.status}
                    missedReason={row.missedReason}
                    isContained={row.isContained}
                    hasRecording={row.hasRecording}
                  />
                </Td>
                <Td align="right" className="tabular">
                  {formatDuration(row.totalSec)}
                </Td>
                <Td align="right" className="tabular">
                  {formatDuration(row.billSec)}
                </Td>
                {/* No left padding, and the row actions' icon size: the two
                    buttons then take about the width the English header alone
                    took (76px), which is all a 1024px card has left when the
                    numbers are long and every enum cell is at its widest. The
                    billed cell's own padding keeps the gap. */}
                <Td align="right" className="pl-0">
                  <span className="flex items-center justify-end gap-1">
                    <CopyIdButton callId={row.callId} copier={copier} />
                    {/* Held empty without a recording, so the copy buttons line up. */}
                    <span className="size-7">
                      {row.hasRecording && <PlayCell callId={row.callId} player={player} />}
                    </span>
                  </span>
                </Td>
              </Tr>
            ))}
        </TBody>
      </DataTable>

      {total > PAGE_SIZE && (
        <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
          <span className="tabular">
            {t('cdr.pageOf', { page: page + 1, pages, total })}
          </span>
          <span className="flex gap-2">
            <Button size="sm" variant="ghost" disabled={page === 0} onClick={() => setPage(page - 1)}>
              {t('common.previous')}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={page + 1 >= pages}
              onClick={() => setPage(page + 1)}
            >
              {t('common.next')}
            </Button>
          </span>
        </div>
      )}
    </>
  )
}

/**
 * The call id in the filter box, when the box's whole content (spaces and a
 * line break around it, as a log line gives it) has the shape of a UUID; it is
 * returned in lower case, the form the server files ids in. Anything else,
 * half an id included, is a number to filter by.
 */
function callIdIn(text: string): string | undefined {
  const trimmed = text.trim()
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(trimmed)
    ? trimmed.toLowerCase()
    : undefined
}

type CallLookup = {
  /** The id the box holds and the server did not give a call for, and why. */
  failure: { callId: string; error: unknown } | null
  /** Looks the call up and opens its page. */
  open: (callId: string) => void
  /** The box holds something else now: drop the lookup and what it said. */
  reset: () => void
}

/**
 * A call id pasted into the filter box opens that call (#714). The list has no
 * id filter (`listCDRs` matches numbers), so the id is looked up on its own
 * (`getCDR`), through the same query the call's page reads, which then opens
 * on data already loaded. A call that does not exist leaves the list and its
 * filters as they were and says so; it is never sent as a number, which would
 * show an empty table as if the filter had matched nothing.
 */
function useCallLookup(): CallLookup {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [failure, setFailure] = useState<CallLookup['failure']>(null)
  // The id whose answer still counts: an answer about an id the box no longer
  // holds, or one that lands after the page was left, opens nothing.
  const pending = useRef<string | null>(null)
  useEffect(
    () => () => {
      pending.current = null
    },
    [],
  )

  const open = (callId: string) => {
    pending.current = callId
    setFailure(null)
    queryClient.fetchQuery(cdrQuery(callId)).then(
      ({ cdr }) => {
        if (pending.current !== callId) return
        pending.current = null
        void navigate({ to: '/admin/cdr/$callId', params: { callId: cdr.callId } })
      },
      (error: unknown) => {
        if (pending.current !== callId) return
        pending.current = null
        setFailure({ callId, error })
      },
    )
  }
  const reset = () => {
    pending.current = null
    setFailure(null)
  }
  return { failure, open, reset }
}

type RowCopier = {
  /** The row whose copy said something last, and what. */
  said: { callId: string; isCopied: boolean } | null
  copy: (callId: string) => void
}

/**
 * Copying a row's call id. "Copied" means a write that succeeded, and it goes
 * back to the plain button after two seconds, so a row copied a while ago
 * does not still claim it; a refused copy says so until the next attempt.
 * Only the row copied last says anything.
 */
function useRowCopy(): RowCopier {
  const [said, setSaid] = useState<RowCopier['said']>(null)

  useEffect(() => {
    if (!said?.isCopied) return
    const timer = setTimeout(() => setSaid(null), 2_000)
    return () => clearTimeout(timer)
  }, [said])

  return {
    said,
    copy: (callId) => {
      // Called inside the click, without an await before it: the copy over
      // plain HTTP is refused outside a user gesture.
      void copyText(callId).then((isCopied) => setSaid({ callId, isCopied }))
    },
  }
}

/**
 * An icon button: the list has no id column, since a 36-character id has no
 * room at 1024px and nobody reads it there. It copies without opening the call.
 */
function CopyIdButton({ callId, copier }: { callId: string; copier: RowCopier }) {
  const { t } = useTranslation()
  const said = copier.said?.callId === callId ? copier.said : null
  const label = !said
    ? t('agent.copyCallId')
    : said.isCopied
      ? t('agent.callIdCopied')
      : t('cdr.callIdCopyFailed')

  return (
    <Button
      size="icon-sm"
      variant="ghost"
      title={label}
      aria-label={label}
      onClick={(event) => {
        // Never a click on the row: copying does not open the call.
        event.stopPropagation()
        copier.copy(callId)
      }}
    >
      {!said ? <Copy /> : said.isCopied ? <Check /> : <CopyX />}
    </Button>
  )
}

/**
 * Why the server would not make the file. A refusal with a rule (a window
 * longer than an export covers) reads as that rule, with its numbers; anything
 * else as any other error.
 */
function exportRefusal(error: unknown, t: ReturnType<typeof useTranslation>['t']): string {
  if (error instanceof ApiError && error.code === 'VALIDATION_FAILED' && typeof error.params.rule === 'string') {
    return t(`errors.rules.${error.params.rule}`, { ...error.params, defaultValue: describeError(error, t) })
  }
  return describeError(error, t)
}

type ExportState = { phase: 'idle' } | { phase: 'running' } | { phase: 'failed'; error: unknown }

/**
 * One server-made file at a time. It is handed over only once the whole
 * answer is in, under the name the server gave it; a refusal is kept to be
 * shown until the filter changes or the next start. Leaving the screen
 * cancels: nothing is downloaded behind the reader's back.
 */
function useServerExport() {
  const [state, setState] = useState<ExportState>({ phase: 'idle' })
  const running = useRef<AbortController | null>(null)

  useEffect(
    () => () => {
      running.current?.abort()
      running.current = null
    },
    [],
  )

  const start = useCallback(
    async (fetchFile: (signal: AbortSignal) => Promise<ServerFile>, fallbackName: string) => {
      running.current?.abort()
      const controller = new AbortController()
      running.current = controller
      setState({ phase: 'running' })
      try {
        const file = await fetchFile(controller.signal)
        if (running.current !== controller) return
        running.current = null
        downloadBlob(file.fileName ?? fallbackName, file.blob)
        setState({ phase: 'idle' })
      } catch (error) {
        if (running.current !== controller) return
        running.current = null
        setState({ phase: 'failed', error })
      }
    },
    [],
  )

  const dismiss = useCallback(
    () => setState((current) => (current.phase === 'failed' ? { phase: 'idle' } : current)),
    [],
  )

  return { state, start, dismiss }
}

/**
 * A start as `YYYY-MM-DD HH:MM:SS`. Each half stays whole, so where the card
 * is narrow the time goes under the date rather than the date breaking.
 */
function StartedAt({ value }: { value: string }) {
  const [date, time] = value.split(' ')
  return (
    <>
      <span className="whitespace-nowrap">{date}</span>
      {time && (
        <>
          {' '}
          <span className="whitespace-nowrap">{time}</span>
        </>
      )}
    </>
  )
}

type RowPlayer = {
  playingCallId: string | null
  loadingCallId: string | null
  toggle: (callId: string) => void
}

/**
 * One shared audio element behind every row's play button: starting a row
 * stops whichever other row was playing, and the recording id is looked up
 * on first use — the list itself only knows hasRecording.
 */
function useRowPlayer(): RowPlayer {
  const audioRef = useRef<HTMLAudioElement | null>(null)
  const [playingCallId, setPlayingCallId] = useState<string | null>(null)
  const [loadingCallId, setLoadingCallId] = useState<string | null>(null)

  useEffect(() => {
    const audio = new Audio()
    audio.addEventListener('ended', () => setPlayingCallId(null))
    audio.addEventListener('error', () => setPlayingCallId(null))
    audioRef.current = audio
    return () => {
      audio.pause()
      audioRef.current = null
    }
  }, [])

  const toggle = (callId: string) => {
    const audio = audioRef.current
    if (!audio) return
    if (playingCallId === callId) {
      audio.pause()
      setPlayingCallId(null)
      return
    }
    setLoadingCallId(callId)
    ledgerApi
      .recordingsByCall(callId)
      .then(({ items }) => {
        if (audioRef.current !== audio) return
        const recording = items[0]
        if (!recording) {
          setPlayingCallId(null)
          return
        }
        audio.src = recordingAudioUrl(recording.id)
        setPlayingCallId(callId)
        return audio.play()
      })
      .catch(() => setPlayingCallId(null))
      .finally(() => setLoadingCallId((current) => (current === callId ? null : current)))
  }

  return { playingCallId, loadingCallId, toggle }
}

function PlayCell({ callId, player }: { callId: string; player: RowPlayer }) {
  const { t } = useTranslation()
  const isPlaying = player.playingCallId === callId
  const isLoading = player.loadingCallId === callId

  return (
    <Button
      size="icon-sm"
      variant="ghost"
      title={isPlaying ? t('cdr.stop') : t('cdr.play')}
      aria-label={isPlaying ? t('cdr.stop') : t('cdr.play')}
      onClick={() => player.toggle(callId)}
    >
      {isLoading ? (
        <Loader2 className="animate-spin" />
      ) : isPlaying ? (
        <Square className="fill-current" />
      ) : (
        <Play />
      )}
    </Button>
  )
}

// Every status the contract can send, so a new one is a type error here
// rather than a grey dot nobody notices.
const STATUS_COLOR: Record<CDRStatus, string> = {
  ANSWERED: 'var(--state-available)',
  FAILED: 'var(--state-breach)',
  BUSY: 'var(--state-ringing)',
  NO_ANSWER: 'var(--state-aux)',
}

function StatusCell(props: {
  status: CDRStatus
  missedReason?: MissedReason
  isContained: boolean
  hasRecording: boolean
}) {
  const { t } = useTranslation()
  return (
    <span className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs">
      <span
        className="size-2 rounded-full"
        style={{ backgroundColor: STATUS_COLOR[props.status] ?? 'var(--state-offline)' }}
      />
      {props.missedReason ? t(`cdr.missedReasons.${props.missedReason}`) : t(`cdr.statuses.${props.status}`)}
      {props.isContained && (
        <span className="rounded-full border px-1.5 py-px text-[11px] text-muted-foreground">
          {t('cdr.contained')}
        </span>
      )}
      {props.hasRecording && (
        <span className="rounded-full border px-1.5 py-px text-[11px] text-muted-foreground">
          {t('cdr.recorded')}
        </span>
      )}
    </span>
  )
}
