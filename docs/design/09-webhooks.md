# CDR Webhooks — Design

Date: 2026-08-26 · Status: **design only; no code written, no migration added, no
contract edited.** Requirement 3 of the customer API set (1 outbound and 2 hangup shipped
on `feat/one-post-calls`). · Author: agent session

> ## Scope, settled by the owner on 2026-08-26
>
> **CDRs only.** One complete CDR per call, delivered once. There is **no generic event
> subscription**: `PARTY_*`, `QUEUE_*`, `AGENT_*` and the rest stay on the SSE stream where
> they already are. **Nothing here subscribes to `events.Hub` and no `Hub` subscriber is
> added.**
>
> That decision is what this design is built on rather than a restriction bolted onto it,
> and it removes three problems the generic version had — see §1.
>
> Also settled: retry schedule as proposed (§6); a failed delivery raises a metric **and** a
> WARN (§6); failed deliveries are kept **30 days** and successful ones **7** (§6); the
> enqueue is **written to the database in the same transaction as the CDR** (§2); and when a
> fuller CDR replaces one already delivered, **the correction is sent** (§2.1).
>
> "One per call" therefore means *one per call as recorded*. A call whose stored row is later
> replaced produces a second delivery carrying the corrected row, and the payload says which
> revision it is so the customer can order them.

---

## 1. The source is the ledger write, not the event stream

An earlier draft routed deliveries off `events.Hub`. Dropping that is not a simplification
of the same design; it is a different and better one, for three reasons.

**The hub disconnects slow consumers.** `Publish` drops subscribers whose buffer is full
(`hub.go:88-104`) — right for a stalled browser tab, fatal for an HTTP delivery to somebody
else's server, which is slow by definition and would be dropped exactly when load is highest.
Not subscribing means never being that consumer.

**The event stream is not durable.** Events live only in the in-memory ring that serves
`Last-Event-ID`; there is no events table. Anything undelivered at a restart is gone. The
ledger, by contrast, *is* the durable record — so taking the CDR from where it is stored
rather than from a notification about it closes the window instead of documenting it.

**`CALL_CDR` is not a CDR.** `registry.go:416` publishes it with a payload of
`{answeredAt, endedAt}` — a notification that a call finished, correct for a browser that
will refetch, useless to a customer who wanted the record. Delivering the stored row sidesteps
this entirely; no event payload has to be widened and no browser on the floor receives a full
CDR it does not read.

The seam is exact:

```
call ends ──▶ CDRAssembler.CallFinished (cdr.go:95)
                  │  assembles the row
                  ▼
              LedgerStore.InsertCDR ──┐ one transaction
              enqueue webhook rows  ──┘
                                       │
                         delivery worker│drains
                                       ▼
                            customer's HTTPS endpoint
```

---

## 2. Enqueue is part of the ledger write

**Owner decision (Q5): the outbox row goes to the database, in the same transaction as the
CDR.** Either both land or neither does. A CDR that exists with no delivery queued is a call
the customer never hears about, with nothing anywhere recording the omission — the failure
mode this decision exists to prevent.

`InsertCDR` (`ledgerstore.go:118`) is a single statement today, so this needs a transaction
around the pair. That is a change to a path every finished call takes, and it deserves the
caution such paths get here: the enqueue must not be able to fail the CDR write for a reason
of its own. It is an insert into a table with no foreign key to anything volatile and no
computation, so the realistic failures are the ones that would fail the CDR write anyway.

### 2.1 When a fuller CDR replaces one already sent

`InsertCDR`'s contract is **"writes the row, or replaces one that saw less of the call"**
(`ledgerstore.go:109-117`): a bot leg killed by a restart can write the call off as ended, and
the four minutes the caller then spent with an agent arrive in a later, fuller row. Two writes,
one call.

**Owner decision (A), 2026-08-26: send the correction.** The customer's ledger being right
outranks the tidiness of exactly one HTTP post per call.

Three cases, and only the third is a second delivery:

| the stored row is replaced and the delivery is… | what happens |
|---|---|
| still `PENDING` | its payload is replaced in place. One delivery, and it carries the right story. |
| `FAILED` | a fresh revision is enqueued; the retry schedule starts over on the correct row. |
| already `DELIVERED` | **a new delivery row** is enqueued carrying the corrected CDR. |

