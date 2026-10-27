import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { TFunction } from 'i18next'

import type { components } from '@/generated/api'

import { ApiError, request, type ErrorCode } from './api'

/**
 * AI outbound campaigns: wire types, REST calls, the pure rules the screens
 * share (field locks, the activity sentence, the error mapping) and the cache
 * update the event stream drives. Every type comes from the generated
 * contract.
 */

export type Campaign = components['schemas']['Campaign']
export type CampaignWrite = components['schemas']['CampaignWrite']
export type CampaignPatch = components['schemas']['CampaignPatch']
export type CampaignRetryPolicyWrite = components['schemas']['CampaignRetryPolicyWrite']
export type CampaignSettings = components['schemas']['CampaignSettings']
export type CampaignStatus = components['schemas']['CampaignStatus']
export type CampaignPauseReason = components['schemas']['CampaignPauseReason']
export type CampaignActivity = components['schemas']['CampaignActivity']
export type CampaignContactCounts = components['schemas']['CampaignContactCounts']
export type CampaignCallingWindow = components['schemas']['CampaignCallingWindow']
export type CampaignTimeRange = components['schemas']['CampaignTimeRange']
export type CampaignRetryPolicy = components['schemas']['CampaignRetryPolicy']
export type CampaignRetryOutcome = components['schemas']['CampaignRetryOutcome']
export type CampaignContact = components['schemas']['CampaignContact']
export type CampaignContactStatus = components['schemas']['CampaignContactStatus']
export type CampaignLeadIntent = components['schemas']['CampaignLeadIntent']
export type CampaignOutcome = components['schemas']['CampaignOutcome']
export type CampaignAttempt = components['schemas']['CampaignAttempt']
export type CampaignContactRow = components['schemas']['CampaignContactRow']
export type CampaignImportResult = components['schemas']['CampaignImportResult']
export type CampaignImportRowResult = components['schemas']['CampaignImportRowResult']
export type CampaignImportRowCode = components['schemas']['CampaignImportRowCode']
export type Weekday = components['schemas']['Weekday']
export type SseCampaignUpdatedPayload = components['schemas']['SseCampaignUpdatedPayload']

export const CAMPAIGN_STATUSES: CampaignStatus[] = [
  'DRAFT', 'RUNNING', 'PAUSED', 'COMPLETED', 'CANCELLED',
]
export const CONTACT_STATUSES: CampaignContactStatus[] = [
  'ACTIVE', 'DONE', 'EXHAUSTED', 'INVALID_NUMBER', 'DNC',
]
export const LEAD_INTENTS: CampaignLeadIntent[] = ['HIGH', 'MEDIUM', 'LOW', 'NONE', 'DO_NOT_CALL']
export const CAMPAIGN_OUTCOMES: CampaignOutcome[] = [
  'ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN', 'TRANSIENT',
  'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED',
]
export const WEEKDAYS: Weekday[] = ['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN']
export const RETRY_OUTCOMES: CampaignRetryOutcome[] = [
  'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN', 'TRANSIENT', 'ORIGINATE_FAILED',
]
/** The outcomes whose retry waits on intervalSec; the other two use the backoff. */
export const INTERVAL_OUTCOMES = ['NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN'] as const
export type IntervalOutcome = (typeof INTERVAL_OUTCOMES)[number]

/** The most contacts the import operation takes in one request. */
export const IMPORT_CHUNK_SIZE = 5000

// --- REST -----------------------------------------------------------------

