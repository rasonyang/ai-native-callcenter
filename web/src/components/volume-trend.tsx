import { useTranslation } from 'react-i18next'
import {
  CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis,
} from 'recharts'

import { useDailyReport } from '@/lib/ledger'

/**
 * Call volume over the last N days: answered against abandoned.
 *
 * Two series, two meanings from the binding palette — the accent for the
 * ordinary case, the breach red for the one that costs a customer. Both are
 * named in the legend, so identity never rests on colour alone.
 */
export function VolumeTrend({ days }: { days: number }) {
  const { t, i18n } = useTranslation()
  const { from, to } = lastDays(days)
  const { data, isPending, isError } = useDailyReport(from, to)

  const dayFormat = new Intl.DateTimeFormat(i18n.language, { month: 'short', day: 'numeric' })
  const rows = (data?.items ?? []).map((row) => ({
    day: dayFormat.format(new Date(row.day)),
    answered: row.answeredCalls,
    abandoned: row.abandonedCalls,
  }))

  return (
    <section className="rounded-md border bg-card p-4">
      <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {t('supervisor.lastDays', { count: days })}
      </h2>
      {isPending || isError || rows.length === 0 ? (
        <p className="flex h-[220px] items-center text-xs text-muted-foreground">
          {isError ? t('supervisor.queuesFailed') : isPending ? t('common.loading') : t('supervisor.noQueueData')}
        </p>
      ) : (
        <div className="h-[220px]">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={rows} margin={{ top: 4, right: 8, bottom: 0, left: -16 }}>
              <CartesianGrid stroke="var(--border)" strokeDasharray="2 4" vertical={false} />
              <XAxis
                dataKey="day"
                tick={{ fill: 'var(--muted-foreground)', fontSize: 12 }}
                tickLine={false}
                axisLine={{ stroke: 'var(--border)' }}
              />
              <YAxis
                allowDecimals={false}
                width={44}
                tick={{ fill: 'var(--muted-foreground)', fontSize: 12 }}
                tickLine={false}
                axisLine={false}
              />
              <Tooltip
                cursor={{ stroke: 'var(--border)' }}
                contentStyle={{
                  borderRadius: 6,
                  border: '1px solid var(--border)',
                  background: 'var(--popover)',
                  fontSize: 13,
                }}
              />
              <Legend
                verticalAlign="bottom"
                height={24}
                iconType="plainline"
                wrapperStyle={{ fontSize: 12 }}
              />
              <Line
                type="monotone"
                dataKey="answered"
                name={t('supervisor.answered')}
                stroke="var(--state-oncall)"
                strokeWidth={2}
                dot={false}
                activeDot={{ r: 4 }}
              />
              <Line
                type="monotone"
                dataKey="abandoned"
                name={t('supervisor.abandoned')}
                stroke="var(--state-breach)"
                strokeWidth={2}
                dot={false}
                activeDot={{ r: 4 }}
              />
            </LineChart>
          </ResponsiveContainer>
        </div>
      )}
    </section>
  )
}

/** A window of whole local days ending tomorrow midnight, as the API expects. */
function lastDays(days: number): { from: string; to: string } {
  const now = new Date()
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const to = new Date(start)
  to.setDate(to.getDate() + 1)
  const from = new Date(start)
  from.setDate(from.getDate() - (days - 1))
  return { from: from.toISOString(), to: to.toISOString() }
}
