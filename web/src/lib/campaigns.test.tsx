import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { describe, expect, it, vi } from 'vitest'

import i18n from '@/lib/i18n'
import en from '@/locales/en/translation.json'
import zh from '@/locales/zh/translation.json'

import { ApiError } from './api'
import {
  CAMPAIGN_ERROR_CODES, CAMPAIGN_FIELDS, campaignApi, hasCallRecord, isPlacedCall, useCampaignMutations, actionsFor, applyCampaignUpdated, blankForm,
  campaignFieldError, campaignKey, campaignPayloadOf, createBodyOf, describeState, formFrom,
  defaultTimeZone, isFieldEditable, patchOf, retryFields, rangesOutsideHours, formProblems,
  type CampaignWrite, type CampaignSettings, type Campaign, type CampaignStatus, type SseCampaignUpdatedPayload,
} from './campaigns'
import { applyToCache } from './use-event-stream'

const COUNTS = { total: 10, active: 6, done: 3, exhausted: 1, invalidNumber: 0, dnc: 0 }

function campaignFixture(over: Partial<Campaign> = {}): Campaign {
  return {
    id: '00000000-0000-4000-8000-000000000001',
    name: 'Spring renewals',
    didId: '00000000-0000-4000-8000-0000000000d1',
    didNumber: '95099',
    status: 'DRAFT',
    timeZone: 'Asia/Shanghai',
    callingWindow: { days: ['MON', 'TUE', 'WED', 'THU', 'FRI'], ranges: [{ start: '09:00', end: '18:00' }] },
    maxConcurrent: 5,
    ringTimeoutSec: 30,
    retryPolicy: {
      maxAttempts: 3,
      maxSystemRetries: 5,
      retryOutcomes: ['NO_ANSWER', 'BUSY'],
      intervalSec: { NO_ANSWER: 7200, BUSY: 1800, REJECTED: 86400, UNKNOWN: 86400 },
      transientBackoff: { baseSec: 300, factor: 3, capSec: 14400 },
    },
    createdAt: '2026-10-01T00:00:00Z',
    contacts: COUNTS,
    ...over,
  }
}

const NOW = new Date('2026-10-12T10:00:00+08:00') // a Monday morning in Shanghai

