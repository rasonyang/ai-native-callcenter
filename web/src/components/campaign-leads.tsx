import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import {
  LEAD_INTENTS, isPlacedCall, useCampaignContacts, type Campaign, type CampaignContact,
  type CampaignLeadIntent,
} from '@/lib/campaigns'
import { describeError } from '@/lib/errors'
import { useCallbackOfCall } from '@/lib/ledger'

const PAGE_SIZE = 50

/**
 * Leads: the contacts the bot rated, by intent (design 10 section 14). Each
 * row links to the contact's last call by lastCallId, when its last outcome
 * says the call took place. A HIGH lead also shows the status of the callback
 * that call filed, resolved with GET /callbacks?callId= and never by phone
 * number, which one customer's several calls and campaigns share. The status
 * is text only: working a callback is an agent's job on their own screen.
 */
export function CampaignLeads({ campaign }: { campaign: Campaign }) {
  const { t } = useTranslation()
  const [intent, setIntent] = useState<CampaignLeadIntent>('HIGH')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCampaignContacts(campaign.id, {
    leadIntent: intent,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []

  return (
    <>
      <div className="mb-3 flex gap-2">
        <Select
          ariaLabel={t('campaign.leadIntent')}
          value={intent}
          onChange={(next) => {
            setIntent(next as CampaignLeadIntent)
            setPage(0)
          }}
          options={LEAD_INTENTS.map((s) => ({ value: s, label: t(`campaign.intents.${s}`) }))}
        />
      </div>
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
      <Pager page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
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
