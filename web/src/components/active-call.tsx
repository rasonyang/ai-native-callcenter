import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Pause, PhoneIncoming, PhoneOff, Play, Shuffle } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { myParty, otherParty, useCallActions, useElapsedSec } from '@/lib/agent'
import type { CallSnapshot } from '@/lib/api'
import { formatDuration } from '@/lib/utils'

/**
 * The call the agent is on, with the controls that apply to its current state.
 *
 * Answering does not happen here in any audio sense: the request tells
 * FreeSWITCH to instruct the agent's phone, and the phone does the rest.
 */
export function ActiveCall({ call, agentId }: { call: CallSnapshot; agentId?: string }) {
  const { t } = useTranslation()
  const actions = useCallActions()
  const [transferTo, setTransferTo] = useState('')
  const [showTransfer, setShowTransfer] = useState(false)

  const mine = myParty(call, agentId)
  const other = otherParty(call, agentId)
  const since = mine?.answeredAt ?? mine?.createdAt ?? call.createdAt
  const elapsedSec = useElapsedSec(since)

  // Only a call delivered to this agent can be answered; DIALING is their own
  // leg on a call they placed, and offering to answer that is nonsense.
  const isRinging = mine?.state === 'RINGING'
  const isDialing = mine?.state === 'DIALING'
  const isHeld = mine?.state === 'HELD'
  const busy =
    actions.answer.isPending || actions.hold.isPending ||
    actions.retrieve.isPending || actions.hangup.isPending || actions.transfer.isPending

  return (
    <div className="space-y-3">
      <div className="flex items-baseline justify-between gap-2">
        <div>
          <div className="text-base font-medium tabular">{other?.number ?? t('call.unknownNumber')}</div>
          <div className="text-xs text-muted-foreground">
            {t(`callTypes.${call.callType}`)} · {t(`partyStates.${mine?.state ?? 'RINGING'}`)}
          </div>
        </div>
        <span className="tabular text-sm text-muted-foreground">{formatDuration(elapsedSec)}</span>
      </div>

      <div className="flex flex-wrap gap-2">
        {isRinging && (
          <Button size="sm" disabled={busy} onClick={() => actions.answer.mutate(call.callId)}>
            <PhoneIncoming />
            {t('call.answer')}
          </Button>
        )}
        {!isRinging && !isDialing && !isHeld && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => actions.hold.mutate(call.callId)}>
            <Pause />
            {t('call.hold')}
          </Button>
        )}
        {isHeld && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => actions.retrieve.mutate(call.callId)}>
            <Play />
            {t('call.retrieve')}
          </Button>
        )}
        {!isRinging && !isDialing && (
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => setShowTransfer((open) => !open)}
          >
            <Shuffle />
            {t('call.transfer')}
          </Button>
        )}
        <Button
          size="sm"
          variant="destructive"
          disabled={busy}
          onClick={() => actions.hangup.mutate(call.callId)}
        >
          <PhoneOff />
          {t('call.hangup')}
        </Button>
      </div>

      {showTransfer && (
        <form
          className="flex gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            if (!transferTo.trim()) return
            actions.transfer.mutate({ callId: call.callId, destination: transferTo.trim() })
            setTransferTo('')
            setShowTransfer(false)
          }}
        >
          <Input
            autoFocus
            className="w-32"
            value={transferTo}
            onChange={(event) => setTransferTo(event.target.value)}
            placeholder={t('call.transferTo')}
            aria-label={t('call.transferTo')}
          />
          <Button size="sm" type="submit" disabled={!transferTo.trim()}>
            {t('call.transfer')}
          </Button>
        </form>
      )}
    </div>
  )
}
