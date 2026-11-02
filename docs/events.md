# Event stream guide

`GET /api/v1/events` (operationId `streamEvents`) is a single ordered
server-sent-events (SSE) stream of everything that happens on the floor: call
legs, calls, queues, agents, phones, callbacks and campaigns. This guide is the
reference for connecting to it, reading it, resuming it and recovering from a
gap. The machine-readable shapes are in [`openapi.json`](openapi.json):
`SseEvent` (the envelope), `SseEventType` (the names) and the `Sse*Payload`
components.

Nothing here is a polling replacement for REST. The stream tells you that
something changed; the REST resources (`/calls/mine`, `/calls`, `/agent/presence`,
`/campaigns/{campaignId}`, ...) are what you read to learn the state from
scratch.

## Connecting

The stream is a long-lived `GET` that never times out on its own and ends when
the client goes away (or when the server drops a slow client, see
[Slow consumers](#slow-consumers)).

### With an API key

```sh
curl -N -H 'Authorization: Bearer <api-key>' \
  'https://aicc.example.com/api/v1/events'
```

`-N` turns off curl's output buffering so events appear as they arrive.

The operation requires the scope `calls:read:own`. A session cookie and a key
are equal credentials: both are accepted, and both are checked against the same
scopes. A missing or invalid credential answers `401`; a key without the scope
answers `403` (`INSUFFICIENT_SCOPE`).

What the key then receives depends on its scopes and on whom it acts for:

| Credential | Receives |
| --- | --- |
| Holds `calls:read:all` | Every event (everything below), exactly as a supervisor or administrator login does. |
| `calls:read:own` and `X-AICC-Agent-ID: <agentId>` | The stream of that agent, as if the agent had opened it themselves: their own leg events, the `CALL_*` events of calls they are on, the queues they staff, and the broadcast events. |
| `calls:read:own` alone, no agent header | Only the broadcast events: `SYSTEM_LINK`, `CALLBACK_CREATED` and `CALLBACK_UPDATED`. A key with no agent has no leg for an event to be about. |

To act for an agent, add the header:

```sh
curl -N \
  -H 'Authorization: Bearer <api-key>' \
  -H 'X-AICC-Agent-ID: 019a1f00-4c2d-7e55-a1b3-6d9f20c8e714' \
  'https://aicc.example.com/api/v1/events'
```

The header is refused on a session request (`AGENT_IMPERSONATION_NOT_ALLOWED`);
a signed-in account acts as itself.

### In a browser

`EventSource` sends the session cookie but cannot set request headers, so a
page uses the cookie session, never an API key:

```js
const es = new EventSource('/api/v1/events');

es.addEventListener('PARTY_ESTABLISHED', (e) => {
  const ev = JSON.parse(e.data); // an SseEvent envelope
  console.log(ev.seq, ev.callId, ev.payload);
});

es.addEventListener('SYSTEM_RESET', () => {
  // Refetch state over REST, see "SYSTEM_RESET" below.
});
```

The server sends no CORS headers, so the page must be served from the
deployment's own origin; a page elsewhere goes through its own backend with an
API key. Every event is
delivered under its own name (the `event:` line), so register one listener per
type; the default `onmessage` handler never fires.

The browser reconnects on its own after a drop and echoes the last `id:` it saw
in the `Last-Event-ID` request header, which is exactly what the resume
mechanism below expects.

A client that cannot set the header (a non-browser SSE library, for instance)
passes the same value as the `lastEventId` query parameter.

## Wire framing

The response is `Content-Type: text/event-stream`, with `Cache-Control:
no-cache, no-transform` and `X-Accel-Buffering: no` so that a reverse proxy
does not buffer it. Each event is written as three lines and a blank line, in
this order:

```
id: 48213907
event: PARTY_ESTABLISHED
data: {"version":1,"seq":48213907,"type":"PARTY_ESTABLISHED", ... }

```

- `id:` is the envelope's `seq`, a global, strictly increasing number. It is
  what the client echoes back to resume. It is omitted on `SYSTEM_RESET`
  (whose `seq` is `0`), so a reset never moves the browser's remembered id.
- `event:` is the envelope's `type`.
- `data:` is one `SseEvent` JSON envelope on a single line.
- Gaps in `seq` are normal and carry no meaning; never treat a missing number
  as a lost event. A given identity sees only the numbers it is allowed to see.
- A comment line `: hb` followed by a blank line is sent every 15 seconds so
  that proxies keep the connection open and a client can notice a dead one.
  SSE parsers ignore it. No `retry:` field is sent, so a browser uses its own
  reconnect delay.

### The envelope

| Field | Type | Always present | Meaning |
| --- | --- | --- | --- |
| `version` | integer | yes | Envelope version, currently `1`. Changes only on a breaking change; new event types and new fields are additive, so ignore what you do not know. |
| `seq` | integer | yes | Global order. Echo the last seen value to resume. |
| `type` | `SseEventType` | yes | The event name. |
| `occurredAt` | date-time | yes | When it happened, UTC. |
| `callId` | UUID | call events | The call. A UUIDv7. |
| `callType` | `INBOUND` / `OUTBOUND` / `CONSULT` / `INTERNAL` | call events | Stamped when the call is created; never changes, even across transfers. |
| `partyId` | UUID | `PARTY_*` | The leg. Two `PARTY_ESTABLISHED` on one call are two parties; compare `partyId`. |
| `agentId` | UUID | when the event is about an agent | The agent. |
| `queueId` | UUID | queue events, queued calls | The queue. |
| `payload` | object | usually | Type-specific body, see the table below. |
| `userData` | object | call events | The call's business data as it now stands, repeated on call events so a screen pop needs no further request. |

Absent optional fields are omitted, not sent as `null`.

## Who receives what

Scope comes from the authenticated identity and nothing in the request can
widen it; `?types=` can only narrow it.

- **A leg event goes only to the agent whose leg it is.** An agent's stream
  carries their own party's `PARTY_*` events and nobody else's: not a
  colleague's, not the customer's. A party with no agent (a customer leg, for
  instance) reaches no agent's stream; it is in the CDR and in the
  supervisor view. A customer hanging up ends the agent's leg too, so the agent
  is told by their own `PARTY_RELEASED`.
- **`CALL_*` events reach every agent on the call.** They are facts about the
  call, not about a leg. When the call is in a queue (`queueId` is set), the
  agents staffing that queue receive the call-scoped events as well, except
  `CALL_TRANSCRIPT` and `CALL_TRANSCRIPTION_STATE`, which go only to agents on
  the call.
- **Queue events** (`QUEUE_JOINED`, `QUEUE_LEFT`, `QUEUE_COUNT`) reach agents
  staffing that queue. `QUEUE_AGENT_OFFERED` and the `AGENT_*` and `DEVICE_*`
  events reach only the agent they are about.
- **Broadcast events** reach everyone who may open the stream: `SYSTEM_LINK`,
  `CALLBACK_CREATED`, `CALLBACK_UPDATED`.
- **Supervision only:** `BOT_SESSION_STARTED` and `CAMPAIGN_UPDATED` are
  delivered only to identities holding `calls:read:all`.
- **`calls:read:all`** (granted to supervisor and administrator logins, and
  available on a key) receives every event of every call, queue, agent and
  campaign.

Resuming applies the same rules to the replayed events as to live ones:
reconnecting never reveals anything the identity could not have seen live.

## Filtering with `?types=`

`?types=PARTY_ESTABLISHED,PARTY_RELEASED,CALL_CDR` delivers only the listed
types.

- Comma-separated, matched exactly (case-sensitive) against `SseEventType`.
  Whitespace around a name and empty items are ignored.
- An absent or empty value means no filter.
- An unknown name is not an error; it simply matches nothing. A list made only
  of unknown names yields a stream that carries nothing but heartbeats.
- The filter is applied on top of the identity's scope (logical AND). Naming a
  type you may not see does not deliver it.
- `SYSTEM_RESET` is a control frame and is sent regardless of the filter.

## Resuming

Every reconnect should carry the last `seq` the client processed, in the
`Last-Event-ID` header (a browser does this itself) or the `lastEventId` query
parameter. The header wins if both are present.

- The server keeps the most recent events in memory (the ring holds 65,536 of
  them) and, on connect, replays every event after the given id that the
  identity may see, then continues with the live tail. There is no gap and no
  duplicate between the replay and the live events.
- With no resume point, the stream starts fresh at "now": nothing is replayed.
- A resume point that cannot be read (not an integer, empty, zero or negative)
  is not an error. It degrades to a fresh stream, so a mangled header can never
  turn into an endless reconnect loop. Such a client has no replay and no
  `SYSTEM_RESET`, so if it knows it missed events it must resync itself.
- A resume point at or beyond the newest event replays nothing and tails.

### `SYSTEM_RESET`

When the resume point is older than the oldest event still in memory, the
server cannot replay the gap. It sends

```
event: SYSTEM_RESET
data: {"version":1,"seq":0,"type":"SYSTEM_RESET","occurredAt":"2026-11-02T08:14:03.512Z","payload":{"oldestSeq":48170112}}

```

(no `id:` line; `seq` is `0`) and then tails live events. The same happens
after a server restart, because the replay memory is empty, in which case
`oldestSeq` is `0`. `SseSystemResetPayload` is `{ "oldestSeq": <int64> }`.

On `SYSTEM_RESET` a client must treat everything it holds as possibly stale and
refetch over REST before relying on further events. Typical refetches:

| Screen / state | Endpoint |
| --- | --- |
| The agent's own calls | `GET /calls/mine` |
| Callers waiting in queues | `GET /calls/waiting` |
| Every live call (`calls:read:all`) | `GET /calls` |
| The agent's presence and phone | `GET /agent/presence` |
| After-call record | `GET /agent/wrap-up` |
| Agent roster (`agent:manage`) | `GET /agents` |
| Callbacks | `GET /callbacks` |
| A campaign | `GET /campaigns/{campaignId}` (`campaigns:read`) |

Then keep the stream open; events after the reset are live.

### Slow consumers

Each connection has a small buffer (currently 64 events). The publisher never
waits for a client: if a client falls so far behind that its buffer is full, the
server closes that response. There is no final event; the connection simply
ends. A browser reconnects by itself with its last `id:`; any other client
should reconnect immediately with `Last-Event-ID` set to the last `seq` it
processed. If the gap has meanwhile left the replay memory, the reconnect is
answered with `SYSTEM_RESET`, as above. Read the stream promptly and do slow
work off the reading loop.

## Event types

Every value of `SseEventType`. "Unstable" means the contract declares no
`Sse*Payload` for it; the payload keys shown are what the server currently
sends, may gain keys, and should be read defensively.

| Type | Payload schema | Meaning |
| --- | --- | --- |
| `PARTY_DIALING` | unstable | A leg is being dialled out (`role`, `state`). |
| `PARTY_RINGING` | unstable | A leg is ringing (`role`, `state`). |
| `PARTY_ESTABLISHED` | unstable | A leg is answered and talking (`role`, `state` `TALKING`). |
| `PARTY_HELD` | unstable | A leg was put on hold. |
| `PARTY_RETRIEVED` | unstable | A leg came off hold. |
| `PARTY_RELEASED` | unstable | A leg ended; `cause` is the hangup cause. |
| `PARTY_CHANGED` | unstable | A leg's attributes changed (`isMuted`, or `reason` `CALL_MERGED` when two calls became one). |
| `PARTY_DTMF` | unstable | A digit was pressed on a leg (`digit`, `durationMs`). |
| `CALL_USER_DATA` | `SseCallUserDataPayload` | The call's business data changed; names the keys that moved. The data itself is on the envelope's `userData`. |
| `CALL_RECORDING_STARTED` | unstable | The call is now being recorded (no payload). List the recordings with `GET /calls/{callId}/recordings`. |
| `CALL_RECORDING_STOPPED` | unstable | Recording stopped (no payload). |
| `CALL_CDR` | unstable | The call is over and its CDR is being written (`answeredAt`, `endedAt`); read it with `GET /cdrs/{callId}`. |
| `CALL_TRANSCRIPT` | `SseTranscriptPayload` | One line of the live transcript; partials have `isFinal` false and no `seq`. |
| `CALL_TRANSCRIPTION_STATE` | `SseTranscriptionStatePayload` | Whether live transcription is complete or degraded, and which speaker is missing. |
| `QUEUE_JOINED` | `SseQueueJoinedPayload` | A caller entered the queue named by `queueId`. |
| `QUEUE_LEFT` | `SseQueueLeftPayload` | A caller left the queue: answered, abandoned or timed out. |
| `QUEUE_COUNT` | `SseQueueCountPayload` | How many callers wait in the queue now; sent whenever the number moves. |
| `QUEUE_AGENT_OFFERED` | unstable | A queued call was offered to this agent, before their phone rings (`queue`, `fromNumber`). |
| `AGENT_LOGGED_IN` | `SseAgentPresencePayload` | The agent signed in; payload is their presence. |
| `AGENT_LOGGED_OUT` | `SseAgentPresencePayload` | The agent signed out. |
| `AGENT_READY` | `SseAgentPresencePayload` | The agent became ready for calls. |
| `AGENT_NOT_READY` | `SseAgentPresencePayload` | The agent became not ready; `reason` says why. |
| `AGENT_AVAILABILITY` | `SseAgentPresencePayload` | The agent's availability changed without a sign-in or ready change. |
| `AGENT_WRAP_UP_DRAFTED` | `SseWrapUpDraftPayload` | The AI drafted the agent's after-call record; sent once, only to that agent. |
| `DEVICE_REGISTERED` | `SseAgentPresencePayload` | The agent's phone registered with the switch. |
| `DEVICE_UNREGISTERED` | `SseAgentPresencePayload` | The agent's phone no longer holds a registration. |
| `DEVICE_REACHABLE` | `SseAgentPresencePayload` | The registered phone answers the switch's keepalive. |
| `DEVICE_UNREACHABLE` | `SseAgentPresencePayload` | The phone is registered but no longer answers; calls to it will ring out. |
| `BOT_SESSION_STARTED` | `SseBotSessionStartedPayload` | The AI conversation is actually running (the bot leg answering is `PARTY_ESTABLISHED`; the model session opens afterwards and can fail). Supervision only. |
| `CALLBACK_CREATED` | `SseCallbackEventPayload` | A callback was created. Broadcast. |
| `CALLBACK_UPDATED` | `SseCallbackEventPayload` | A callback moved (claimed, completed, attempted, ...). Broadcast. |
| `CAMPAIGN_UPDATED` | `SseCampaignUpdatedPayload` | An outbound campaign's progress; at most once a second per campaign. Supervision only. |
| `SYSTEM_LINK` | unstable | Whether the switch connection is up (`isUp`). Broadcast. |
| `SYSTEM_RESET` | `SseSystemResetPayload` | The resume point is gone; refetch state, see above. |

A state event means a state changed. The server does not publish an event to
make a client refresh; a duplicate or illegal transition is dropped rather than
announced. If a screen of yours is stale, that is a missing event worth
reporting.

## Example: a queued call, answered and finished

The raw text below is what an agent's stream (or any stream holding
`calls:read:all`, which would additionally carry the customer's own leg events)
carries while an inbound call waits in a queue, is offered to the agent,
answered, recorded and ended. The agent is `019a1f00-4c2d-7e55-a1b3-6d9f20c8e714`, the
queue `019a1f00-8a31-7b62-9c07-d41e5a63f2b8`. Each `data:` value is one line on
the wire; it is wrapped here only for reading.

```
id: 48213901
event: QUEUE_JOINED
data: {"version":1,"seq":48213901,"type":"QUEUE_JOINED","occurredAt":"2026-11-02T08:15:41.204Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","payload":{"queueName":"Support","fromNumber":"4155550142","joinedAt":"2026-11-02T08:15:41.190Z"}}

id: 48213902
event: QUEUE_COUNT
data: {"version":1,"seq":48213902,"type":"QUEUE_COUNT","occurredAt":"2026-11-02T08:15:41.205Z","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","payload":{"queueName":"Support","waiting":1,"longestWaitAt":"2026-11-02T08:15:41.190Z"}}

id: 48213903
event: QUEUE_AGENT_OFFERED
data: {"version":1,"seq":48213903,"type":"QUEUE_AGENT_OFFERED","occurredAt":"2026-11-02T08:15:46.870Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","agentId":"019a1f00-4c2d-7e55-a1b3-6d9f20c8e714","payload":{"queue":"Support","fromNumber":"4155550142"}}

id: 48213904
event: PARTY_RINGING
data: {"version":1,"seq":48213904,"type":"PARTY_RINGING","occurredAt":"2026-11-02T08:15:47.102Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","partyId":"019a3c2e-9d02-7f13-b6a4-0e7c58d2f391","agentId":"019a1f00-4c2d-7e55-a1b3-6d9f20c8e714","payload":{"role":"TARGET","state":"RINGING"}}

id: 48213905
event: PARTY_ESTABLISHED
data: {"version":1,"seq":48213905,"type":"PARTY_ESTABLISHED","occurredAt":"2026-11-02T08:15:51.644Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","partyId":"019a3c2e-9d02-7f13-b6a4-0e7c58d2f391","agentId":"019a1f00-4c2d-7e55-a1b3-6d9f20c8e714","payload":{"role":"TARGET","state":"TALKING"}}

id: 48213906
event: QUEUE_LEFT
data: {"version":1,"seq":48213906,"type":"QUEUE_LEFT","occurredAt":"2026-11-02T08:15:51.650Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","payload":{"queueName":"Support","fromNumber":"4155550142","waitSec":10}}

id: 48213907
event: CALL_RECORDING_STARTED
data: {"version":1,"seq":48213907,"type":"CALL_RECORDING_STARTED","occurredAt":"2026-11-02T08:15:51.702Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8"}

id: 48213911
event: CALL_USER_DATA
data: {"version":1,"seq":48213911,"type":"CALL_USER_DATA","occurredAt":"2026-11-02T08:16:12.331Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","payload":{"changedKeys":["orderId"],"deletedKeys":[]},"userData":{"orderId":"A-10442"}}

id: 48213915
event: PARTY_RELEASED
data: {"version":1,"seq":48213915,"type":"PARTY_RELEASED","occurredAt":"2026-11-02T08:19:30.018Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","partyId":"019a3c2e-9d02-7f13-b6a4-0e7c58d2f391","agentId":"019a1f00-4c2d-7e55-a1b3-6d9f20c8e714","payload":{"role":"TARGET","state":"RELEASED","cause":"NORMAL_CLEARING"},"userData":{"orderId":"A-10442"}}

id: 48213916
event: CALL_RECORDING_STOPPED
data: {"version":1,"seq":48213916,"type":"CALL_RECORDING_STOPPED","occurredAt":"2026-11-02T08:19:30.020Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","userData":{"orderId":"A-10442"}}

id: 48213917
event: CALL_CDR
data: {"version":1,"seq":48213917,"type":"CALL_CDR","occurredAt":"2026-11-02T08:19:30.031Z","callId":"019a3c2e-7b10-7c4e-9a52-3f1d8e6b2a07","callType":"INBOUND","queueId":"019a1f00-8a31-7b62-9c07-d41e5a63f2b8","payload":{"answeredAt":"2026-11-02T08:15:51.644Z","endedAt":"2026-11-02T08:19:30.018Z"},"userData":{"orderId":"A-10442"}}

: hb

```

The ids jump (`48213907` to `48213911`, ...) because other events on the floor
were not addressed to this identity; that is expected. After `CALL_CDR`, read the
CDR with `GET /cdrs/{callId}`.

The one-off `: hb` line is the 15-second heartbeat.
