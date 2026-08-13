import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  ChevronDown, LogIn, LogOut, Pause, PhoneIncoming, PhoneOff, Play, Shuffle,
} from 'lucide-react'
import { DropdownMenu } from 'radix-ui'

import { StatusDot } from '@/components/status-pill'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  SELECTABLE_REASONS, myParty, useCallActions, useElapsedSec, useMyCalls,
  usePresence, usePresenceActions,
} from '@/lib/agent'
import type { CallSnapshot, NotReadyReason } from '@/lib/api'
import { AVAILABILITY_COLOR } from '@/lib/agent'
import { formatDuration } from '@/lib/utils'

/**
 * The agent's controls, living in the topbar beside the breadcrumb.
 *
 * It never touches audio: the Chrome softphone owns the microphone and the
 * registration, FreeSWITCH owns the call. This expresses intent and shows what
 * the switch reports back.
 */
export function SoftphoneBar() {
  const { t } = useTranslation()
  const { data: presence } = usePresence(true)
  const { data: calls } = useMyCalls(Boolean(presence && presence.state !== 'LOGGED_OUT'))

  if (!presence) return null

  const call = calls?.items?.[0]
  return (
    <div className="flex h-8 items-center gap-2 rounded-md border px-2">
      {presence.state === 'LOGGED_OUT' ? (
        <SignIn />
      ) : (
        <>
          <PresenceControl
            availability={presence.availability}
            reason={presence.reason}
            enteredAt={presence.enteredAt}
            state={presence.state}
          />
          <span className="h-5 w-px bg-border" />
          {call ? <CallControls call={call} /> : (
            <span className="px-1 text-xs text-muted-foreground">{t('agent.idle')}</span>
          )}
        </>
      )}
    </div>
  )
}

function SignIn() {
  const { t } = useTranslation()
  const { signIn } = usePresenceActions()
  const [extension, setExtension] = useState('')

  return (
    <form
      className="flex items-center gap-2"
      onSubmit={(event) => {
        event.preventDefault()
        if (extension.trim()) signIn.mutate(extension.trim())
      }}
    >
      <span className="text-xs text-muted-foreground">{t('agent.signInPrompt')}</span>
      <Input
        className="h-6 w-24 text-xs"
        value={extension}
        onChange={(event) => setExtension(event.target.value)}
        placeholder={t('agent.extension')}
        aria-label={t('agent.extension')}
      />
      <Button size="sm" type="submit" className="h-6 px-2 text-xs" disabled={signIn.isPending}>
        <LogIn className="size-3" />
        {t('agent.signIn')}
      </Button>
    </form>
  )
}

/** State pill with the menu of everything the agent may switch to. */
function PresenceControl({
  availability,
  reason,
  enteredAt,
  state,
}: {
  availability: Parameters<typeof StatusDot>[0]['availability']
  reason?: NotReadyReason
  enteredAt: string
  state: string
}) {
  const { t } = useTranslation()
  const { signOut, ready, notReady } = usePresenceActions()
  const elapsedSec = useElapsedSec(enteredAt)

  const label =
    availability === 'NOT_READY' && reason
      ? t(`reasons.${reason}`)
      : t(`availability.${availability}`)

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button className="flex items-center gap-1.5 rounded-sm px-1 text-sm hover:bg-muted">
          <StatusDot availability={availability} />
          <span style={{ color: AVAILABILITY_COLOR[availability] }}>{label}</span>
          <span className="tabular text-xs text-muted-foreground">{formatDuration(elapsedSec)}</span>
          <ChevronDown className="size-3 text-muted-foreground" />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="start"
          sideOffset={6}
          className="z-50 min-w-40 rounded-md border bg-popover p-1 text-sm shadow-md"
        >
          {state !== 'READY' && (
            <DropdownMenu.Item
              className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 outline-none data-[highlighted]:bg-muted"
              onSelect={() => ready.mutate()}
            >
              <StatusDot availability="READY" />
              {t('agent.goReady')}
            </DropdownMenu.Item>
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

/** The call itself: who, how long, and the actions its state allows. */
function CallControls({ call }: { call: CallSnapshot }) {
  const { t } = useTranslation()
  const actions = useCallActions()
  const [transferTo, setTransferTo] = useState('')

  const agentId = call.parties.find((p) => p.agentId)?.agentId
  const mine = myParty(call, agentId)
  const other = call.parties.find((p) => !p.agentId)
  const elapsedSec = useElapsedSec(mine?.answeredAt ?? mine?.createdAt)

  const isRinging = mine?.state === 'RINGING' || mine?.state === 'DIALING'
  const isHeld = mine?.state === 'HELD'
  // Answering is not instant: the phone gathers ICE before it picks up, which
  // takes seconds. Saying so beats a button that looks like it did nothing.
  const isAnswering = actions.answer.isPending || (actions.answer.isSuccess && isRinging)

  return (
    <div className="flex items-center gap-2">
      <span className="tabular text-sm">{other?.number ?? t('call.unknownNumber')}</span>
      <span className="tabular text-xs text-muted-foreground">{formatDuration(elapsedSec)}</span>

      {isRinging ? (
        <Button
          size="sm"
          className="h-6 gap-1 px-2 text-xs"
          style={{ backgroundColor: 'var(--state-available)' }}
          disabled={isAnswering}
          onClick={() => actions.answer.mutate(call.callId)}
        >
          <PhoneIncoming className="size-3" />
          {isAnswering ? t('call.answering') : t('call.answer')}
        </Button>
      ) : (
        <>
          {isHeld ? (
            <IconButton title={t('call.retrieve')} onClick={() => actions.retrieve.mutate(call.callId)}>
              <Play className="size-3" />
            </IconButton>
          ) : (
            <IconButton title={t('call.hold')} onClick={() => actions.hold.mutate(call.callId)}>
              <Pause className="size-3" />
            </IconButton>
          )}
          <TransferMenu
            value={transferTo}
            onChange={setTransferTo}
            onSubmit={() => {
              if (!transferTo.trim()) return
              actions.transfer.mutate({ callId: call.callId, destination: transferTo.trim() })
              setTransferTo('')
            }}
          />
        </>
      )}

      <Button
        size="sm"
        variant="destructive"
        className="h-6 px-2 text-xs"
        title={t('call.hangup')}
        onClick={() => actions.hangup.mutate(call.callId)}
      >
        <PhoneOff className="size-3" />
      </Button>
    </div>
  )
}

function IconButton({
  title,
  onClick,
  children,
}: {
  title: string
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <Button size="sm" variant="outline" className="h-6 px-2" title={title} onClick={onClick}>
      {children}
    </Button>
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
        <Button size="sm" variant="outline" className="h-6 px-2" title={t('call.transfer')}>
          <Shuffle className="size-3" />
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
