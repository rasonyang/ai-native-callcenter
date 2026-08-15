import { Link, createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useEffect, useRef, useState } from 'react'
import { Loader2, Play, Square } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  formatDuration, ledgerApi, recordingAudioUrl, useCDRs, type CDRStatus,
} from '@/lib/ledger'

/** The finished-call ledger, filterable, newest first. */
export const Route = createFileRoute('/_app/admin/cdr/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'SUPERVISOR'),
  component: CDRExplorer,
})

const PAGE_SIZE = 50
const STATUSES: Array<CDRStatus | ''> = ['', 'ANSWERED', 'NO_ANSWER', 'BUSY', 'FAILED']

function CDRExplorer() {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState('')
  const [numberQuery, setNumberQuery] = useState('')
  const [page, setPage] = useState(0)

  const { data, isPending, isError, error } = useCDRs({
    status: status || undefined,
    fromNumber: numberQuery || undefined,
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
              value={numberQuery}
              onChange={(e) => {
                setNumberQuery(e.target.value)
                setPage(0)
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

      <DataTable>
        <THead>
          <Th>{t('cdr.startedAt')}</Th>
          <Th>{t('cdr.from')}</Th>
          <Th>{t('cdr.did')}</Th>
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
              <Td className="tabular">{row.fromNumber || '—'}</Td>
              <Td className="tabular">{row.did || '—'}</Td>
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
              <Td align="right">
                {row.hasRecording && <PlayCell callId={row.callId} player={player} />}
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
      size="sm"
      variant="ghost"
      className="size-8 p-0"
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

const STATUS_COLOR: Record<string, string> = {
  ANSWERED: 'var(--state-available)',
  FAILED: 'var(--state-breach)',
  BUSY: 'var(--state-ringing)',
  NO_ANSWER: 'var(--state-aux)',
}

function StatusCell(props: {
  status: string
  missedReason?: string
  isContained: boolean
  hasRecording: boolean
}) {
  const { t } = useTranslation()
  return (
    <span className="flex items-center gap-1.5 text-xs">
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
