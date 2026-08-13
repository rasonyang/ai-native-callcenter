import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useQuery } from '@tanstack/react-query'

import { KpiCard } from '@/components/kpi-card'
import { PageHeader } from '@/components/page-header'
import { StatusDot } from '@/components/status-pill'
import { DataTable, TBody, THead, TableMessage, Td, Th, Tr } from '@/components/table'
import { AVAILABILITY_COLOR, useRoster } from '@/lib/agent'
import { api, type Availability } from '@/lib/api'

/**
 * Supervisor wallboard.
 *
 * Every number here is computed from something the system actually knows.
 * Queue depth, service level and abandon rate need the queue metrics that
 * arrive with the reporting work, so they are absent rather than invented.
 */
export const Route = createFileRoute('/_app/supervisor/')({ component: Wallboard })

function Wallboard() {
  const { t } = useTranslation()
  const { data: roster, isPending } = useRoster(true)
  const health = useQuery({ queryKey: ['health'], queryFn: api.health, retry: false })

  const agents = roster?.items ?? []
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
          label={t('wallboard.streamClients')}
          value={health.data?.sseClients ?? '—'}
          note={t('wallboard.consolesConnected')}
        />
      </div>

      <div className="grid grid-cols-[1fr_360px] items-start gap-4">
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

        <DataTable>
          <THead>
            <Th>{t('supervisor.name')}</Th>
            <Th>{t('supervisor.status')}</Th>
            <Th align="right">{t('supervisor.ext')}</Th>
          </THead>
          <TBody>
            {isPending && <TableMessage colSpan={3}>{t('common.loading')}</TableMessage>}
            {!isPending && agents.length === 0 && (
              <TableMessage colSpan={3}>{t('supervisor.noAgents')}</TableMessage>
            )}
            {agents.map((row) => (
              <Tr key={row.agentId}>
                <Td>{row.displayName}</Td>
                <Td>
                  <span className="flex items-center gap-1.5 text-xs">
                    <StatusDot availability={row.availability} />
                    {row.availability === 'NOT_READY' && row.reason
                      ? t(`reasons.${row.reason}`)
                      : t(`availability.${row.availability}`)}
                  </span>
                </Td>
                <Td align="right" className="tabular">
                  {row.extensionNumber ?? '—'}
                </Td>
              </Tr>
            ))}
          </TBody>
        </DataTable>
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
