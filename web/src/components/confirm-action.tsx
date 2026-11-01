import { Popover } from 'radix-ui'
import { useTranslation } from 'react-i18next'
import type { ReactNode } from 'react'

import { Button } from '@/components/ui/button'

/**
 * The confirm step for a destructive action that has a label, not just an icon
 * (cancelling a campaign, deleting a draft). Same popover and buttons as
 * ConfirmDelete: the action only happens on the second, destructive button.
 */
export function ConfirmAction({
  trigger,
  message,
  confirmLabel,
  onConfirm,
  disabled,
}: {
  trigger: ReactNode
  message: string
  confirmLabel: string
  onConfirm: () => void
  disabled?: boolean
}) {
  const { t } = useTranslation()
  return (
    <Popover.Root>
      <Popover.Trigger asChild>
        <Button size="lg" variant="outline" disabled={disabled}>
          {trigger}
        </Button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={4}
          className="z-50 w-72 rounded-md border bg-popover p-3 text-sm shadow-md"
        >
          <p className="mb-3 text-xs text-muted-foreground wrap-anywhere">{message}</p>
          <div className="flex justify-end gap-2">
            <Popover.Close asChild>
              <Button size="sm" variant="ghost">
                {t('common.cancel')}
              </Button>
            </Popover.Close>
            <Popover.Close asChild>
              <Button size="sm" variant="destructive" onClick={onConfirm}>
                {confirmLabel}
              </Button>
            </Popover.Close>
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
