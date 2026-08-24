import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useSession } from '@/lib/session'
import {
  useCallbackMutations, useCallbacks, type Callback, type CallbackStatus,
} from '@/lib/ledger'

/**
 * The promises queue: what the bot agreed to have someone follow up on.
 * Claiming makes ownership visible so two agents never ring the same person;
 * the list moves live on the event stream.
 */
export const Route = createFileRoute('/_app/agent/callbacks')({
  beforeLoad: ({ context }) => requireRole(context.user, 'AGENT'),
  component: CallbacksPage,
})

const FILTERS: Array<CallbackStatus | ''> = ['', 'OPEN', 'CLAIMED', 'DONE', 'DISMISSED']

function CallbacksPage() {
  const { t, i18n } = useTranslation()
  const { data: user } = useSession()
  const [filter, setFilter] = useState('')
  const { data, isPending, isError, error } = useCallbacks(filter || undefined)
  const { claim, complete } = useCallbackMutations()

  const rows = data?.items ?? []
  const timeFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <>
      <PageHeader
        title={t('nav.callbacks')}
        description={t('callbacks.hint')}
        actions={
          <Select
            value={filter}
            onChange={setFilter}
            options={FILTERS.map((s) => ({
              value: s,
              label: s === '' ? t('callbacks.all') : t(`callbacks.statuses.${s}`),
            }))}
          />
        }
      />

      {(claim.isError || complete.isError) && (
        <p className="mb-3 text-xs text-muted-foreground">
          {describeError(claim.error ?? complete.error, t)}
        </p>
      )}

      <DataTable>
        <THead>
          <Th>{t('callbacks.requestedAt')}</Th>
          <Th>{t('callbacks.number')}</Th>
          <Th>{t('callbacks.message')}</Th>
          <Th>{t('callbacks.status')}</Th>
          <Th align="right">{t('supervisor.actions')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={5}>{describeError(error, t)}</TableMessage>}
          {!isPending && rows.length === 0 && (
            <TableMessage colSpan={5}>{t('callbacks.empty')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular">{timeFormat.format(new Date(row.createdAt))}</Td>
              <Td className="tabular font-medium">{row.phoneNumber}</Td>
              <Td className="max-w-md">
                <span className="block truncate" title={row.message}>
                  {row.message || '—'}
                </span>
              </Td>
              <Td>
                <StatusPill callback={row} isMine={row.handledBy === user?.userId} />
              </Td>
              <Td align="right">
                <RowActions
                  callback={row}
                  isMine={row.handledBy === user?.userId}
                  onClaim={() => claim.mutate(row.id)}
                  onComplete={(status) => complete.mutate({ id: row.id, status })}
                />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
    </>
  )
}

const STATUS_COLOR: Record<CallbackStatus, string> = {
  OPEN: 'var(--state-ringing)',
  CLAIMED: 'var(--state-oncall)',
  DONE: 'var(--state-available)',
  DISMISSED: 'var(--state-offline)',
}

function StatusPill({ callback, isMine }: { callback: Callback; isMine: boolean }) {
  const { t } = useTranslation()
  return (
    <span className="flex items-center gap-1.5 text-xs">
      <span
        className="size-2 rounded-full"
        style={{ backgroundColor: STATUS_COLOR[callback.status] }}
      />
      {t(`callbacks.statuses.${callback.status}`)}
      {callback.status === 'CLAIMED' && isMine && (
        <span className="text-muted-foreground">{t('callbacks.byYou')}</span>
      )}
    </span>
  )
}

function RowActions(props: {
  callback: Callback
  isMine: boolean
  onClaim: () => void
  onComplete: (status: 'DONE' | 'DISMISSED') => void
}) {
  const { t } = useTranslation()
  const { callback } = props

  if (callback.status === 'OPEN') {
    return (
      <Button size="sm" variant="ghost" onClick={props.onClaim}>
        {t('callbacks.claim')}
      </Button>
    )
  }
  if (callback.status === 'CLAIMED') {
    return (
      <>
        <Button size="sm" variant="ghost" onClick={() => props.onComplete('DONE')}>
          {t('callbacks.markDone')}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => props.onComplete('DISMISSED')}>
          {t('callbacks.dismiss')}
        </Button>
      </>
    )
  }
  return null
}
