import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useMutation } from '@tanstack/react-query'
import {
  ChevronDown, Grid3x3, LogIn, LogOut, Mic, MicOff, Pause, Phone, PhoneIncoming,
  PhoneOff, PhoneOutgoing, Play, Shuffle,
} from 'lucide-react'
import { DropdownMenu, Popover } from 'radix-ui'

import { Keypad } from '@/components/keypad'
import { StatusDot } from '@/components/status-pill'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  SELECTABLE_REASONS, myParty, otherParty, useCallActions, useElapsedSec,
  useIsWrapUpPending, useMyCalls, usePresence, usePresenceActions,
} from '@/lib/agent'
import {
  callApi, type Availability, type CallSnapshot, type NotReadyReason, type PartyState, type Presence,
} from '@/lib/api'
import { describeError } from '@/lib/errors'
import { optionsUrl, usePhoneBridge } from '@/lib/phone-bridge'
import { phoneChipFor } from '@/lib/phone'
import { cn, formatDuration } from '@/lib/utils'

/**
 * The agent's controls, living in the topbar beside the breadcrumb.
 *
 * It never touches audio: the Chrome softphone owns the microphone and the
 * registration, FreeSWITCH owns the call. This expresses intent and shows what
 * the switch reports back.
 */
export function SoftphoneBar() {
  const { data: presence } = usePresence(true)
  const { data: calls } = useMyCalls(Boolean(presence && presence.state !== 'LOGGED_OUT'))
  const chip = usePhoneChip(presence)

  if (!presence) return null

  const call = calls?.items?.[0]
  if (presence.state === 'LOGGED_OUT') {
    return (
      <div className="flex h-10 items-center gap-1 rounded-md border bg-card px-2">
        <SignIn />
      </div>
    )
  }

  // Four segments divided by hairlines: whether the phone is there at all,
  // who the agent is to the queue, what call they are on, what they can do to
  // it. The phone comes first because none of the rest means anything without
  // one.
  return (
    <div className="flex h-10 shrink-0 items-center gap-1 rounded-md border bg-card pl-1.5 pr-1">
      <PhoneChip chip={chip} extensionNumber={presence.extensionNumber} />
      <Divider />
      <PresenceControl
        availability={presence.availability}
        reason={presence.reason}
        enteredAt={presence.enteredAt}
        state={presence.state}
        hasCall={Boolean(call)}
        isPhoneReady={chip.isReadyAllowed}
      />
      <Divider />
      <span className="flex w-56 min-w-0 items-center px-1.5">
        <CallInfo call={call} />
      </span>
      <Divider />
      <ActionBar call={call} />
    </div>
  )
}

/** The hairline between two segments of the bar. */
function Divider() {
  return <span aria-hidden className="h-4 w-px shrink-0 bg-border" />
}

/**
 * Whether this browser has a phone, said in one line.
 *
 * The switch and the extension are asked separately and neither is taken on
 * trust for what the other knows: see `phoneChipFor`.
 */
function usePhoneChip(presence: Presence | null | undefined) {
  const { detected, state } = usePhoneBridge()
  return phoneChipFor(
    Boolean(presence?.isDeviceRegistered),
    detected ? state : undefined,
    presence?.extensionNumber,
  )
}

/**
 * The phone segment: a dot, what is wrong, and the one thing to do about it.
 *
 * It is not a duplicate of the presence pill beside it. Presence is what the
 * agent chose; this is whether the choice can be honoured.
 */
