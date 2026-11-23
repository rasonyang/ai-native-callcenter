import { Link, createFileRoute, useNavigate } from '@tanstack/react-router'
import { Pause, Play, Upload } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CampaignFormView } from '@/components/campaign-form'
import { CampaignImportDialog } from '@/components/campaign-import'
import { CampaignLeads } from '@/components/campaign-leads'
import { CampaignOutcomes } from '@/components/campaign-outcomes'
import { CampaignState } from '@/components/campaign-state'
import { ConfirmAction } from '@/components/confirm-action'
import { KpiCard } from '@/components/kpi-card'
import { NumberSearch } from '@/components/number-search'
import { PageHeader } from '@/components/page-header'
import { Pager } from '@/components/pager'
import { Select } from '@/components/record-dialog'
import { CellText, DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { Badge } from '@/components/ui/badge'
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
import { describeError, fieldErrorText } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import { formatDuration } from '@/lib/ledger'

/**
 * One campaign: its state and counters (live), its settings, its contacts,
 * the leads it produced, every attempt it made and what they came to.
 */
export const Route = createFileRoute('/_app/admin/campaigns/$campaignId')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  validateSearch: campaignDetailSearch,
  component: CampaignDetail,
})

const PAGE_SIZE = 50

const CAMPAIGN_TABS = ['contacts', 'leads', 'attempts', 'outcomes', 'settings'] as const
type CampaignTab = (typeof CAMPAIGN_TABS)[number]

/**
 * The open tab, its filter and its page, kept in the address so that Back from
 * a call record, a reload and a shared link all land where the reader was.
 * Every default is left out: no tab means Contacts, no page the first page,
 * no filter every row (on Leads, HIGH). `page` counts from 1. `number` is the
 * phone number searched on Contacts, Leads or Attempts, as it was typed.
 */
interface CampaignDetailSearch {
  tab?: Exclude<CampaignTab, 'contacts'>
  status?: CampaignContactStatus
  intent?: CampaignLeadIntent
  outcome?: CampaignOutcome
  number?: string
  page?: number
}

const oneOf = <T extends string>(values: readonly T[], raw: unknown): T | undefined =>
  values.find((value) => value === raw)

/** The longest searched number the address keeps, as the search box does. */
const MAX_NUMBER_LENGTH = 64

/**
 * A searched number from the address, trimmed. The router reads a value of
 * digits alone ("13800138000") as a JSON number, so a whole number is taken
 * back as its digits; anything else that is not text is dropped.
 */
function searchedNumber(raw: unknown): string | undefined {
  if (typeof raw === 'number') return Number.isSafeInteger(raw) && raw > 0 ? String(raw) : undefined
  if (typeof raw !== 'string') return undefined
  const value = raw.trim()
  return value !== '' && value.length <= MAX_NUMBER_LENGTH ? value : undefined
}

/**
 * Reads the address. Whatever it does not understand is dropped, never an
 * error, and a filter the open tab does not have is dropped with it. Every
 * key is returned, set or undefined: the router lays this over the raw query,
 * so a key left out would let the raw value through.
 */
export function campaignDetailSearch(raw: Record<string, unknown>): CampaignDetailSearch {
  const tab = oneOf(CAMPAIGN_TABS, raw.tab) ?? 'contacts'
  const intent = oneOf(LEAD_INTENTS, raw.intent)
  const isPaged = tab === 'contacts' || tab === 'leads' || tab === 'attempts'
  return {
    tab: tab === 'contacts' ? undefined : tab,
    status: tab === 'contacts' ? oneOf(CONTACT_STATUSES, raw.status) : undefined,
    intent: tab === 'contacts' || (tab === 'leads' && intent !== 'HIGH') ? intent : undefined,
    outcome: tab === 'attempts' ? oneOf(CAMPAIGN_OUTCOMES, raw.outcome) : undefined,
    number: isPaged ? searchedNumber(raw.number) : undefined,
    page: isPaged && typeof raw.page === 'number' && Number.isSafeInteger(raw.page) && raw.page > 1
      ? raw.page
      : undefined,
  }
}

/** What a table tab shows: its filter and its page, counted from 0 as the tables count. */
interface TabView {
  status?: CampaignContactStatus
  intent?: CampaignLeadIntent
  outcome?: CampaignOutcome
  number?: string
  page: number
}