**Enqueue only when the row actually changed.** `InsertCDR` replaces only when the new row ends
later, so the insert must report whether it replaced anything — `ON CONFLICT … WHERE
cdrs.ended_at < excluded.ended_at … RETURNING call_id` — and the enqueue runs only for a
returned row. Without that, every idempotent re-write would post a duplicate of a CDR the
customer already has and nothing would have changed.

**`revision` is what makes the correction usable.** A second delivery for one `callId` is
useless if the receiver cannot tell it from the first, or tell which is newer. Each delivery
carries `revision` (1, 2, …) and the customer's rule is **last revision wins per `callId`**.
Ordering is not assumed: deliveries are concurrent (§6), so revision 2 can arrive before
revision 1 and the number, not the arrival time, decides.

This is what makes the unique key a *revision* key rather than a call key — see §4.2.

## 3. One call, one delivery per matching subscription

A deployment holds **several subscriptions** and they are independent of each other: a
customer's CRM, their reporting system, a staging endpoint they are still testing against.

```
call ends ──▶ one CDR ──┬──▶ subscription A  →  https://crm.customer.com/aicc/cdr
                        ├──▶ subscription B  →  https://bi.customer.com/hooks/cdr
                        └──▶ subscription C  →  filtered out, nothing enqueued
```

One CDR therefore produces **N delivery rows, one per subscription whose filter matches**, and
each then lives its own life: A succeeds on the first attempt, B is four retries into a backoff
because its endpoint is down, C was never enqueued. Nothing about B's outage touches A, and a
correction (§2.1) fans out the same way.

This is why `subscription_id` is in the unique key alongside `call_id` and `revision`: the
constraint prevents enqueueing the same revision of the same call **for one subscriber** twice,
while the same CDR reaching three subscribers is three rows and always was.

It is also why the resource has full CRUD (§10) rather than being a pair of environment
variables: subscriptions are added, disabled and removed at different times by different
operators, and `is_enabled` exists so one failing endpoint can be silenced without disturbing
the others.

---

## 4. Data model

### 4.1 `webhook_subscriptions`

| column | type | note |
|---|---|---|
| `subscription_id` | `uuid` PK | |
| `name` | `text NOT NULL` | operator-facing label; a list of URLs is unreadable |
| `url` | `text NOT NULL` | HTTPS endpoint |
| `filter` | `jsonb NOT NULL DEFAULT '{}'` | see §5 |
| `auth_token` | `text NOT NULL DEFAULT ''` | the customer's own credential — see §8 |
| `is_enabled` | `boolean NOT NULL DEFAULT true` | |
| `created_at` / `updated_at` | `timestamptz NOT NULL` | |

No `event_types` column. There is one event and it is the CDR; a column offering a choice that
does not exist would be a promise the platform cannot keep.

`is_enabled` is a column rather than a delete because disabling is what an operator does when
a customer's endpoint is down: configuration and history stay, deliveries stop, and turning it
back on is one flag.

### 4.2 `webhook_deliveries` — the outbox

| column | type | note |
|---|---|---|
| `delivery_id` | `uuid` PK | travels to the customer as their deduplication key |
| `subscription_id` | `uuid NOT NULL REFERENCES webhook_subscriptions ON DELETE CASCADE` | |
| `call_id` | `uuid NOT NULL` | |
| `revision` | `int NOT NULL DEFAULT 1` | **`UNIQUE (subscription_id, call_id, revision)`** — §2.1 |
| `payload` | `jsonb NOT NULL` | the CDR as it will be sent, frozen at enqueue |
| `status` | `text NOT NULL` | `PENDING` \| `DELIVERED` \| `FAILED` |
| `attempt_count` | `int NOT NULL DEFAULT 0` | |
| `next_attempt_at` | `timestamptz NOT NULL` | the worker's claim key |
| `last_status_code` | `int` | nullable: no attempt has been made yet |
| `last_error` | `text NOT NULL DEFAULT ''` | |
| `created_at` `timestamptz NOT NULL` / `delivered_at` `timestamptz` | | |

Index on `(status, next_attempt_at)` for the worker's claim, and on `(subscription_id, call_id)`
for the enqueue's own lookup and for an operator asking what a subscriber was told about a call.

