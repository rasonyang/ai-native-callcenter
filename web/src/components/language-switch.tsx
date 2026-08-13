import { useTranslation } from 'react-i18next'
import { Languages } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { SUPPORTED_LANGUAGES, type Language } from '@/lib/i18n'

const LABELS: Record<Language, string> = { en: 'English', zh: '中文' }

/** Cycles the interface language; the choice persists in localStorage. */
export function LanguageSwitch({ className }: { className?: string }) {
  const { i18n, t } = useTranslation()
  const current = (SUPPORTED_LANGUAGES.find((l) => i18n.language.startsWith(l)) ??
    'en') as Language

  const next = SUPPORTED_LANGUAGES[
    (SUPPORTED_LANGUAGES.indexOf(current) + 1) % SUPPORTED_LANGUAGES.length
  ] as Language

  return (
    <Button
      variant="ghost"
      size="sm"
      className={className}
      title={t('common.language')}
      aria-label={t('common.language')}
      onClick={() => void i18n.changeLanguage(next)}
    >
      <Languages />
      {LABELS[current]}
    </Button>
  )
}