function CampaignDetail() {
  const { campaignId } = Route.useParams()
  const search = Route.useSearch()
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

  const view: TabView = {
    status: search.status, intent: search.intent, outcome: search.outcome, number: search.number,
    page: (search.page ?? 1) - 1,
  }
  // A move inside the page replaces the entry, so one Back still leaves the
  // campaign. Switching tab passes no view: the new tab opens unfiltered on
  // its first page.
  const show = (tab: string, next: Partial<TabView> = {}) =>
    void navigate({
      to: '/admin/campaigns/$campaignId',
      params: { campaignId },
      search: campaignDetailSearch({ ...next, tab, page: (next.page ?? 0) + 1 }),
      replace: true,
      resetScroll: false,
    })

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

      <Tabs value={search.tab ?? 'contacts'} onValueChange={(tab) => show(tab)}>
        <TabsList className="px-0">
          <TabsTrigger value="contacts">{t('campaign.tabs.contacts')}</TabsTrigger>
          <TabsTrigger value="leads">{t('campaign.tabs.leads')}</TabsTrigger>
          <TabsTrigger value="attempts">{t('campaign.tabs.attempts')}</TabsTrigger>
          <TabsTrigger value="outcomes">{t('campaign.tabs.outcomes')}</TabsTrigger>
          <TabsTrigger value="settings">{t('campaign.tabs.settings')}</TabsTrigger>
        </TabsList>
        <TabsContent value="contacts" className="pt-4">
          <ContactsTab campaign={data} view={view} onView={(next) => show('contacts', next)} />
        </TabsContent>
        <TabsContent value="leads" className="pt-4">
          <CampaignLeads
            campaign={data}
            intent={view.intent ?? 'HIGH'}
            number={view.number}
            page={view.page}
            onChange={(next) => show('leads', next)}
          />
        </TabsContent>
        <TabsContent value="attempts" className="pt-4">
          <AttemptsTab campaign={data} view={view} onView={(next) => show('attempts', next)} />
        </TabsContent>
        <TabsContent value="outcomes" className="pt-4">
          <CampaignOutcomes campaign={data} />
        </TabsContent>
        <TabsContent value="settings" className="pt-4">
          <SettingsTab key={data.id} campaign={data} />
        </TabsContent>
      </Tabs>

      <CampaignImportDialog
        campaignId={data.id}
        campaignName={data.name}
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

export function ContactsTab({ campaign, view, onView }: {
  campaign: Campaign
  view: TabView
  onView: (next: TabView) => void
}) {
  const { t, i18n } = useTranslation()
  const { status, intent, number, page } = view
  const { data, isPending, isError, error } = useCampaignContacts(campaign.id, {
    status,
    leadIntent: intent,
    phoneNumber: number,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const isRefused = Boolean(fieldErrorText(error, 'phoneNumber', t))
  const time = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })

  return (
    <>
      <div className="mb-3 flex items-start gap-2">
        <NumberSearch value={number} error={error} onSearch={(next) => onView({ ...view, number: next, page: 0 })} />
        <Select
          ariaLabel={t('campaign.contactStatus')}
          value={status ?? ''}
          onChange={(next) => onView({ ...view, status: oneOf(CONTACT_STATUSES, next), page: 0 })}
          options={[
            { value: '', label: t('campaign.allContactStatuses') },
            ...CONTACT_STATUSES.map((s) => ({ value: s, label: t(`campaign.contactStatuses.${s}`) })),
          ]}
        />
        <Select
          ariaLabel={t('campaign.leadIntent')}
          value={intent ?? ''}
          onChange={(next) => onView({ ...view, intent: oneOf(LEAD_INTENTS, next), page: 0 })}
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
          {isError && !isRefused && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {((!isPending && !isError && rows.length === 0) || isRefused) && (
            <TableMessage colSpan={6}>{t(number ? 'campaign.search.noContacts' : 'campaign.noContacts')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-32" title={row.numberRaw}>
                  {row.number}
                </CellText>
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
                {row.frequencyHoldEndsAt
                  ? t('campaign.frequencyHeld', { when: time.format(new Date(row.frequencyHoldEndsAt)) })
                  : row.nextAttemptAt
                    ? time.format(new Date(row.nextAttemptAt))
                    : '—'}
              </Td>
              <Td className="text-xs">
                {row.leadIntent ? t(`campaign.intents.${row.leadIntent}`) : '—'}
              </Td>
            </Tr>
          ))}
        </TBody>
      </DataTable>
      <Pager
        page={page}
        pageSize={PAGE_SIZE}
        total={data?.total ?? 0}
        onPage={(next) => onView({ ...view, page: next })}
      />
    </>
  )
}

export function AttemptsTab({ campaign, view, onView }: {
  campaign: Campaign
  view: TabView
  onView: (next: TabView) => void
}) {
  const { t, i18n } = useTranslation()
  const { outcome, number, page } = view
  const { data, isPending, isError, error } = useCampaignAttempts(campaign.id, {
    outcome,
    phoneNumber: number,
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  })
  const rows = data?.items ?? []
  const isRefused = Boolean(fieldErrorText(error, 'phoneNumber', t))
  const time = new Intl.DateTimeFormat(i18n.language, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })

  return (
    <>
      <div className="mb-3 flex items-start gap-2">
        <NumberSearch value={number} error={error} onSearch={(next) => onView({ ...view, number: next, page: 0 })} />
        <Select
          ariaLabel={t('campaign.outcome')}
          value={outcome ?? ''}
          onChange={(next) => onView({ ...view, outcome: oneOf(CAMPAIGN_OUTCOMES, next), page: 0 })}
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
          {isError && !isRefused && <TableMessage colSpan={6}>{describeError(error, t)}</TableMessage>}
          {((!isPending && !isError && rows.length === 0) || isRefused) && (
            <TableMessage colSpan={6}>{t(number ? 'campaign.search.noAttempts' : 'campaign.noAttempts')}</TableMessage>
          )}
          {rows.map((row) => (
            <Tr key={row.id}>
              <Td className="tabular text-xs">{time.format(new Date(row.startedAt))}</Td>
              <Td className="tabular">
                <span className="flex items-center gap-2">
                  <CellText maxWidth="max-w-40" minWidth="min-w-upto-32">{row.number}</CellText>
                  {row.isTestNumber && <Badge>{t('campaign.testNumber')}</Badge>}
                </span>
              </Td>
              <Td className="text-xs">
                {row.outcome ? t(`campaign.outcomes.${row.outcome}`) : t('campaign.attemptStates.DIALING')}
              </Td>
              <Td className="text-xs text-muted-foreground">
                <CellText maxWidth="max-w-40" minWidth="min-w-upto-36">{row.hangupCause || '—'}</CellText>
              </Td>
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
      <Pager
        page={page}
        pageSize={PAGE_SIZE}
        total={data?.total ?? 0}
        onPage={(next) => onView({ ...view, page: next })}
      />
    </>
  )
}
