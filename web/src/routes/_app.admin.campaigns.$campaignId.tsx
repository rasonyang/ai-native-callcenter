import { Link, createFileRoute, useNavigate } from '@tanstack/react-router'
import { Pause, Play, Upload } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CampaignFormView } from '@/components/campaign-form'
import { CampaignImportDialog } from '@/components/campaign-import'
import { CampaignLeads } from '@/components/campaign-leads'
import { CampaignState } from '@/components/campaign-state'
import { ConfirmAction } from '@/components/confirm-action'
import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useNameThisPage } from '@/lib/breadcrumb'
import { useDIDs, useQueues } from '@/lib/catalog'
import {
  CAMPAIGN_OUTCOMES, CONTACT_STATUSES, hasCallRecord, LEAD_INTENTS, actionsFor, formFrom, isFinished, patchOf,
  useCampaign, useCampaignAttempts, useCampaignContacts, useCampaignMutations, useCampaignSettings,
  useInvalidateContacts, type Campaign, type CampaignContactStatus, type CampaignLeadIntent,
  type CampaignOutcome,
} from '@/lib/campaigns'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { formatDuration } from '@/lib/ledger'

/**
 * One campaign: its state and counters (live), its settings, its contacts,
 * the leads it produced and every attempt it made.
 */
export const Route = createFileRoute('/_app/admin/campaigns/$campaignId')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: CampaignDetail,
})

const PAGE_SIZE = 50

function CampaignDetail() {
  const { campaignId } = Route.useParams()
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { data, isPending, isError, error } = useCampaign(campaignId)
  const mutations = useCampaignMutations()
  const refreshContacts = useInvalidateContacts()
  const [isImporting, setIsImporting] = useState(false)

  useNameThisPage(data?.name)

  if (isPending) return <p className="text-xs text-muted-foreground">{t('common.loading')}</p>
  if (isError || !data) {
    return <p className="text-xs text-muted-foreground">{describeError(error, t)}</p>
  }

  const actions = actionsFor(data.status)
  const failure = [
    mutations.start, mutations.pause, mutations.resume, mutations.cancel, mutations.remove,
  ].find((m) => m.isError)?.error
  const clearFailures = () => {
    for (const m of [mutations.start, mutations.pause, mutations.resume, mutations.cancel, mutations.remove]) m.reset()
  }

  return (
    <>
      <PageHeader
        title={data.name}
        description={`${data.didNumber} · ${data.timeZone}`}
        actions={
          <>
            {!isFinished(data.status) && (
              <Button size="lg" variant="outline" onClick={() => setIsImporting(true)}>
                <Upload />
                {t('campaign.import.open')}
              </Button>
            )}
            {actions.start && (
              <Button
                size="lg"
                disabled={mutations.start.isPending || data.contacts.active === 0}
                title={data.contacts.active === 0 ? t('campaign.startNeedsContacts') : undefined}
                onClick={() => {
                  clearFailures()
                  mutations.start.mutate(data.id)
                }}
              >
                <Play />
                {t('campaign.actions.start')}
              </Button>
            )}
            {actions.pause && (
              <Button
                size="lg"
                variant="outline"
                disabled={mutations.pause.isPending}
                onClick={() => {
                  clearFailures()
                  mutations.pause.mutate(data.id)
                }}
              >
                <Pause />
                {t('campaign.actions.pause')}
              </Button>
            )}
            {actions.resume && (
              <Button
                size="lg"
                disabled={mutations.resume.isPending}
                onClick={() => {
                  clearFailures()
                  mutations.resume.mutate(data.id)
                }}
              >
                <Play />
                {t('campaign.actions.resume')}
              </Button>
            )}
            {actions.cancel && (
              <ConfirmAction
                trigger={t('campaign.actions.cancel')}
                message={t('campaign.actions.cancelConfirm', { name: data.name })}
                confirmLabel={t('campaign.actions.cancelConfirmButton')}
                disabled={mutations.cancel.isPending}
                onConfirm={() => {
                  clearFailures()
                  mutations.cancel.mutate(data.id)
                }}
              />
            )}
            {actions.remove && (
              <ConfirmAction
                trigger={t('common.delete')}
                message={t('campaign.actions.deleteConfirm', { name: data.name })}
                confirmLabel={t('common.delete')}
                disabled={mutations.remove.isPending}
                onConfirm={() => {
                  clearFailures()
                  mutations.remove.mutate(data.id, {
                    onSuccess: () => void navigate({ to: '/admin/campaigns' }),
                  })
                }}
              />
            )}
          </>
        }
      />

      <div className="mb-4 space-y-2">
        <div>
          <CampaignState campaign={data} />
        </div>
        <PauseNotice campaign={data} onResume={() => mutations.resume.mutate(data.id)} />
        {Boolean(failure) && (
          <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
            {describeError(failure, t)}
          </p>
        )}
      </div>

      <div className="mb-4 grid grid-cols-4 gap-4 xl:grid-cols-7">
        <KpiCard label={t('campaign.counts.total')} value={data.contacts.total} />
        <KpiCard label={t('campaign.counts.active')} value={data.contacts.active} />
        <KpiCard label={t('campaign.counts.done')} value={data.contacts.done} />
        <KpiCard label={t('campaign.counts.exhausted')} value={data.contacts.exhausted} />
        <KpiCard label={t('campaign.counts.invalidNumber')} value={data.contacts.invalidNumber} />
        <KpiCard label={t('campaign.counts.dnc')} value={data.contacts.dnc} />
        <KpiCard
          label={t('campaign.inFlight')}
          value={data.status === 'RUNNING' ? (data.inFlight ?? 0) : '—'}
        />
      </div>

      <Tabs defaultValue="contacts">
        <TabsList className="px-0">
          <TabsTrigger value="contacts">{t('campaign.tabs.contacts')}</TabsTrigger>
          <TabsTrigger value="leads">{t('campaign.tabs.leads')}</TabsTrigger>
          <TabsTrigger value="attempts">{t('campaign.tabs.attempts')}</TabsTrigger>
          <TabsTrigger value="settings">{t('campaign.tabs.settings')}</TabsTrigger>
        </TabsList>
        <TabsContent value="contacts" className="pt-4">
          <ContactsTab campaign={data} />
        </TabsContent>
        <TabsContent value="leads" className="pt-4">
          <CampaignLeads campaign={data} />
        </TabsContent>
        <TabsContent value="attempts" className="pt-4">
          <AttemptsTab campaign={data} />
        </TabsContent>
        <TabsContent value="settings" className="pt-4">
          <SettingsTab key={data.id} campaign={data} />
        </TabsContent>
      </Tabs>

      <CampaignImportDialog
        campaignId={data.id}
        flowFields={data.flowFields}
        open={isImporting}
        onOpenChange={setIsImporting}
        onImported={() => refreshContacts(data.id)}
      />
    </>
  )
}