function PhoneChip({
  chip,
  extensionNumber,
}: {
  chip: ReturnType<typeof usePhoneChip>
  extensionNumber?: string
}) {
  const { t } = useTranslation()
  const { reprovision, openOnboarding, provisionErrorCode, extensionId } = usePhoneBridge()

  // A server that will not mint a session at all outranks whatever the
  // extension is reporting: there is no phone for it to register with, and no
  // amount of retrying binds one.
  const label = provisionErrorCode === 'CONFLICT' ? t('phone.noExtension') : t(chip.labelKey)

  // Nothing is set up yet, so the chip is the way into setting it up.
  if (chip.action === 'setup') {
    return (
      <Button variant="ghost" size="sm" className="gap-1.5" onClick={openOnboarding}>
        <StatusDot color={chip.dot} />
        {label}
      </Button>
    )
  }

  return (
    <span className="flex items-center gap-1.5">
      <Badge className="gap-1.5">
        <StatusDot color={chip.dot} />
        {label}
        {chip.kind === 'ready' && extensionNumber && (
          <span className="tabular">{` \u00b7 ${extensionNumber}`}</span>
        )}
      </Badge>
      {/* An override is undone where it was made, so the chip links there
          rather than offering to replace a credential the extension has been
          told to keep dormant. With no id to address there is no link to
          render, and the chip simply says what is going on. */}
      {chip.action === 'options' && optionsUrl('account', extensionId) && (
        <Button asChild variant="ghost" size="sm">
          <a
            href={optionsUrl('account', extensionId) ?? undefined}
            target="_blank"
            rel="noopener noreferrer"
          >
            {t('phone.openOptions')}
          </a>
        </Button>
      )}
      {(chip.action === 'retry' || chip.action === 'reprovision') && (
        <Button variant="ghost" size="sm" onClick={reprovision}>
          {chip.action === 'retry' ? t('phone.retry') : t('phone.reprovision')}
        </Button>
      )}
    </span>
  )
}

/**
 * Signing in takes no input: the agent is bound to one extension in
 * configuration and signs in there. A refusal (no phone bound, or somebody
 * else on it) is reported where the button is.
 */
function SignIn() {
  const { t } = useTranslation()
  const { signIn } = usePresenceActions()

  return (
    <div className="flex items-center gap-2">
      <Button
        size="sm"
        className="h-6 px-2 text-xs"
        disabled={signIn.isPending}
        onClick={() => signIn.mutate(undefined)}
      >
        <LogIn className="size-3" />
        {t('agent.signIn')}
      </Button>
      {signIn.isError ? (
        <span className="text-xs" style={{ color: 'var(--state-breach)' }}>
          {describeError(signIn.error, t)}
        </span>
      ) : (
        <span className="text-xs text-muted-foreground">{t('agent.signInPrompt')}</span>
      )}
    </div>
  )
}

