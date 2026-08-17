import { Bot } from 'lucide-react'
import {
  Component,
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'

import { useRoster } from '@/lib/agent'
import { cn } from '@/lib/utils'
import { useCallTranscript, type Line, type TranscriptionState } from '@/lib/transcript'
import type { StreamStatus } from '@/lib/use-event-stream'

/**
 * The live transcript of the call in front of the agent.
 *
 * It renders both phases of one conversation — what the bot and the customer
 * said before the transfer, and what the agent and the customer say after —
 * because to the person reading it that is a single conversation.
 */

/** The dot's colour per state. Colour appears only as a dot, per the design system. */
const STATE_COLOR: Record<TranscriptionState, string> = {
  IDLE: 'var(--state-offline)',
  CONNECTING: 'var(--state-ringing)',
  LIVE: 'var(--state-available)',
  DEGRADED: 'var(--state-ringing)',
  ERROR: 'var(--state-breach)',
  STOPPED: 'var(--state-offline)',
  ENDED: 'var(--state-offline)',
}

/** Distance from the bottom, in px, still counted as "at the bottom". */
const STICK_THRESHOLD = 24

export function LiveTranscript({
  callId,
  myAgentId,
  streamStatus,
  className,
}: {
  callId?: string
  myAgentId?: string
  streamStatus: StreamStatus
  className?: string
}) {
  const { t } = useTranslation()
  const { lines, hasEarlier, loadEarlier, state, isLoading, isUnavailable } = useCallTranscript(
    callId,
    streamStatus,
  )
  const scroller = useRef<HTMLDivElement>(null)
  const [isFollowing, setFollowing] = useState(true)
  const wasSticky = useRef(true)

  // Measured before the mutation paints, so a line growing from partial to
  // final does not shift a reader who was at the bottom.
  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    wasSticky.current = el.scrollHeight - el.scrollTop - el.clientHeight < STICK_THRESHOLD
  })

  useEffect(() => {
    const el = scroller.current
    if (!el || !isFollowing) return
    // Never yank the page out from under a selection someone is copying.
    const selection = document.getSelection()
    if (selection && !selection.isCollapsed && el.contains(selection.anchorNode)) return
    if (wasSticky.current) el.scrollTop = el.scrollHeight
  }, [lines, isFollowing])

  const onScroll = useCallback(() => {
    const el = scroller.current
    if (!el) return
    setFollowing(el.scrollHeight - el.scrollTop - el.clientHeight < STICK_THRESHOLD)
  }, [])

  const jumpToLatest = useCallback(() => {
    const el = scroller.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    setFollowing(true)
  }, [])

  let body: ReactNode
  if (isUnavailable) {
    body = <p className="text-xs text-muted-foreground">{t('transcript.unavailable')}</p>
  } else if (isLoading && lines.length === 0) {
    body = <p className="text-xs text-muted-foreground">{t('transcript.loading')}</p>
  } else if (lines.length === 0) {
    body = <p className="text-xs text-muted-foreground">{t('transcript.empty')}</p>
  } else {
    body = (
      <ol className="flex flex-col gap-1.5">
        {lines.map((line) => (
          <TranscriptRow key={line.utteranceId} line={line} myAgentId={myAgentId} />
        ))}
      </ol>
    )
  }

  return (
    <section className={cn('flex flex-col rounded-md border bg-card p-4', className)}>
      <div className="mb-3 flex shrink-0 items-center justify-between gap-2">
        <h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t('transcript.title')}
        </h2>
        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
          {!isFollowing && lines.length > 0 && (
            <button
              type="button"
              onClick={jumpToLatest}
              className="text-primary hover:underline"
            >
              {t('transcript.jumpToLatest')}
            </button>
          )}
          <span
            aria-hidden="true"
            className="size-1.5 rounded-full"
            style={{ background: STATE_COLOR[state] }}
          />
          {t(`transcript.status.${state}`)}
        </span>
      </div>

      <div
        ref={scroller}
        onScroll={onScroll}
        role="log"
        className="min-h-0 flex-1 overflow-y-auto"
      >
        {hasEarlier && lines.length > 0 && (
          <button
            type="button"
            onClick={() => void loadEarlier()}
            className="mb-2 text-xs text-muted-foreground hover:text-foreground"
          >
            {t('transcript.loadEarlier')}
          </button>
        )}
        {body}
      </div>
    </section>
  )
}

