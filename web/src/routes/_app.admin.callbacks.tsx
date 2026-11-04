import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CALLBACK_FILTERS, CALLBACK_PAGE_SIZE, LastAttempt, StatusPill } from '@/components/callback-parts'
import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useCallbacks } from '@/lib/ledger'

/**
 * The callback queue as the people who run the floor see it: every promise,
 * who has claimed it and how the last attempt went. Read-only — agents work
 * the queue from their own screen.
 */
export const Route = createFileRoute('/_app/admin/callbacks')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: TeamCallbacks,
})

const COLUMNS = 5

function TeamCallbacks() {
  const { t, i18n } = useTranslation()
  const [filter, setFilter] = useState('OPEN')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCallbacks(filter || undefined, {
    limit: CALLBACK_PAGE_SIZE,
    offset: page * CALLBACK_PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const format = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  // The API names the handler. A handler it cannot name is an account since
  // removed: that reads as unknown, never as a raw id.
  const handlerName = (row: { handledBy?: string; handledByName?: string }) => {
    if (!row.handledBy) return '—'
    return row.handledByName ?? t('callbacks.unknownUser')
  }

  return (
    <>
      <PageHeader
        title={t('nav.callbacks')}
        description={t('callbacks.teamHint')}
        actions={
          <Select
            value={filter}
            onChange={(next) => {
              setFilter(next)
              setPage(0)
            }}
            options={CALLBACK_FILTERS.map((s) => ({
              value: s,
              label: s === '' ? t('callbacks.all') : t(`callbacks.statuses.${s}`),
            }))}
          />
        }
      />
      <DataTable>
        <THead>
          <Th>{t('callbacks.requestedAt')}</Th>
          <Th>{t('callbacks.number')}</Th>
          <Th>{t('callbacks.message')}</Th>
          <Th>{t('callbacks.status')}</Th>
          <Th>{t('callbacks.handledBy')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={COLUMNS}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={COLUMNS}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={COLUMNS}>{t('callbacks.empty')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular whitespace-nowrap">{format.format(new Date(row.createdAt))}</Td>
              <Td className="tabular font-medium">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.phoneNumber}</CellText>
              </Td>
              <Td>
                <CellText maxWidth="max-w-md" title={row.message || undefined}>
                  {row.message || '—'}
                </CellText>
              </Td>
              <Td>
                <StatusPill status={row.status} />
                <LastAttempt callback={row} format={format} />
              </Td>
              <Td>
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{handlerName(row)}</CellText>
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <Pager page={page} pageSize={CALLBACK_PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
      <p className="mt-3 text-xs text-muted-foreground">{t('callbacks.teamNote')}</p>
    </>
  )
}