/** State pill with the menu of everything the agent may switch to. */
function PresenceControl({
  availability,
  reason,
  enteredAt,
  state,
  hasCall,
  isPhoneReady,
}: {
  availability: Availability
  reason?: NotReadyReason
  enteredAt: string
  state: string
  /** On a call the call segment owns the clock, so the chip drops its own. */
  hasCall: boolean
  /**
   * Whether a call could actually be delivered. Going ready without a phone
   * puts the agent at the head of a queue that cannot ring them, which the
   * server refuses anyway (409 DEVICE_NOT_REGISTERED) — offering the item and
   * letting it fail tells the agent nothing about what to fix.
   */
  isPhoneReady: boolean
}) {
  const { t } = useTranslation()
  const { signOut, ready, notReady } = usePresenceActions()
  const elapsedSec = useElapsedSec(enteredAt)
  // After-call work the agent has not confirmed keeps them out of the queue on
  // purpose: going ready with the last call unwritten is how a call becomes
  // unreportable. It is guidance, not a lock — lunch and signing out are still
  // theirs to choose.
  const isWrapUpPending = useIsWrapUpPending()

  const label =
    availability === 'NOT_READY' && reason
      ? t(`reasons.${reason}`)
      : t(`availability.${availability}`)

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        {/* The dot carries the state; the label stays ordinary ink. Colour on
            both would make every glance a colour-matching exercise, and red is
            reserved for destructive actions and breached timers. */}
        <Button variant="ghost" className="px-2">
          <StatusDot availability={availability} />
          {label}
          {!hasCall && (
            <span className="tabular text-xs text-muted-foreground">
              {formatDuration(elapsedSec, { padMinutes: true })}
            </span>
          )}
          <ChevronDown className="size-3 text-muted-foreground" />
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="start"
          sideOffset={6}
          className="z-50 min-w-40 rounded-md border bg-popover p-1 text-sm shadow-md"
        >
          {state !== 'READY' && (
            <DropdownMenu.Item
              className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 outline-none data-[highlighted]:bg-muted data-disabled:opacity-50"
              disabled={isWrapUpPending || !isPhoneReady}
              title={
                isWrapUpPending
                  ? t('agent.wrapUpBlocks')
                  : !isPhoneReady
                    ? t('phone.readyBlocked')
                    : undefined
              }
              onSelect={() => ready.mutate()}
            >
              <StatusDot availability="READY" />
              {t('agent.goReady')}
            </DropdownMenu.Item>
          )}
          {isWrapUpPending && (
            <p className="px-2 py-1 text-xs text-muted-foreground">{t('agent.wrapUpBlocks')}</p>
          )}
          {!isWrapUpPending && !isPhoneReady && state !== 'READY' && (
            <p className="px-2 py-1 text-xs text-muted-foreground">{t('phone.readyBlocked')}</p>
          )}
          {ready.isError && (
            <p className="px-2 py-1 text-xs" style={{ color: 'var(--state-breach)' }}>
              {describeError(ready.error, t)}
            </p>
          )}
          {SELECTABLE_REASONS.map((r) => (
            <DropdownMenu.Item
              key={r}
              className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 outline-none data-[highlighted]:bg-muted"
              onSelect={() => notReady.mutate(r)}
            >
              <StatusDot availability="NOT_READY" />
              {t(`reasons.${r}`)}
            </DropdownMenu.Item>
          ))}
          <div className="my-1 h-px bg-border" />
          <DropdownMenu.Item
            className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 outline-none data-[highlighted]:bg-muted"
            onSelect={() => signOut.mutate()}
          >
            <LogOut className="size-4" />
            {t('agent.signOut')}
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}

/** The state colour of a leg — carried by the direction icon, nothing else. */
const PARTY_STATE_COLOR: Record<PartyState, string> = {
  DIALING: 'var(--state-ringing)',
  RINGING: 'var(--state-ringing)',
  TALKING: 'var(--state-oncall)',
  HELD: 'var(--state-ringing)',
  RELEASED: 'var(--state-offline)',
}

/** Who is on the line and for how long. Fixed width, so nothing jitters. */
function CallInfo({ call }: { call?: CallSnapshot }) {
  const { t } = useTranslation()
  // Who "I" am comes from the session. Both parties of an extension-to-extension
  // call carry an agentId, so taking the first one showed the callee the
  // caller's leg — the bar read the placer's DIALING on the receiver's screen.
  const { data: presence } = usePresence(true)
  const agentId = presence?.agentId
  const mine = call ? myParty(call, agentId) : undefined
  const other = call ? otherParty(call, agentId) : undefined
  const isEstablished = mine?.state === 'TALKING' || mine?.state === 'HELD'
  const elapsedSec = useElapsedSec(isEstablished ? (mine?.answeredAt ?? mine?.createdAt) : undefined)

  if (!call) {
    return <span className="truncate text-xs text-muted-foreground">{t('agent.idle')}</span>
  }

  // Which way the call went is the agent's own position in it, not the call
  // type: an internal call is outgoing for whoever placed it and incoming for
  // whoever was rung.
  const DirectionIcon = mine?.role === 'ORIGINATOR' ? PhoneOutgoing : PhoneIncoming
  return (
    <span className="flex min-w-0 items-center gap-2">
      <DirectionIcon
        className="size-3.5 shrink-0"
        style={{ color: PARTY_STATE_COLOR[mine?.state ?? 'RINGING'] }}
      />
      <span className="tabular truncate text-sm font-medium">
        {/* A leg the switch has not raised yet, or has not reached this
            snapshot, is still a number the agent knows: it rides their own
            leg, which is where the ledger reads it from too. */}
        {other?.number ?? mine?.otherNumber ?? t('call.unknownNumber')}
      </span>
      {isEstablished ? (
        <span className="tabular shrink-0 text-xs text-muted-foreground">
          {formatDuration(elapsedSec, { padMinutes: true })}
        </span>
      ) : (
        <span className="shrink-0 text-xs text-muted-foreground">
          {t(`partyStates.${mine?.state ?? 'RINGING'}`)}
        </span>
      )}
    </span>
  )
}

