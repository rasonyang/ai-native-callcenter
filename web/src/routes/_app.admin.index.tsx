import { Link, createFileRoute } from '@tanstack/react-router'
import { ArrowRight, BarChart3, Bot, FileClock, Megaphone, Upload } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { useQuery } from '@tanstack/react-query'

import { CampaignState, ContactCounts } from '@/components/campaign-state'
import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { api } from '@/lib/api'
import { useCampaigns } from '@/lib/campaigns'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { useCallbacks, useOverview } from '@/lib/ledger'

/** The administrator's sales landing page; all figures come from the API. */
export const Route = createFileRoute('/_app/admin/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN'),
  component: SalesWorkspace,
})

function todayRange(): { from: string; to: string } {
  const now = new Date()
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const end = new Date(start)
  end.setDate(end.getDate() + 1)
  return { from: start.toISOString(), to: end.toISOString() }
}

function SalesWorkspace() {
  const { t, i18n } = useTranslation()
  const { from, to } = useMemo(todayRange, [])
  const overview = useOverview(from, to)
  const campaigns = useCampaigns({ status: 'RUNNING', limit: 5, offset: 0 })
  const callbacks = useCallbacks('OPEN')
  const health = useQuery({ queryKey: ['system', 'health'], queryFn: api.health })
  const totalCalls = overview.data?.totalCalls
  const answeredCalls = overview.data?.answeredCalls
  const answerRate = totalCalls !== undefined && answeredCalls !== undefined && totalCalls > 0
    ? Math.round((answeredCalls / totalCalls) * 100) : undefined
  const runningCount = campaigns.data?.total
  const openCallbacks = callbacks.data?.items ?? []
  const timeFormat = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={t('sales.title')}
        description={t('sales.description')}
        actions={
          <Link to="/admin/campaigns/new" className="inline-flex h-9 items-center gap-2 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground hover:bg-primary/90">
            <Megaphone className="size-4" />{t('sales.newCampaign')}
          </Link>
        }
      />

      <section aria-label={t('sales.title')} className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <KpiCard label={t('sales.todayCalls')} value={overview.isPending ? '—' : (totalCalls ?? '—')} note={overview.isError ? describeError(overview.error, t) : t('sales.allCallTypes')} />
        <KpiCard label={t('sales.answeredCalls')} value={overview.isPending ? '—' : (answeredCalls ?? '—')} note={answerRate === undefined ? t('sales.noCallData') : t('sales.answerRate', { rate: answerRate })} />
        <KpiCard label={t('sales.runningCampaigns')} value={campaigns.isPending ? '—' : (runningCount ?? '—')} note={campaigns.isError ? describeError(campaigns.error, t) : t('sales.runningCampaignsNote', { total: runningCount ?? 0 })} />
        <KpiCard label={t('sales.pendingCallbacks')} value={callbacks.isPending || callbacks.isError ? '—' : openCallbacks.length} note={callbacks.isError ? describeError(callbacks.error, t) : t('sales.pendingCallbacksNote')} />
      </section>

      <section className="rounded-md border bg-card">
        <div className="flex items-center justify-between gap-3 border-b px-4 py-3">
          <div>
            <h2 className="text-sm font-semibold">{t('sales.campaignsTitle')}</h2>
            {campaigns.data && campaigns.data.total > campaigns.data.items.length && <p className="mt-0.5 text-xs text-muted-foreground">{t('sales.runningCampaignsPartialNote', { shown: campaigns.data.items.length, total: campaigns.data.total })}</p>}
          </div>
          <Link to="/admin/campaigns" className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-primary">{t('sales.viewAllCampaigns')} <ArrowRight className="size-3" /></Link>
        </div>
        <DataTable>
          <THead>
            <Th>{t('sales.campaignName')}</Th>
            <Th>{t('sales.campaignState')}</Th>
            <Th>{t('sales.campaignContacts')}</Th>
          </THead>
          <TBody>
            {campaigns.isPending && <TableMessage colSpan={3}>{t('common.loading')}</TableMessage>}
            {campaigns.isError && <TableMessage colSpan={3}>{describeError(campaigns.error, t)}</TableMessage>}
            {!campaigns.isPending && !campaigns.isError && campaigns.data?.items.length === 0 && (
              <TableMessage colSpan={3}>{t('sales.noCampaigns')}</TableMessage>
            )}
            {campaigns.data?.items.map((campaign) => (
              <Tr key={campaign.id}>
                <Td className="font-medium">
                  <Link
                    to="/admin/campaigns/$campaignId"
                    params={{ campaignId: campaign.id }}
                    className="text-foreground hover:text-primary"
                  >
                    <CellText maxWidth="max-w-xs" minWidth="min-w-upto-24 xl:min-w-upto-48">
                      {campaign.name}
                    </CellText>
                  </Link>
                </Td>
                <Td><CampaignState campaign={campaign} /></Td>
                <Td><ContactCounts counts={campaign.contacts} /></Td>
              </Tr>
            ))}
          </TBody>
        </DataTable>
      </section>

      <div className="grid grid-cols-1 items-start gap-4 xl:grid-cols-2">
        <section className="rounded-md border bg-card">
          <div className="flex items-start justify-between gap-3 border-b px-4 py-3">
            <div><h2 className="text-sm font-semibold">{t('sales.callbacksTitle')}</h2><p className="mt-0.5 text-xs text-muted-foreground">{t('sales.callbacksDescription')}</p></div>
            <Link to="/admin/callbacks" className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-primary">{t('sales.viewAllCampaigns')} <ArrowRight className="size-3" /></Link>
          </div>
          <div className="divide-y">
            {callbacks.isPending && <p className="px-4 py-5 text-xs text-muted-foreground">{t('common.loading')}</p>}
            {callbacks.isError && <p className="px-4 py-5 text-xs text-muted-foreground">{describeError(callbacks.error, t)}</p>}
            {!callbacks.isPending && !callbacks.isError && openCallbacks.length === 0 && <p className="px-4 py-6 text-center text-xs text-muted-foreground">{t('sales.noCallbacks')}</p>}
            {openCallbacks.slice(0, 5).map((item) => (
              <div key={item.id} className="flex items-start justify-between gap-3 px-4 py-3">
                <div className="min-w-0"><p className="truncate text-sm font-medium tabular">{item.phoneNumber}</p><p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{item.message || '—'}</p></div>
                <time className="shrink-0 text-xs tabular text-muted-foreground">{timeFormat.format(new Date(item.createdAt))}</time>
              </div>
            ))}
          </div>
        </section>

        <section className="rounded-md border bg-card">
          <div className="border-b px-4 py-3"><h2 className="text-sm font-semibold">{t('sales.quickActionsTitle')}</h2></div>
          <div className="grid grid-cols-1 divide-y sm:grid-cols-2 sm:divide-x sm:divide-y-0">
            <QuickAction to="/admin/campaigns" icon={Upload} title={t('sales.importContacts')} description={t('sales.importContactsHint')} />
            <QuickAction to="/admin/bots" icon={Bot} title={t('sales.editSalesFlow')} description={t('sales.editSalesFlowHint')} />
            <QuickAction to="/admin/cdr" icon={FileClock} title={t('sales.callHistory')} description={t('sales.callHistoryHint')} />
            <QuickAction to="/admin/reports" icon={BarChart3} title={t('sales.salesReports')} description={t('sales.salesReportsHint')} />
          </div>
        </section>
      </div>

      <section className="rounded-md border bg-card p-4">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('sales.systemHealth')}</h2>
        <dl className="mt-2 divide-y">
          <HealthRow label={t('sales.eventStream')} value={health.isError ? t('stream.offline') : (health.data ? t('sales.consolesLive', { count: health.data.sseClients }) : '—')} isDegraded={health.isError} />
          <HealthRow label={t('sales.replayWindow')} value={health.data ? `#${health.data.oldestSeq}` : '—'} />
          {(health.data?.trunks ?? []).map((trunk) => <HealthRow key={`${trunk.profile}::${trunk.name}`} label={t('sales.trunk', { name: trunk.name })} value={`${trunk.state} · ${trunk.address}`} isDegraded={!trunk.isUp} />)}
          {health.data && (health.data.trunks ?? []).length === 0 && <HealthRow label={t('sales.trunks')} value={t('sales.noTrunks')} isDegraded />}
        </dl>
        <p className="mt-3 text-xs text-muted-foreground">{t('sales.healthHint')}</p>
      </section>
    </div>
  )
}

function QuickAction({ to, icon: Icon, title, description }: {
  to: '/admin/campaigns' | '/admin/bots' | '/admin/cdr' | '/admin/reports'
  icon: typeof Upload
  title: string
  description: string
}) {
  return <Link to={to} className="flex min-w-0 items-start gap-3 p-4 hover:bg-muted/30"><Icon className="mt-0.5 size-4 shrink-0 text-muted-foreground" /><span className="min-w-0"><span className="block text-sm font-medium">{title}</span><span className="mt-1 block text-xs text-muted-foreground">{description}</span></span></Link>
}

function HealthRow({ label, value, isDegraded }: { label: string; value: string; isDegraded?: boolean }) {
  return <div className="flex h-8 items-center justify-between gap-2"><dt className="text-sm text-muted-foreground">{label}</dt><dd className="tabular text-sm" style={isDegraded ? { color: 'var(--state-breach)' } : undefined}>{value}</dd></div>
}
