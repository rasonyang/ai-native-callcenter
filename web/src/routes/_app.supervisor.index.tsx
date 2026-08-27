import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { QueuePerformance } from '@/components/queue-performance'
import { VolumeTrend } from '@/components/volume-trend'
import { requireRole } from '@/lib/guards'
import { StatusDot } from '@/components/status-pill'
import { AVAILABILITY_COLOR, useRoster, useWaitingCalls } from '@/lib/agent'
import { type Availability } from '@/lib/api'
import { formatDuration, useOverview } from '@/lib/ledger'

/**
 * Supervisor wallboard.
 *
 * Every number here is computed from something the system actually knows.
 * Live queue depth and longest-wait need queue-count events the platform does
 * not publish yet; the per-queue panel therefore reports the finished-call
 * ledger for the window rather than inventing a live figure.
 */
export const Route = createFileRoute('/_app/supervisor/')({
  beforeLoad: ({ context }) => requireRole(context.user, 'SUPERVISOR'),
  component: Wallboard,
})

function Wallboard() {
  const { t } = useTranslation()
  const { data: roster } = useRoster(true)
  // Every queue's waiting callers: /calls/waiting answers a supervisor with
  // the whole floor, the same split ListCalls makes.
  const { data: queueDepth } = useWaitingCalls(true)
  // Today's ledger numbers; refreshed by CALL_CDR events on the stream.
  const { data: today } = useOverview()

  const agents = roster?.items ?? []
  const waiting = queueDepth?.items ?? []
  const byAvailability = agents.reduce<Record<string, number>>((acc, row) => {
    acc[row.availability] = (acc[row.availability] ?? 0) + 1
    return acc
  }, {})

  const available = byAvailability.READY ?? 0
  const onCall = byAvailability.ON_CALL ?? 0
  const signedIn = agents.filter((a) => a.state !== 'LOGGED_OUT').length
  const unreachable = byAvailability.DEVICE_UNREACHABLE ?? 0

  return (
    <>
      <PageHeader title={t('nav.wallboard')} />

      {/* The fourth card used to count live event streams, read from
          /system/health — ADMIN-only, so on the one page whose only reader is
          a supervisor it answered 403 and showed an em dash to everybody,
          always. Callers waiting took its place: that is the floor, it moves
          minute to minute, and it is the number a supervisor opens this page
          for. */}
      <div className="mb-4 grid grid-cols-4 gap-4">
        <KpiCard
          label={t('wallboard.agentsAvailable')}
          value={`${available} / ${signedIn}`}
          note={t('wallboard.availableOfSignedIn')}
        />
        <KpiCard label={t('wallboard.onCall')} value={onCall} note={t('wallboard.rightNow')} />
        <KpiCard
          label={t('wallboard.devicesUnreachable')}
          value={unreachable}
          note={t('wallboard.registeredButSilent')}
          tone={unreachable > 0 ? '--state-breach' : undefined}
        />
        <KpiCard
          label={t('wallboard.inQueue')}
          value={waiting.length}
          note={t('wallboard.waitingNow')}
          tone={waiting.length > 0 ? '--state-ringing' : undefined}
        />
      </div>

      <div className="mb-4 grid grid-cols-4 gap-4">
        <KpiCard
          label={t('wallboard.callsToday')}
          value={today?.totalCalls ?? '—'}
          note={t('wallboard.answeredOf', {
            answered: today?.answeredCalls ?? 0,
            total: today?.totalCalls ?? 0,
          })}
        />
        <KpiCard
          label={t('wallboard.abandonedToday')}
          value={today?.abandonedCalls ?? '—'}
          note={t('wallboard.inQueues')}
          tone={today && today.abandonedCalls > 0 ? '--state-breach' : undefined}
        />
        <KpiCard
          label={t('wallboard.botContained')}
          value={
            today && today.totalCalls > 0
              ? `${Math.round((today.containedCalls / today.totalCalls) * 100)}%`
              : '—'
          }
          note={t('wallboard.containedOf', { count: today?.containedCalls ?? 0 })}
        />
        <KpiCard
          label={t('wallboard.avgWait')}
          value={today && today.queueCalls > 0 ? formatDuration(today.avgWaitSec) : '—'}
          note={t('wallboard.acrossQueueCalls', { count: today?.queueCalls ?? 0 })}
        />
      </div>

      {/* Halves, not 1fr + 520px. Six columns never fitted 520px, so the
          panel scrolled sideways and the queue's own name — the column that
          says which row you are reading — was the first to go. */}
      <div className="mb-4 grid grid-cols-2 items-start gap-4 [&>*]:min-w-0">
        <VolumeTrend days={7} />
        <QueuePerformance />
      </div>

      <div className="grid grid-cols-1 items-start gap-4">
        <div className="rounded-md border bg-card p-4">
          <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {t('wallboard.agentStates')}
          </h2>
          {signedIn === 0 ? (
            <p className="text-xs text-muted-foreground">{t('wallboard.nobodySignedIn')}</p>
          ) : (
            <>
              <div className="flex h-2 overflow-hidden rounded-full">
                {STATE_ORDER.filter((state) => byAvailability[state]).map((state) => (
                  <span
                    key={state}
                    style={{
                      width: `${((byAvailability[state] ?? 0) / agents.length) * 100}%`,
                      backgroundColor: AVAILABILITY_COLOR[state],
                    }}
                  />
                ))}
              </div>
              <div className="mt-3 flex flex-wrap gap-4">
                {STATE_ORDER.filter((state) => byAvailability[state]).map((state) => (
                  <span key={state} className="flex items-center gap-1.5 text-xs">
                    <StatusDot availability={state} />
                    {t(`availability.${state}`)}
                    <span className="tabular font-medium">{byAvailability[state]}</span>
                  </span>
                ))}
              </div>
            </>
          )}
        </div>
      </div>
    </>
  )
}

const STATE_ORDER: Availability[] = [
  'READY',
  'ON_CALL',
  'WRAP_UP',
  'NOT_READY',
  'DEVICE_UNREACHABLE',
  'LOGGED_OUT',
]