The unique key includes `revision`, so a correction is a **new row** rather than a mutated one.
Keeping the superseded row is the point: the `deliveries` endpoint (§10) has to be able to answer
"what did we send them, and when" for both, and an in-place update would erase the very history
somebody is asking about.

`call_id` is deliberately **not** a foreign key to `cdrs`: the delivery records what was sent and
must survive the CDR being swept. No `event_seq` — there is no event, and ordering is §6.

**The payload is frozen at enqueue, not rendered at delivery.** A retry two hours later must
send the call as it was recorded, not re-read a row a sweeper may have removed.

**Migration**: two new tables, no back-fill. `internal/store/migrate_test.go` still requires
the migration to run against a database that already holds rows, so the fixture is small but
must exist.

---

## 5. Filters

CDR-only scope makes filters both simpler and more useful than the generic version: the fields
are the CDR's own, which are far richer than the SSE envelope's.

```json
{ "callType": ["OUTBOUND"], "did": ["95001"], "status": ["ANSWERED"] }
```

A key present means the CDR's field must be in the list; keys are ANDed; an absent key does
not constrain. Filterable, first cut: `callType`, `did`, `queueId`, `status`, `isContained` —
all real columns on `store.CDR` (`ledgerstore.go:31-63`).

> Note against the previous draft, which listed `did` as filterable on the SSE envelope: it is
> not an envelope field and that was an error. Under CDR scope it *is* a real field, so the key
> returns for a different and now-correct reason.

**No expression language** — no JSONPath, no CEL, no `"talkSec > 30"`. The repository's stated
taste is that everything checkable is validated at load rather than mid-flight; an expression
language moves validation to delivery time, where a bad filter is discovered as a customer
receiving silence. The allowed keys are a closed list checked when the subscription is written;
an unknown key is 422 at creation, not a surprise at 3am.

A filter that matches nothing enqueues nothing. **No row is written for a non-match** — the
outbox is a work queue, not an audit of calls that did not qualify.

---

## 6. Delivery

**Ordering is not a constraint here**, and that is a direct dividend of the CDR-only scope.
Each CDR is a complete, independent record of a finished call; a customer who receives
yesterday's before this morning's can still file both. The generic design needed strict
per-subscription serialisation because `PARTY_ESTABLISHED` after `PARTY_RELEASED` is unusable
— none of that applies. The worker may deliver concurrently, bounded by a modest
per-subscription cap so one deployment cannot flood a customer.

Because delivery is concurrent, **the customer must order by `revision`, not by arrival**.
Revision 2 of a call can land before revision 1 of the same call; the rule they implement is
last-revision-wins per `callId`, and the contract must say so in as many words.

**Retry (Q2, accepted as proposed):** 6 attempts at 10s, 1m, 5m, 30m, 2h, 6h, then `FAILED`.
Per-request timeout 10s. A 2xx is success; everything else, timeouts included, retries. 4xx
other than 408/429 is arguably permanent, but telling "your payload is malformed" from "our WAF
hiccuped" by status code is guesswork, so all failures are treated alike and the attempt cap
ends it.

**Failure is announced (Q3):** a counter in `internal/obs/callmetrics.go` — the one place every
instrument name lives — **and** a WARN naming the subscription, the call and the last status.
Both, because a metric is what a dashboard watches and a log line is what an operator greps at
2am with a customer on the phone.

---

## 7. Retention

**Owner decision (B): `FAILED` rows are kept 30 days, `DELIVERED` rows 7.** Two windows rather
than one, because the two kinds of row are read for different reasons and at different ages: a
delivered row is checked within hours of a customer asking "did you send it", while a failed one
is what gets looked up weeks later when somebody reconciles a month of records and finds a gap.

```
AICC_WEBHOOK_RETENTION_DELIVERED_DAYS=7
AICC_WEBHOOK_RETENTION_FAILED_DAYS=30
```

`0` disables either sweep for a deployment that wants to keep everything. Both go in
`.env.example`, which is the registry.

A sweeper on the model of `recording.NewSweeper` (`main.go:367`), but **on by default**, unlike
that one — and the difference is worth stating because it inverts an existing precedent.
Recordings are irreplaceable customer audio, so deleting any unasked would be wrong; delivery
rows are operational exhaust that grows with every call forever, and a table nobody ever prunes
is a defect waiting on a busy month.

