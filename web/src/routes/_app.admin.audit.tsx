import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useMemo, useState } from 'react'

import { PageHeader } from '@/components/page-header'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useAuditLogs, type AuditEntry } from '@/lib/ledger'

/**
 * Who changed what.
 *
 * Every mutating request that succeeded is here, recorded by middleware rather
 * than by each handler, and until this page there was no way to read any of it
 * short of psql.
 */
export const Route = createFileRoute('/_app/admin/audit')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: AuditTrail,
})

const PAGE_SIZE = 50

/**
 * The methods worth filtering by, as prefixes of the stored action.
 *
 * The action is "METHOD /route/template", so a method is a literal prefix of
 * it — no category has to be invented, and none can drift from what is stored.
 * The trailing space matters: "DELETE " cannot be confused with anything else.
 */
const METHODS = ['', 'POST ', 'PUT ', 'DELETE ', 'PATCH ']

function AuditTrail() {
  const { t, i18n } = useTranslation()
  const [method, setMethod] = useState('')
  const [pathQuery, setPathQuery] = useState('')
  const [page, setPage] = useState(0)

  // One prefix, built from both controls: the method narrows the verb, the
  // text narrows the route. Sending them separately would need a second
  // filter the stored value does not support.
  const actionPrefix = method + pathQuery.trim()

  const { data, isPending, isError, error } = useAuditLogs({
    actionPrefix: actionPrefix || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })

  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
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

  const reset = (apply: () => void) => {
    apply()
    setPage(0)
  }

  return (
    <>
      <PageHeader
        title={t('nav.audit')}
        description={t('audit.hint')}
        actions={
          <div className="flex items-center gap-2">
            <Input
              className="h-8 w-56"
              placeholder={t('audit.filterPath')}
              value={pathQuery}
              onChange={(e) => reset(() => setPathQuery(e.target.value))}
            />
            <Select
              value={method}
              onChange={(next) => reset(() => setMethod(next))}
              options={METHODS.map((m) => ({
                value: m,
                label: m === '' ? t('audit.allMethods') : m.trim(),
              }))}
            />
          </div>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('audit.occurredAt')}</Th>
          <Th>{t('audit.actor')}</Th>
          <Th>{t('audit.action')}</Th>
          <Th>{t('audit.target')}</Th>
          <Th>{t('audit.detail')}</Th>
          <Th align="right">{t('audit.ip')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('audit.noEntries')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.auditId}>
              <Td className="tabular text-muted-foreground">
                {timeFormat.format(new Date(row.occurredAt))}
              </Td>
              <Td>
                <ActorCell entry={row} unknownLabel={t('audit.deletedAccount')} />
              </Td>
              <Td className="font-mono text-xs">{row.action}</Td>
              <Td className="text-muted-foreground">
                {row.targetKind ? `${row.targetKind} ${shortId(row.targetId)}` : '—'}
              </Td>
              <Td className="max-w-96 truncate text-xs text-muted-foreground">
                {describeDetail(row)}
              </Td>
              <Td align="right" className="tabular text-muted-foreground">
                {row.ip ?? '—'}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      {total > PAGE_SIZE && (
        <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
          <span className="tabular">
            {t('audit.pageOf', { page: page + 1, pages, total })}
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

/**
 * The account that acted.
 *
 * An account can be deleted while its actions stay recorded, so a row may have
 * an id and no name. Showing the id in that case is the point: cleaning the
 * roster must not erase who did what, and a blank cell would read as "nobody".
 */
function ActorCell({ entry, unknownLabel }: { entry: AuditEntry; unknownLabel: string }) {
  if (entry.actorUsername) {
    return <span className="font-medium">{entry.actorUsername}</span>
  }
  if (entry.actorId) {
    return (
      <span className="text-muted-foreground" title={entry.actorId}>
        {unknownLabel} {shortId(entry.actorId)}
      </span>
    )
  }
  return <span className="text-muted-foreground">—</span>
}

/** Ids are for recognising a row again, not for reading aloud. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

/**
 * What the request carried, in one line.
 *
 * Secret-looking fields are already "[redacted]" by the time they are stored,
 * so there is nothing to hide here — but the body is still one line of a table,
 * so it is shown as written and truncated by the cell.
 */
function describeDetail(entry: AuditEntry): string {
  const detail = entry.detail as Record<string, unknown> | undefined
  const request = detail?.request
  if (typeof request === 'string' && request !== '') {
    return request
  }
  return '—'
}
