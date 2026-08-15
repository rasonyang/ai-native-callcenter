import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export const KEYPAD_DIGITS = ['1', '2', '3', '4', '5', '6', '7', '8', '9', '*', '0', '#'] as const

export type KeypadDigit = (typeof KEYPAD_DIGITS)[number]

/**
 * The twelve-key pad, shared by the topbar dialler and the in-call panel.
 *
 * It only reports which key was pressed; what a digit means — a character
 * appended to a number about to be dialled, or a tone sent into a live call —
 * belongs to the caller.
 */
export function Keypad({
  onDigit,
  disabled,
  className,
}: {
  onDigit: (digit: KeypadDigit) => void
  disabled?: boolean
  className?: string
}) {
  return (
    <div className={cn('grid grid-cols-3 gap-1', className)}>
      {KEYPAD_DIGITS.map((digit) => (
        <Button
          key={digit}
          type="button"
          variant="outline"
          disabled={disabled}
          className="tabular h-8"
          onClick={() => onDigit(digit)}
        >
          {digit}
        </Button>
      ))}
    </div>
  )
}