/**
 * Why a campaign is paused. A person's pause says who and how to continue; a
 * breaker's says what failed and that nothing will resume it but a person.
 */
function PauseNotice({ campaign, onResume }: { campaign: Campaign; onResume: () => void }) {
  const { t } = useTranslation()
  if (campaign.status !== 'PAUSED' || !campaign.pauseReason) return null
  const isBreaker = campaign.pauseReason !== 'MANUAL'
  return (
    <div
      role="status"
      className="flex items-center justify-between gap-4 rounded-md border bg-card p-3 text-sm"
      style={isBreaker ? { borderColor: 'var(--state-breach)' } : undefined}
    >
      <div>
        <div className="font-medium">{t(`campaign.pauseReasons.${campaign.pauseReason}`)}</div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          {t(`campaign.pauseExplain.${campaign.pauseReason}`)}
        </div>
      </div>
      <Button size="sm" onClick={onResume}>
        {t('campaign.actions.resume')}
      </Button>
    </div>
  )
}

function SettingsTab({ campaign }: { campaign: Campaign }) {
  const { t } = useTranslation()
  const dids = useDIDs()
  const queues = useQueues()
  const settings = useCampaignSettings()
  const { update } = useCampaignMutations()
  const [form, setForm] = useState(() => formFrom(campaign))
  const [isSaved, setIsSaved] = useState(false)

  return (
    <div>
      <CampaignFormView
        value={form}
        onChange={(next) => {
          setIsSaved(false)
          setForm(next)
        }}
        campaign={campaign}
        settings={settings.data}
        dids={dids.data?.items ?? []}
        queues={queues.data?.items ?? []}
        error={update.error}
        isSaving={update.isPending}
        submitLabel={t('common.save')}
        onSubmit={() => {
          const body = patchOf(campaign, form)
          if (Object.keys(body).length === 0) {
            setIsSaved(true)
            return
          }
          update.mutate(
            { id: campaign.id, body },
            {
              onSuccess: (saved) => {
                setForm(formFrom(saved))
                setIsSaved(true)
              },
            },
          )
        }}
      />
      {isSaved && !update.isError && (
        <p role="status" className="mt-3 text-xs text-muted-foreground">
          {t('campaign.form.saved')}
        </p>
      )}
    </div>
  )
}

