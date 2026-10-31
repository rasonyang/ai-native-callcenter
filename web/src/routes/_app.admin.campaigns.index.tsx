import { Link, createFileRoute } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CampaignState, ContactCounts } from '@/components/campaign-state'
import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { buttonVariants } from '@/components/ui/button'
import { CAMPAIGN_STATUSES, useCampaigns, type CampaignStatus } from '@/lib/campaigns'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'

/**
 * Campaigns, newest first. Progress is live: the event stream rewrites the
 * cached rows as the dialer reports, so nothing here polls.
 */
export const Route = createFileRoute('/_app/admin/campaigns/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: CampaignsPage,
})

const PAGE_SIZE = 50

function CampaignsPage() {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState<CampaignStatus | ''>('')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCampaigns({
    status: status || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })

  const rows = data?.items ?? []
  const total = data?.total ?? 0
  const created = new Intl.DateTimeFormat(i18n.language, { month: 'short', day: 'numeric' })

  return (
    <>
      <PageHeader
        title={t('nav.campaigns')}
        description={t('campaign.hint')}
        actions={
          <>
            <Select
              ariaLabel={t('campaign.filterStatus')}
              value={status}
              onChange={(next) => {
                setStatus(next as CampaignStatus | '')
                setPage(0)
              }}
              options={[
                { value: '', label: t('campaign.allStatuses') },
                ...CAMPAIGN_STATUSES.map((s) => ({ value: s, label: t(`campaign.statuses.${s}`) })),
              ]}
            />
            <Link to="/admin/campaigns/new" className={buttonVariants({ size: 'lg' })}>
              <Plus />
              {t('campaign.new')}
            </Link>
          </>
        }
      />

      <DataTable>
        <THead>
          <Th>{t('campaign.name')}</Th>
          <Th>{t('campaign.state')}</Th>
          <Th>{t('campaign.contacts')}</Th>
          <Th align="right">{t('campaign.inFlight')}</Th>
          <Th>{t('campaign.did')}</Th>
          <Th>{t('campaign.createdAt')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('campaign.none')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="font-medium">
                <Link
                  to="/admin/campaigns/$campaignId"
                  params={{ campaignId: row.id }}
                  className="block text-foreground hover:text-primary"
                >
                  <CellText maxWidth="max-w-xs" minWidth="min-w-upto-24 xl:min-w-upto-56">{row.name}</CellText>
                </Link>
              </Td>
              <Td>
                <CampaignState campaign={row} />
              </Td>
              <Td>
                <ContactCounts counts={row.contacts} />
              </Td>
              <Td align="right" className="tabular">
                {row.status === 'RUNNING' ? (row.inFlight ?? 0) : '—'}
              </Td>
              <Td className="tabular text-xs text-muted-foreground">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.didNumber}</CellText>
              </Td>
              <Td className="tabular text-xs text-muted-foreground">
                {created.format(new Date(row.createdAt))}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>

      <Pager page={page} pageSize={PAGE_SIZE} total={total} onPage={setPage} />
    </>
  )
}
