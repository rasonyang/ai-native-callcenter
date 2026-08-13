/**
 * Event stream types and the single EventSource that feeds the whole app.
 *
 * Scope is decided by the server from the session identity; the client only
 * ever narrows. Reconnection and Last-Event-ID resume are handled by the
 * browser's own EventSource implementation.
 */

export type CallType = 'INBOUND' | 'OUTBOUND' | 'CONSULT' | 'INTERNAL'

/** Party-scoped lifecycle: one event per call leg. */
export type PartyEventType =
  | 'PARTY_DIALING'
  | 'PARTY_RINGING'
  | 'PARTY_ESTABLISHED'
  | 'PARTY_HELD'
  | 'PARTY_RETRIEVED'
  | 'PARTY_RELEASED'
  | 'PARTY_CHANGED'
  | 'PARTY_DTMF'

/** Call-scoped facts about the aggregate. */
export type CallEventType =
  | 'CALL_USER_DATA'
  | 'CALL_RECORDING_STARTED'
  | 'CALL_RECORDING_STOPPED'
  | 'CALL_CDR'

export type EventType =
  | PartyEventType
  | CallEventType
  | 'QUEUE_JOINED'
  | 'QUEUE_LEFT'
  | 'QUEUE_COUNT'
  | 'QUEUE_AGENT_OFFERED'
  | 'AGENT_LOGGED_IN'
  | 'AGENT_LOGGED_OUT'
  | 'AGENT_READY'
  | 'AGENT_NOT_READY'
  | 'AGENT_AVAILABILITY'
  | 'DEVICE_REGISTERED'
  | 'DEVICE_UNREGISTERED'
  | 'DEVICE_IN_SERVICE'
  | 'BOT_SESSION_STARTED'
  | 'BOT_TRANSCRIPT'
  | 'BOT_INTERRUPTED'
  | 'BOT_SESSION_ENDED'
  | 'CALLBACK_CREATED'
  | 'CALLBACK_UPDATED'
  | 'SYSTEM_LINK'
  | 'SYSTEM_RESET'

export interface AiccEvent {
  version: number
  seq: number
  type: EventType
  occurredAt: string
  callId?: string
  callType?: CallType
  partyId?: string
  agentId?: string
  queueId?: string
  payload?: Record<string, unknown>
  userData?: Record<string, unknown>
}

export type EventHandler = (event: AiccEvent) => void

/**
 * Opens the stream and dispatches to handlers registered per type.
 *
 * A SYSTEM_RESET means the resume point aged out of the server ring: the
 * caller must refetch its snapshots before trusting incremental state again.
 */
export function connectEvents(handlers: {
  onEvent?: EventHandler
  onReset?: () => void
  onOpen?: () => void
  onError?: () => void
}): () => void {
  const source = new EventSource('/api/v1/events', { withCredentials: true })

  if (handlers.onOpen) source.addEventListener('open', handlers.onOpen)
  if (handlers.onError) source.addEventListener('error', handlers.onError)

  source.addEventListener('message', (message) => {
    // Named events arrive with their own listener; this is the fallback.
    dispatch(message)
  })

  const dispatch = (message: MessageEvent<string>) => {
    let event: AiccEvent
    try {
      event = JSON.parse(message.data) as AiccEvent
    } catch {
      return
    }
    if (event.type === 'SYSTEM_RESET') handlers.onReset?.()
    handlers.onEvent?.(event)
  }

  // EventSource delivers by event name, so every known type needs a listener.
  const types: EventType[] = [
    'PARTY_DIALING', 'PARTY_RINGING', 'PARTY_ESTABLISHED', 'PARTY_HELD',
    'PARTY_RETRIEVED', 'PARTY_RELEASED', 'PARTY_CHANGED', 'PARTY_DTMF',
    'CALL_USER_DATA', 'CALL_RECORDING_STARTED', 'CALL_RECORDING_STOPPED', 'CALL_CDR',
    'QUEUE_JOINED', 'QUEUE_LEFT', 'QUEUE_COUNT', 'QUEUE_AGENT_OFFERED',
    'AGENT_LOGGED_IN', 'AGENT_LOGGED_OUT', 'AGENT_READY', 'AGENT_NOT_READY',
    'AGENT_AVAILABILITY', 'DEVICE_REGISTERED', 'DEVICE_UNREGISTERED',
    'DEVICE_IN_SERVICE', 'BOT_SESSION_STARTED', 'BOT_TRANSCRIPT',
    'BOT_INTERRUPTED', 'BOT_SESSION_ENDED', 'CALLBACK_CREATED',
    'CALLBACK_UPDATED', 'SYSTEM_LINK', 'SYSTEM_RESET',
  ]
  for (const type of types) source.addEventListener(type, dispatch)

  return () => source.close()
}