`PENDING` rows are never swept. A delivery still owed is work, not history; the retry schedule
(§6) is what ends it, by reaching `DELIVERED` or `FAILED`.

---

## 8. Authenticating to the customer: their key, not ours

**Owner decision, 2026-08-26: no HMAC.** The subscription carries a token the customer
supplies, and every delivery presents it. Their endpoint authenticates us with a header
comparison and needs no signature library.

```
POST <subscription url>
Authorization: Bearer <the token they gave us>
X-AICC-Webhook-Id: <deliveryId>
Content-Type: application/json
```

### The two keys point in opposite directions and must never hold the same value

| | direction | who holds it | who checks it |
|---|---|---|---|
| `AICC_API_KEY` (`X-AICC-Api-Key`) | **inbound** — their system calls us | the customer | us |
| `webhook_subscriptions.auth_token` | **outbound** — we call their system | us, per subscription | the customer |

Sending our inbound key outward would hand the credential that places and ends calls to a third
party and leave it in their access logs. The column is theirs, they choose the value, and
nothing in the platform ever populates it from `AICC_API_KEY`.

### What is given up, said plainly

A static bearer token proves **who is sending**, not **what was sent**. An HMAC over
`timestamp.body` proves the payload itself came from us unaltered and is worthless once the
request is over; a token rides in a header on every delivery, so anything capturing one request
— a proxy, a TLS terminator, the customer's own access log — can replay it or forge a body to
that endpoint. Over HTTPS to one endpoint the customer controls, that risk is much smaller than
it sounds, which is why most webhook platforms offer the pattern. `deliveryId` gives their side
a deduplication key that blunts naive replay.

**One mechanism, not two.** Supporting bearer *and* HMAC would give the platform two ways to
say one thing, which this repository treats as a defect in itself. If a customer later requires
payload signatures, an additional header is purely additive — no existing subscription changes,
no contract version breaks.

### Storage

Stored recoverably, because it must be presented verbatim on every delivery. Worth stating
because "secrets are hashed here" is otherwise a fair assumption: passwords use argon2id
(`internal/auth`) precisely because verification only needs a comparison, and this is the
opposite case.

Audit redaction is covered, and was checked rather than assumed: `isSecretField`
(`internal/httpapi/audit.go:156`) matches the substrings `password`, `secret`, `token`,
`apikey`, `credential`; `authToken` contains `token`.

**Write-only.** The customer supplied the token and already has it, so there is nothing to hand
back: `POST`/`PUT` accept it, `GET` never returns it, replacing it is another `PUT`. That
removes any need for a rotate operation.

---

## 9. Payload

The stored CDR, as `GET /cdrs/{callId}` already serves it, wrapped in a delivery header:

```json
{
  "deliveryId": "…uuid…",
  "subscriptionId": "…uuid…",
  "revision": 1,
  "attempt": 1,
  "cdr": { "callId": "…", "callType": "OUTBOUND", "status": "ANSWERED",
           "startedAt": "…", "talkSec": 40, "billSec": 40, "legs": [], "userData": {} }
}
```

`revision` is the field a receiver keys on: **last revision wins per `callId`** (§2.1), and it
is in the body rather than only in a header so that a payload logged on its own still says which
version of the call it is.

`cdr`, not `event` — the two are different things, and naming the field for what it carries
keeps a future generic-event webhook from having to reuse a field that means something else.
The shape is `store.CDR` (`ledgerstore.go:31`), already a wire type guarded by
`TestLedgerTypesMarshalPerTheNamingSpec`, so it needs no second definition and cannot drift
from what the API serves.

---

## 10. API surface

Spec-first: `docs/openapi.json` before any code.

| | |
|---|---|
| `GET /webhook-subscriptions` | list |
| `POST /webhook-subscriptions` | create; `authToken` write-only, never echoed |
| `GET /webhook-subscriptions/{subscriptionId}` | read; `authToken` omitted |
| `PUT /webhook-subscriptions/{subscriptionId}` | update |
| `DELETE /webhook-subscriptions/{subscriptionId}` | delete, cascading its deliveries |
| `GET /webhook-subscriptions/{subscriptionId}/deliveries` | recent attempts, for diagnosis |

