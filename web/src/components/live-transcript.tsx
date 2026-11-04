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
  const content = useRef<HTMLDivElement>(null)
  const [isFollowing, setFollowingState] = useState(true)
  // The synchronous truth behind isFollowing: the pin runs inside layout
  // effects and observer callbacks, which must not wait for a render.
  const following = useRef(true)
  const setFollowing = useCallback((value: boolean) => {
    following.current = value
    setFollowingState(value)
  }, [])

  // Following is a mode the reader leaves and re-enters; it is never inferred
  // from how tall the content is. Several things scroll this element that are
  // not the reader: our own pin, the browser's scroll anchoring when the hook's
  // window drops a line at the top, and the clamp when the panel grows or the
  // lines are replaced. They all fire `scroll`, and some land away from the
  // bottom, so a position alone cannot tell them apart. Leaving the bottom
  // therefore needs a signal that only the reader produces (wheel, touch, a key,
  // a pointer held on the scrollbar), seen shortly before the scroll event. A
  // scroll that ends at the bottom always resumes following.
  const intentUntil = useRef(0)
  const isPointerDown = useRef(false)
  const noteIntent = useCallback(() => {
    intentUntil.current = performance.now() + 600
  }, [])
  const hasIntent = () => isPointerDown.current || performance.now() < intentUntil.current

  const pin = useCallback(() => {
    const el = scroller.current
    if (!el || !following.current) return
    // Never yank the page out from under a selection someone is copying.
    const selection = document.getSelection()
    if (selection && !selection.isCollapsed && el.contains(selection.anchorNode)) return
    el.scrollTop = el.scrollHeight
  }, [])

  // The row at the top of the view before "Load earlier" prepends, and where it
  // sat. Chromium's scroll anchoring does not apply at scrollTop 0, which is
  // where that button is clicked, so the reader's line is restored explicitly.
  // It is spent only by a prepend (an earlier first seq): the 400-line window
  // trimming the top also changes the first line, and must not restore a stale
  // place. A load that prepends nothing leaves it unspent and harmless.
  const anchor = useRef<{ row: Element; offset: number; firstSeq: number } | null>(null)

  // A new call starts in following mode, whatever the previous reader did. Only
  // a defined callId counts: the cockpit passes undefined when a call ends and
  // the panel keeps showing that transcript until the next call replaces it.
  const lastCallId = useRef(callId)
  useLayoutEffect(() => {
    if (callId && lastCallId.current && callId !== lastCallId.current) setFollowing(true)
    if (callId) lastCallId.current = callId
  }, [callId, setFollowing])

  // After every change to the lines, before paint, so the first paint of a
  // fresh snapshot already shows the last line.
  useLayoutEffect(() => {
    const el = scroller.current
    const held = anchor.current
    const firstSeq = firstSeqOf(lines)
    if (el && held && firstSeq !== undefined && firstSeq < held.firstSeq) {
      anchor.current = null
      if (held.row.isConnected) {
        const delta =
          held.row.getBoundingClientRect().top - el.getBoundingClientRect().top - held.offset
        el.scrollTop += delta
        if (el.scrollHeight - el.scrollTop - el.clientHeight >= STICK_THRESHOLD) setFollowing(false)
        return
      }
    }
    pin()
  }, [lines, pin, setFollowing])

  // A size change that is not a new line: a partial growing to several rows, a
  // window resize that rewraps the text, the cockpit's flex height changing.
  useEffect(() => {
    const el = scroller.current
    const inner = content.current
    if (!el || !inner || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(pin)
    observer.observe(el)
    observer.observe(inner)
    return () => observer.disconnect()
  }, [pin])

  // Keys are heard on the window: the log is not focusable, so after a click in
  // it the browser keyboard-scrolls it while the key event goes to the body.
  useEffect(() => {
    const release = () => {
      isPointerDown.current = false
    }
    window.addEventListener('pointerup', release)
    window.addEventListener('pointercancel', release)
    window.addEventListener('keydown', noteIntent, true)
    return () => {
      window.removeEventListener('pointerup', release)
      window.removeEventListener('pointercancel', release)
      window.removeEventListener('keydown', noteIntent, true)
    }
  }, [noteIntent])

  const onScroll = useCallback(() => {
    const el = scroller.current
    if (!el) return
    if (el.scrollHeight - el.scrollTop - el.clientHeight < STICK_THRESHOLD) {
      setFollowing(true)
    } else if (hasIntent()) {
      setFollowing(false)
    }
  }, [setFollowing])

  const onWheel = useCallback(
    (event: React.WheelEvent) => {
      noteIntent()
      const el = scroller.current
      // An upward wheel leaves the bottom at once: waiting for the scroll event
      // would let a line that arrives in between pull the reader back.
      if (el && event.deltaY < 0 && el.scrollTop > 0 && el.scrollHeight > el.clientHeight) {
        setFollowing(false)
      }
    },
    [noteIntent, setFollowing],
  )

  const jumpToLatest = useCallback(() => {
    const el = scroller.current
    if (!el) return
    setFollowing(true)
    el.scrollTop = el.scrollHeight
  }, [setFollowing])

  const loadEarlierKeepingPlace = useCallback(async () => {
    const el = scroller.current
    const firstSeq = firstSeqOf(lines)
    if (el && firstSeq !== undefined) {
      const top = el.getBoundingClientRect().top
      const row = [...el.querySelectorAll('li')].find((li) => li.getBoundingClientRect().bottom > top)
      if (row) {
        anchor.current = { row, offset: row.getBoundingClientRect().top - top, firstSeq }
      }
    }
    try {
      await loadEarlier()
    } catch {
      anchor.current = null
    }
  }, [loadEarlier, lines])

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
        onWheel={onWheel}
        onTouchMove={noteIntent}
        onPointerDown={() => {
          isPointerDown.current = true
        }}
        role="log"
        className="min-h-0 flex-1 overflow-y-auto"
      >
        <div ref={content}>
          {hasEarlier && lines.length > 0 && (
            <button
              type="button"
              onClick={() => void loadEarlierKeepingPlace()}
              className="mb-2 text-xs text-muted-foreground hover:text-foreground"
            >
              {t('transcript.loadEarlier')}
            </button>
          )}
          {body}
        </div>
      </div>
    </section>
  )
}

/** The seq of the oldest final line; a partial has none. */
function firstSeqOf(lines: Line[]): number | undefined {
  return lines.find((l) => l.seq !== undefined)?.seq
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