/**
 * Everything the agent can do to the call, as icon buttons.
 *
 * Mute is shown because the reference cockpit shows it and its absence reads
 * as a missing feature — but this platform has no endpoint that mutes a leg,
 * so it is disabled and says why rather than pretending.
 */
function ActionBar({ call }: { call?: CallSnapshot }) {
  const { t } = useTranslation()
  const actions = useCallActions()
  const [transferTo, setTransferTo] = useState('')
  const { data: presence } = usePresence(true)

  const agentId = presence?.agentId
  const mine = call ? myParty(call, agentId) : undefined
  // Being rung and ringing somebody share no button. RINGING is a call handed
  // to this agent and can be answered; DIALING is one they placed, and there is
  // nothing to answer — offering the key put an Answer button on the screen of
  // the person who had just dialled. Same split the cockpit makes.
  const isOffered = mine?.state === 'RINGING'
  const isUnanswered = isOffered || mine?.state === 'DIALING'
  const isHeld = mine?.state === 'HELD'
  const isEstablished = mine?.state === 'TALKING' || isHeld
  const isMuted = Boolean(mine?.isMuted)
  // Answering is not instant: the phone gathers ICE before it picks up, which
  // takes seconds. Saying so beats a button that looks like it did nothing.
  const isAnswering = actions.answer.isPending || (actions.answer.isSuccess && isOffered)

  return (
    <span className="flex items-center gap-0.5">
      {isOffered && call && (
        <Button
          size="icon"
          title={t('call.answer')}
          aria-label={t('call.answer')}
          className="text-white hover:opacity-90"
          style={{ backgroundColor: 'var(--state-available)' }}
          disabled={isAnswering}
          onClick={() => actions.answer.mutate(call.callId)}
        >
          <Phone />
        </Button>
      )}
      <Button
        variant="ghost"
        size="icon"
        disabled={!isEstablished || !call}
        title={isMuted ? t('call.unmute') : t('call.mute')}
        aria-label={isMuted ? t('call.unmute') : t('call.mute')}
        className={cn(isMuted && 'bg-primary/6 text-primary')}
        onClick={() =>
          call && (isMuted ? actions.unmute.mutate(call.callId) : actions.mute.mutate(call.callId))
        }
      >
        {isMuted ? <MicOff /> : <Mic />}
      </Button>
      <Button
        variant="ghost"
        size="icon"
        disabled={!isEstablished || !call}
        title={isHeld ? t('call.retrieve') : t('call.hold')}
        aria-label={isHeld ? t('call.retrieve') : t('call.hold')}
        className={cn(isHeld && 'bg-primary/6 text-primary')}
        onClick={() =>
          call && (isHeld ? actions.retrieve.mutate(call.callId) : actions.hold.mutate(call.callId))
        }
      >
        {isHeld ? <Play /> : <Pause />}
      </Button>
      {call && isEstablished && (
        <TransferMenu
          value={transferTo}
          onChange={setTransferTo}
          onSubmit={() => {
            if (!transferTo.trim()) return
            actions.transfer.mutate({ callId: call.callId, destination: transferTo.trim() })
            setTransferTo('')
          }}
        />
      )}
      <DialPadPopover call={call} />
      <Button
        size="icon"
        variant="destructive"
        disabled={!call}
        title={isUnanswered ? t('call.decline') : t('call.hangup')}
        aria-label={isUnanswered ? t('call.decline') : t('call.hangup')}
        onClick={() => call && actions.hangup.mutate(call.callId)}
      >
        <PhoneOff />
      </Button>
    </span>
  )
}

