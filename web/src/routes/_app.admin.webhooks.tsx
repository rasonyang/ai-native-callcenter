import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useMemo, useState } from 'react'

import { PageHeader } from '@/components/page-header'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  useWebhookDeliveries,
  useWebhookSubscriptions,
  type WebhookDelivery,
  type WebhookFilter,
  type WebhookSubscription,
} from '@/lib/webhooks'

/**
 * Where finished calls are sent, and whether they arrived.
 *
 * A window, not a workbench (design 09 §11): subscriptions are created and
 * edited through the API, and this page answers the two questions an operator
 * actually has at the moment something is wrong — which subscribers exist, and
 * what came back the last time we called one.
 *
 * The token is not here and cannot be. It is write-only at the contract, so the
 * server does not serve it and there is nothing to leak; what a screen needs is
 * whether there is one at all, which hasAuthToken says.
 */
export const Route = createFileRoute('/_app/admin/webhooks')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: Webhooks,
})

function Webhooks() {
  const { t, i18n } = useTranslation()
  const { data, isPending, isError, error } = useWebhookSubscriptions()
  const [selected, setSelected] = useState<string | undefined>()

  const subscriptions = data?.items ?? []
  // A subscription that was being looked at and has since been deleted must not
  // leave the deliveries panel showing somebody else's history.
  const current = subscriptions.find((s) => s.subscriptionId === selected)

  const timeFormat = useMemo(
    () =>
      new Intl.DateTimeFormat(i18n.language, {
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      }),
    [i18n.language],
  )

  return (
    <>
      <PageHeader title={t('webhooks.title')} description={t('webhooks.description')} />

      <DataTable>
        <THead>
          <Th>{t('webhooks.name')}</Th>
          <Th>{t('webhooks.url')}</Th>
          <Th>{t('webhooks.filter')}</Th>
          <Th>{t('webhooks.token')}</Th>
          <Th align="right">{t('webhooks.state')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={5}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && subscriptions.length === 0 && (
            <TableMessage colSpan={5}>{t('webhooks.none')}</TableMessage>
          )}
          {subscriptions.map((sub) => (
            <Tr
              key={sub.subscriptionId}
              onClick={() =>
                setSelected(sub.subscriptionId === selected ? undefined : sub.subscriptionId)
              }
              className={sub.subscriptionId === selected ? 'bg-muted' : 'cursor-pointer'}
            >
              <Td className="font-medium">{sub.name}</Td>
              <Td className="max-w-96 truncate font-mono text-xs text-muted-foreground">
                {sub.url}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {describeFilter(sub.filter, t('webhooks.everyCall'))}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {sub.hasAuthToken ? t('webhooks.tokenSet') : t('webhooks.tokenNone')}
              </Td>
              <Td align="right">
                <StateBadge sub={sub} enabled={t('webhooks.enabled')} paused={t('webhooks.paused')} />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {current && (
        <Deliveries
          subscription={current}
          formatTime={(at) => timeFormat.format(new Date(at))}
        />
      )}
    </>
  )
}

/** A subscription's recent attempts, newest first. */
function Deliveries({
  subscription,
  formatTime,
}: {
  subscription: WebhookSubscription
  formatTime: (at: string) => string
}) {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useWebhookDeliveries(subscription.subscriptionId)
  const rows = data?.items ?? []

  return (
    <section className="mt-6">
      <h2 className="mb-2 text-sm font-medium">
        {t('webhooks.deliveriesFor', { name: subscription.name })}
      </h2>
      <DataTable>
        <THead>
          <Th>{t('webhooks.at')}</Th>
          <Th>{t('webhooks.call')}</Th>
          <Th align="right">{t('webhooks.revision')}</Th>
          <Th>{t('webhooks.status')}</Th>
          <Th align="right">{t('webhooks.attempts')}</Th>
          <Th align="right">{t('webhooks.code')}</Th>
          <Th>{t('webhooks.lastError')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={7}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={7}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={7}>{t('webhooks.noDeliveries')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.deliveryId}>
              <Td className="tabular text-muted-foreground">{formatTime(row.createdAt)}</Td>
              <Td className="font-mono text-xs">{shortId(row.callId)}</Td>
              <Td align="right" className="tabular">
                {row.revision}
              </Td>
              <Td>
                <DeliveryStatus row={row} />
              </Td>
              <Td align="right" className="tabular">
                {row.attemptCount}
              </Td>
              <Td align="right" className="tabular text-muted-foreground">
                {row.lastStatusCode ?? '—'}
              </Td>
              <Td className="max-w-80 truncate text-xs text-muted-foreground">
                {row.lastError || '—'}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
    </section>
  )
}

/**
 * A dot and a word, which is how every other status on this platform reads
 * (the ledger's StatusCell). Colour lives in the state tokens rather than in a
 * palette class, so a status here and the same status on the CDR screen cannot
 * drift apart.
 */
const DELIVERY_COLOR: Record<string, string> = {
  DELIVERED: 'var(--state-available)',
  PENDING: 'var(--state-ringing)',
  FAILED: 'var(--state-breach)',
}

function Dot({ color }: { color: string }) {
  return <span className="size-2 rounded-full" style={{ backgroundColor: color }} />
}

function StateBadge({
  sub,
  enabled,
  paused,
}: {
  sub: WebhookSubscription
  enabled: string
  paused: string
}) {
  return (
    <span className="flex items-center justify-end gap-1.5 text-xs">
      <Dot color={sub.isEnabled ? 'var(--state-available)' : 'var(--state-offline)'} />
      {sub.isEnabled ? enabled : paused}
    </span>
  )
}

/**
 * FAILED is the one an operator is looking for: the retry schedule ran out and
 * that call was never told to this subscriber.
 */
function DeliveryStatus({ row }: { row: WebhookDelivery }) {
  const { t } = useTranslation()
  return (
    <span className="flex items-center gap-1.5 text-xs">
      <Dot color={DELIVERY_COLOR[row.status] ?? 'var(--state-offline)'} />
      {t(`webhooks.statuses.${row.status}`)}
    </span>
  )
}

/**
 * A filter in one line.
 *
 * The empty filter is every call, and saying so beats an empty cell: a blank
 * there reads as "misconfigured" when it is the commonest and most deliberate
 * setting there is.
 */
function describeFilter(filter: WebhookFilter, everyCall: string): string {
  const parts: string[] = []
  const add = (key: string, values: readonly (string | boolean)[] | undefined) => {
    if (values && values.length > 0) parts.push(`${key}=${values.join(',')}`)
  }
  add('callType', filter.callType)
  add('did', filter.did)
  add('status', filter.status)
  add('queueId', filter.queueId?.map(shortId))
  add('isContained', filter.isContained)
  return parts.length > 0 ? parts.join(' · ') : everyCall
}

/** Ids are for recognising a row again, not for reading aloud. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}