describe('the sentence a campaign state reads as', () => {
  const tEn = i18n.getFixedT('en')
  const tZh = i18n.getFixedT('zh')
  const here = { now: NOW, viewerTimeZone: 'Asia/Shanghai' }

  it('says it is waiting for calling hours, and when they open', () => {
    const campaign = campaignFixture({
      status: 'RUNNING', activity: 'WAITING_WINDOW', nextDialAt: '2026-10-13T09:00:00+08:00',
    })
    expect(describeState(campaign, tEn, { locale: 'en', ...here }).line).toBe(
      'Running · waiting for calling hours (tomorrow 09:00)',
    )
    expect(describeState(campaign, tZh, { locale: 'zh', ...here }).line).toBe(
      '运行中 · 等待拨打时段（明天 09:00）',
    )
  })

  it('says today for a time later on the same day, in the campaign’s own zone', () => {
    const campaign = campaignFixture({
      status: 'RUNNING', activity: 'WAITING_RETRY', nextDialAt: '2026-10-12T14:30:00+08:00',
    })
    expect(describeState(campaign, tEn, { locale: 'en', ...here }).line).toBe(
      'Running · waiting to retry (today 14:30)',
    )
  })

  it('reads the day in the campaign’s zone, not the browser’s', () => {
    // 22:30 in New York is already tomorrow in Shanghai's morning, but the
    // campaign lives in New York: it is still tonight there.
    const campaign = campaignFixture({
      status: 'RUNNING', activity: 'WAITING_WINDOW', timeZone: 'America/New_York',
      nextDialAt: '2026-10-13T09:00:00-04:00',
    })
    const line = describeState(campaign, tEn, {
      locale: 'en', now: new Date('2026-10-12T22:30:00-04:00'), viewerTimeZone: 'Asia/Shanghai',
    }).line
    expect(line).toBe('Running · waiting for calling hours (tomorrow 09:00 America/New_York)')
  })

  it('shows a date and the time for a later day', () => {
    const campaign = campaignFixture({
      status: 'RUNNING', activity: 'WAITING_WINDOW', nextDialAt: '2026-10-19T09:00:00+08:00',
    })
    const line = describeState(campaign, tEn, { locale: 'en', ...here }).line
    expect(line).toContain('09:00')
    expect(line).toMatch(/Oct/)
    expect(line).not.toMatch(/tomorrow|today/)
  })

  it('names the activity alone when the dialer has no time to give', () => {
    expect(
      describeState(campaignFixture({ status: 'RUNNING', activity: 'DIALING' }), tEn, { locale: 'en', ...here }).line,
    ).toBe('Running · dialling')
    expect(
      describeState(campaignFixture({ status: 'RUNNING', activity: 'AT_CAPACITY' }), tZh, { locale: 'zh', ...here }).line,
    ).toBe('运行中 · 受容量限制暂缓')
  })

  it('reads as the status alone when nothing is known about the activity', () => {
    expect(describeState(campaignFixture({ status: 'RUNNING' }), tEn, { locale: 'en', ...here }).line).toBe('Running')
    expect(describeState(campaignFixture(), tEn, { locale: 'en', ...here }).line).toBe('Draft')
  })

  it('does not report an activity for a campaign that is not running', () => {
    const words = describeState(
      campaignFixture({ status: 'COMPLETED', activity: 'DIALING' }), tEn, { locale: 'en', ...here },
    )
    expect(words.line).toBe('Completed')
  })

  it('tells a breaker pause, which needs a person, from a manual one', () => {
    const manual = describeState(campaignFixture({ status: 'PAUSED', pauseReason: 'MANUAL' }), tEn, { locale: 'en', ...here })
    const trunk = describeState(campaignFixture({ status: 'PAUSED', pauseReason: 'TRUNK_FAILURE' }), tEn, { locale: 'en', ...here })
    const provider = describeState(campaignFixture({ status: 'PAUSED', pauseReason: 'PROVIDER_FAILURE' }), tZh, { locale: 'zh', ...here })
    expect(manual.isBreakerPause).toBe(false)
    expect(trunk.isBreakerPause).toBe(true)
    expect(trunk.line).toBe('Paused · Paused automatically: the trunk is failing')
    expect(provider.isBreakerPause).toBe(true)
    expect(provider.line).toContain('语音服务商')
  })

  it('treats a missing outbound endpoint as a pause that needs a person', () => {
    const words = describeState(
      campaignFixture({ status: 'PAUSED', pauseReason: 'OUTBOUND_NOT_CONFIGURED' }), tEn, { locale: 'en', ...here },
    )
    expect(words.isBreakerPause).toBe(true)
    expect(words.line).toBe('Paused · Paused automatically: this deployment has no outbound endpoint')
  })
})

describe('the pause reasons', () => {
  const REASONS = ['MANUAL', 'TRUNK_FAILURE', 'PROVIDER_FAILURE', 'OUTBOUND_NOT_CONFIGURED'] as const

  it.each([
    ['en', en],
    ['zh', zh],
  ])('has a label and an explanation for every reason in %s', (_lang, bundle) => {
    const campaign = (bundle as unknown as {
      campaign: { pauseReasons: Record<string, string>; pauseExplain: Record<string, string> }
    }).campaign
    for (const reason of REASONS) {
      expect(campaign.pauseReasons[reason], `label ${reason}`).toBeTruthy()
      expect(campaign.pauseExplain[reason], `explain ${reason}`).toBeTruthy()
    }
  })

  it('names the setting a missing outbound endpoint is fixed in, and no longer hedges the trunk reason', () => {
    expect(en.campaign.pauseExplain.OUTBOUND_NOT_CONFIGURED).toContain('AICC_OUTBOUND_ENDPOINT')
    expect(en.campaign.pauseExplain.OUTBOUND_NOT_CONFIGURED).toContain('deploy/carrier.md')
    expect(zh.campaign.pauseExplain.OUTBOUND_NOT_CONFIGURED).toContain('AICC_OUTBOUND_ENDPOINT')
    expect(en.campaign.pauseExplain.TRUNK_FAILURE).not.toContain('AICC_OUTBOUND_ENDPOINT')
    expect(zh.campaign.pauseExplain.TRUNK_FAILURE).not.toContain('AICC_OUTBOUND_ENDPOINT')
  })
})

