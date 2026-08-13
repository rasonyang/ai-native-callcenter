import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { Headphones, Inbox } from 'lucide-react'

import { SoftphoneBar } from '@/components/softphone-bar'
import { StatePill } from '@/components/state-dot'
import { useElapsedSec, usePresence } from '@/lib/agent'
import { formatDuration } from '@/lib/utils'

/**
 * Agent cockpit.
 *
 * The three-column shell is in place; the call panels fill in as call control
 * lands. Presence is live now, which is what decides whether anything is
 * routed here at all.
 */
export const Route = createFileRoute('/_app/agent')({ component: AgentCockpit })

function AgentCockpit() {
  const { t } = useTranslation()
  const { data: presence } = usePresence(true)
  const elapsedSec = useElapsedSec(presence?.enteredAt)

  return (
    <div className="flex h-full flex-col gap-4">
      <SoftphoneBar />

      <div className="grid flex-1 grid-cols-[320px_1fr_280px] gap-4">
        <div className="flex flex-col gap-4">
          <Panel title={t('agent.activeCall')}>
            <Empty icon={<Headphones className="size-4" />} text={t('agent.noActiveCall')} />
          </Panel>
          <Panel title={t('agent.myQueue')}>
            <Empty icon={<Inbox className="size-4" />} text={t('agent.queueEmpty')} />
          </Panel>
        </div>

        <div className="flex flex-col gap-4">
          <Panel title={t('agent.contact')}>
            <p className="text-xs text-muted-foreground">{t('agent.noContact')}</p>
          </Panel>
          <Panel title={t('agent.transcript')} className="flex-1">
            <p className="text-xs text-muted-foreground">{t('agent.noTranscript')}</p>
          </Panel>
        </div>

        <div className="flex flex-col gap-4">
          <Panel title={t('agent.presence')}>
            {presence && presence.state !== 'LOGGED_OUT' ? (
              <dl className="space-y-2 text-sm">
                <Row label={t('agent.status')}>
                  <StatePill availability={presence.availability} reason={presence.reason} />
                </Row>
                <Row label={t('agent.timeInState')}>
                  <span className="tabular">{formatDuration(elapsedSec)}</span>
                </Row>
                <Row label={t('agent.extension')}>
                  <span className="tabular">{presence.extensionNumber}</span>
                </Row>
              </dl>
            ) : (
              <p className="text-xs text-muted-foreground">{t('agent.signInPrompt')}</p>
            )}
          </Panel>
        </div>
      </div>
    </div>
  )
}

function Panel({
  title,
  className,
  children,
}: {
  title: string
  className?: string
  children: React.ReactNode
}) {
  return (
    <section className={`rounded-md border bg-card p-4 ${className ?? ''}`}>
      <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </h2>
      {children}
    </section>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

function Empty({ icon, text }: { icon: React.ReactNode; text: string }) {
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      {icon}
      {text}
    </div>
  )
}
