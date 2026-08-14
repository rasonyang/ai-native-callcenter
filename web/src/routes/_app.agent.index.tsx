import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation } from '@tanstack/react-query'
import { PhoneOutgoing } from 'lucide-react'

import { StatusPill } from '@/components/status-pill'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import { myParty, useElapsedSec, useMyCalls, usePresence } from '@/lib/agent'
import { callApi, type CallSnapshot } from '@/lib/api'
import { formatDuration } from '@/lib/utils'

/**
 * Agent cockpit: the queue and the call on the left, the customer in the
 * middle, wrap-up and today's numbers on the right. Call controls live in the
 * topbar, where they stay reachable from anywhere.
 */
export const Route = createFileRoute('/_app/agent/')({ component: AgentCockpit })

function AgentCockpit() {
  const { t } = useTranslation()
  const { data: presence } = usePresence(true)
  const signedIn = Boolean(presence && presence.state !== 'LOGGED_OUT')
  const { data: calls } = useMyCalls(signedIn)
  const call = calls?.items?.[0]

  return (
    <div className="grid grid-cols-[320px_1fr_280px] items-start gap-4">
      <div className="flex flex-col gap-4">
        {call && <IncomingOrActive call={call} />}
        {signedIn && !call && <DialCard />}
        <Card title={t('agent.myQueue')} aside={t('agent.waitingCount', { count: 0 })}>
          {/* Queue depth arrives with the queue metrics; an honest empty row
              beats a number nobody computed. */}
          <Empty text={t('agent.queueEmpty')} />
        </Card>
      </div>

      <div className="flex flex-col gap-4">
        <Card title={t('agent.contact')}>
          {call ? (
            <div>
              <div className="tabular text-base font-medium">
                {call.parties.find((p) => !p.agentId)?.number ?? t('call.unknownNumber')}
              </div>
              <div className="mt-1 text-xs text-muted-foreground">
                {t(`callTypes.${call.callType}`)}
              </div>
            </div>
          ) : (
            <Empty text={t('agent.noContact')} />
          )}
        </Card>
        <Card title={t('agent.transcript')}>
          <Empty text={t('agent.noTranscript')} />
        </Card>
      </div>

      <div className="flex flex-col gap-4">
        <Card title={t('agent.presence')}>
          {presence && signedIn ? (
            <dl className="space-y-2">
              <Row label={t('agent.status')}>
                <StatusPill availability={presence.availability} reason={presence.reason} />
              </Row>
              <Row label={t('agent.extension')}>
                <span className="tabular">{presence.extensionNumber}</span>
              </Row>
            </dl>
          ) : (
            <Empty text={t('agent.signInPrompt')} />
          )}
        </Card>
        <Card title={t('agent.today')}>
          <Empty text={t('agent.statsLater')} />
        </Card>
      </div>
    </div>
  )
}

/** Click-to-dial: the agent's leg answers itself, then the customer rings. */
function DialCard() {
  const { t } = useTranslation()
  const [destination, setDestination] = useState('')
  const dial = useMutation({ mutationFn: callApi.dial })

  const place = () => {
    const number = destination.trim()
    if (number) dial.mutate(number, { onSuccess: () => setDestination('') })
  }

  return (
    <Card title={t('agent.dialOut')}>
      <div className="flex gap-2">
        <Input
          className="h-8 tabular"
          placeholder={t('agent.dialPlaceholder')}
          value={destination}
          inputMode="tel"
          onChange={(e) => setDestination(e.target.value.replace(/[^0-9]/g, ''))}
          onKeyDown={(e) => e.key === 'Enter' && place()}
        />
        <Button size="sm" disabled={!destination.trim() || dial.isPending} onClick={place}>
          <PhoneOutgoing />
          {t('agent.dial')}
        </Button>
      </div>
      {dial.isError && (
        <p className="mt-2 text-xs text-muted-foreground">{describeError(dial.error, t)}</p>
      )}
      {dial.isSuccess && (
        <p className="mt-2 text-xs text-muted-foreground">{t('agent.dialPlaced')}</p>
      )}
    </Card>
  )
}

/** The call panel: ringing shows who is calling, talking shows the timer. */
function IncomingOrActive({ call }: { call: CallSnapshot }) {
  const { t } = useTranslation()
  const agentId = call.parties.find((p) => p.agentId)?.agentId
  const mine = myParty(call, agentId)
  const other = call.parties.find((p) => !p.agentId)
  const elapsedSec = useElapsedSec(mine?.answeredAt ?? mine?.createdAt)
  const isRinging = mine?.state === 'RINGING' || mine?.state === 'DIALING'

  return (
    <Card title={isRinging ? t('call.incoming') : t('agent.activeCall')}>
      <div className="tabular text-base font-medium">
        {other?.number ?? t('call.unknownNumber')}
      </div>
      <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span>{t(`callTypes.${call.callType}`)}</span>
        <span>·</span>
        <span>{t(`partyStates.${mine?.state ?? 'RINGING'}`)}</span>
        <span>·</span>
        <span className="tabular">{formatDuration(elapsedSec)}</span>
      </div>
      <p className="mt-3 text-xs text-muted-foreground">{t('call.controlsInTopbar')}</p>
    </Card>
  )
}

function Card({
  title,
  aside,
  children,
}: {
  title: string
  aside?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="rounded-md border bg-card p-4">
      <div className="mb-3 flex items-baseline justify-between">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {title}
        </h2>
        {aside && <span className="text-xs text-muted-foreground">{aside}</span>}
      </div>
      {children}
    </section>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

function Empty({ text }: { text: string }) {
  return <p className="text-xs text-muted-foreground">{text}</p>
}
