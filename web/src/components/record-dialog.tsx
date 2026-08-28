import { Dialog } from 'radix-ui'
import { useTranslation } from 'react-i18next'
import { cloneElement, isValidElement, useId, useState } from 'react'
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
  submitLabel,
  onSubmit,
  children,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  error?: ReactNode
  isSaving?: boolean
  /** Names the act where "Save" would understate it, as Publish does. */
  submitLabel?: string
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
                {submitLabel ?? t('common.save')}
              </Button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/**
 * The record a form is editing, with the last attempt's refusal forgotten
 * whenever the form opens or closes.
 *
 * A mutation remembers its failure until the next one is fired, and these
 * dialogs read that state directly. So a refusal survived the form it belonged
 * to: cancel a number the server would not take, press Add again, and the
 * empty form opened already carrying "some fields did not pass validation" —
 * a sentence true of nothing on screen, and pointing at fields the reader had
 * not filled in yet. Opening is a new attempt and has nothing to report yet.
 *
 * A drop-in for useState: only a transition into or out of "no record" clears
 * anything, so editing a field mid-form leaves the error from that form's own
 * failed save where it is.
 */
export function useRecordForm<T>(...attempts: Array<{ reset: () => void }>) {
  const [editing, set] = useState<T | null>(null)
  const setEditing = (next: T | null) => {
    if ((editing === null) !== (next === null)) {
      for (const attempt of attempts) attempt.reset()
    }
    set(next)
  }
  return [editing, setEditing] as const
}

/**
 * A labelled field, so every form lines up the same way.
 *
 * The label is bound to the control it names: it was previously rendered
 * beside one, which reads the same on screen and leaves a screen reader — and
 * a click on the label — with nothing to act on. The control keeps its own id
 * when it has one.
 */
export function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  const generatedID = useId()
  const child = isValidElement<{ id?: string }>(children) ? children : undefined
  const controlID = child?.props.id ?? generatedID

  return (
    <div className="space-y-1">
      <Label htmlFor={child ? controlID : undefined}>{label}</Label>
      {child ? cloneElement(child, { id: controlID }) : children}
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
  disabled,
  id,
}: {
  value: string
  onChange: (next: string) => void
  options: Array<{ value: string; label: string }>
  ariaLabel?: string
  disabled?: boolean
  id?: string
}) {
  return (
    <select
      id={id}
      aria-label={ariaLabel}
      disabled={disabled}
      className="h-8 w-full rounded-md border bg-card px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
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