const editable = (status: CampaignStatus) => CAMPAIGN_FIELDS.filter((f) => isFieldEditable(status, f))

describe('which fields can change in which status', () => {
  const STATUSES: CampaignStatus[] = ['DRAFT', 'RUNNING', 'PAUSED', 'COMPLETED', 'CANCELLED']
  it('opens every field in a draft, and in a form that has no campaign yet', () => {
    expect(editable('DRAFT')).toEqual(CAMPAIGN_FIELDS)
    expect(CAMPAIGN_FIELDS.every((f) => isFieldEditable(undefined, f))).toBe(true)
  })

  it('opens only the concurrency, the window and the retry policy while running or paused', () => {
    for (const status of ['RUNNING', 'PAUSED'] as const) {
      expect(editable(status)).toEqual(['callingWindow', 'maxConcurrent', 'retryPolicy'])
    }
  })

  it('opens nothing once the campaign is over', () => {
    expect(editable('COMPLETED')).toEqual([])
    expect(editable('CANCELLED')).toEqual([])
  })

  it('offers the lifecycle actions each status allows', () => {
    const offered = Object.fromEntries(
      STATUSES.map((s) => [s, Object.entries(actionsFor(s)).filter(([, v]) => v).map(([k]) => k)]),
    )
    expect(offered).toEqual({
      DRAFT: ['start', 'remove'],
      RUNNING: ['pause', 'cancel'],
      PAUSED: ['resume', 'cancel'],
      COMPLETED: [],
      CANCELLED: [],
    })
  })

  describe('the PATCH body', () => {
    it('is empty for a form nobody touched, whatever the status', () => {
      for (const status of STATUSES) {
        const campaign = campaignFixture({ status, followUpQueueId: '00000000-0000-4000-8000-0000000000a1', defaultCountryCode: '86' })
        expect(patchOf(campaign, formFrom(campaign))).toEqual({})
      }
    })

    it('sends only what changed in a draft', () => {
      const campaign = campaignFixture()
      const form = { ...formFrom(campaign), name: 'Autumn renewals', maxConcurrent: '8' }
      expect(patchOf(campaign, form)).toEqual({ name: 'Autumn renewals', maxConcurrent: 8 })
    })

    it('never sends a locked field while running, even if the form changed it', () => {
      const campaign = campaignFixture({ status: 'RUNNING' })
      const form = {
        ...formFrom(campaign),
        name: 'Renamed', timeZone: 'UTC', ringTimeoutSec: '45', didId: 'other', defaultCountryCode: '1',
        followUpQueueId: '00000000-0000-4000-8000-0000000000a2',
        maxConcurrent: '9',
      }
      expect(patchOf(campaign, form)).toEqual({ maxConcurrent: 9 })
    })

    it('sends a changed window and a changed retry policy while paused', () => {
      const campaign = campaignFixture({ status: 'PAUSED', pauseReason: 'MANUAL' })
      const form = {
        ...formFrom(campaign),
        days: ['MON' as const],
        ranges: [{ start: '22:00', end: '02:00' }, { start: '09:00', end: '12:00' }],
        maxAttempts: '4',
        intervalMin: { ...formFrom(campaign).intervalMin, BUSY: '15' },
      }
      const patch = patchOf(campaign, form)
      expect(patch.callingWindow).toEqual({
        days: ['MON'],
        ranges: [{ start: '22:00', end: '02:00' }, { start: '09:00', end: '12:00' }],
      })
      expect(patch.retryPolicy?.maxAttempts).toBe(4)
      expect(patch.retryPolicy?.intervalSec?.BUSY).toBe(900)
      expect(Object.keys(patch).toSorted()).toEqual(['callingWindow', 'retryPolicy'])
    })

    it('sends nothing for a finished campaign', () => {
      const campaign = campaignFixture({ status: 'CANCELLED' })
      expect(patchOf(campaign, { ...formFrom(campaign), name: 'x', maxConcurrent: '99' })).toEqual({})
    })

    it('clears the two optional fields with null', () => {
      const campaign = campaignFixture({
        followUpQueueId: '00000000-0000-4000-8000-0000000000a1', defaultCountryCode: '86',
      })
      const form = { ...formFrom(campaign), followUpQueueId: '', defaultCountryCode: '' }
      expect(patchOf(campaign, form)).toEqual({ followUpQueueId: null, defaultCountryCode: null })
    })

    it('keeps an interval that is not a whole number of minutes exactly as stored', () => {
      const campaign = campaignFixture({
        retryPolicy: { ...campaignFixture().retryPolicy, intervalSec: { NO_ANSWER: 90, BUSY: 1800, REJECTED: 86400, UNKNOWN: 86400 } },
      })
      expect(patchOf(campaign, formFrom(campaign))).toEqual({})
    })
  })

  describe('the create body', () => {
    it('type-checks with only the four required fields', () => {
      const minimal: CampaignWrite = {
        name: 'Q4', didId: 'did-1', timeZone: 'UTC',
        callingWindow: { days: ['MON'], ranges: [{ start: '09:00', end: '18:00' }] },
      }
      expect(createBodyOf(blankForm('UTC'))).not.toHaveProperty('retryPolicy')
      expect(Object.keys(minimal)).toHaveLength(4)
    })

    it('sends the required fields and leaves the rest to the server', () => {
      const form = { ...blankForm('Asia/Shanghai'), name: ' Q4 ', didId: 'did-1' }
      expect(createBodyOf(form)).toEqual({
        name: 'Q4',
        didId: 'did-1',
        timeZone: 'Asia/Shanghai',
        callingWindow: { days: ['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN'], ranges: [{ start: '09:00', end: '18:00' }] },
      })
    })

    it('adds what the operator filled in, and the retry policy only when edited', () => {
      const form = {
        ...blankForm('UTC'), name: 'Q4', didId: 'did-1', maxConcurrent: '12', ringTimeoutSec: '25',
        defaultCountryCode: '44', followUpQueueId: 'queue-1', isDefaultRetry: false,
        ...retryFields(campaignFixture().retryPolicy), maxAttempts: '2',
      }
      const body = createBodyOf(form)
      expect(body).toMatchObject({
        maxConcurrent: 12, ringTimeoutSec: 25, defaultCountryCode: '44', followUpQueueId: 'queue-1',
      })
      expect(body.retryPolicy?.maxAttempts).toBe(2)
      expect(body.retryPolicy?.transientBackoff).toEqual({ baseSec: 300, factor: 3, capSec: 14400 })
    })
  })
})

