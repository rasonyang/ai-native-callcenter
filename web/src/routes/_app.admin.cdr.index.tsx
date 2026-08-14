import { Link, createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { formatDuration, useCDRs, type CDRStatus } from '@/lib/ledger'

/** The finished-call ledger, filterable, newest first. */
export const Route = createFileRoute('/_app/admin/cdr/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'SUPERVISOR'),
  component: CDRExplorer,
})

const PAGE_SIZE = 50
const STATUSES: Array<CDRStatus | ''> = ['', 'ANSWERED', 'MISSED', 'FAILED', 'NO_ANSWER']

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
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('cdr.noCalls')}</TableMessage>
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

const STATUS_COLOR: Record<string, string> = {
  ANSWERED: 'var(--state-available)',
  MISSED: 'var(--state-breach)',
  FAILED: 'var(--state-breach)',
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
