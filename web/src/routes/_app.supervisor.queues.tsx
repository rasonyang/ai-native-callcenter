import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useQueries } from '@tanstack/react-query'

import { PageHeader } from '@/components/page-header'
import { StatusDot } from '@/components/status-pill'
import { DataTable, TBody, THead, TableFooter, TableMessage, Td, Th, Tr } from '@/components/table'
import { catalogApi, useQueues, type Queue } from '@/lib/catalog'
import { requireRole } from '@/lib/guards'
import { formatDuration } from '@/lib/utils'

/**
 * The queues as configured, with who staffs each one. Waiting-caller depth
 * would need queue-count events the platform does not publish yet, so this
 * answers coverage rather than live load.
 */
export const Route = createFileRoute('/_app/supervisor/queues')({
  beforeLoad: ({ context }) => requireRole(context.user, 'SUPERVISOR'),
  component: QueuesPage,
})

function QueuesPage() {
  const { t } = useTranslation()
  const { data, isPending, isError } = useQueues()
  const queues = data?.items ?? []

  // One staffing request per queue: the roster is small and the answer is what
  // makes the page worth opening.
  const staffing = useQueries({
    queries: queues.map((queue) => ({
      queryKey: ['catalog', 'queues', queue.id, 'agents'],
      queryFn: () => catalogApi.queueAgents(queue.id),
    })),
  })

  return (
    <div>
      <PageHeader title={t('nav.queues')} description={t('supervisor.queuesHint')} />

      <DataTable>
        <THead>
          <Th>{t('supervisor.queue')}</Th>
          <Th>{t('admin.queueExtension')}</Th>
          <Th>{t('admin.strategy')}</Th>
          <Th align="right">{t('supervisor.staffed')}</Th>
          <Th align="right">{t('supervisor.sla')}</Th>
          <Th align="right">{t('admin.maxWait')}</Th>
          <Th>{t('admin.enabled')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{t('supervisor.queuesFailed')}</TableMessage>}
          {!isPending && !isError && queues.length === 0 && (
            <TableMessage colSpan={7}>{t('admin.noQueues')}</TableMessage>
          )}
          {queues.map((queue, index) => (
            <QueueRow
              key={queue.id}
              queue={queue}
              staffedCount={staffing[index]?.data?.items.length}
            />
          ))}
        </TBody>
      </DataTable>

      {queues.length > 0 && (
        <TableFooter>
          <span>{t('supervisor.queueCount', { count: queues.length })}</span>
        </TableFooter>
      )}
    </div>
  )
}

function QueueRow({ queue, staffedCount }: { queue: Queue; staffedCount?: number }) {
  const { t } = useTranslation()
  return (
    <Tr>
      <Td>
        <span className="font-medium">{queue.displayName || queue.name}</span>
        <span className="ml-2 text-xs text-muted-foreground">{queue.name}</span>
      </Td>
      <Td className="tabular">{queue.extNumber}</Td>
      <Td className="text-muted-foreground">{t(`admin.strategies.${queue.strategy}`)}</Td>
      <Td align="right" className="tabular">
        {staffedCount ?? '—'}
      </Td>
      <Td align="right" className="tabular">
        {formatDuration(queue.slaThresholdSec)}
      </Td>
      {/* Zero is not a duration here, it is the absence of one. Formatted
          like any other number it read "0:00" — a queue that waits no time at
          all, which is the exact opposite of the queue that waits forever.
          The administrator's own list has always said so; this one did not,
          and the two pages disagreed about the same row. */}
      <Td align="right" className="tabular">
        {queue.maxWaitSec > 0 ? formatDuration(queue.maxWaitSec) : t('admin.noLimit')}
      </Td>
      <Td>
        <span className="inline-flex items-center gap-1.5 text-sm">
          <StatusDot availability={queue.isEnabled ? 'READY' : 'LOGGED_OUT'} />
          {queue.isEnabled ? t('common.yes') : t('common.no')}
        </span>
      </Td>
    </Tr>
  )
}