const range = (start: string, end: string) => [{ start, end }]

describe('pre-validating a calling window against the outer hours', () => {
  // The server's rule (dayrange.Range.Within) and the table of
  // TestCampaignSettingsAreTheValuesTheServerEnforces, plus the cases that
  // distinguish a midnight-crossing outer range.
  const cases: Array<[string, [string, string], [string, string], boolean]> = [
    ['default hours: the hours themselves', ['08:00', '21:00'], ['08:00', '21:00'], true],
    ['default hours: inside', ['08:00', '21:00'], ['10:00', '12:00'], true],
    ['default hours: a minute before the start', ['08:00', '21:00'], ['07:59', '21:00'], false],
    ['default hours: a minute past the end', ['08:00', '21:00'], ['08:00', '21:01'], false],
    ['default hours: entirely outside', ['08:00', '21:00'], ['22:00', '23:00'], false],
    ['default hours: a night range crossing midnight', ['08:00', '21:00'], ['22:00', '02:00'], false],
    ['default hours: ending at 24:00', ['08:00', '21:00'], ['20:00', '24:00'], false],
    ['narrow: the hours themselves', ['10:00', '12:00'], ['10:00', '12:00'], true],
    ['narrow: inside', ['10:00', '12:00'], ['10:30', '11:30'], true],
    ['narrow: a minute before', ['10:00', '12:00'], ['09:59', '12:00'], false],
    ['narrow: a minute past', ['10:00', '12:00'], ['10:00', '12:01'], false],
    ['narrow: entirely outside', ['10:00', '12:00'], ['13:00', '14:00'], false],
    ['night: the hours themselves', ['20:00', '04:00'], ['20:00', '04:00'], true],
    ['night: crossing midnight inside', ['20:00', '04:00'], ['22:00', '02:00'], true],
    ['night: before midnight only', ['20:00', '04:00'], ['23:00', '23:30'], true],
    ['night: after midnight only', ['20:00', '04:00'], ['01:00', '03:00'], true],
    ['night: a minute before the start', ['20:00', '04:00'], ['19:59', '04:00'], false],
    ['night: a minute past the end', ['20:00', '04:00'], ['20:00', '04:01'], false],
    ['night: a same-day range spanning the gap', ['20:00', '04:00'], ['03:00', '21:00'], false],
    ['night: entirely in the gap', ['20:00', '04:00'], ['09:00', '12:00'], false],
    ['night: end at 00:00 means the end of the day', ['20:00', '04:00'], ['22:00', '00:00'], true],
    ['night: end at 24:00', ['20:00', '04:00'], ['22:00', '24:00'], true],
  ]
  it.each(cases)('%s', (_name, outer, window, isAccepted) => {
    const outside = rangesOutsideHours(range(...window), { start: outer[0], end: outer[1] })
    expect(outside.length === 0).toBe(isAccepted)
  })

  it('names which of several ranges is outside', () => {
    const ranges = [...range('09:00', '12:00'), ...range('20:00', '22:00')]
    expect(rangesOutsideHours(ranges, { start: '08:00', end: '21:00' })).toEqual([1])
  })

  it('leaves text that is not a range to the server', () => {
    const outer = { start: '08:00', end: '21:00' }
    for (const bad of [['9:00', '18:00'], ['', ''], ['10:00', '10:00'], ['25:00', '26:00']]) {
      expect(rangesOutsideHours(range(bad[0], bad[1]), outer)).toEqual([])
    }
  })

  it('judges nothing without settings, and only what a locked-down edit can still change', () => {
    const form = { ...blankForm('UTC'), ranges: range('03:00', '04:00'), ringTimeoutSec: '99' }
    expect(formProblems(form, undefined)).toEqual({ outsideRanges: [] })
    const settings: CampaignSettings = {
      outboundHours: { start: '08:00', end: '21:00' },
      defaults: { maxConcurrent: 5, ringTimeoutSec: 30, retryPolicy: campaignFixture().retryPolicy },
      limits: { maxConcurrent: { min: 1, max: 10 }, ringTimeoutSec: { min: 5, max: 59 } },
    }
    expect(formProblems(form, settings)).toEqual({ outsideRanges: [0], ringTimeoutSec: true })
    // A running campaign's ring timeout is locked; an untouched window is not re-judged.
    const running = campaignFixture({
      status: 'RUNNING', callingWindow: { days: ['MON'], ranges: range('03:00', '04:00') },
    })
    expect(formProblems(formFrom(running), settings, running)).toEqual({ outsideRanges: [] })
    expect(formProblems({ ...formFrom(running), maxConcurrent: '11' }, settings, running)).toEqual({
      outsideRanges: [], maxConcurrent: true,
    })
  })
})