**Role: ADMIN, and deliberately not reachable by `AICC_API_KEY`.** The key is the credential
for placing and ending calls — the customer's system doing its job. Configuring where the
platform sends data is administration, the same kind of decision as defining a queue or a DID,
which this repository already puts behind `requireRole(auth.RoleAdmin)` (`server.go:195`).
Letting the dialling credential also redirect outbound data would mean a leaked key can
exfiltrate every finished call to an endpoint of the attacker's choosing.

Naming per 07: `subscriptionId` / `subscription_id`, `isEnabled`, `authToken`, `createdAt`,
SCREAMING_SNAKE for `status`.

---

## 11. Frontend: a window, not a workbench

**Owner decision, 2026-08-26: the screen lives under the System group, and it is read-only.**

`/admin/webhooks`, ADMIN, alongside `/admin/cdr`, `/admin/reports` and `/admin/audit`
(`web/src/lib/nav.ts:80-92`). It belongs there rather than under Manage for the same reason the
audit trail does: Manage is where an operator changes how calls are handled — queues, numbers,
extensions, bots — and a webhook subscription changes nothing about a call. It is an account of
what the platform is doing and whether it is working.

What it shows:

- the subscriptions, with `isEnabled`, the URL, and the filter as configured;
- per subscription, its recent deliveries — status, attempt count, last status code, last error;
- `authToken` **never**, on any screen. It is write-only at the contract (§8) and the UI cannot
  display what it is not served.

What it does not do: create, edit, enable, disable or delete. Those go through the API by an
administrator's session. This mirrors flows, where `internal/httpapi` serves the resource and
the repository is content that `aicc flowadd` was the only way in for a while — a read-only
screen that tells the truth ships sooner than an editor, and nothing about the editor is harder
to add later.

> **The consequence, stated so nobody discovers it in a demo:** with no editor, the first
> subscription of a deployment is created by an API call an operator makes themselves. If that
> proves awkward, the cheap answer is a CLI subcommand on the model of `aicc flowadd` rather
> than rushing the editor.

Design-system rules apply as written in `web/CLAUDE.md` — 13px base, borders not shadows,
tabular-nums for the attempt counts and status codes, `nav.webhooks` in both `en` and `zh`, no
hardcoded user-facing strings.

---

## 12. What this design no longer needs

Recorded because the previous draft carried them and their absence is the point:

- **No `Hub` subscriber, no fan-out matcher, no `event_seq` ordering, no per-subscription
  serialisation.** All of it existed to make a firehose survivable.
- **No lossy `Publish → outbox` window.** The enqueue is in the CDR's transaction.
- **No enrichment of `CALL_CDR`**, and no widening of the SSE envelope.
- **No `correlationId` interaction.** That design (settled by the owner 2026-08-24,
  `docs/verification/TASKS.md`) ties a request to *the event it produced* and belongs to the
  generic-event surface. A CDR webhook is not a response to a request. Listed only so the next
  reader does not go looking for a link that was never made.

---

## 13. Sizing, honestly

Smaller than the generic design by a wide margin, and still not a patch: two tables with a
migration exercised against a populated database, a new sqlc file, a six-operation contract
resource, a transactional change to the path every finished call takes, a delivery worker with
retry semantics, a retention sweeper, and a read-only screen. A milestone's worth of work,
where the generic version was two.

---

## 14. Nothing is open

Every question this design raised has been answered by the owner on 2026-08-26:

| | |
|---|---|
| Scope | CDR only; no generic event subscription, no `events.Hub` subscriber |
| Enqueue durability | in the same transaction as the CDR write |
| A fuller CDR replacing a delivered one | **send the correction**, ordered by `revision` |
| Retry | 6 attempts, 10s → 6h, 10s per-request timeout |
| Failure signal | a metric **and** a WARN |
| Retention | `FAILED` 30 days, `DELIVERED` 7, `PENDING` never |
| Authentication outward | the customer's own bearer token, no HMAC |
| Configuration role | ADMIN; `AICC_API_KEY` must not reach it |
| Screen | `/admin/webhooks`, under the System group |
| Screen scope | read-only; creation and editing go through the API |

The design is ready to implement. First step is `docs/openapi.json`, not the migration.
