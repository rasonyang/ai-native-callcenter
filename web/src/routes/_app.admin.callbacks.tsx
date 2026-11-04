import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CALLBACK_FILTERS, CALLBACK_PAGE_SIZE, LastAttempt, StatusPill } from '@/components/callback-parts'
import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { useRoster } from '@/lib/agent'
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

const COLUMNS = 6

function TeamCallbacks() {
  const { t, i18n } = useTranslation()
  const [filter, setFilter] = useState('OPEN')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCallbacks(filter || undefined, {
    limit: CALLBACK_PAGE_SIZE,
    offset: page * CALLBACK_PAGE_SIZE,
  })
  const roster = useRoster(true)
  const rows = data?.items ?? []
  const format = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  // A handler is a user id. The roster knows the agents it lists; anyone
  // else (an account since removed) reads as unknown, never as a raw id.
  const names = new Map((roster.data?.items ?? []).map((agent) => [agent.userId, agent.displayName]))
  const handlerName = (userId: string | undefined) => {
    if (!userId) return '—'
    if (roster.isPending) return '…'
    return names.get(userId) ?? t('callbacks.unknownUser')
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
          <Th>{t('callbacks.lastAttemptHeading')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={COLUMNS}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={COLUMNS}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={COLUMNS}>{t('callbacks.empty')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular">{format.format(new Date(row.createdAt))}</Td>
              <Td className="tabular font-medium">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.phoneNumber}</CellText>
              </Td>
              <Td>
                <CellText maxWidth="max-w-md" title={row.message || undefined}>
                  {row.message || '—'}
                </CellText>
              </Td>
              <Td><StatusPill status={row.status} /></Td>
              <Td>
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{handlerName(row.handledBy)}</CellText>
              </Td>
              <Td>
                {row.lastAttemptAt ? <LastAttempt callback={row} format={format} /> : '—'}
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
