import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useMemo, useState } from 'react'

import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useQueues } from '@/lib/catalog'
import {
  formatDuration, useDailyReport, useOverview, useQueueReport,
} from '@/lib/ledger'

/** Aggregates over the finished-call ledger for a chosen window. */
export const Route = createFileRoute('/_app/admin/reports')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: ReportsPage,
})

type Window = 'today' | '7d' | '30d'

/** From/to in RFC3339, local midnight boundaries, matching the API default. */
function windowRange(window: Window): { from: string; to: string } {
  const now = new Date()
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const to = new Date(start)
  to.setDate(to.getDate() + 1)
  const from = new Date(start)
  if (window === '7d') from.setDate(from.getDate() - 6)
  if (window === '30d') from.setDate(from.getDate() - 29)
  return { from: from.toISOString(), to: to.toISOString() }
}

function ReportsPage() {
  const { t, i18n } = useTranslation()
  const [window, setWindow] = useState<Window>('today')
  const { from, to } = useMemo(() => windowRange(window), [window])

  const overview = useOverview(from, to)
  const byQueue = useQueueReport(from, to)
  const daily = useDailyReport(from, to)
  const queues = useQueues()

  const queueName = (queueId: string | null) =>
    queues.data?.items.find((q) => q.id === queueId)?.name ?? t('reports.unknownQueue')

  const dayFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', weekday: 'short',
  })
  const data = overview.data

  return (
    <>
      <PageHeader
        title={t('nav.reports')}
        description={t('reports.hint')}
        actions={
          <Select
            value={window}
            onChange={(next) => setWindow(next as Window)}
            options={[
              { value: 'today', label: t('reports.today') },
              { value: '7d', label: t('reports.last7') },
              { value: '30d', label: t('reports.last30') },
            ]}
          />
        }
      />

      {overview.isError && (
        <p className="mb-4 text-xs text-muted-foreground">{describeError(overview.error, t)}</p>
      )}

      <div className="mb-4 grid grid-cols-4 gap-4">
        <KpiCard
          label={t('reports.totalCalls')}
          value={data?.totalCalls ?? '—'}
          note={t('reports.inWindow')}
        />
        <KpiCard
          label={t('reports.answered')}
          value={data ? `${data.answeredCalls} / ${data.totalCalls}` : '—'}
          note={t('reports.abandonedNote', { count: data?.abandonedCalls ?? 0 })}
          tone={data && data.abandonedCalls > 0 ? '--state-breach' : undefined}
        />
        <KpiCard
          label={t('reports.contained')}
          value={
            data && data.totalCalls > 0
              ? `${Math.round((data.containedCalls / data.totalCalls) * 100)}%`
              : '—'
          }
          note={t('reports.containedNote', { count: data?.containedCalls ?? 0 })}
        />
        <KpiCard
          label={t('reports.avgBot')}
          value={data && data.avgBotSec > 0 ? formatDuration(data.avgBotSec) : '—'}
          note={t('reports.avgTalkNote', {
            talk: data && data.avgTalkSec > 0 ? formatDuration(data.avgTalkSec) : '—',
          })}
        />
      </div>

      <div className="grid grid-cols-2 items-start gap-4">
        <DataTable>
          <THead>
            <Th>{t('reports.queue')}</Th>
            <Th align="right">{t('reports.calls')}</Th>
            <Th align="right">{t('reports.answeredShort')}</Th>
            <Th align="right">
              <span title={t('reports.slaHint')}>{t('reports.sla')}</span>
            </Th>
            <Th align="right">{t('reports.avgWait')}</Th>
            <Th align="right">{t('reports.maxWait')}</Th>
          </THead>
          <TBody>
            {byQueue.isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
            {byQueue.data?.items.length === 0 && (
              <TableMessage colSpan={6}>{t('reports.noQueueCalls')}</TableMessage>
            )}
            {byQueue.data?.items.map((row) => (
              <Tr key={row.queueId ?? 'none'}>
                <Td>{queueName(row.queueId)}</Td>
                <Td align="right" className="tabular">{row.totalCalls}</Td>
                <Td align="right" className="tabular">{row.answeredCalls}</Td>
                {/* Over every call offered — the same denominator the
                    supervisor's wallboard uses, since C27 found the two
                    disagreeing on one queue on one day. */}
                <Td align="right" className="tabular">
                  {row.totalCalls > 0
                    ? `${Math.round((row.answeredWithinSla / row.totalCalls) * 100)}%`
                    : '—'}
                </Td>
                <Td align="right" className="tabular">{formatDuration(row.avgWaitSec)}</Td>
                <Td align="right" className="tabular">{formatDuration(row.maxWaitSec)}</Td>
              </Tr>
            ))}
          </TBody>
        </DataTable>

        <DataTable>
          <THead>
            <Th>{t('reports.day')}</Th>
            <Th align="right">{t('reports.calls')}</Th>
            <Th align="right">{t('reports.answeredShort')}</Th>
            <Th align="right">{t('reports.containedShort')}</Th>
            <Th align="right">{t('reports.abandonedShort')}</Th>
          </THead>
          <TBody>
            {daily.isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
            {daily.data?.items.length === 0 && (
              <TableMessage colSpan={5}>{t('reports.noCalls')}</TableMessage>
            )}
            {daily.data?.items.map((row) => (
              <Tr key={row.day}>
                <Td>{dayFormat.format(new Date(row.day))}</Td>
                <Td align="right" className="tabular">{row.totalCalls}</Td>
                <Td align="right" className="tabular">{row.answeredCalls}</Td>
                <Td align="right" className="tabular">{row.containedCalls}</Td>
                <Td align="right" className="tabular">{row.abandonedCalls}</Td>
              </Tr>
            ))}
          </TBody>
        </DataTable>
      </div>
    </>
  )
}