function ContactsTab({ campaign }: { campaign: Campaign }) {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState<CampaignContactStatus | ''>('')
  const [intent, setIntent] = useState<CampaignLeadIntent | ''>('')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCampaignContacts(campaign.id, {
    status: status || undefined,
    leadIntent: intent || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const time = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <>
      <div className="mb-3 flex gap-2">
        <Select
          ariaLabel={t('campaign.contactStatus')}
          value={status}
          onChange={(next) => {
            setStatus(next as CampaignContactStatus | '')
            setPage(0)
          }}
          options={[
            { value: '', label: t('campaign.allContactStatuses') },
            ...CONTACT_STATUSES.map((s) => ({ value: s, label: t(`campaign.contactStatuses.${s}`) })),
          ]}
        />
        <Select
          ariaLabel={t('campaign.leadIntent')}
          value={intent}
          onChange={(next) => {
            setIntent(next as CampaignLeadIntent | '')
            setPage(0)
          }}
          options={[
            { value: '', label: t('campaign.allIntents') },
            ...LEAD_INTENTS.map((s) => ({ value: s, label: t(`campaign.intents.${s}`) })),
          ]}
        />
      </div>
      <DataTable>
        <THead>
          <Th>{t('campaign.number')}</Th>
          <Th>{t('campaign.contactStatus')}</Th>
          <Th align="right">{t('campaign.attemptsCount')}</Th>
          <Th>{t('campaign.lastOutcome')}</Th>
          <Th>{t('campaign.nextAttempt')}</Th>
          <Th>{t('campaign.leadIntent')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('campaign.noContacts')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular" >
                <span title={row.numberRaw}>{row.number}</span>
              </Td>
              <Td className="text-xs">{t(`campaign.contactStatuses.${row.status}`)}</Td>
              <Td align="right" className="tabular text-xs">
                {row.attempts}
                {row.systemFailures > 0 && (
                  <span className="text-muted-foreground" title={t('campaign.systemFailures')}>
                    {' '}+{row.systemFailures}
                  </span>
                )}
              </Td>
              <Td className="text-xs text-muted-foreground">
                {row.lastOutcome ? t(`campaign.outcomes.${row.lastOutcome}`) : '—'}
              </Td>
              <Td className="tabular text-xs text-muted-foreground">
                {row.nextAttemptAt ? time.format(new Date(row.nextAttemptAt)) : '—'}
              </Td>
              <Td className="text-xs">
                {row.leadIntent ? t(`campaign.intents.${row.leadIntent}`) : '—'}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <Pager page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
    </>
  )
}

function AttemptsTab({ campaign }: { campaign: Campaign }) {
  const { t, i18n } = useTranslation()
  const [outcome, setOutcome] = useState<CampaignOutcome | ''>('')
  const [page, setPage] = useState(0)
  const { data, isPending, isError, error } = useCampaignAttempts(campaign.id, {
    outcome: outcome || undefined,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const time = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })

  return (
    <>
      <div className="mb-3 flex gap-2">
        <Select
          ariaLabel={t('campaign.outcome')}
          value={outcome}
          onChange={(next) => {
            setOutcome(next as CampaignOutcome | '')
            setPage(0)
          }}
          options={[
            { value: '', label: t('campaign.allOutcomes') },
            ...CAMPAIGN_OUTCOMES.map((o) => ({ value: o, label: t(`campaign.outcomes.${o}`) })),
          ]}
        />
      </div>
      <DataTable>
        <THead>
          <Th>{t('campaign.startedAt')}</Th>
          <Th>{t('campaign.number')}</Th>
          <Th>{t('campaign.outcome')}</Th>
          <Th>{t('campaign.hangupCause')}</Th>
          <Th align="right">{t('campaign.talkTime')}</Th>
          <Th align="right">{t('campaign.call')}</Th>
        </THead>
        <TBody>
          {isPending && <TableMessage colSpan={6}>{t('common.loading')}</TableMessage>}
          {isError && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {!isPending && !isError && rows.length === 0 && (
            <TableMessage colSpan={6}>{t('campaign.noAttempts')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular text-xs">{time.format(new Date(row.startedAt))}</Td>
              <Td className="tabular">{row.number}</Td>
              <Td className="text-xs">
                {row.outcome ? t(`campaign.outcomes.${row.outcome}`) : t('campaign.attemptStates.DIALING')}
              </Td>
              <Td className="text-xs text-muted-foreground">{row.hangupCause || '—'}</Td>
              <Td align="right" className="tabular text-xs">
                {row.talkSec === undefined ? '—' : formatDuration(row.talkSec)}
              </Td>
              <Td align="right" className="text-xs">
                {row.state === 'FINISHED' && hasCallRecord(row.outcome) ? (
                  <Link
                    to="/admin/cdr/$callId"
                    params={{ callId: row.callId }}
                    className="text-primary hover:underline"
                  >
                    {t('campaign.openCall')}
                  </Link>
                ) : (
                  '—'
                )}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <Pager page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
    </>
  )
}
