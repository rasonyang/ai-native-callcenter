import { Link, createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'
import { useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation } from '@tanstack/react-query'
import {
  ArrowRightLeft, Grid3x3, Mic, MicOff, Pause, Phone, PhoneOff, PhoneOutgoing,
  Play, Timer, Users,
} from 'lucide-react'
import { Popover } from 'radix-ui'

import { Keypad } from '@/components/keypad'
import { LiveTranscriptBoundary } from '@/components/live-transcript'
import { Select } from '@/components/record-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { describeError } from '@/lib/errors'
import {
  myParty, otherParty, useCallActions, useElapsedSec, useMyCalls, usePresence,
  usePresenceActions, useWaitingCalls,
} from '@/lib/agent'
import { callApi, type CallSnapshot, type WaitingCall } from '@/lib/api'
import { useContactFor } from '@/lib/contacts'
import { useCallbacks, useDispositions, useMyDay } from '@/lib/ledger'
import { useStreamStatus } from '@/lib/use-event-stream'
import { cn, formatDuration } from '@/lib/utils'

/**
 * Agent cockpit: the call and the work waiting on the left, the caller in the
 * middle, presence and wrap-up on the right.
 *
 * Every panel is fed by an endpoint that exists. The live transcript is the
 * centre column's third card: it carries both phases of one conversation,
 * because to the agent reading it that is what it is. The reference design
 * also carries a live queue list and CRM tabs (orders, notes, interaction
 * history); the platform publishes no queue-depth events and has no contact
 * store, so those panels are deliberately absent rather than faked.
 */
export const Route = createFileRoute('/_app/agent/')({ component: AgentCockpit })

function AgentCockpit() {
  const streamStatus = useStreamStatus()
  const { data: presence } = usePresence(true)
  const signedIn = Boolean(presence && presence.state !== 'LOGGED_OUT')
  const { data: calls } = useMyCalls(signedIn)
  const call = calls?.items?.[0]

  return (
    <div className="flex h-full min-h-0 gap-4">
      <div className="flex w-[320px] shrink-0 flex-col gap-4">
        {call ? <CallPanel call={call} /> : signedIn ? <DialCard /> : null}
        <QueueCard signedIn={signedIn} />
        <CallbacksCard />
      </div>

      <div className="flex min-w-0 flex-1 flex-col gap-4">
        <CallerCard call={call} />
        <LiveTranscriptBoundary
          callId={call?.callId}
          myAgentId={call?.parties.find((p) => p.agentId)?.agentId}
          streamStatus={streamStatus}
          className="min-h-0 flex-1"
        />
      </div>

      <div className="flex w-[280px] shrink-0 flex-col gap-4">
        <WrapUpCard />
        <TodayCard />
      </div>
    </div>
  )
}

// --- Left column -----------------------------------------------------------

/** Ringing shows who is calling; established shows the timer and the controls. */
function CallPanel({ call }: { call: CallSnapshot }) {
  const { t } = useTranslation()
  const actions = useCallActions()
  const mine = myParty(call, call.parties.find((p) => p.agentId)?.agentId)
  const other = otherParty(call, mine?.agentId)
  const elapsedSec = useElapsedSec(mine?.answeredAt ?? mine?.createdAt)

  const isRinging = mine?.state === 'RINGING' || mine?.state === 'DIALING'
  const isHeld = mine?.state === 'HELD'
  const isMuted = Boolean(mine?.isMuted)
  // Answering is not instant: the phone gathers ICE before it picks up, which
  // takes seconds. Saying so beats a button that looks like it did nothing.
  const isAnswering = actions.answer.isPending || (actions.answer.isSuccess && isRinging)

  return (
    <Card title={isRinging ? t('call.incoming') : t('agent.activeCall')} aside={<CallBadges call={call} />}>
      <div className="tabular text-base font-semibold">
        {other?.number ?? t('call.unknownNumber')}
      </div>
      <div className="tabular mt-1 text-sm text-muted-foreground">
        {isRinging
          ? t(`partyStates.${mine?.state ?? 'RINGING'}`)
          : isHeld
            ? `${t('partyStates.HELD')} · ${formatDuration(elapsedSec, { padMinutes: true })}`
            : formatDuration(elapsedSec, { padMinutes: true })}
      </div>

      {isRinging ? (
        <div className="mt-3 flex gap-2">
          <Button
            className="flex-1 text-white hover:opacity-90"
            style={{ backgroundColor: 'var(--state-available)' }}
            disabled={isAnswering}
            onClick={() => actions.answer.mutate(call.callId)}
          >
            <Phone />
            {isAnswering ? t('call.answering') : t('call.accept')}
          </Button>
          <Button
            variant="outline"
            className="flex-1"
            onClick={() => actions.hangup.mutate(call.callId)}
          >
            {t('call.decline')}
          </Button>
        </div>
      ) : (
        // Six controls in the reference's 3×2 grid. Five are wired; conference
        // is present and disabled with the reason on the tooltip, because a
        // missing button reads as a missing feature and a fake one is worse
        // than either.
        <div
          role="group"
          aria-label={t('call.controls')}
          className="mt-3 grid grid-cols-3 gap-1.5"
        >
          <CallActionButton
            label={isMuted ? t('call.unmute') : t('call.mute')}
            active={isMuted}
            onClick={() =>
              isMuted ? actions.unmute.mutate(call.callId) : actions.mute.mutate(call.callId)
            }
          >
            {isMuted ? <MicOff /> : <Mic />}
          </CallActionButton>
          <CallActionButton
            label={isHeld ? t('call.retrieve') : t('call.hold')}
            active={isHeld}
            onClick={() =>
              isHeld ? actions.retrieve.mutate(call.callId) : actions.hold.mutate(call.callId)
            }
          >
            {isHeld ? <Play /> : <Pause />}
          </CallActionButton>
          <TransferButton callId={call.callId} />
          <CallActionButton
            disabled
            label={t('call.conference')}
            title={t('call.conferenceUnavailable')}
          >
            <Users />
          </CallActionButton>
          <DTMFButton callId={call.callId} />
          <Button
            variant="destructive"
            className="w-full"
            title={t('call.hangup')}
            aria-label={t('call.hangup')}
            onClick={() => actions.hangup.mutate(call.callId)}
          >
            <PhoneOff />
          </Button>
        </div>
      )}
    </Card>
  )
}

/** One cell of the control grid: an outlined icon button that fills its column. */
function CallActionButton({
  label,
  title,
  active,
  disabled,
  onClick,
  children,
}: {
  label: string
  title?: string
  active?: boolean
  disabled?: boolean
  onClick?: () => void
  children: ReactNode
}) {
  return (
    <Button
      variant="outline"
      aria-label={label}
      title={title ?? label}
      disabled={disabled}
      className={cn('w-full', active && 'border-primary/30 bg-primary/6 text-primary')}
      onClick={onClick}
    >
      {children}
    </Button>
  )
}

/**
 * The in-call pad. Every key is a tone on its way to the far end the moment it
 * is pressed — this is how an agent walks a caller's connection through an
 * external IVR. Composing a number here would mean nothing: the call is up.
 */
function DTMFButton({ callId }: { callId: string }) {
  const { t } = useTranslation()
  const { sendDtmf } = useCallActions()
  const [sent, setSent] = useState('')

  return (
    <Popover.Root onOpenChange={(open) => open && setSent('')}>
      <Popover.Trigger asChild>
        <Button
          variant="outline"
          className="w-full"
          title={t('call.dtmf')}
          aria-label={t('agent.keypad')}
        >
          <Grid3x3 />
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="start"
          sideOffset={6}
          className="z-50 w-44 rounded-md border bg-popover p-3 shadow-md"
        >
          <div className="flex flex-col gap-2">
            <Keypad
              onDigit={(digit) => {
                sendDtmf.mutate({ callId, digits: digit })
                setSent((current) => current + digit)
              }}
            />
            <p className="tabular min-h-4 text-xs text-muted-foreground">
              {sent ? t('call.dtmfSent', { digits: sent }) : t('call.dtmfHint')}
            </p>
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

/** Transfer sends the far end to a queue extension or a number. */
function TransferButton({ callId }: { callId: string }) {
  const { t } = useTranslation()
  const actions = useCallActions()
  const [destination, setDestination] = useState('')
  const [open, setOpen] = useState(false)

  const send = () => {
    const target = destination.trim()
    if (!target) return
    actions.transfer.mutate({ callId, destination: target })
    setDestination('')
    setOpen(false)
  }

  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger asChild>
        <Button
          variant="outline"
          className="w-full"
          title={t('call.transfer')}
          aria-label={t('call.transfer')}
        >
          <ArrowRightLeft />
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="start"
          sideOffset={6}
          className="z-50 rounded-md border bg-popover p-2 shadow-md"
        >
          <form
            className="flex items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              send()
            }}
          >
            <Input
              autoFocus
              className="tabular h-8 w-32"
              value={destination}
              placeholder={t('call.transferTo')}
              aria-label={t('call.transferTo')}
              onChange={(event) => setDestination(event.target.value)}
            />
            <Button type="submit" size="sm">
              {t('call.transfer')}
            </Button>
          </form>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

/** Click-to-dial: the agent's own phone answers first, then the customer rings. */
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
          className="tabular h-8"
          placeholder={t('agent.dialPlaceholder')}
          value={destination}
          inputMode="tel"
          onChange={(event) => setDestination(event.target.value.replace(/[^0-9*#+]/g, ''))}
          onKeyDown={(event) => event.key === 'Enter' && place()}
        />
        <KeypadButton onDigit={(digit) => setDestination((current) => current + digit)} />
        <Button disabled={!destination.trim() || dial.isPending} onClick={place}>
          <PhoneOutgoing />
          {t('agent.dial')}
        </Button>
      </div>
      {dial.isError && (
        <p className="mt-2 text-xs" style={{ color: 'var(--state-breach)' }}>
          {describeError(dial.error, t)}
        </p>
      )}
      {dial.isSuccess && (
        <p className="mt-2 text-xs text-muted-foreground">{t('agent.dialPlaced')}</p>
      )}
    </Card>
  )
}

/**
 * The keypad composes a number before dialling. It does not send DTMF into a
 * live call: no endpoint carries digits to the switch.
 */
function KeypadButton({ onDigit }: { onDigit: (digit: string) => void }) {
  const { t } = useTranslation()
  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <Button variant="outline" size="icon" title={t('agent.keypad')} aria-label={t('agent.keypad')}>
          <Grid3x3 />
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={6}
          className="z-50 w-44 rounded-md border bg-popover p-3 shadow-md"
        >
          <Keypad onDigit={onDigit} />
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

/**
 * The line the agent is working: who is waiting in their queues, longest wait
 * first, which is the order the switch will serve them in.
 *
 * The wait turns red past the queue's own SLA threshold rather than past a
 * number invented here — a queue that promises twenty seconds and one that
 * promises two minutes are not breached at the same moment.
 */
function QueueCard({ signedIn }: { signedIn: boolean }) {
  const { t } = useTranslation()
  const { data, isPending } = useWaitingCalls(signedIn)
  const items = data?.items ?? []

  return (
    <Card
      title={t('agent.myQueue')}
      aside={<span className="tabular">{t('agent.waitingCount', { count: items.length })}</span>}
      className="min-h-0 flex-1"
      bodyClassName="min-h-0 flex-1 overflow-y-auto"
    >
      {!signedIn ? (
        <Empty text={t('agent.signInPrompt')} />
      ) : isPending ? (
        <Empty text={t('common.loading')} />
      ) : items.length === 0 ? (
        <Empty text={t('agent.queueEmpty')} />
      ) : (
        <ul aria-label={t('agent.myQueue')} className="-mx-4 divide-y">
          {items.map((waiting) => (
            <WaitingRow key={waiting.callId} waiting={waiting} />
          ))}
        </ul>
      )}
    </Card>
  )
}

function WaitingRow({ waiting }: { waiting: WaitingCall }) {
  const waitedSec = useElapsedSec(waiting.joinedAt)
  const isBreached = waiting.slaThresholdSec > 0 && waitedSec > waiting.slaThresholdSec

  return (
    <li className="flex h-9 items-center gap-2 px-4">
      <span className="tabular min-w-0 flex-1 truncate text-sm">{waiting.fromNumber}</span>
      <span className="truncate text-xs text-muted-foreground">
        {waiting.queueDisplayName || waiting.queueName}
      </span>
      <span
        className="tabular w-12 shrink-0 text-right text-sm"
        style={isBreached ? { color: 'var(--state-breach)' } : undefined}
      >
        {formatDuration(waitedSec, { padMinutes: true })}
      </span>
    </li>
  )
}

/** The promises waiting to be kept: the agent's real inbound work list. */
function CallbacksCard() {
  const { t } = useTranslation()
  const { data, isPending } = useCallbacks('OPEN')
  const items = data?.items ?? []

  return (
    <Card
      title={t('nav.callbacks')}
      aside={
        <>
          <span className="tabular">{t('agent.openCount', { count: items.length })}</span>
          <Link to="/agent/callbacks" className="hover:text-primary">
            {t('common.viewAll')}
          </Link>
        </>
      }
      className="min-h-0 flex-1"
      bodyClassName="min-h-0 flex-1 overflow-y-auto"
    >
      {isPending ? (
        <Empty text={t('common.loading')} />
      ) : items.length === 0 ? (
        <Empty text={t('callbacks.empty')} />
      ) : (
        <ul className="-mx-4 divide-y">
          {items.map((callback) => (
            <li key={callback.id} className="flex h-9 items-center gap-2 px-4">
              <span className="tabular min-w-0 flex-1 truncate text-sm">
                {callback.phoneNumber}
              </span>
              <span className="truncate text-xs text-muted-foreground">{callback.message}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}

// --- Centre column ---------------------------------------------------------

/**
 * Who is on the phone: the contact behind the number where one is known, the
 * number itself where it is not, plus whatever the flow collected.
 *
 * The lookup is by exact number. A near match would put somebody else's name
 * on the card, and an agent greeting a customer by the wrong name is worse
 * than an agent greeting an unknown number.
 */
function CallerCard({ call }: { call?: CallSnapshot }) {
  const { t } = useTranslation()
  const other = call ? otherParty(call, call.parties.find((p) => p.agentId)?.agentId) : undefined
  const { contact } = useContactFor(other?.number)

  if (!call) {
    return (
      <Card title={t('agent.contact')}>
        <Empty text={t('agent.noContact')} />
      </Card>
    )
  }

  const userData = Object.entries(call.userData ?? {})
  // The heading carries the name where there is one, so everything else the
  // agent might read out loud lines up beneath it.
  const subline = [
    contact ? other?.number : undefined,
    contact?.company || undefined,
    other?.otherNumber ? t('agent.dialled', { number: other.otherNumber }) : undefined,
  ].filter((part): part is string => Boolean(part))

  return (
    <section className="rounded-md border bg-card p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className={cn('text-lg font-semibold', !contact && 'tabular')}>
              {contact?.name || other?.number || t('call.unknownNumber')}
            </span>
            {contact?.tags.map((tag) => <Badge key={tag}>{tag}</Badge>)}
            <CallBadges call={call} />
          </div>
          {subline.length > 0 && (
            <div className="tabular mt-0.5 text-sm text-muted-foreground">
              {subline.join(' · ')}
            </div>
          )}
        </div>
        <div className="shrink-0 text-right">
          <div className="text-xs text-muted-foreground">{t('agent.started')}</div>
          <div className="tabular text-sm">{formatClock(call.createdAt)}</div>
        </div>
      </div>

      {contact?.notes && (
        <p className="mt-3 border-t pt-3 text-sm text-muted-foreground">{contact.notes}</p>
      )}

      {userData.length > 0 && (
        <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 border-t pt-3">
          {userData.map(([key, value]) => (
            <div key={key} className="flex items-baseline justify-between gap-2">
              <dt className="truncate text-xs text-muted-foreground">{key}</dt>
              <dd className="tabular truncate text-sm">{String(value)}</dd>
            </div>
          ))}
        </dl>
      )}
    </section>
  )
}

/** Call type, language and queue — the badges that fit beside a heading. */
function CallBadges({ call }: { call: CallSnapshot }) {
  const { t } = useTranslation()
  return (
    <>
      <Badge>{t(`callTypes.${call.callType}`)}</Badge>
      {call.queue?.name && <Badge>{call.queue.name}</Badge>}
      {call.language && <Badge>{call.language}</Badge>}
    </>
  )
}

// --- Right column ----------------------------------------------------------

/**
 * After-call work: what the call was about, and the button that ends it.
 *
 * It begins when the call ends and lasts until the agent files it — the timer
 * counts up, because nothing is going to take the decision off them. While it
 * runs the switch keeps them out of routing, so the next caller does not reach
 * somebody still writing up the last one.
 *
 * The disposition is required and the call being filed against is the
 * platform's answer, never the browser's: a client that named a call could
 * write a disposition onto anybody's.
 */
function WrapUpCard() {
  const { t } = useTranslation()
  const { data: presence } = usePresence(true)
  const { wrapUp } = usePresenceActions()
  const { data: vocabulary } = useDispositions()
  const [dispositionCode, setDispositionCode] = useState('')
  const [note, setNote] = useState('')

  const inWrapUp = presence?.availability === 'WRAP_UP'
  const elapsedSec = useElapsedSec(presence?.enteredAt)
  const dispositions = vocabulary?.items ?? []

  // The form is offered while there is still a call to file against. The
  // server is the judge of that and answers a conflict when there is not.
  const canFile = inWrapUp || Boolean(presence?.wrapUpCallId)

  const complete = () => {
    wrapUp.mutate(
      { dispositionCode, note: note.trim() || undefined },
      {
        onSuccess: () => {
          setDispositionCode('')
          setNote('')
        },
      },
    )
  }

  return (
    <Card
      title={t('agent.wrapUp')}
      aside={
        inWrapUp ? (
          <span className="tabular flex items-center gap-1 text-sm">
            <Timer className="size-3.5" style={{ color: 'var(--state-acw)' }} />
            {formatDuration(elapsedSec, { padMinutes: true })}
          </span>
        ) : undefined
      }
    >
      {!canFile ? (
        <Empty text={t('agent.wrapUpIdle')} />
      ) : (
        <div className="flex flex-col gap-2">
          <Select
            ariaLabel={t('agent.disposition')}
            value={dispositionCode}
            onChange={setDispositionCode}
            options={[
              { value: '', label: t('agent.chooseDisposition') },
              ...dispositions.map((d) => ({ value: d.code, label: d.label })),
            ]}
          />
          <textarea
            className="min-h-16 w-full rounded-md border bg-card p-2 text-sm outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
            placeholder={t('agent.wrapUpNote')}
            aria-label={t('agent.wrapUpNote')}
            value={note}
            maxLength={4000}
            onChange={(event) => setNote(event.target.value)}
          />
          {/* Nothing chosen means nothing to press: the disposition is what
              makes the call reportable, and the server refuses without one. */}
          <Button
            className="w-full"
            disabled={!dispositionCode || wrapUp.isPending}
            onClick={complete}
          >
            {t('agent.done')}
          </Button>
          {wrapUp.isError && (
            <p className="text-xs" style={{ color: 'var(--state-breach)' }}>
              {describeError(wrapUp.error, t)}
            </p>
          )}
        </div>
      )}
    </Card>
  )
}

/**
 * The agent's own day. Presence used to sit here and said what the softphone
 * bar already says — the status, the extension, the time in state — so this is
 * the question the corner was not answering: how has the day gone.
 *
 * Handle time is talk plus after-call work; occupancy is that against the time
 * signed in. Both come from the server, which has the ledger and the presence
 * history to derive them from.
 */
function TodayCard() {
  const { t } = useTranslation()
  const { data: presence } = usePresence(true)
  const signedIn = Boolean(presence && presence.state !== 'LOGGED_OUT')
  const { data: today, isPending } = useMyDay(signedIn)

  if (!signedIn) {
    return (
      <Card title={t('agent.today')}>
        <Empty text={t('agent.signInPrompt')} />
      </Card>
    )
  }

  return (
    <Card title={t('agent.today')}>
      {isPending || !today ? (
        <Empty text={t('common.loading')} />
      ) : (
        <dl className="divide-y">
          <Row label={t('agent.callsHandled')}>
            <span className="tabular font-semibold">{today.callsHandled}</span>
          </Row>
          <Row label={t('agent.aht')}>
            <span className="tabular font-semibold">{formatDuration(today.avgHandleSec)}</span>
          </Row>
          <Row label={t('agent.acwAvg')}>
            <span className="tabular font-semibold">{formatDuration(today.avgWrapUpSec)}</span>
          </Row>
          <Row label={t('agent.occupancy')}>
            <span className="tabular font-semibold">{today.occupancyPct}%</span>
          </Row>
        </dl>
      )}
    </Card>
  )
}

// --- Shared ----------------------------------------------------------------

function Card({
  title,
  aside,
  children,
  className,
  bodyClassName,
}: {
  title: string
  aside?: ReactNode
  children: ReactNode
  className?: string
  bodyClassName?: string
}) {
  return (
    <section className={cn('flex flex-col rounded-md border bg-card p-4', className)}>
      <div className="mb-3 flex shrink-0 items-center justify-between gap-2">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {title}
        </h2>
        {/* Secondary information sits at the end of the title line, in the
            same recessive ink as the title itself. */}
        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">{aside}</span>
      </div>
      <div className={bodyClassName}>{children}</div>
    </section>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex h-8 items-center justify-between gap-2">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

function Empty({ text }: { text: string }) {
  return <p className="text-xs text-muted-foreground">{text}</p>
}

function formatClock(iso: string): string {
  return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}