/**
 * One line. Memoized on what can actually change: a partial's text grows and
 * then it becomes a final, and nothing else about a rendered line moves.
 */
const TranscriptRow = memo(function TranscriptRow({
  line,
  myAgentId,
}: {
  line: Line
  myAgentId?: string
}) {
  const { t } = useTranslation()
  const { data: roster } = useRoster(line.speaker === 'HUMAN_AGENT')

  let label: string
  if (line.speaker === 'HUMAN_AGENT') {
    if (line.agentId && line.agentId === myAgentId) {
      label = t('transcript.speakers.YOU')
    } else {
      const entry = roster?.items?.find((a) => a.agentId === line.agentId)
      label = entry?.displayName ?? t('transcript.speakers.HUMAN_AGENT')
    }
  } else {
    label = t(`transcript.speakers.${line.speaker}`)
  }

  const isMine = line.speaker === 'HUMAN_AGENT' && line.agentId === myAgentId

  return (
    // A partial is hidden from assistive technology: announcing every revision
    // is the same word three times a second, which makes the log unusable.
    <li className="flex gap-3 text-[13px]" aria-hidden={line.isFinal ? undefined : 'true'}>
      <span
        className={cn(
          'flex w-16 shrink-0 items-center gap-1 pt-px text-xs font-medium',
          isMine ? 'text-foreground' : 'text-muted-foreground',
        )}
      >
        {/* The bot is the only labelled speaker with an icon. Scarcity is what
            makes the one icon mean anything. */}
        {line.speaker === 'BOT' && <Bot className="size-3 shrink-0" aria-hidden="true" />}
        <span className="truncate">{label}</span>
      </span>
      <span className={cn('min-w-0 flex-1', !line.isFinal && 'text-muted-foreground')}>
        {line.kind === 'TEXT' ? line.text : <ToolLine line={line} />}
      </span>
    </li>
  )
})

function ToolLine({ line }: { line: Line }) {
  const { t } = useTranslation()
  const key = line.kind === 'TOOL_CALL' ? 'cdr.toolCalled' : 'cdr.toolAnswered'
  return <span className="text-muted-foreground">{t(key, { tool: line.text })}</span>
}

/**
 * Keeps a transcript failure inside the transcript.
 *
 * The cockpit's other cards are call control. A render fault here must cost
 * the agent their transcript and nothing else — not the mute button.
 */
export class LiveTranscriptBoundary extends Component<
  { callId?: string; myAgentId?: string; streamStatus: StreamStatus; className?: string },
  { hasFailed: boolean }
> {
  state = { hasFailed: false }

  static getDerivedStateFromError() {
    return { hasFailed: true }
  }

  componentDidCatch(error: unknown) {
    console.error('the transcript panel failed', error)
  }

  render() {
    if (this.state.hasFailed) return <TranscriptUnavailable className={this.props.className} />
    return <LiveTranscript {...this.props} />
  }
}

function TranscriptUnavailable({ className }: { className?: string }) {
  return (
    <section className={cn('flex flex-col rounded-md border bg-card p-4', className)}>
      <h2 className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        <TranscriptTitle />
      </h2>
      <UnavailableText />
    </section>
  )
}

function TranscriptTitle() {
  const { t } = useTranslation()
  return <>{t('transcript.title')}</>
}

function UnavailableText() {
  const { t } = useTranslation()
  return <p className="text-xs text-muted-foreground">{t('transcript.unavailable')}</p>
}
