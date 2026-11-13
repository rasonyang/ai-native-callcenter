import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTable, TBody, THead, Td, Th, Tr } from '@/components/table'
import { useCampaignOutcomeSummary, type Campaign } from '@/lib/campaigns'
import { describeError } from '@/lib/errors'

/**
 * What a campaign's calls came to (getCampaignOutcomeSummary): attempts and
 * contacts by outcome, contacts by lead, the two kinds of opt-out and the
 * callbacks its HIGH leads filed, by where they stand. Every number is a count
 * the server made from the stored records, in the contract's order with the
 * zeros; the screen adds up only a column's total.
 */
export function CampaignOutcomes({ campaign }: { campaign: Campaign }) {
  const { t } = useTranslation()
  const { data, isPending, isError, error } = useCampaignOutcomeSummary(campaign.id)

  if (isPending) return <p className="text-xs text-muted-foreground">{t('common.loading')}</p>
  if (isError || !data) return <p className="text-xs text-muted-foreground">{describeError(error, t)}</p>

  const lastOutcome = new Map(data.contactsByLastOutcome.map((row) => [row.outcome, row.count]))

  return (
    <div className="grid gap-4 xl:grid-cols-2">
      <Section title={t('campaign.summary.byOutcome')}>
        <DataTable>
          <THead>
            <Th>{t('campaign.outcome')}</Th>
            <Th align="right">{t('campaign.summary.attempts')}</Th>
            <Th align="right">{t('campaign.summary.contactsLastOutcome')}</Th>
          </THead>
          <TBody>
            {data.attemptsByOutcome.map((row) => (
              <Tr key={row.outcome}>
                <Td className="text-xs">{t(`campaign.outcomes.${row.outcome}`)}</Td>
                <Td align="right" className="tabular text-xs">{row.count}</Td>
                <Td align="right" className="tabular text-xs">{lastOutcome.get(row.outcome) ?? 0}</Td>
              </Tr>
            ))}
            <Tr className="font-medium">
              <Td className="text-xs">{t('campaign.summary.total')}</Td>
              <Td align="right" className="tabular text-xs">{columnTotal(data.attemptsByOutcome)}</Td>
              <Td align="right" className="tabular text-xs">{columnTotal(data.contactsByLastOutcome)}</Td>
            </Tr>
          </TBody>
        </DataTable>
      </Section>

      <div className="space-y-4">
        <Section title={t('campaign.summary.byLead')}>
          <DataTable>
            <THead>
              <Th>{t('campaign.leadIntent')}</Th>
              <Th align="right">{t('campaign.summary.contacts')}</Th>
            </THead>
            <TBody>
              {data.contactsByLeadIntent.map((row) => (
                <Tr key={row.leadIntent}>
                  <Td className="text-xs">{t(`campaign.intents.${row.leadIntent}`)}</Td>
                  <Td align="right" className="tabular text-xs">{row.count}</Td>
                </Tr>
              ))}
            </TBody>
          </DataTable>
        </Section>

        <Section title={t('campaign.summary.optOuts')} note={t('campaign.summary.optOutsNote')}>
          <DataTable>
            <THead>
              <Th>{t('campaign.summary.reason')}</Th>
              <Th align="right">{t('campaign.summary.contacts')}</Th>
            </THead>
            <TBody>
              <Tr>
                <Td className="text-xs">{t('campaign.summary.saidInConversation')}</Td>
                <Td align="right" className="tabular text-xs">{data.optOuts.saidInConversation}</Td>
              </Tr>
              <Tr>
                <Td className="text-xs">{t('campaign.summary.alreadyListed')}</Td>
                <Td align="right" className="tabular text-xs">{data.optOuts.alreadyListed}</Td>
              </Tr>
            </TBody>
          </DataTable>
        </Section>

        <Section title={t('campaign.summary.callbacks')}>
          <DataTable>
            <THead>
              <Th>{t('campaign.summary.callbackStatus')}</Th>
              <Th align="right">{t('campaign.summary.callbackCount')}</Th>
            </THead>
            <TBody>
              {data.callbacksByStatus.map((row) => (
                <Tr key={row.status}>
                  <Td className="text-xs">{t(`callbacks.statuses.${row.status}`)}</Td>
                  <Td align="right" className="tabular text-xs">{row.count}</Td>
                </Tr>
              ))}
              <Tr className="font-medium">
                <Td className="text-xs">{t('campaign.summary.total')}</Td>
                <Td align="right" className="tabular text-xs">{columnTotal(data.callbacksByStatus)}</Td>
              </Tr>
            </TBody>
          </DataTable>
        </Section>
      </div>
    </div>
  )
}

/** What a column adds up to. */
function columnTotal(rows: Array<{ count: number }>): number {
  return rows.reduce((total, row) => total + row.count, 0)
}

function Section({ title, note, children }: { title: string; note?: string; children: ReactNode }) {
  return (
    <section>
      <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">{title}</h2>
      {children}
      {note && <p className="mt-2 text-xs text-muted-foreground">{note}</p>}
    </section>
  )
}
