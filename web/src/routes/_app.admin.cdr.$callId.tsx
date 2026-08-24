import { createFileRoute } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { PageHeader } from '@/components/page-header'
import { RecordingPlayer } from '@/components/recording-player'
import { describeError } from '@/lib/errors'
import { requireRole } from '@/lib/guards'
import {
  formatDuration, recordingAudioUrl, useCDR,
  type CDR, type TranscriptLine,
} from '@/lib/ledger'

/** One finished call: its facts, its recording, and the words spoken. */
export const Route = createFileRoute('/_app/admin/cdr/$callId')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: CallDetail,
})

function CallDetail() {
  const { callId } = Route.useParams()
  const { t, i18n } = useTranslation()
  const { data, isPending, isError, error } = useCDR(callId)

  if (isPending) {
    return <p className="text-xs text-muted-foreground">{t('common.loading')}</p>
  }
  if (isError || !data) {
    return <p className="text-xs text-muted-foreground">{describeError(error, t)}</p>
  }

  const { cdr, transcript, recordings } = data
  const startFormat = new Intl.DateTimeFormat(i18n.language, {
    dateStyle: 'medium', timeStyle: 'medium',
  })

  return (
    <>
      <PageHeader
        title={`${cdr.fromNumber || t('cdr.unknownNumber')} → ${cdr.did || cdr.toNumber || '—'}`}
        description={`${startFormat.format(new Date(cdr.startedAt))} · ${cdr.callId}`}
      />

      <div className="mb-4 grid grid-cols-[1fr_360px] items-start gap-4">
        <div className="rounded-md border bg-card p-4">
          <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {t('cdr.journey')}
          </h2>
          <Journey cdr={cdr} />
          <dl className="mt-4 grid grid-cols-3 gap-x-4 gap-y-3">
            <Fact label={t('cdr.status')}>
              {cdr.missedReason
                ? t(`cdr.missedReasons.${cdr.missedReason}`)
                : t(`cdr.statuses.${cdr.status}`)}
            </Fact>
            <Fact label={t('cdr.duration')}>
              <span className="tabular">{formatDuration(cdr.totalSec)}</span>
            </Fact>
            <Fact label={t('cdr.hangupCause')}>{cdr.hangupCause || '—'}</Fact>
            <Fact label={t('cdr.botTime')}>
              <span className="tabular">{formatDuration(cdr.botSec)}</span>
            </Fact>
            <Fact label={t('cdr.waitTime')}>
              <span className="tabular">{formatDuration(cdr.queueWaitSec)}</span>
            </Fact>
            <Fact label={t('cdr.talkTime')}>
              <span className="tabular">{formatDuration(cdr.talkSec)}</span>
            </Fact>
            <Fact label={t('cdr.billTime')}>
              <span className="tabular">{formatDuration(cdr.billSec)}</span>
            </Fact>
          </dl>
          {cdr.userData && Object.keys(cdr.userData).length > 0 && (
            <>
              <h2 className="mb-2 mt-4 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                {t('cdr.userData')}
              </h2>
              <dl className="grid grid-cols-2 gap-x-4 gap-y-2">
                {Object.entries(cdr.userData).map(([key, value]) => (
                  <Fact key={key} label={key}>
                    {typeof value === 'string' ? value : JSON.stringify(value)}
                  </Fact>
                ))}
              </dl>
            </>
          )}
        </div>

        <div className="rounded-md border bg-card p-4">
          <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {t('cdr.recording')}
          </h2>
          {recordings.length === 0 ? (
            <p className="text-xs text-muted-foreground">{t('cdr.noRecording')}</p>
          ) : (
            recordings.map((recording) => (
              <div key={recording.id} className="mb-2 last:mb-0">
                <RecordingPlayer src={recordingAudioUrl(recording.id)} durationSec={recording.durationSec} />
                <p className="mt-1 text-xs text-muted-foreground tabular">
                  {formatDuration(recording.durationSec)} ·{' '}
                  {(recording.sizeBytes / 1024 / 1024).toFixed(1)} MB · {recording.backend}
                </p>
              </div>
            ))
          )}
        </div>
      </div>

      <div className="rounded-md border bg-card p-4">
        <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('cdr.transcript')}
        </h2>
        {transcript.length === 0 ? (
          <p className="text-xs text-muted-foreground">{t('cdr.noTranscript')}</p>
        ) : (
          <ol className="space-y-2">
            {transcript.map((entry) => (
              <TranscriptRow key={entry.seq} entry={entry} startedAt={cdr.startedAt} />
            ))}
          </ol>
        )}
      </div>
    </>
  )
}

function Journey({ cdr }: { cdr: CDR }) {
  const { t } = useTranslation()
  if (cdr.legs.length === 0) {
    return <p className="text-xs text-muted-foreground">—</p>
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-xs">
      {cdr.legs.map((leg, index) => (
        <span key={index} className="flex items-center gap-1.5">
          {index > 0 && <span className="text-muted-foreground/50">→</span>}
          <span className="rounded-full border px-2 py-0.5">
            {t(`cdr.legs.${leg.kind}`)}
            {leg.label && <span className="ml-1 text-muted-foreground">{leg.label}</span>}
            <span className="ml-1 tabular text-muted-foreground">
              {formatDuration(leg.durationSec)}
            </span>
          </span>
        </span>
      ))}
    </div>
  )
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-[13px]">{children}</dd>
    </div>
  )
}

/** Seconds into the call, so the transcript reads against the recording. */
function offsetLabel(occurredAt: string, startedAt: string): string {
  const offset = (new Date(occurredAt).getTime() - new Date(startedAt).getTime()) / 1000
  return formatDuration(offset)
}

function TranscriptRow({ entry, startedAt }: { entry: TranscriptLine; startedAt: string }) {
  const { t } = useTranslation()
  const isCustomer = entry.speaker === 'CUSTOMER'

  let body: React.ReactNode
  if (entry.kind === 'TEXT') {
    body = <span>{String(entry.content.text ?? '')}</span>
  } else if (entry.kind === 'TOOL_CALL') {
    body = (
      <span className="text-muted-foreground">
        {t('cdr.toolCalled', { tool: String(entry.content.name ?? '') })}
      </span>
    )
  } else if (entry.kind === 'TOOL_RESULT') {
    body = (
      <span className="text-muted-foreground">
        {t('cdr.toolAnswered', { tool: String(entry.content.name ?? '') })}
      </span>
    )
  } else {
    body = <span className="text-muted-foreground">{JSON.stringify(entry.content)}</span>
  }

  return (
    <li className="flex gap-3 text-[13px]">
      <span className="w-12 shrink-0 pt-px text-right text-xs tabular text-muted-foreground">
        {offsetLabel(entry.occurredAt, startedAt)}
      </span>
      <span
        className={`w-14 shrink-0 pt-px text-xs font-medium ${
          isCustomer ? 'text-foreground' : 'text-primary'
        }`}
      >
        {t(`cdr.speakers.${entry.speaker}`)}
      </span>
      <span className="min-w-0 flex-1">{body}</span>
    </li>
  )
}
