import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { LogIn, LogOut, PhoneOff, Play } from 'lucide-react'
import { DropdownMenu } from 'radix-ui'

import { StatePill } from '@/components/state-dot'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { SELECTABLE_REASONS, useElapsedSec, usePresence, usePresenceActions } from '@/lib/agent'
import type { NotReadyReason } from '@/lib/api'
import { describeError } from '@/lib/errors'
import { formatDuration } from '@/lib/utils'

/**
 * Presence control for the signed-in agent.
 *
 * The bar never touches audio: the Chrome softphone owns the media and the
 * registration, FreeSWITCH owns the call, and this only expresses intent.
 */
export function SoftphoneBar({ defaultExtension }: { defaultExtension?: string }) {
  const { t } = useTranslation()
  const { data: presence, isPending } = usePresence(true)
  const { signIn, signOut, ready, notReady } = usePresenceActions()
  const [extension, setExtension] = useState(defaultExtension ?? '')

  const elapsedSec = useElapsedSec(presence?.enteredAt)
  const busy = signIn.isPending || signOut.isPending || ready.isPending || notReady.isPending
  const failure = signIn.error ?? signOut.error ?? ready.error ?? notReady.error

  if (isPending || presence === null) return null

  const isSignedIn = presence !== undefined && presence.state !== 'LOGGED_OUT'

  return (
    <div className="flex items-center gap-3 rounded-md border bg-card px-3 py-2">
      {isSignedIn ? (
        <>
          <StatePill availability={presence.availability} reason={presence.reason} />
          <span className="tabular text-xs text-muted-foreground">{formatDuration(elapsedSec)}</span>
          <span className="text-xs text-muted-foreground">
            {t('agent.extension')} {presence.extensionNumber}
          </span>

          <div className="ml-auto flex items-center gap-1">
            {presence.state === 'READY' ? (
              <NotReadyMenu
                disabled={busy}
                onSelect={(reason) => notReady.mutate(reason)}
              />
            ) : (
              <Button size="sm" disabled={busy} onClick={() => ready.mutate()}>
                <Play />
                {t('agent.goReady')}
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              onClick={() => signOut.mutate()}
              title={t('agent.signOut')}
            >
              <LogOut />
            </Button>
          </div>
        </>
      ) : (
        <form
          className="flex w-full items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            if (extension.trim()) signIn.mutate(extension.trim())
          }}
        >
          <PhoneOff className="size-4 text-muted-foreground" />
          <span className="text-xs text-muted-foreground">{t('agent.signInPrompt')}</span>
          <Input
            className="ml-auto w-28"
            value={extension}
            onChange={(event) => setExtension(event.target.value)}
            placeholder={t('agent.extension')}
            aria-label={t('agent.extension')}
          />
          <Button size="sm" type="submit" disabled={busy || !extension.trim()}>
            <LogIn />
            {t('agent.signIn')}
          </Button>
        </form>
      )}

      {failure && (
        <p role="alert" className="text-xs text-destructive">
          {describeError(failure, t)}
        </p>
      )}
    </div>
  )
}

function NotReadyMenu({
  disabled,
  onSelect,
}: {
  disabled: boolean
  onSelect: (reason: NotReadyReason) => void
}) {
  const { t } = useTranslation()
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <Button size="sm" variant="outline" disabled={disabled}>
          {t('agent.goNotReady')}
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={4}
          className="z-50 min-w-32 rounded-md border bg-popover p-1 text-sm shadow-none"
        >
          {SELECTABLE_REASONS.map((reason) => (
            <DropdownMenu.Item
              key={reason}
              className="cursor-default rounded-sm px-2 py-1 outline-none data-[highlighted]:bg-muted"
              onSelect={() => onSelect(reason)}
            >
              {t(`reasons.${reason}`)}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}
