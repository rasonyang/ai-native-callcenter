import { Link } from '@tanstack/react-router'
import { Download } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { exportLeads, leadsFileName } from '@/lib/campaign-export'
import {
  LEAD_INTENTS, campaignApi, isPlacedCall, useCampaignContacts, type Campaign, type CampaignContact,
  type CampaignLeadIntent, type ContactQuery,
} from '@/lib/campaigns'
import { describeError } from '@/lib/errors'
import { ledgerApi, useCallbackOfCall } from '@/lib/ledger'
import { useCsvExport } from '@/lib/use-csv-export'

const PAGE_SIZE = 50

/**
 * Leads: the contacts the bot rated, by intent (design 10 section 14). Each
 * row links to the contact's last call by lastCallId, when its last outcome
 * says the call took place. A HIGH lead also shows the status of the callback
 * that call filed, resolved with GET /callbacks?callId= and never by phone
 * number, which one customer's several calls and campaigns share. The status
 * is text only: working a callback is an agent's job on their own screen.
 *
 * The intent and the page (counted from 0) are the caller's, which keeps them
 * in the address.
 *
 * Export writes every lead the filter matches, all pages, as a CSV file
 * (campaign-export.ts). The table and the export read the same filter object,
 * so a filter added to the tab narrows both.
 */
export function CampaignLeads({ campaign, intent, page, onChange }: {
  campaign: Campaign
  intent: CampaignLeadIntent
  page: number
  onChange: (next: { intent: CampaignLeadIntent; page: number }) => void
}) {
  const { t } = useTranslation()
  const filter = { leadIntent: intent } satisfies ContactQuery
  const { data, isPending, isError, error } = useCampaignContacts(campaign.id, {
    ...filter,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const csv = useCsvExport()

  const startExport = () =>
    void csv.start(leadsFileName(campaign, filter), (signal, onProgress) =>
      exportLeads({
        fetchPage: ({ offset, limit }, pageSignal) =>
          campaignApi.contacts(campaign.id, { ...filter, limit, offset }, pageSignal),
        fetchCallbackStatus: async (callId, lookupSignal) =>
          (await ledgerApi.callbacksOfCall(callId, lookupSignal)).items[0]?.status,
        countryCode: campaign.defaultCountryCode,
        signal,
        onProgress,
      }),
    )

  return (
    <>
      <div className="mb-3 flex items-center gap-2">
        <Select
          ariaLabel={t('campaign.leadIntent')}
          value={intent}
          onChange={(next) => onChange({ intent: next as CampaignLeadIntent, page: 0 })}
          options={LEAD_INTENTS.map((s) => ({ value: s, label: t(`campaign.intents.${s}`) }))}
        />
        {csv.state.phase === 'running' ? (
          <>
            <span role="status" className="shrink-0 tabular text-xs whitespace-nowrap text-muted-foreground">
              {csv.state.progress
                ? t('campaign.export.progress', { ...csv.state.progress })
                : t('campaign.export.starting')}
            </span>
            <Button variant="outline" className="shrink-0" onClick={csv.cancel}>
              {t('campaign.export.cancel')}
            </Button>
          </>
        ) : (
          <Button
            variant="outline"
            className="shrink-0"
            title={t('campaign.export.hint')}
            onClick={startExport}
          >
            <Download />
            {t('campaign.export.start')}
          </Button>
        )}
      </div>
      {csv.state.phase === 'failed' && (
        <p role="alert" className="mb-3 text-xs" style={{ color: 'var(--state-breach)' }}>
          {t('campaign.export.failed', { reason: describeError(csv.state.error, t) })}
        </p>
      )}
      <DataTable>
        <THead>
          <Th>{t('campaign.number')}</Th>
          <Th>{t('campaign.leadIntent')}</Th>
          <Th>{t('campaign.leadNotes')}</Th>
          <Th>{t('campaign.callback')}</Th>
          <Th align="right">{t('campaign.call')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={5}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={5}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={5}>{t('campaign.noLeads')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.number}</CellText>
              </Td>
              <Td className="text-xs">{row.leadIntent ? t(`campaign.intents.${row.leadIntent}`) : '—'}</Td>
              <Td className="text-xs text-muted-foreground">
                <CellText maxWidth="max-w-96">{row.leadNotes || '—'}</CellText>
              </Td>
              <Td className="text-xs text-muted-foreground">
                {row.leadIntent === 'HIGH' && row.lastCallId ? <CallbackStatus callId={row.lastCallId} /> : '—'}
              </Td>
              <Td align="right" className="text-xs">
                <LastCallLink contact={row} />
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <Pager
        page={page}
        pageSize={PAGE_SIZE}
        total={data?.total ?? 0}
        onPage={(next) => onChange({ intent, page: next })}
      />
    </>
  )
}

/** The status of the callback a call filed, or a dash when it filed none. */
function CallbackStatus({ callId }: { callId: string }) {
  const { t } = useTranslation()
  const { data } = useCallbackOfCall({ callId })
  return <>{data ? t(`callbacks.statuses.${data.status}`) : '—'}</>
}

function LastCallLink({ contact }: { contact: CampaignContact }) {
  const { t } = useTranslation()
  if (!contact.lastCallId || !isPlacedCall(contact.lastOutcome)) return <>—</>
  return (
    <Link
      to="/admin/cdr/$callId"
      params={{ callId: contact.lastCallId }}
      className="text-primary hover:underline"
    >
      {t('campaign.openCall')}
    </Link>
  )
}
