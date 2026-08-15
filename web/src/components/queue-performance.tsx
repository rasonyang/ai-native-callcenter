import { useTranslation } from 'react-i18next'

import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { useQueues } from '@/lib/catalog'
import { formatDuration, useQueueReport } from '@/lib/ledger'

/**
 * Per-queue performance for today.
 *
 * The reference wallboard shows live depth and longest current wait; the
 * platform publishes no queue-count events, so this reports what the ledger
 * knows — volume, abandonment, service level and waits — rather than a live
 * figure it cannot compute.
 */
export function QueuePerformance() {
  const { t } = useTranslation()
  const { data, isPending, isError } = useQueueReport()
  const queues = useQueues()

  const nameFor = (queueId: string | null) => {
    if (!queueId) return t('supervisor.unassignedQueue')
    const queue = queues.data?.items.find((q) => q.id === queueId)
    return queue?.displayName || queue?.name || queueId.slice(0, 8)
  }

  const rows = data?.items ?? []

  return (
    <section>
      <h2 className="mb-3 flex items-baseline justify-between text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {t('supervisor.perQueue')}
        <span className="font-normal normal-case">{t('reports.today')}</span>
      </h2>
      <DataTable>
        <THead>
          <Th>{t('supervisor.queue')}</Th>
          <Th align="right">{t('supervisor.calls')}</Th>
          <Th align="right">{t('supervisor.abandoned')}</Th>
          <Th align="right">{t('supervisor.sla')}</Th>
          <Th align="right" className="whitespace-nowrap">
            {t('supervisor.avgWait')}
          </Th>
          <Th align="right" className="whitespace-nowrap">
            {t('supervisor.longestWait')}
          </Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{t('supervisor.queuesFailed')}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('supervisor.noQueueData')}</TableMessage>
          )}
          {rows.map((row) => {
            // Service level is the share of answered calls that beat the
            // queue's threshold; with nothing answered there is nothing to rate.
            const sla =
              row.answeredCalls > 0
                ? Math.round((row.answeredWithinSla / row.answeredCalls) * 100)
                : undefined
            return (
              <Tr key={row.queueId ?? 'none'}>
                <Td>{nameFor(row.queueId)}</Td>
                <Td align="right" className="tabular">
                  {row.totalCalls}
                </Td>
                <Td align="right" className="tabular">
                  <span
                    style={row.abandonedCalls > 0 ? { color: 'var(--state-breach)' } : undefined}
                  >
                    {row.abandonedCalls}
                  </span>
                </Td>
                <Td align="right" className="tabular">
                  {sla === undefined ? '—' : `${sla}%`}
                </Td>
                <Td align="right" className="tabular">
                  {formatDuration(row.avgWaitSec)}
                </Td>
                <Td align="right" className="tabular">
                  {formatDuration(row.maxWaitSec)}
                </Td>
              </Tr>
            )
          })}
        </TBody>
      </DataTable>
    </section>
  )
}
