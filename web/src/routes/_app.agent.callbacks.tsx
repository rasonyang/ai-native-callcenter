import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState, type ReactNode } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Ban, Check, Hand, PhoneOutgoing, Undo2 } from 'lucide-react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { callApi } from '@/lib/api'
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
  const { claim, release, complete } = useCallbackMutations()
  // Click-to-dial from the row: the agent's own phone rings first, then the
  // customer. The callback stays CLAIMED — whether the call kept the promise
  // is for the agent to say afterwards, not for the dial to assume.
  const dial = useMutation({
    mutationFn: (row: Callback) => callApi.dialForCallback(row.id, row.phoneNumber),
  })

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

      {(claim.isError || release.isError || complete.isError || dial.isError) && (
        <p className="mb-3 text-xs text-muted-foreground">
          {describeError(claim.error ?? release.error ?? complete.error ?? dial.error, t)}
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
                <LastAttempt callback={row} format={timeFormat} />
              </Td>
              <Td align="right">
                <RowActions
                  callback={row}
                  isMine={row.handledBy === user?.userId}
                  isDialing={dial.isPending}
                  onClaim={() => claim.mutate(row.id)}
                  onCallBack={() => dial.mutate(row)}
                  onRelease={() => release.mutate(row.id)}
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

/**
 * What happened the last time somebody rang from this row. A dial is not a
 * kept promise — no answer, busy, "call me later" all leave the callback
 * open — so the outcome sits under the status for the agent to read before
 * deciding, rather than closing the row for them.
 */
function LastAttempt({ callback, format }: { callback: Callback; format: Intl.DateTimeFormat }) {
  const { t } = useTranslation()
  if (!callback.lastAttemptAt) return null
  const when = format.format(new Date(callback.lastAttemptAt))
  const outcome = callback.lastAttemptStatus
    ? t(`callbacks.attempt.${callback.lastAttemptStatus}`, { defaultValue: callback.lastAttemptStatus })
    : t('callbacks.attempt.IN_PROGRESS')
  return (
    <span className="mt-0.5 block text-xs text-muted-foreground">
      {t('callbacks.lastAttempt', { when, outcome })}
    </span>
  )
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

/**
 * What can happen to a callback, as icons with the word in the tooltip.
 *
 * Open: claim it. Claimed by you: ring them, then say how it went — done,
 * dismissed — or put it back for somebody else. Claimed by a colleague: hands
 * off. Closed: nothing left to do.
 */
function RowActions(props: {
  callback: Callback
  isMine: boolean
  isDialing: boolean
  onClaim: () => void
  onCallBack: () => void
  onRelease: () => void
  onComplete: (status: 'DONE' | 'DISMISSED') => void
}) {
  const { t } = useTranslation()
  const { callback, isMine } = props

  if (callback.status === 'OPEN') {
    return (
      <IconAction label={t('callbacks.claim')} onClick={props.onClaim}>
        <Hand />
      </IconAction>
    )
  }
  if (callback.status === 'CLAIMED') {
    if (!isMine) {
      return <span className="text-xs text-muted-foreground">{t('callbacks.claimedByOther')}</span>
    }
    return (
      <span className="inline-flex items-center gap-0.5">
        <IconAction
          label={props.isDialing ? t('callbacks.ringing') : t('callbacks.callBack')}
          disabled={props.isDialing}
          onClick={props.onCallBack}
        >
          <PhoneOutgoing />
        </IconAction>
        <IconAction label={t('callbacks.markDone')} onClick={() => props.onComplete('DONE')}>
          <Check />
        </IconAction>
        <IconAction label={t('callbacks.dismiss')} onClick={() => props.onComplete('DISMISSED')}>
          <Ban />
        </IconAction>
        <IconAction label={t('callbacks.release')} onClick={props.onRelease}>
          <Undo2 />
        </IconAction>
      </span>
    )
  }
  return null
}

function IconAction({
  label,
  disabled,
  onClick,
  children,
}: {
  label: string
  disabled?: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <Button
      size="sm"
      variant="ghost"
      className="size-7 p-0"
      title={label}
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </Button>
  )
}
