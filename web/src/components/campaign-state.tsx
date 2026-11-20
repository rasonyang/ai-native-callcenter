import { useTranslation } from 'react-i18next'

import {
  describeState, type Campaign, type CampaignContactCounts, type CampaignStatus,
} from '@/lib/campaigns'

/** One token per status; color appears only as a dot (web/CLAUDE.md). */
export const STATUS_COLOR: Record<CampaignStatus, string> = {
  DRAFT: 'var(--state-offline)',
  RUNNING: 'var(--state-available)',
  PAUSED: 'var(--state-ringing)',
  COMPLETED: 'var(--state-aux)',
  CANCELLED: 'var(--state-offline)',
}

/**
 * Status dot and the sentence the dialer's state reads as: "Running · waiting
 * for calling hours (tomorrow 09:00)". A pause by a breaker is red, because it
 * will not end until a person resumes it. Only the text after the status word
 * wraps; the word stays on one line, level with the text's first line (#648).
 */
export function CampaignState({
  campaign,
}: {
  campaign: Pick<Campaign, 'status' | 'pauseReason' | 'activity' | 'nextDialAt' | 'timeZone'>
}) {
  const { t, i18n } = useTranslation()
  const words = describeState(campaign, t, { locale: i18n.language })
  return (
    <span className="inline-flex items-baseline gap-1.5 text-sm">
      <span
        className="inline-block size-2 shrink-0 rounded-full"
        style={{ backgroundColor: words.isBreakerPause ? 'var(--state-breach)' : STATUS_COLOR[campaign.status] }}
      />
      <span className="shrink-0 whitespace-nowrap font-medium">{words.status}</span>
      {(words.activity ?? words.pause) && (
        <span className="text-xs text-muted-foreground">· {words.activity ?? words.pause}</span>
      )}
    </span>
  )
}

/** "37 active · 120 done · 3 exhausted …", zero counts left out. */
export function ContactCounts({ counts }: { counts: CampaignContactCounts }) {
  const { t } = useTranslation()
  const parts: Array<[string, number]> = [
    ['active', counts.active],
    ['done', counts.done],
    ['exhausted', counts.exhausted],
    ['invalidNumber', counts.invalidNumber],
    ['dnc', counts.dnc],
  ]
  return (
    <span className="tabular text-xs text-muted-foreground">
      <span className="text-foreground">{counts.total}</span>
      {parts
        .filter(([, n]) => n > 0)
        .map(([key, n]) => ` · ${n} ${t(`campaign.counts.${key}`)}`)
        .join('')}
    </span>
  )
}
