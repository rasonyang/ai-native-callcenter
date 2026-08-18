import { useQuery } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'

import type { components } from '@/generated/api'

import { request } from './api'
import type { AiccEvent } from './events'
import { useEventListener, type StreamStatus } from './use-event-stream'

/**
 * The live transcript: the REST snapshot, the streamed tail, and the rule that
 * joins them without losing or duplicating a line.
 *
 * All wire types come from the generated contract.
 */

export type Speaker = components['schemas']['Speaker']

export type TranscriptLine = components['schemas']['TranscriptLine']

export type TranscriptionState = components['schemas']['TranscriptionState']

export type CallTranscript = components['schemas']['CallTranscript']

/** A rendered line. A partial has no seq: it has no place in the order yet. */
export interface Line {
  utteranceId: string
  speaker: Speaker
  kind: TranscriptLine['kind']
  text: string
  isFinal: boolean
  seq?: number
  agentId?: string
  offsetMs?: number
}

export const TRANSCRIPT_KEY = (callId: string) => ['transcript', callId] as const

/** The newest rows kept in the DOM; older ones are fetched on demand (§12.10). */
const WINDOW = 400

export function fetchTranscript(callId: string, sinceSeq = 0, limit = 500) {
  const query = new URLSearchParams({ sinceSeq: String(sinceSeq), limit: String(limit) })
  return request<CallTranscript>(`/calls/${callId}/transcript?${query}`)
}

function lineFromRow(row: TranscriptLine): Line {
  return {
    utteranceId: row.utteranceId || `seq-${row.seq}`,
    speaker: row.speaker,
    kind: row.kind,
    text: String(row.content?.text ?? ''),
    isFinal: true,
    seq: row.seq,
    agentId: row.agentId,
    offsetMs: row.offsetMs,
  }
}

function lineFromEvent(event: AiccEvent): Line | null {
  const payload = event.payload as Record<string, unknown> | undefined
  if (!payload || typeof payload.utteranceId !== 'string') return null
  return {
    utteranceId: payload.utteranceId,
    speaker: payload.speaker as Speaker,
    kind: (payload.kind ?? 'TEXT') as TranscriptLine['kind'],
    text: String(payload.text ?? ''),
    isFinal: payload.isFinal === true,
    seq: typeof payload.seq === 'number' ? payload.seq : undefined,
    agentId: typeof payload.agentId === 'string' ? payload.agentId : undefined,
    offsetMs: typeof payload.offsetMs === 'number' ? payload.offsetMs : undefined,
  }
}

/**
 * Inserts a line into an ordered array, replacing any earlier version of the
 * same utterance.
 *
 * Identity is the utteranceId, never the text: repeated text is legitimate
 * ("yes", "yes"). Finals sort by seq; a partial has none and sorts after
 * everything numbered, which is where the speaker is currently talking.
 */
export function mergeLine(lines: Line[], incoming: Line): Line[] {
  const at = lines.findIndex((l) => l.utteranceId === incoming.utteranceId)
  const next = at === -1 ? [...lines, incoming] : lines.map((l, i) => (i === at ? incoming : l))

  next.sort((a, b) => {
    if (a.seq === undefined && b.seq === undefined) return 0
    if (a.seq === undefined) return 1
    if (b.seq === undefined) return -1
    return a.seq - b.seq
  })
  return next
}

/** The status the panel shows, from the call, the stream and the server. */
export function statusFor(
  hasCall: boolean,
  streamStatus: StreamStatus,
  reported: TranscriptionState,
  hadCall: boolean,
): TranscriptionState {
  if (!hasCall) return hadCall ? 'ENDED' : 'IDLE'
  // A stream we cannot hear is indistinguishable from transcription that has
  // stopped, and saying "transcribing" while offline would be a lie.
  if (streamStatus !== 'connected') return 'ERROR'
  // Whatever the server last said, and nothing invented in its place. This used
  // to fall back to CONNECTING, which made a call nobody is transcribing
  // indistinguishable from one whose stream is on its way — and the panel would
  // sit at "Connecting…" for the life of a call that was never going to
  // connect. The server says CONNECTING when a tap is attached; before that it
  // says IDLE, and IDLE is the truth.
  return reported
}