const refusal = (code: string, params: Record<string, unknown> = {}) =>
  new ApiError(422, { code: code as never, message: 'x', params })

describe('the defaults of a new campaign', () => {
  it('checks all seven days', () => {
    expect(blankForm('UTC').days).toEqual(['MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN'])
  })

  it('starts in the browser’s zone when the browser lists it, else in UTC', () => {
    const own = Intl.DateTimeFormat().resolvedOptions().timeZone
    const listed = Intl.supportedValuesOf('timeZone').includes(own)
    expect(defaultTimeZone()).toBe(listed ? own : 'UTC')
    const spy = vi.spyOn(Intl, 'supportedValuesOf').mockReturnValue(['Asia/Singapore', 'UTC'])
    const resolved = vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions')
    resolved.mockReturnValue({ timeZone: 'Asia/Shanghai' } as Intl.ResolvedDateTimeFormatOptions)
    expect(defaultTimeZone()).toBe('UTC')
    resolved.mockReturnValue({ timeZone: 'Asia/Singapore' } as Intl.ResolvedDateTimeFormatOptions)
    expect(defaultTimeZone()).toBe('Asia/Singapore')
    spy.mockRestore()
    resolved.mockRestore()
  })
})

describe('which attempts link to a call record', () => {
  it('offers no link for a number that never reached the switch', () => {
    expect(hasCallRecord('DNC_SKIPPED')).toBe(false)
    expect(hasCallRecord('INVALID_NUMBER')).toBe(false)
  })
  it('keeps it for every outcome that placed a call, and for a failed placement', () => {
    for (const o of ['ANSWERED', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN', 'TRANSIENT', 'BOT_FAILED', 'OVERFLOW', 'ORIGINATE_FAILED'] as const) {
      expect(hasCallRecord(o)).toBe(true)
    }
  })
})

describe('which leads link to their last call', () => {
  it('links only an outcome that says the call rang', () => {
    for (const o of ['ANSWERED', 'NO_ANSWER', 'BUSY', 'REJECTED', 'TRANSIENT', 'BOT_FAILED', 'OVERFLOW'] as const) {
      expect(isPlacedCall(o)).toBe(true)
    }
    for (const o of ['DNC_SKIPPED', 'INVALID_NUMBER', 'ORIGINATE_FAILED', 'UNKNOWN', undefined] as const) {
      expect(isPlacedCall(o)).toBe(false)
    }
  })
})

describe('creating a campaign', () => {
  it('refetches the campaign lists, mounted or not', async () => {
    const urls: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      urls.push(`${init.method ?? 'GET'} ${String(input)}`)
      const body = init.method === 'POST' ? campaignFixture() : { items: [], total: 0 }
      return new Response(JSON.stringify(body), { status: init.method === 'POST' ? 201 : 200 })
    }))
    const client = new QueryClient()
    await client.fetchQuery({
      queryKey: ['campaigns', 'list', { limit: 50, offset: 0 }],
      queryFn: () => campaignApi.list({ limit: 50, offset: 0 }),
    })
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )
    const { result } = renderHook(() => useCampaignMutations(), { wrapper })
    await act(async () => {
      await result.current.create.mutateAsync(createBodyOf({ ...blankForm('UTC'), name: 'n', didId: 'd' }))
    })
    await waitFor(() => expect(urls.filter((u) => u === 'GET /api/v1/campaigns?limit=50&offset=0')).toHaveLength(2))
    vi.unstubAllGlobals()
  })
})

