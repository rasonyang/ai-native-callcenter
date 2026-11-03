import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { PageHeader } from '@/components/page-header'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useCallbacks } from '@/lib/ledger'

/** A read-only view for sales managers; agents claim tasks in their own queue. */
export const Route = createFileRoute('/_app/admin/callbacks')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: TeamCallbacks,
})

function TeamCallbacks() {
  const { t, i18n } = useTranslation()
  const { data, isPending, isError, error } = useCallbacks('OPEN')
  const format = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <>
      <PageHeader title={t('sales.callbacksTitle')} description={t('sales.callbacksDescription')} />
      <DataTable>
        <THead>
          <Th>{t('callbacks.requestedAt')}</Th>
          <Th>{t('callbacks.number')}</Th>
          <Th>{t('callbacks.message')}</Th>
          <Th>{t('callbacks.status')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={4}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={4}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && data?.items.length === 0 && (
            <TableMessage colSpan={4}>{t('sales.noCallbacks')}</TableMessage>
          )}
          {data?.items.map((item) => (
            <Tr key={item.id}>
              <Td className="tabular">{format.format(new Date(item.createdAt))}</Td>
              <Td><CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{item.phoneNumber}</CellText></Td>
              <Td><CellText maxWidth="max-w-md" wrap>{item.message || '—'}</CellText></Td>
              <Td>{t('callbacks.statuses.OPEN')}</Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <p className="mt-3 text-xs text-muted-foreground">{t('sales.callbacksReadOnlyNote')}</p>
    </>
  )
}
