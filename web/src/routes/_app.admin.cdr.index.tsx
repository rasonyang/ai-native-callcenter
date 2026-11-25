import { Link, createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { useEffect, useRef, useState } from 'react'
import { Check, Copy, CopyX, Loader2, Play, Square } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ApiError } from '@/lib/api'
import { copyText } from '@/lib/clipboard'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  cdrQuery, formatDuration, ledgerApi, recordingAudioUrl, useCDRs,
  type CDRStatus, type MissedReason,
} from '@/lib/ledger'

/** The finished-call ledger, filterable, newest first. */
export const Route = createFileRoute('/_app/admin/cdr/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: CDRExplorer,
})

const PAGE_SIZE = 50
const STATUSES: Array<CDRStatus | ''> = ['', 'ANSWERED', 'NO_ANSWER', 'BUSY', 'FAILED']

function CDRExplorer() {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState('')
  // What the box holds, and the number the list is filtered by. They differ
  // only while the box holds a call id: that opens the call and filters nothing.
  const [box, setBox] = useState('')
  const [numberQuery, setNumberQuery] = useState('')
  const [page, setPage] = useState(0)
  const lookup = useCallLookup()
  const copier = useRowCopy()

  const { data, isPending, isError, error } = useCDRs({
    status: status || undefined,
    number: numberQuery || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })

  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const player = useRowPlayer()
  const timeFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })

  return (
    <>
      <PageHeader
        title={t('nav.cdr')}
        description={t('cdr.hint')}
        actions={
          <div className="flex items-center gap-2">
            <Input
              className="h-8 w-44"
              placeholder={t('cdr.filterNumber')}
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
                setNumberQuery(value)
                setPage(0)
              }}
              onKeyDown={(e) => {
                // Asks again for the id already in the box, after a failure.
                const callId = e.key === 'Enter' ? callIdIn(box) : undefined
                if (callId) lookup.open(callId)
              }}
            />
            <Select
              value={status}
              onChange={(next) => {
                setStatus(next)
                setPage(0)
              }}
              options={STATUSES.map((s) => ({
                value: s,
                label: s === '' ? t('cdr.allStatuses') : t(`cdr.statuses.${s}`),
              }))}
            />
          </div>
        }
      />

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

      <DataTable>
        <THead>
          <Th>{t('cdr.startedAt')}</Th>
          <Th>{t('cdr.from')}</Th>
          <Th>{t('cdr.to')}</Th>
          <Th>{t('cdr.journey')}</Th>
          <Th>{t('cdr.status')}</Th>
          <Th align="right">{t('cdr.duration')}</Th>
          <Th align="right">{t('cdr.playColumn')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={7}>{t('cdr.noCalls')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.callId}>
              <Td className="tabular">
                <Link
                  to="/admin/cdr/$callId"
                  params={{ callId: row.callId }}
                  className="text-foreground hover:text-primary"
                >
                  {timeFormat.format(new Date(row.startedAt))}
                </Link>
              </Td>
              <Td className="tabular">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-29">
                  {row.fromNumber || '—'}
                </CellText>
              </Td>
              <Td className="tabular">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-28">
                  {row.toNumber || '—'}
                </CellText>
              </Td>
              <Td className="text-xs text-muted-foreground">
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
              {/* No left padding, and the row actions' icon size: the two
                  buttons then take about the width the English header alone
                  took (76px), which is all a 1024px card has left when the
                  numbers are long and every enum cell is at its widest. The
                  duration cell's own padding keeps the gap. */}
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