describe('what a refusal says and where', () => {
  const t = i18n.getFixedT('en')

  it('puts the flow and DID refusals under the DID field', () => {
    expect(campaignFieldError(refusal('CAMPAIGN_FLOW_HAS_TRANSFER'), 'didId', t)).toMatch(/hand the caller to a person/)
    expect(campaignFieldError(refusal('CAMPAIGN_DID_NOT_OUTBOUND'), 'didId', t)).toMatch(/does not allow outbound/)
    expect(campaignFieldError(refusal('CAMPAIGN_FLOW_HAS_TRANSFER'), 'name', t)).toBeUndefined()
  })

  it('puts the ring timeout and window refusals on their fields', () => {
    expect(campaignFieldError(refusal('CAMPAIGN_RING_TIMEOUT_TOO_LONG'), 'ringTimeoutSec', t)).toMatch(/60 seconds/)
    expect(campaignFieldError(refusal('CAMPAIGN_WINDOW_OUTSIDE_LIMIT'), 'callingWindow', t)).toMatch(/hours this deployment permits/)
  })

  it('puts a validation rule on the field it names, nested ones on their parent', () => {
    const named = refusal('VALIDATION_FAILED', { field: 'callingWindow.ranges', rule: 'OVERLAP' })
    expect(campaignFieldError(named, 'callingWindow', t)).toBe('Time ranges overlap.')
    expect(campaignFieldError(named, 'name', t)).toBeUndefined()
    const zone = refusal('VALIDATION_FAILED', { field: 'timeZone', rule: 'TIME_ZONE_INVALID' })
    expect(campaignFieldError(zone, 'timeZone', t)).toMatch(/Asia\/Shanghai/)
  })

  it('has no field for the locked and transition refusals, which show above the buttons', () => {
    expect(campaignFieldError(refusal('CAMPAIGN_FIELD_LOCKED'), 'name', t)).toBeUndefined()
    expect(campaignFieldError(refusal('CAMPAIGN_INVALID_TRANSITION'), 'name', t)).toBeUndefined()
  })

  // react-i18next answers an unknown key with the key itself, so a missing
  // message renders as "errors.CAMPAIGN_FIELD_LOCKED". Pinned per language.
  it.each([
    ['en', en],
    ['zh', zh],
  ])('has a sentence for every campaign error code in %s', (_lang, bundle) => {
    const errors = (bundle as unknown as { errors: Record<string, string> }).errors
    expect(CAMPAIGN_ERROR_CODES).toHaveLength(8)
    for (const code of CAMPAIGN_ERROR_CODES) {
      expect(errors[code], code).toBeTruthy()
    }
  })

  it.each([
    ['en', en],
    ['zh', zh],
  ])('has a sentence for every validation rule and import code in %s', (_lang, bundle) => {
    const b = bundle as unknown as {
      errors: { rules: Record<string, string> }
      campaign: { import: { codes: Record<string, string> } }
    }
    for (const rule of [
      'EMPTY', 'TOO_LONG', 'OUT_OF_RANGE', 'TIME_ZONE_INVALID', 'DID_NOT_FOUND', 'DID_DISABLED',
      'DID_HAS_NO_PUBLISHED_FLOW', 'QUEUE_NOT_FOUND', 'NOT_NULLABLE', 'INVALID', 'UNKNOWN_DAY',
      'DUPLICATE_DAY', 'TOO_MANY', 'INVALID_TIME', 'OVERLAP', 'NOT_RETRYABLE', 'UNKNOWN_OUTCOME',
      'NUMBER_EMPTY', 'NUMBER_INVALID', 'NOT_BELOW_ANSWER_WINDOW', 'OUTSIDE_PERMITTED_HOURS',
    ]) {
      expect(b.errors.rules[rule], rule).toBeTruthy()
    }
    for (const code of [
      'NUMBER_EMPTY', 'NUMBER_INVALID', 'TIME_ZONE_INVALID', 'SLOTS_TOO_MANY', 'SLOT_UNKNOWN',
      'SLOT_TOO_LONG', 'ROW_SHAPE',
    ]) {
      expect(b.campaign.import.codes[code], code).toBeTruthy()
    }
  })
})

