import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useQuery } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * Supervisor wallboard. M1 shows live server health so the event stream and
 * session plumbing are visible end to end; queue KPIs follow in M2.
 */
export const Route = createFileRoute('/_app/supervisor')({ component: Wallboard })

function Wallboard() {
  const { t } = useTranslation()
  const health = useQuery({ queryKey: ['health'], queryFn: api.health, retry: false })

  return (
    <section className="rounded-md border bg-card p-4">
      <h1 className="text-base font-medium">{t('nav.wallboard')}</h1>
      <dl className="mt-3 grid grid-cols-2 gap-4 text-sm">
        <div>
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">
            {t('stream.connected')}
          </dt>
          <dd className="tabular text-xl">{health.data?.sseClients ?? '—'}</dd>
        </div>
        <div>
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">seq</dt>
          <dd className="tabular text-xl">{health.data?.oldestSeq ?? '—'}</dd>
        </div>
      </dl>
    </section>
  )
}