/**
 * One pad, two jobs. Idle it composes a number and dials it; on a call each
 * key goes straight out as a tone towards the far end, which is what reaches
 * an IVR the caller was transferred to. Nothing is queued in between — an
 * agent pressing 3 for "accounts" expects the tone now, not on submit.
 */
function DialPadPopover({ call }: { call?: CallSnapshot }) {
  const { t } = useTranslation()
  const [number, setNumber] = useState('')
  const dial = useMutation({ mutationFn: callApi.dial })
  const { sendDtmf } = useCallActions()
  const hasCall = Boolean(call)
  // Dialling out with the last call still unwritten is the same problem as
  // going ready: the record would be left standing on its defaults while the
  // agent starts another conversation. Tones into a live call are unaffected —
  // that is this pad's other job, and it belongs to the call in progress.
  const isWrapUpPending = useIsWrapUpPending()

  const place = () => {
    const destination = number.trim()
    if (destination) dial.mutate(destination, { onSuccess: () => setNumber('') })
  }

  const press = (digit: string) => {
    if (call) sendDtmf.mutate({ callId: call.callId, digits: digit })
    setNumber((current) => current + digit)
  }

  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <Button
          variant="ghost"
          size="icon"
          disabled={!hasCall && isWrapUpPending}
          title={!hasCall && isWrapUpPending ? t('agent.wrapUpBlocks') : t('agent.keypad')}
          aria-label={t('agent.keypad')}
        >
          <Grid3x3 />
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={6}
          className="z-50 w-48 rounded-md border bg-popover p-3 shadow-md"
        >
          <div className="flex flex-col gap-2">
            <Input
              className="tabular h-8"
              value={number}
              readOnly={hasCall}
              inputMode="tel"
              placeholder={t('agent.dialPlaceholder')}
              aria-label={hasCall ? t('call.dtmf') : t('agent.dialPlaceholder')}
              onChange={(event) => setNumber(event.target.value.replace(/[^0-9*#+]/g, ''))}
              onKeyDown={(event) => event.key === 'Enter' && !hasCall && place()}
            />
            <Keypad onDigit={press} />
            {hasCall ? (
              <p className="text-xs text-muted-foreground">{t('call.dtmfHint')}</p>
            ) : (
              <Button
                className="w-full"
                disabled={!number.trim() || dial.isPending}
                onClick={place}
              >
                <Phone />
                {t('agent.dial')}
              </Button>
            )}
            {dial.isError && (
              <p className="text-xs" style={{ color: 'var(--state-breach)' }}>
                {describeError(dial.error, t)}
              </p>
            )}
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

function TransferMenu({
  value,
  onChange,
  onSubmit,
}: {
  value: string
  onChange: (next: string) => void
  onSubmit: () => void
}) {
  const { t } = useTranslation()
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button
          variant="ghost"
          size="icon"
          title={t('call.transfer')}
          aria-label={t('call.transfer')}
        >
          <Shuffle />
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={6}
          className="z-50 rounded-md border bg-popover p-2 shadow-md"
        >
          <form
            className="flex items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              onSubmit()
            }}
          >
            <Input
              autoFocus
              className="h-7 w-28 text-xs"
              value={value}
              onChange={(event) => onChange(event.target.value)}
              placeholder={t('call.transferTo')}
              aria-label={t('call.transferTo')}
            />
            <Button size="sm" type="submit" className="h-7 px-2 text-xs">
              {t('call.transfer')}
            </Button>
          </form>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}