/**
 * Follows one call's transcript.
 *
 * The order is subscribe, then snapshot, then merge, then tail. It looks
 * backwards and it is the only order without a hole: subscribing first can
 * only duplicate lines, and duplicates are removable because seq is dense,
 * while snapshotting first loses whatever arrives between the read and the
 * subscribe, and loss is not removable.
 */
export function useCallTranscript(callId: string | undefined, streamStatus: StreamStatus) {
  const [lines, setLines] = useState<Line[]>([])
  // Seeded from the snapshot the server returns and then updated by the
  // stream, so the panel never has to invent a state of its own.
  const [reported, setReported] = useState<TranscriptionState>('IDLE')
  // Whether the stream has reported a state since this call was selected. The
  // snapshot is a *past* answer — it is requested when the call id appears and
  // arrives a round trip later — so seeding from it after the stream has
  // already spoken moves the panel backwards. Observed on a live call: the
  // server published CONNECTING at tap attach and LIVE 199ms later, the
  // snapshot was taken inside that window, and it overwrote LIVE on arrival.
  // The panel then sat on "Connecting…" for the rest of the call while the
  // transcript ran normally underneath it.
  const isStateFromStream = useRef(false)
  const hadCall = useRef(false)

  // Lines that arrive before the snapshot resolves wait here rather than
  // rendering, so the merge can drop the ones the snapshot also carries.
  const buffer = useRef<Line[]>([])
  const isSnapshotted = useRef(false)

  useEffect(() => {
    buffer.current = []
    isSnapshotted.current = false
    isStateFromStream.current = false
    setLines([])
    setReported('IDLE')
    if (callId) hadCall.current = true
  }, [callId])

  useEventListener('CALL_TRANSCRIPT', (event) => {
    if (!callId || event.callId !== callId) return
    const line = lineFromEvent(event)
    if (!line) return
    if (!isSnapshotted.current) {
      buffer.current.push(line)
      return
    }
    setLines((current) => mergeLine(current, line))
  })

  useEventListener('CALL_TRANSCRIPTION_STATE', (event) => {
    if (!callId || event.callId !== callId) return
    const state = (event.payload as Record<string, unknown> | undefined)?.state
    if (typeof state !== 'string') return
    isStateFromStream.current = true
    setReported(state as TranscriptionState)
  })

  const snapshot = useQuery({
    queryKey: TRANSCRIPT_KEY(callId ?? ''),
    enabled: Boolean(callId),
    queryFn: () => fetchTranscript(callId as string),
    // The panel owns its failures: a transcript that cannot load must not
    // retry its way into looking like a broken call.
    retry: 1,
  })

  useEffect(() => {
    if (!snapshot.data) return
    let merged = snapshot.data.items.map(lineFromRow)
    const highest = merged.reduce((max, l) => Math.max(max, l.seq ?? 0), 0)
    for (const line of buffer.current) {
      // Anything the snapshot already carries is dropped by seq; a partial has
      // no seq and is always newer than the snapshot, so it survives.
      if (line.seq !== undefined && line.seq <= highest) continue
      merged = mergeLine(merged, line)
    }
    buffer.current = []
    isSnapshotted.current = true
    setLines(merged)
    // Only if the stream has not already said something newer.
    if (!isStateFromStream.current) setReported(snapshot.data.state)
  }, [snapshot.data])

  const loadEarlier = useCallback(async () => {
    if (!callId) return
    const oldest = lines.find((l) => l.seq !== undefined)?.seq ?? 0
    if (oldest <= 1) return
    const page = await fetchTranscript(callId, Math.max(0, oldest - WINDOW - 1), WINDOW)
    setLines((current) =>
      page.items.map(lineFromRow).reduce((acc, line) => mergeLine(acc, line), current),
    )
  }, [callId, lines])

  const state = statusFor(Boolean(callId), streamStatus, reported, hadCall.current)
  const windowed = lines.length > WINDOW ? lines.slice(lines.length - WINDOW) : lines

  return {
    lines: windowed,
    hasEarlier: lines.length > WINDOW || (lines.find((l) => l.seq !== undefined)?.seq ?? 1) > 1,
    loadEarlier,
    state,
    isLoading: snapshot.isLoading,
    isUnavailable: snapshot.isError,
  }
}