function query(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

export interface CampaignFilter {
  status?: CampaignStatus
  limit?: number
  offset?: number
}

export interface ContactFilter {
  status?: CampaignContactStatus
  leadIntent?: CampaignLeadIntent
  limit?: number
  offset?: number
}

export interface AttemptFilter {
  contactId?: string
  outcome?: CampaignOutcome
  limit?: number
  offset?: number
}

export const campaignApi = {
  list: (filter: CampaignFilter) =>
    request<components['schemas']['CampaignList']>(`/campaigns${query({ ...filter })}`),
  get: (id: string) => request<Campaign>(`/campaigns/${id}`),
  settings: () => request<CampaignSettings>('/campaigns/settings'),
  create: (body: CampaignWrite) =>
    request<Campaign>('/campaigns', { method: 'POST', body: JSON.stringify(body) }),
  update: (id: string, body: CampaignPatch) =>
    request<Campaign>(`/campaigns/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  remove: (id: string) => request<void>(`/campaigns/${id}`, { method: 'DELETE' }),
  start: (id: string) => request<Campaign>(`/campaigns/${id}/start`, { method: 'POST' }),
  pause: (id: string) => request<Campaign>(`/campaigns/${id}/pause`, { method: 'POST' }),
  resume: (id: string) => request<Campaign>(`/campaigns/${id}/resume`, { method: 'POST' }),
  cancel: (id: string) => request<Campaign>(`/campaigns/${id}/cancel`, { method: 'POST' }),
  contacts: (id: string, filter: ContactFilter) =>
    request<components['schemas']['CampaignContactList']>(
      `/campaigns/${id}/contacts${query({ ...filter })}`,
    ),
  importContacts: (id: string, contacts: CampaignContactRow[]) =>
    request<CampaignImportResult>(`/campaigns/${id}/contacts`, {
      method: 'POST',
      body: JSON.stringify({ contacts }),
    }),
  attempts: (id: string, filter: AttemptFilter) =>
    request<components['schemas']['CampaignAttemptList']>(
      `/campaigns/${id}/attempts${query({ ...filter })}`,
    ),
}

// --- Cache keys and queries -------------------------------------------------

export const CAMPAIGNS_KEY = ['campaigns'] as const
const LIST_KEY = ['campaigns', 'list'] as const
export const campaignKey = (id: string) => ['campaigns', 'detail', id] as const
const contactsKey = (id: string) => ['campaigns', 'contacts', id] as const
const attemptsKey = (id: string) => ['campaigns', 'attempts', id] as const

export function useCampaigns(filter: CampaignFilter) {
  return useQuery({
    queryKey: [...LIST_KEY, filter],
    queryFn: () => campaignApi.list(filter),
  })
}

const SETTINGS_KEY = ['campaigns', 'settings'] as const

/**
 * The deployment's campaign rules: outer calling hours, defaults and limits.
 * They move only with the server's configuration, so they are kept for ten
 * minutes. The form works without them (no pre-validation, no placeholders).
 */
export function useCampaignSettings() {
  return useQuery({
    queryKey: SETTINGS_KEY,
    queryFn: campaignApi.settings,
    staleTime: 10 * 60_000,
  })
}

export function useCampaign(id: string) {
  return useQuery({ queryKey: campaignKey(id), queryFn: () => campaignApi.get(id) })
}

export function useCampaignContacts(id: string, filter: ContactFilter) {
  return useQuery({
    queryKey: [...contactsKey(id), filter],
    queryFn: () => campaignApi.contacts(id, filter),
  })
}

export function useCampaignAttempts(id: string, filter: AttemptFilter) {
  return useQuery({
    queryKey: [...attemptsKey(id), filter],
    queryFn: () => campaignApi.attempts(id, filter),
  })
}

/** Writes that change one campaign, each refreshing what it moved. */
export function useCampaignMutations() {
  const queryClient = useQueryClient()
  const store = (campaign: Campaign) => {
    queryClient.setQueryData(campaignKey(campaign.id), campaign)
    // Every cached list, mounted or not: the list page is not on screen while a
    // campaign is being created, and must not open on a copy without it.
    void queryClient.invalidateQueries({ queryKey: LIST_KEY, refetchType: 'all' })
  }
  return {
    create: useMutation({ mutationFn: campaignApi.create, onSuccess: store }),
    update: useMutation({
      mutationFn: ({ id, body }: { id: string; body: CampaignPatch }) =>
        campaignApi.update(id, body),
      onSuccess: store,
    }),
    remove: useMutation({
      mutationFn: campaignApi.remove,
      onSuccess: (_data, id) => {
        queryClient.removeQueries({ queryKey: campaignKey(id) })
        void queryClient.invalidateQueries({ queryKey: LIST_KEY })
      },
    }),
    start: useMutation({ mutationFn: campaignApi.start, onSuccess: store }),
    pause: useMutation({ mutationFn: campaignApi.pause, onSuccess: store }),
    resume: useMutation({ mutationFn: campaignApi.resume, onSuccess: store }),
    cancel: useMutation({ mutationFn: campaignApi.cancel, onSuccess: store }),
  }
}

export function useInvalidateContacts() {
  const queryClient = useQueryClient()
  return (id: string) => {
    void queryClient.invalidateQueries({ queryKey: contactsKey(id) })
    void queryClient.invalidateQueries({ queryKey: campaignKey(id) })
    void queryClient.invalidateQueries({ queryKey: LIST_KEY })
  }
}

// --- Live updates -------------------------------------------------------------

/**
 * Applies a CAMPAIGN_UPDATED payload to every cached copy of that campaign.
 *
 * The payload carries the fields that move (status, pause reason, activity,
 * next dial time, in-flight, counters) and not the ones an operator edits, so
 * it is merged over the cached record rather than replacing it. A field the
 * payload leaves out is absent on the campaign too (activity outside RUNNING,
 * pauseReason outside PAUSED), so it is cleared, not kept.
 *
 * The lists are refetched only when the event changes what they would show: a
 * campaign no cache holds yet is new, and one whose status changed may now
 * belong to a different filter. Nothing else is refetched on the list, so a
 * busy campaign costs no request.
 */
export function applyCampaignUpdated(
  queryClient: QueryClient,
  payload: SseCampaignUpdatedPayload,
): void {
  const merge = (campaign: Campaign): Campaign => ({
    ...campaign,
    status: payload.status,
    pauseReason: payload.pauseReason,
    activity: payload.activity,
    nextDialAt: payload.nextDialAt,
    inFlight: payload.inFlight,
    contacts: payload.contacts,
  })

  let isListed = false
  let hasStatusChanged = false
  queryClient.setQueriesData<components['schemas']['CampaignList']>(
    { queryKey: LIST_KEY },
    (list) => {
      if (!list) return list
      let touched = false
      const items = list.items.map((item) => {
        if (item.id !== payload.campaignId) return item
        touched = true
        if (item.status !== payload.status) hasStatusChanged = true
        return merge(item)
      })
      if (touched) isListed = true
      return touched ? { ...list, items } : list
    },
  )

  const detail = queryClient.getQueryData<Campaign>(campaignKey(payload.campaignId))
  if (detail) {
    if (detail.status !== payload.status) hasStatusChanged = true
    queryClient.setQueryData(campaignKey(payload.campaignId), merge(detail))
  }

  if (hasStatusChanged || (!isListed && !detail)) {
    void queryClient.invalidateQueries({ queryKey: LIST_KEY })
  }
  // The counters moved, so a contacts or attempts table that is on screen is
  // behind. Only a mounted table refetches; the rest wait until opened.
  void queryClient.invalidateQueries({ queryKey: contactsKey(payload.campaignId) })
  void queryClient.invalidateQueries({ queryKey: attemptsKey(payload.campaignId) })
}

/** Reads the payload out of a stream envelope, or undefined when it is not one. */
export function campaignPayloadOf(event: {
  payload?: Record<string, unknown>
}): SseCampaignUpdatedPayload | undefined {
  const payload = event.payload
  if (!payload || typeof payload.campaignId !== 'string' || typeof payload.status !== 'string') {
    return undefined
  }
  return payload as unknown as SseCampaignUpdatedPayload
}

// --- Field locks --------------------------------------------------------------

export type CampaignField =
  | 'name' | 'didId' | 'timeZone' | 'callingWindow' | 'maxConcurrent' | 'ringTimeoutSec'
  | 'retryPolicy' | 'defaultCountryCode' | 'followUpQueueId'

export const CAMPAIGN_FIELDS: CampaignField[] = [
  'name', 'didId', 'timeZone', 'callingWindow', 'maxConcurrent', 'ringTimeoutSec',
  'retryPolicy', 'defaultCountryCode', 'followUpQueueId',
]

/** What may still change in RUNNING and PAUSED (docs/design/10-campaigns.md section 14). */
const LIVE_EDITABLE: ReadonlySet<CampaignField> = new Set([
  'maxConcurrent', 'callingWindow', 'retryPolicy',
])

/**
 * Whether a field may be edited in this status, exactly as the API decides it:
 * every field in DRAFT, three in RUNNING and PAUSED, none once finished.
 */
export function isFieldEditable(status: CampaignStatus | undefined, field: CampaignField): boolean {
  if (status === undefined || status === 'DRAFT') return true
  if (status === 'RUNNING' || status === 'PAUSED') return LIVE_EDITABLE.has(field)
  return false
}

/**
 * Whether an attempt's call can have a CDR. A number skipped for the
 * do-not-call list or refused as invalid never reached the switch.
 * ORIGINATE_FAILED may or may not have a placement-failure record, so it keeps
 * its link and the call page copes with either.
 */
export function hasCallRecord(outcome: CampaignOutcome | undefined): boolean {
  return outcome !== 'DNC_SKIPPED' && outcome !== 'INVALID_NUMBER'
}

/**
 * Whether a contact's last outcome says its last call certainly took place,
 * so the call page has a record to show. Stricter than hasCallRecord: a lead
 * row links only to a call that rang, and ORIGINATE_FAILED (perhaps no record)
 * and UNKNOWN (reconciled because no record was found) do not promise one.
 */
export function isPlacedCall(outcome: CampaignOutcome | undefined): boolean {
  return outcome !== undefined && hasCallRecord(outcome) &&
    outcome !== 'ORIGINATE_FAILED' && outcome !== 'UNKNOWN'
}

export function isFinished(status: CampaignStatus): boolean {
  return status === 'COMPLETED' || status === 'CANCELLED'
}

/** Which lifecycle actions a status offers. */
export function actionsFor(status: CampaignStatus): {
  start: boolean; pause: boolean; resume: boolean; cancel: boolean; remove: boolean
} {
  return {
    start: status === 'DRAFT',
    pause: status === 'RUNNING',
    resume: status === 'PAUSED',
    cancel: status === 'RUNNING' || status === 'PAUSED',
    remove: status === 'DRAFT',
  }
}

// --- The form ---------------------------------------------------------------

/** Form state: numbers stay text until they are sent. */
export interface CampaignForm {
  name: string
  didId: string
  timeZone: string
  days: Weekday[]
  ranges: CampaignTimeRange[]
  maxConcurrent: string
  ringTimeoutSec: string
  defaultCountryCode: string
  followUpQueueId: string
  /** Create only: leave the retry policy to the deployment's defaults. */
  isDefaultRetry: boolean
  maxAttempts: string
  maxSystemRetries: string
  retryOutcomes: CampaignRetryOutcome[]
  /** Minutes, as the operator thinks of them; sent as seconds. */
  intervalMin: Record<IntervalOutcome, string>
  backoffBaseSec: string
  backoffFactor: string
  backoffCapSec: string
}

export const DEFAULT_WINDOW: CampaignCallingWindow = {
  days: [...WEEKDAYS],
  ranges: [{ start: '09:00', end: '18:00' }],
}

/**
 * The zone a new campaign starts in: the browser's own when the list of zones
 * the browser can name holds it, else UTC. A name outside that list would be
 * offered nowhere and is the kind of value the server rejects.
 */
export function defaultTimeZone(): string {
  const own = Intl.DateTimeFormat().resolvedOptions().timeZone
  let known: string[] = []
  try {
    known = Intl.supportedValuesOf('timeZone')
  } catch {
    return own || 'UTC'
  }
  return own && known.includes(own) ? own : 'UTC'
}

const minutesOf = (sec: number | undefined) => (sec === undefined ? '' : String(sec / 60))

export function retryFields(policy: CampaignRetryPolicy): Pick<
  CampaignForm,
  'maxAttempts' | 'maxSystemRetries' | 'retryOutcomes' | 'intervalMin' | 'backoffBaseSec' | 'backoffFactor' | 'backoffCapSec'
> {
  const interval = policy.intervalSec as Partial<Record<IntervalOutcome, number>>
  return {
    maxAttempts: String(policy.maxAttempts),
    maxSystemRetries: String(policy.maxSystemRetries),
    retryOutcomes: [...policy.retryOutcomes],
    intervalMin: {
      NO_ANSWER: minutesOf(interval.NO_ANSWER),
      BUSY: minutesOf(interval.BUSY),
      REJECTED: minutesOf(interval.REJECTED),
      UNKNOWN: minutesOf(interval.UNKNOWN),
    },
    backoffBaseSec: String(policy.transientBackoff.baseSec),
    backoffFactor: String(policy.transientBackoff.factor),
    backoffCapSec: String(policy.transientBackoff.capSec),
  }
}

/** Retry inputs before the operator asks to edit: the policy comes from the settings then. */
const EMPTY_RETRY_FIELDS: ReturnType<typeof retryFields> = {
  maxAttempts: '',
  maxSystemRetries: '',
  retryOutcomes: [],
  intervalMin: { NO_ANSWER: '', BUSY: '', REJECTED: '', UNKNOWN: '' },
  backoffBaseSec: '',
  backoffFactor: '',
  backoffCapSec: '',
}

export function blankForm(timeZone: string): CampaignForm {
  return {
    name: '',
    didId: '',
    timeZone,
    days: [...DEFAULT_WINDOW.days],
    ranges: DEFAULT_WINDOW.ranges.map((r) => ({ start: r.start, end: r.end })),
    maxConcurrent: '',
    ringTimeoutSec: '',
    defaultCountryCode: '',
    followUpQueueId: '',
    isDefaultRetry: true,
    ...EMPTY_RETRY_FIELDS,
  }
}

export function formFrom(campaign: Campaign): CampaignForm {
  return {
    name: campaign.name,
    didId: campaign.didId,
    timeZone: campaign.timeZone,
    days: [...campaign.callingWindow.days],
    ranges: campaign.callingWindow.ranges.map((r) => ({ ...r })),
    maxConcurrent: String(campaign.maxConcurrent),
    ringTimeoutSec: String(campaign.ringTimeoutSec),
    defaultCountryCode: campaign.defaultCountryCode ?? '',
    followUpQueueId: campaign.followUpQueueId ?? '',
    isDefaultRetry: false,
    ...retryFields(campaign.retryPolicy),
  }
}

const intOrUndefined = (text: string): number | undefined => {
  const trimmed = text.trim()
  if (trimmed === '') return undefined
  const value = Number(trimmed)
  return Number.isFinite(value) ? Math.round(value) : undefined
}

function retryPolicyOf(form: CampaignForm): CampaignRetryPolicyWrite {
  const intervalSec: Partial<Record<IntervalOutcome, number>> = {}
  for (const outcome of INTERVAL_OUTCOMES) {
    const minutes = intOrUndefinedFloat(form.intervalMin[outcome])
    if (minutes !== undefined) intervalSec[outcome] = Math.round(minutes * 60)
  }
  return {
    maxAttempts: intOrUndefined(form.maxAttempts),
    maxSystemRetries: intOrUndefined(form.maxSystemRetries),
    retryOutcomes: RETRY_OUTCOMES.filter((o) => form.retryOutcomes.includes(o)),
    intervalSec,
    transientBackoff: {
      baseSec: intOrUndefined(form.backoffBaseSec),
      factor: intOrUndefined(form.backoffFactor),
      capSec: intOrUndefined(form.backoffCapSec),
    },
  }
}

function intOrUndefinedFloat(text: string): number | undefined {
  const trimmed = text.trim()
  if (trimmed === '') return undefined
  const value = Number(trimmed)
  return Number.isFinite(value) ? value : undefined
}

export const windowOf = (form: CampaignForm): CampaignCallingWindow => ({
  days: WEEKDAYS.filter((d) => form.days.includes(d)),
  ranges: form.ranges.map((r) => ({ start: r.start, end: r.end })),
})

/** The create body: required fields always, optional ones only when filled. */
export function createBodyOf(form: CampaignForm): CampaignWrite {
  const body: CampaignWrite = {
    name: form.name.trim(),
    didId: form.didId,
    timeZone: form.timeZone.trim(),
    callingWindow: windowOf(form),
  }
  const maxConcurrent = intOrUndefined(form.maxConcurrent)
  if (maxConcurrent !== undefined) body.maxConcurrent = maxConcurrent
  const ring = intOrUndefined(form.ringTimeoutSec)
  if (ring !== undefined) body.ringTimeoutSec = ring
  if (form.defaultCountryCode.trim() !== '') body.defaultCountryCode = form.defaultCountryCode.trim()
  if (form.followUpQueueId !== '') body.followUpQueueId = form.followUpQueueId
  if (!form.isDefaultRetry) body.retryPolicy = retryPolicyOf(form)
  return body
}

const sameJSON = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)

/**
 * The PATCH body: only the fields that differ from the stored campaign AND that
 * the status lets change. A locked field is never sent, so an untouched form
 * cannot trip CAMPAIGN_FIELD_LOCKED, and a field the operator did not edit is
 * never rewritten. The two clearable fields go as null.
 */
export function patchOf(original: Campaign, form: CampaignForm): CampaignPatch {
  const patch: CampaignPatch = {}
  const can = (field: CampaignField) => isFieldEditable(original.status, field)

  if (can('name') && form.name.trim() !== original.name) patch.name = form.name.trim()
  if (can('didId') && form.didId !== original.didId) patch.didId = form.didId
  if (can('timeZone') && form.timeZone.trim() !== original.timeZone) {
    patch.timeZone = form.timeZone.trim()
  }
  if (can('callingWindow') && !sameJSON(windowOf(form), {
    days: original.callingWindow.days,
    ranges: original.callingWindow.ranges.map((r) => ({ start: r.start, end: r.end })),
  })) {
    patch.callingWindow = windowOf(form)
  }
  const maxConcurrent = intOrUndefined(form.maxConcurrent)
  if (can('maxConcurrent') && maxConcurrent !== undefined && maxConcurrent !== original.maxConcurrent) {
    patch.maxConcurrent = maxConcurrent
  }
  const ring = intOrUndefined(form.ringTimeoutSec)
  if (can('ringTimeoutSec') && ring !== undefined && ring !== original.ringTimeoutSec) {
    patch.ringTimeoutSec = ring
  }
  if (can('retryPolicy')) {
    const next = retryPolicyOf(form)
    const stored = retryPolicyOf(formFrom(original))
    if (!sameJSON(next, stored)) patch.retryPolicy = next
  }
  if (can('defaultCountryCode') && form.defaultCountryCode.trim() !== (original.defaultCountryCode ?? '')) {
    patch.defaultCountryCode = form.defaultCountryCode.trim() === '' ? null : form.defaultCountryCode.trim()
  }
  if (can('followUpQueueId') && form.followUpQueueId !== (original.followUpQueueId ?? '')) {
    patch.followUpQueueId = form.followUpQueueId === '' ? null : form.followUpQueueId
  }
  return patch
}

// --- Pre-validation -----------------------------------------------------------

const MINUTES_PER_DAY = 24 * 60

interface Minutes {
  start: number
  end: number
}

function parseClock(text: string): number | undefined {
  const match = /^(\d{2}):(\d{2})$/.exec(text)
  if (!match) return undefined
  const hours = Number(match[1])
  const minutes = Number(match[2])
  return hours > 23 || minutes > 59 ? undefined : hours * 60 + minutes
}

/**
 * A range as minutes since midnight, read the way the server reads it
 * (dayrange.New): "24:00" and "00:00" as an end mean the end of the day, an end
 * before the start crosses midnight, equal ends are no range. Undefined for
 * text that is not a range, which the server answers itself.
 */
export function parseRange(range: CampaignTimeRange): Minutes | undefined {
  const start = parseClock(range.start.trim())
  if (start === undefined) return undefined
  const endText = range.end.trim()
  let end = endText === '24:00' ? MINUTES_PER_DAY : parseClock(endText)
  if (end === undefined) return undefined
  if (end === 0 && start !== 0) end = MINUTES_PER_DAY
  return start === end ? undefined : { start, end }
}

/** The range split at midnight into pieces that each sit inside one day. */
function pieces(r: Minutes): Array<[number, number]> {
  return r.end < r.start ? [[r.start, MINUTES_PER_DAY], [0, r.end]] : [[r.start, r.end]]
}

/**
 * Whether every minute of `range` lies inside `outer`, both possibly crossing
 * midnight (dayrange.Range.Within): 22:00-02:00 is inside 20:00-04:00, and
 * 08:00-23:00 is not inside 20:00-04:00 even though both ends are, because a
 * same-day range never spans the gap.
 */
export function isRangeWithin(range: Minutes, outer: Minutes): boolean {
  const outerPieces = pieces(outer)
  return pieces(range).every(([from, to]) =>
    outerPieces.some(([oFrom, oTo]) => oFrom <= from && to <= oTo),
  )
}

/** Indexes of the window ranges that parse and lie outside the outer hours. */
export function rangesOutsideHours(
  ranges: CampaignTimeRange[],
  outerHours: CampaignTimeRange | undefined,
): number[] {
  const outer = outerHours ? parseRange(outerHours) : undefined
  if (!outer) return []
  const outside: number[] = []
  ranges.forEach((r, index) => {
    const parsed = parseRange(r)
    if (parsed && !isRangeWithin(parsed, outer)) outside.push(index)
  })
  return outside
}

export interface FormProblems {
  /** Indexes of ranges outside the deployment's calling hours. */
  outsideRanges: number[]
  maxConcurrent?: boolean
  ringTimeoutSec?: boolean
}

const isOutside = (text: string, limit: { min: number; max: number }): boolean => {
  const value = intOrUndefined(text)
  return value !== undefined && (value < limit.min || value > limit.max)
}

/**
 * What the settings let the form refuse before the request. Only a field the
 * operator can change AND has changed from the stored campaign is judged, as
 * the server judges only what a write carries. Without settings nothing is
 * judged; the server's 422 remains the backstop for every rule.
 */
export function formProblems(
  form: CampaignForm,
  settings: CampaignSettings | undefined,
  original?: Campaign,
): FormProblems {
  const problems: FormProblems = { outsideRanges: [] }
  if (!settings) return problems
  const can = (field: CampaignField) => isFieldEditable(original?.status, field)
  if (can('callingWindow') && (!original || !sameJSON(windowOf(form).ranges, original.callingWindow.ranges.map((r) => ({ start: r.start, end: r.end }))))) {
    problems.outsideRanges = rangesOutsideHours(form.ranges, settings.outboundHours)
  }
  if (can('maxConcurrent') && form.maxConcurrent !== String(original?.maxConcurrent ?? '')
    && isOutside(form.maxConcurrent, settings.limits.maxConcurrent)) {
    problems.maxConcurrent = true
  }
  if (can('ringTimeoutSec') && form.ringTimeoutSec !== String(original?.ringTimeoutSec ?? '')
    && isOutside(form.ringTimeoutSec, settings.limits.ringTimeoutSec)) {
    problems.ringTimeoutSec = true
  }
  return problems
}

export const hasProblems = (p: FormProblems): boolean =>
  p.outsideRanges.length > 0 || Boolean(p.maxConcurrent) || Boolean(p.ringTimeoutSec)

// --- Words ---------------------------------------------------------------------

/**
 * "tomorrow 09:00", "today 14:30" or "Mon, Oct 19, 09:00" for an instant, as
 * read on the wall clocks of the campaign's own zone: the calling window is
 * written there, so "waiting for calling hours (tomorrow 09:00)" must agree
 * with it. The zone is named only when it is not the reader's own.
 */
export function describeWhen(
  at: string | Date,
  options: { timeZone: string; now?: Date; locale: string; viewerTimeZone?: string },
  t: TFunction,
): string {
  const { timeZone, locale } = options
  const now = options.now ?? new Date()
  const date = typeof at === 'string' ? new Date(at) : at
  const dayOf = (d: Date) => {
    const parts = new Intl.DateTimeFormat('en-CA', {
      timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
    }).formatToParts(d)
    const get = (type: string) => Number(parts.find((p) => p.type === type)?.value)
    return Date.UTC(get('year'), get('month') - 1, get('day')) / 86_400_000
  }
  const time = new Intl.DateTimeFormat(locale, {
    timeZone, hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).format(date)
  const diff = dayOf(date) - dayOf(now)

  let when: string
  if (diff === 0) when = t('campaign.when.today', { time })
  else if (diff === 1) when = t('campaign.when.tomorrow', { time })
  else {
    when = new Intl.DateTimeFormat(locale, {
      timeZone, weekday: 'short', month: 'short', day: 'numeric',
      hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    }).format(date)
  }
  const viewer = options.viewerTimeZone ?? Intl.DateTimeFormat().resolvedOptions().timeZone
  return viewer === timeZone ? when : `${when} ${timeZone}`
}

export interface StateWords {
  /** "Running", "Paused", ... */
  status: string
  /** "waiting for calling hours (tomorrow 09:00)", when the dialer says why it is idle. */
  activity?: string
  /** Why a campaign is paused: a breaker pause needs a person. */
  pause?: string
  /** True when the pause came from a breaker and needs a person to resume. */
  isBreakerPause: boolean
  /** The one-line form, "Running · waiting for calling hours (tomorrow 09:00)". */
  line: string
}

/**
 * The sentence a campaign's state reads as (docs/design/10-campaigns.md
 * section 4.1): status, then what the dialer is doing about it.
 */
export function describeState(
  campaign: Pick<Campaign, 'status' | 'pauseReason' | 'activity' | 'nextDialAt' | 'timeZone'>,
  t: TFunction,
  options: { locale: string; now?: Date; viewerTimeZone?: string },
): StateWords {
  const status = t(`campaign.statuses.${campaign.status}`)
  let activity: string | undefined
  if (campaign.status === 'RUNNING' && campaign.activity) {
    const when = campaign.nextDialAt
      ? describeWhen(campaign.nextDialAt, { ...options, timeZone: campaign.timeZone }, t)
      : undefined
    activity = when
      ? t(`campaign.activitiesWhen.${campaign.activity}`, { when })
      : t(`campaign.activities.${campaign.activity}`)
  }
  const pause =
    campaign.status === 'PAUSED' && campaign.pauseReason
      ? t(`campaign.pauseReasons.${campaign.pauseReason}`)
      : undefined
  const isBreakerPause =
    campaign.status === 'PAUSED' &&
    (campaign.pauseReason === 'TRUNK_FAILURE' ||
      campaign.pauseReason === 'PROVIDER_FAILURE' ||
      campaign.pauseReason === 'OUTBOUND_NOT_CONFIGURED')
  const detail = activity ?? pause
  return { status, activity, pause, isBreakerPause, line: detail ? `${status} · ${detail}` : status }
}

// --- Errors --------------------------------------------------------------------

/** Every error code the campaign and do-not-call operations can answer with. */
export const CAMPAIGN_ERROR_CODES: ErrorCode[] = [
  'NUMBER_ON_DNC', 'DID_CHANNELS_EXHAUSTED', 'CAMPAIGN_FLOW_HAS_TRANSFER',
  'CAMPAIGN_DID_NOT_OUTBOUND', 'CAMPAIGN_RING_TIMEOUT_TOO_LONG', 'CAMPAIGN_WINDOW_OUTSIDE_LIMIT',
  'CAMPAIGN_FIELD_LOCKED', 'CAMPAIGN_INVALID_TRANSITION',
]

/** The form field each campaign refusal belongs on; the rest show above the buttons. */
const ERROR_FIELD: Partial<Record<ErrorCode, string>> = {
  CAMPAIGN_FLOW_HAS_TRANSFER: 'didId',
  CAMPAIGN_DID_NOT_OUTBOUND: 'didId',
  CAMPAIGN_RING_TIMEOUT_TOO_LONG: 'ringTimeoutSec',
  CAMPAIGN_WINDOW_OUTSIDE_LIMIT: 'callingWindow',
}

/**
 * The text a refusal shows under one input, or undefined when it is not about
 * that input. `field` is the wire name; a nested rule ("callingWindow.days")
 * belongs to its parent field.
 */
export function campaignFieldError(error: unknown, field: string, t: TFunction): string | undefined {
  if (!(error instanceof ApiError)) return undefined
  const owner = ERROR_FIELD[error.code]
  if (owner) return owner === field ? t(`errors.${error.code}`) : undefined
  if (error.code !== 'VALIDATION_FAILED') return undefined
  const named = typeof error.params.field === 'string' ? error.params.field : ''
  const root = named.split(/[.[]/)[0]
  if (root !== field || typeof error.params.rule !== 'string') return undefined
  return t(`errors.rules.${error.params.rule}`, { defaultValue: t('errors.VALIDATION_FAILED') })
}

/** True when this refusal has a field to sit under, so it is not repeated at the bottom. */
export function isFieldBound(error: unknown): boolean {
  if (!(error instanceof ApiError)) return false
  if (ERROR_FIELD[error.code]) return true
  return error.code === 'VALIDATION_FAILED' && typeof error.params.field === 'string'
}
