import { Dialog } from 'radix-ui'
import { useTranslation } from 'react-i18next'
import type { FormEvent, ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

/**
 * The short create-or-edit form every configuration page uses. A dialog, not a
 * drawer: these forms are a handful of fields, and the list behind them is
 * what the operator is comparing against.
 */
export function RecordDialog({
  open,
  onOpenChange,
  title,
  error,
  isSaving,
  onSubmit,
  children,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  error?: ReactNode
  isSaving?: boolean
  onSubmit: () => void
  children: ReactNode
}) {
  const { t } = useTranslation()

  const submit = (event: FormEvent) => {
    event.preventDefault()
    onSubmit()
  }

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[440px] -translate-x-1/2 -translate-y-1/2 rounded-md border bg-card p-4 shadow-md">
          <Dialog.Title className="text-base font-medium">{title}</Dialog.Title>
          <form onSubmit={submit} className="mt-4 space-y-3">
            {children}
            {error && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                {error}
              </p>
            )}
            <div className="flex justify-end gap-2 pt-2">
              <Dialog.Close asChild>
                <Button type="button" variant="ghost" size="sm">
                  {t('common.cancel')}
                </Button>
              </Dialog.Close>
              <Button type="submit" size="sm" disabled={isSaving}>
                {t('common.save')}
              </Button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/** A labelled field, so every form lines up the same way. */
export function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <div className="space-y-1">
      <Label>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

/** A plain select styled like the inputs beside it. */
export function Select({
  value,
  onChange,
  options,
  ariaLabel,
}: {
  value: string
  onChange: (next: string) => void
  options: Array<{ value: string; label: string }>
  ariaLabel?: string
}) {
  return (
    <select
      aria-label={ariaLabel}
      className="h-8 w-full rounded-md border bg-card px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
      value={value}
      onChange={(event) => onChange(event.target.value)}
    >
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  )
}

export { Input }
