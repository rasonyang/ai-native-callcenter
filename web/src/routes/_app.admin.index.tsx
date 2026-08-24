import { Link, createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useQuery } from '@tanstack/react-query'

import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { api } from '@/lib/api'
import { useDIDs, useExtensions, useQueues } from '@/lib/catalog'
import { requireRole } from '@/lib/guards'
import { useRoster } from '@/lib/agent'

/**
 * Administration overview: how much of the platform is configured, and whether
 * the parts an operator cannot see are alive.
 */
export const Route = createFileRoute('/_app/admin/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: AdminOverview,
})

function AdminOverview() {
  const { t } = useTranslation()
  const extensions = useExtensions()
  const queues = useQueues()
  const dids = useDIDs()
  const roster = useRoster(true)
  const health = useQuery({ queryKey: ['system', 'health'], queryFn: api.health })

  const enabledDIDs = dids.data?.items.filter((did) => did.isEnabled).length ?? 0
  const withFlow = dids.data?.items.filter((did) => did.flowId).length ?? 0
  const boundExtensions = extensions.data?.items.filter((e) => e.agentId).length ?? 0

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t('nav.overview')} description={t('admin.overviewHint')} />

      <div className="grid grid-cols-4 gap-4">
        <KpiCard
          label={t('nav.extensions')}
          value={extensions.data?.items.length ?? '—'}
          note={t('admin.boundExtensions', { count: boundExtensions })}
        />
        <KpiCard
          label={t('nav.queues')}
          value={queues.data?.items.length ?? '—'}
          note={t('admin.queuesNote', {
            count: queues.data?.items.filter((q) => q.isEnabled).length ?? 0,
          })}
        />
        <KpiCard
          label={t('nav.numbers')}
          value={dids.data?.items.length ?? '—'}
          note={t('admin.numbersNote', { enabled: enabledDIDs, withFlow })}
        />
        <KpiCard
          label={t('nav.agents')}
          value={roster.data?.items.length ?? '—'}
          note={t('admin.agentsNote', {
            count: roster.data?.items.filter((a) => a.state !== 'LOGGED_OUT').length ?? 0,
          })}
        />
      </div>

      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('admin.systemHealth')}
        </h2>
        <dl className="mt-2 divide-y">
          <HealthRow
            label={t('admin.eventStream')}
            value={
              health.isError
                ? t('stream.offline')
                : t('admin.consolesLive', { count: health.data?.sseClients ?? 0 })
            }
            isDegraded={health.isError}
          />
          <HealthRow
            label={t('admin.replayWindow')}
            value={health.data ? `#${health.data.oldestSeq}` : '—'}
          />
        </dl>
        <p className="mt-3 text-xs text-muted-foreground">{t('admin.healthHint')}</p>
      </section>

      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('admin.shortcuts')}
        </h2>
        <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1">
          <Shortcut to="/admin/extensions" label={t('nav.extensions')} />
          <Shortcut to="/admin/routing" label={t('nav.routing')} />
          <Shortcut to="/admin/numbers" label={t('nav.numbers')} />
          <Shortcut to="/admin/cdr" label={t('nav.cdr')} />
          <Shortcut to="/admin/reports" label={t('nav.reports')} />
        </div>
      </section>
    </div>
  )
}

function HealthRow({
  label,
  value,
  isDegraded,
}: {
  label: string
  value: string
  isDegraded?: boolean
}) {
  return (
    <div className="flex h-8 items-center justify-between gap-2">
      <dt className="text-sm text-muted-foreground">{label}</dt>
      <dd
        className="tabular text-sm"
        style={isDegraded ? { color: 'var(--state-breach)' } : undefined}
      >
        {value}
      </dd>
    </div>
  )
}

function Shortcut({ to, label }: { to: string; label: string }) {
  return (
    <Link to={to} className="text-sm text-muted-foreground hover:text-primary">
      {label}
    </Link>
  )
}
