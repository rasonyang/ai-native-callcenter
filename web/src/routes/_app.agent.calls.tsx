import { createFileRoute } from '@tanstack/react-router'
import { Headphones } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Fragment, useEffect, useState } from 'react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { RecordingPlayer } from '@/components/recording-player'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  formatDuration, ledgerApi, recordingAudioUrl, useMyCDRs,
  type CDR, type CDRStatus, type RecordingRow,
} from '@/lib/ledger'

/**
 * My calls: every finished call this agent was on, newest first, with what
 * they filed about it.
 *
 * The agent is the session, never a filter — there is no way to ask this page
 * for somebody else's calls. It is deliberately not the CDR explorer: no
 * transcripts, no other people's work. Recordings appear exactly as far as
 * the calls do — the agent replays their own, nobody else's.
 */
export const Route = createFileRoute('/_app/agent/calls')({
  beforeLoad: ({ context }) => requireRole(context.user, 'AGENT'),
  component: MyCallsPage,
})

const PAGE_SIZE = 50
const STATUSES: Array<CDRStatus | ''> = ['', 'ANSWERED', 'NO_ANSWER', 'BUSY', 'FAILED']

function MyCallsPage() {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState('')
  const [numberQuery, setNumberQuery] = useState('')
  const [page, setPage] = useState(0)

  const { data, isPending, isError, error } = useMyCDRs({
    status: status || undefined,
    fromNumber: numberQuery || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })

  const [openCallId, setOpenCallId] = useState<string | null>(null)
  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const timeFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <>
      <PageHeader
        title={t('nav.myCalls')}
        description={t('myCalls.hint')}
        actions={
          <div className="flex items-center gap-2">
            <Input
              className="h-8 w-44"
              placeholder={t('cdr.filterNumber')}
              value={numberQuery}
              onChange={(event) => {
                setNumberQuery(event.target.value)
                setPage(0)
              }}
            />
            <Select
              value={status}
              ariaLabel={t('cdr.status')}
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
          <Th>{t('myCalls.customer')}</Th>
          <Th>{t('myCalls.queue')}</Th>
          <Th>{t('myCalls.disposition')}</Th>
          <Th>{t('myCalls.note')}</Th>
          <Th align="right">{t('myCalls.talkTime')}</Th>
          <Th align="right">{t('cdr.playColumn')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={7}>{t('myCalls.empty')}</TableMessage>
          )}
          {rows.map((row) => (
            <Fragment key={row.callId}>
            <Tr>
              <Td className="tabular">{timeFormat.format(new Date(row.startedAt))}</Td>
              <Td className="tabular">{customerOf(row) || '—'}</Td>
              <Td className="text-xs text-muted-foreground">
                {row.legs.find((leg) => leg.kind === 'QUEUE')?.label || '—'}
              </Td>
              <Td>
                {row.wrapUp?.dispositionLabel ? (
                  <Badge>{row.wrapUp.dispositionLabel}</Badge>
                ) : (
                  <span className="text-xs text-muted-foreground">{t('myCalls.notFiled')}</span>
                )}
              </Td>
              <Td className="max-w-md">
                <span className="block truncate text-xs text-muted-foreground" title={row.wrapUp?.note}>
                  {row.wrapUp?.note || '—'}
                </span>
              </Td>
              <Td align="right" className="tabular">
                {formatDuration(row.talkSec)}
              </Td>
              <Td align="right">
                {row.hasRecording && (
                  <Button
                    size="sm"
                    variant="ghost"
                    className="size-8 p-0"
                    title={t('myCalls.listen')}
                    aria-label={t('myCalls.listen')}
                    aria-expanded={openCallId === row.callId}
                    onClick={() =>
                      setOpenCallId((current) => (current === row.callId ? null : row.callId))
                    }
                  >
                    <Headphones className={openCallId === row.callId ? 'text-primary' : ''} />
                  </Button>
                )}
              </Td>
            </Tr>
            {openCallId === row.callId && (
              <Tr>
                <Td colSpan={7}>
                  <MyRecording callId={row.callId} />
                </Td>
              </Tr>
            )}
            </Fragment>
          ))}
        </TBody>
      </DataTable>

      {total > PAGE_SIZE && (
        <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
          <span className="tabular">{t('cdr.pageOf', { page: page + 1, pages, total })}</span>
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
 * The recording behind one of the agent's own rows, looked up on first
 * listen — the list itself only knows hasRecording.
 */
function MyRecording({ callId }: { callId: string }) {
  const { t } = useTranslation()
  const [recordings, setRecordings] = useState<RecordingRow[] | null>(null)
  const [isFailed, setFailed] = useState(false)

  useEffect(() => {
    let alive = true
    ledgerApi
      .recordingsByCall(callId)
      .then(({ items }) => alive && setRecordings(items))
      .catch(() => alive && setFailed(true))
    return () => {
      alive = false
    }
  }, [callId])

  if (isFailed) {
    return <p className="py-1 text-xs text-muted-foreground">{t('player.failed')}</p>
  }
  if (recordings === null) {
    return <p className="py-1 text-xs text-muted-foreground">{t('common.loading')}</p>
  }
  if (recordings.length === 0) {
    return <p className="py-1 text-xs text-muted-foreground">{t('cdr.noRecording')}</p>
  }
  return (
    <div className="max-w-xl py-1">
      {recordings.map((recording) => (
        <RecordingPlayer
          key={recording.id}
          src={recordingAudioUrl(recording.id)}
          durationSec={recording.durationSec}
        />
      ))}
    </div>
  )
}

/**
 * The person on the other end. On an inbound call that is who rang in; on one
 * the platform placed it is who was rung — the agent's own number is never the
 * interesting one on their own list.
 */
function customerOf(cdr: CDR): string {
  return cdr.callType === 'OUTBOUND' ? cdr.toNumber : cdr.fromNumber
}