const listOf = (queryClient: QueryClient) =>
  queryClient.getQueryData<{ items: Campaign[] }>(['campaigns', 'list', { limit: 50, offset: 0 }])

describe('the event stream updating the cache', () => {
  const ID = '00000000-0000-4000-8000-000000000001'
  const OTHER = '00000000-0000-4000-8000-000000000002'

  function seeded() {
    const queryClient = new QueryClient()
    const running = campaignFixture({ status: 'RUNNING', activity: 'DIALING', inFlight: 3 })
    queryClient.setQueryData(['campaigns', 'list', { limit: 50, offset: 0 }], {
      items: [campaignFixture({ id: OTHER, name: 'Other' }), running],
      total: 2,
    })
    queryClient.setQueryData(campaignKey(ID), running)
    return queryClient
  }

  const payload = (over: Partial<SseCampaignUpdatedPayload> = {}): SseCampaignUpdatedPayload => ({
    campaignId: ID,
    status: 'RUNNING',
    activity: 'WAITING_WINDOW',
    nextDialAt: '2026-10-13T09:00:00+08:00',
    contacts: { ...COUNTS, active: 5, done: 4 },
    ...over,
  })

  it('rewrites the row in the list and the open campaign from the payload', () => {
    const queryClient = seeded()
    applyCampaignUpdated(queryClient, payload())
    const row = listOf(queryClient)?.items.find((c) => c.id === ID)
    expect(row).toMatchObject({
      status: 'RUNNING', activity: 'WAITING_WINDOW', nextDialAt: '2026-10-13T09:00:00+08:00',
      contacts: { active: 5, done: 4 }, name: 'Spring renewals',
    })
    // inFlight is absent from this payload, so the stale 3 does not linger.
    expect(row?.inFlight).toBeUndefined()
    expect(queryClient.getQueryData<Campaign>(campaignKey(ID))).toMatchObject({
      activity: 'WAITING_WINDOW', contacts: { active: 5 },
    })
  })

  it('leaves every other campaign alone', () => {
    const queryClient = seeded()
    applyCampaignUpdated(queryClient, payload())
    expect(listOf(queryClient)?.items.find((c) => c.id === OTHER)).toMatchObject({ name: 'Other', status: 'DRAFT' })
  })

  it('clears a pause reason when the campaign is running again', () => {
    const queryClient = seeded()
    applyCampaignUpdated(queryClient, payload({ status: 'PAUSED', pauseReason: 'TRUNK_FAILURE', activity: undefined, nextDialAt: undefined }))
    expect(queryClient.getQueryData<Campaign>(campaignKey(ID))).toMatchObject({ status: 'PAUSED', pauseReason: 'TRUNK_FAILURE' })
    applyCampaignUpdated(queryClient, payload({ status: 'RUNNING', pauseReason: undefined }))
    expect(queryClient.getQueryData<Campaign>(campaignKey(ID))?.pauseReason).toBeUndefined()
  })

  it('refetches the lists only when the status changed or the campaign is unknown', () => {
    const queryClient = seeded()
    const stale = () =>
      queryClient.getQueryCache().find({ queryKey: ['campaigns', 'list', { limit: 50, offset: 0 }] })?.state.isInvalidated

    applyCampaignUpdated(queryClient, payload())
    expect(stale()).toBe(false)

    applyCampaignUpdated(queryClient, payload({ status: 'PAUSED', pauseReason: 'MANUAL' }))
    expect(stale()).toBe(true)
  })

  it('refetches the lists for a campaign nobody has loaded yet', () => {
    const queryClient = seeded()
    const stale = () =>
      queryClient.getQueryCache().find({ queryKey: ['campaigns', 'list', { limit: 50, offset: 0 }] })?.state.isInvalidated
    applyCampaignUpdated(queryClient, payload({ campaignId: '00000000-0000-4000-8000-000000000099' }))
    expect(stale()).toBe(true)
  })

  it('is what the app’s event handler does with a CAMPAIGN_UPDATED envelope', () => {
    const queryClient = seeded()
    applyToCache(queryClient, {
      version: 1, seq: 7, type: 'CAMPAIGN_UPDATED', occurredAt: '2026-10-12T02:00:00Z',
      payload: payload() as unknown as Record<string, unknown>,
    })
    expect(listOf(queryClient)?.items.find((c) => c.id === ID)?.activity).toBe('WAITING_WINDOW')
  })

  it('ignores an envelope whose payload is not a campaign update', () => {
    expect(campaignPayloadOf({ payload: { nope: 1 } })).toBeUndefined()
    expect(campaignPayloadOf({})).toBeUndefined()
    const queryClient = seeded()
    applyToCache(queryClient, {
      version: 1, seq: 8, type: 'CAMPAIGN_UPDATED', occurredAt: '2026-10-12T02:00:00Z', payload: {},
    })
    expect(listOf(queryClient)?.items.find((c) => c.id === ID)?.activity).toBe('DIALING')
  })
})
