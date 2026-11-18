import { Search, X } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'
import { fieldErrorText } from '@/lib/errors'

/**
 * A search for one phone number, matched exactly on the whole number, as the
 * campaign listings take it (`phoneNumber`). The number is typed as it is
 * dialled at home and sent as typed: the screen never asks for, shows or adds
 * a country code, and the server completes a national number with the
 * campaign's.
 *
 * Half a number is not a search, so nothing is asked while typing: Enter
 * searches, and emptying the box or its clear button drops the search at once.
 * The searched value is the caller's (it lives in the address), so Back and a
 * reload bring it back; the box starts from it. A value the server cannot read
 * as a number is said under the box, from the listing's own error, while the
 * box still holds that value.
 */
export function NumberSearch({ value, onSearch, error }: {
  /** The number searched now; undefined when the list is not narrowed. */
  value: string | undefined
  onSearch: (next: string | undefined) => void
  /** The listing's error, if any: a refused phoneNumber is shown here. */
  error?: unknown
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState(value ?? '')
  const [searched, setSearched] = useState(value)
  // The search moved without the box (Back, a reload, a tab that dropped it).
  if (value !== searched) {
    setSearched(value)
    setDraft(value ?? '')
  }
  const refusal = draft.trim() === (value ?? '') ? fieldErrorText(error, 'phoneNumber', t) : undefined

  return (
    <div className="w-64 shrink-0 space-y-1">
      <form
        role="search"
        className="relative"
        onSubmit={(event) => {
          event.preventDefault()
          const next = draft.trim()
          onSearch(next === '' ? undefined : next)
        }}
      >
        <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input
          aria-label={t('campaign.search.label')}
          aria-invalid={Boolean(refusal)}
          title={t('campaign.search.hint')}
          className="tabular pl-7 pr-8"
          inputMode="tel"
          enterKeyHint="search"
          autoComplete="off"
          maxLength={64}
          placeholder={t('campaign.search.placeholder')}
          value={draft}
          onChange={(event) => {
            setDraft(event.target.value)
            if (event.target.value.trim() === '' && value !== undefined) onSearch(undefined)
          }}
        />
        {draft !== '' && (
          <button
            type="button"
            aria-label={t('campaign.search.clear')}
            title={t('campaign.search.clear')}
            className="absolute right-1 top-1/2 inline-flex size-6 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
            onClick={() => {
              setDraft('')
              if (value !== undefined) onSearch(undefined)
            }}
          >
            <X className="size-3.5" />
          </button>
        )}
      </form>
      {refusal && (
        <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
          {refusal}
        </p>
      )}
    </div>
  )
}
