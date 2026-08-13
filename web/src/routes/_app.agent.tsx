import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

/**
 * Agent workspace. M1 establishes the route and shell; the three-column
 * cockpit (active call, live transcript, wrap-up) arrives with the telephony
 * core in M2.
 */
export const Route = createFileRoute('/_app/agent')({ component: AgentDashboard })

function AgentDashboard() {
  const { t } = useTranslation()
  return (
    <section className="rounded-md border bg-card p-4">
      <h1 className="text-base font-medium">{t('nav.dashboard')}</h1>
      <p className="mt-1 text-xs text-muted-foreground">{t('app.tagline')}</p>
    </section>
  )
}
