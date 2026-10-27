# AI Outbound Campaigns — Design

Date: 2026-10-11, committed 2026-10-12 · Status: **accepted design; phase 1 not built** — no code,
no contract change yet; the facts in §0 were re-verified against `main` at `83e26a3` · Issue:
[#89](https://github.com/rasonyang/ai-native-callcenter/issues/89) · Written under
[#154](https://github.com/rasonyang/ai-native-callcenter/issues/154) · Prerequisite:
[#62](https://github.com/rasonyang/ai-native-callcenter/issues/62) (done) · Author: agent session

> ## Scope, settled by the owner on 2026-10-11
>
> **AI outbound only.** A campaign dials a list, and every answered call talks to a flow. What a
> campaign produces is a **structured sales lead** per contact, which people follow up later
> through the callbacks that already exist. There are no agent campaigns.
>
> **No transfer to a human inside a campaign call.** AI-to-human hand-off during a sales call has
> not worked well in practice; follow-up is asynchronous.
>
> **Customer first, bot on answer.** The customer is dialled; only when they answer is the bot leg
> created and a provider session opened. Most customers never answer, and an unanswered call
> must cost no bot resources.
>
> **No predictive or progressive dialing for agents; over-dial against bot capacity.** There is
> no agent idle time to optimise, but bot concurrency (a cloud provider's quota or a self-hosted
> gateway's limit) is a hard ceiling and most customers do not answer. The dialer admits calls by
> expected answers (§6), not by reserving a bot for every ringing leg. Its difficulty is
> correctness — attempts neither lost nor doubled, outcomes classified right, calling hours
> honoured, overflow held very low — not throughput modelling.

## Phase 1 at a glance

- **In:** AI outbound campaigns only. A campaign dials a contact list, each answered call talks to
  the DID's flow, and each contact yields a structured lead. The customer is dialled first and the
  bot leg is created only on answer. Admission is by expected answers (over-dialing against bot
  capacity, §6). Contacts, attempts, a global do-not-call list, retries and breakers, calling hours
  per contact time zone, leads and the SPA screens for them.
- **Out (§1):** agent campaigns, any hand-off to a human inside a campaign call, predictive pacing,
  answering-machine detection (deferred, §13.2), caller-id rotation, AI disclosure.
- **Settled here (they were #89's open questions):**
  - a do-not-call hit on a single `createCall` returns **422 `NUMBER_ON_DNC`** and writes **no
    CDR** (§12);
  - the calling window's time zone is **per contact, falling back to the campaign's** (§3.1, §3.2,
    §8.1).
- **Topology decision (§2):** AI outbound stays on ESL originate (`DialAI`), not on the dialplan.
- **Delivery:** §17 splits the work into prerequisites and slices, each with its issue.

---

## 0. Facts this design rests on

Verified against `main` at `83e26a3` (2026-10-12). The first draft was verified at `a1e2c5e`;
the rows that changed since are listed under the table.

| # | Fact | Where |
|---|------|-------|
| F1 | An AI outbound call originates the customer leg into `&park()`; nothing runs on it until it answers. | `internal/telephony/adapter.go:323-331` |
| F2 | On `CHANNEL_ANSWER` the armed continuation bridges the customer leg to `sofia/gateway/<botGateway>/<DID>`. | `internal/outbound/outbound.go:515-533` (the continuation inside `DialAI`), `:577-585` (`arm`), `:589-611` (`HandleSwitchEvent`) |
| F3 | The provider session is created when the UAS receives that bridge INVITE — after the customer answered. | `internal/aicall/orchestrator.go:336` (`runCall`, which starts at `:253`) |
| F4 | `DialAI` is idempotent on a caller-supplied `CallID`: an existing CDR or a live call returns `ErrAlreadyPlaced` without dialling. | `internal/outbound/outbound.go:449-458` |
| F5 | `AnswerWindow` defaults to 60 s; an answer after it is logged and **not bridged**, and nothing hangs that leg up. | `outbound.go:89-91`, `outbound.go:598-604` |
| F6 | The token bucket paces originates per second; it does not bound how many calls are in flight. | `internal/outbound/limiter.go` |
| F7 | `rescueCaller` closes the session without a transfer when the DID has no fallback queue. | `internal/aicall/actions.go:418` |
| F8 | The CDR stores the switch's `hangup_cause`, not only `releaseReason`. | `internal/store/sql/ledger.sql:31` |
| F9 | `callbacks` (OPEN / DONE / DISMISSED) with claim / complete / release is in the contract. | `internal/store/migrations/00005_call_ledger.sql:97`, `docs/openapi.json` `/callbacks*` (`:4227`) |
| F10 | Dial strings must be 3–20 ASCII digits — no `+`. | `outbound.go:194-204` (`isDialable`) |
| F11 | `originate` is sent with `bgapi`; the job result (`BACKGROUND_JOB`) is consumed nowhere, and `BACKGROUND_JOB` is not even in `telephony.Subscriptions`, so the application never receives it. An originate that fails before a channel exists is visible only by absence. | `adapter.go:331`; `internal/telephony/switchevent.go:295-310` (`Subscriptions`); no `BACKGROUND_JOB` reference under `internal/` |
| F12 | The CDR is written in one place, `LedgerStore.InsertCDR`, and the webhook outbox is enqueued in the same transaction. | `internal/store/ledgerstore.go:155`, `docs/design/09-webhooks.md` §2 |
| F13 | `BgAPI` already returns the Job-UUID; originate pins `origination_uuid` to a leg id we mint. | `internal/esl/client.go:126`, `adapter.go:325` |
| F14 | An AI outbound leg awaiting answer is armed only in the in-memory `outbound.Service.pending` map and is lost on an application restart. | `outbound.go:110` (`pending`), `outbound.go:577-585` (`arm`) |
| F15 | `ShowChannels()` (`show channels as json`) exists and its comment names restart reconciliation, but **nothing calls it**; the registry never adopts channels that predate a restart. `docs/design/01-telephony.md` §6 already specifies the behaviour (on start and every ESL reconnect: `show channels as json`, then adopt / orphan-hangup / close), so §8.2 implements an existing design rather than adding one. | `adapter.go:403-405`; `docs/design/01-telephony.md` §6 |
| F16 | Built-in flow tools are named in snake_case: `transfer_to_agent`, `take_message`, `hangup`. | `internal/flow/builtin.go:15-19` |
| F17 | Bot concurrency is capped by `AICC_BOT_MAX_CALLS` (default 220), enforced by the UAS, which answers INVITEs beyond it with `486 Busy Here`. Inbound and outbound share it. | `internal/voice/uas.go:52`, `uas.go:536`, `cmd/aicc/wiring.go:433`, `internal/config/config.go:336` |
| F18 | `dids` has no channel-limit column; `trunks.max_channels` went with the whole table in 00021 (the gateway is switch configuration). | `internal/store/migrations/00002_telephony.sql:121-135` (`dids`), `:142` (`trunks.max_channels`), `00021_the_trunk_is_the_switchs_to_hold.sql` |
| F19 | Switch events carry `aicc_did`, and an AI outbound leg carries it from the originate on (C53), so the registry can count live channels per DID. | `internal/telephony/switchevent.go:182`, `coordinator.go:384`, `outbound.go:487` and `:512` (`DialAI` vars) |
| F20 | The click-to-dial originate sets **no** `aicc_did`; `export_vars` exports `aicc_call_id` only. The caller id exists only as `effective_caller_id_number` = the default outbound number. | `outbound.go:282-292` (`export_vars`), `outbound.go:372-385` |
| F21 | The customer (trunk) leg of a click-to-dial is created by the deployment's outbound dialplan `bridge`; it inherits `aicc_call_id` only, its caller id comes from `trunk_clid`, and no `aicc_did` is set. An agent dialling by hand from the softphone takes the same dialplan. That dialplan belongs to the deployment, not the repository; before its bridge it runs the repository's `aicc_outbound.lua`, which already computes this DID (`did`). | `deploy/dev/freeswitch/dialplan/aicc/00_pstn_gateway.xml:52-72`, `freeswitch/scripts/aicc_outbound.lua:41-50` |

**Rows whose references changed since `a1e2c5e`** (the facts themselves still hold):

| Row | Draft | Now |
|---|---|---|
| F2 | `outbound.go:581` | `outbound.go:515-533`, `:577-585`, `:589-611` |
| F3 | `orchestrator.go:335` | `orchestrator.go:336` |
| F4 | `outbound.go:452` | `outbound.go:449-458` |
| F5 | `outbound.go:91`, `:596` | `outbound.go:89-91`, `:598-604` |
| F9 | `00005_call_ledger.sql:97` | adds `openapi.json:4227` |
| F10 | `outbound.go:195` | `outbound.go:194-204` |
| F11 | `adapter.go:331`; no handler | adds: `BACKGROUND_JOB` is not in `telephony.Subscriptions` (`switchevent.go:295-310`) |
| F14 | `outbound.go (arm)` | `outbound.go:110`, `:577-585` |
| F15 | `adapter.go:405` | `adapter.go:403-405`; adds the pointer to `01-telephony.md` §6 |
| F17 | `wiring.go:431` | `wiring.go:433`; adds `config.go:336` |
| F18 | `00002_telephony.sql:121` | `00002_telephony.sql:121-135`, `:142` |
| F19 | `outbound.go (DialAI vars)` | `outbound.go:487`, `:512` |
| F20 | `outbound.go:293`, `:379-386` | `outbound.go:282-292`, `:372-385` |
| F21 | `00_pstn_gateway.xml:61-72` | `00_pstn_gateway.xml:52-72`; `aicc_outbound.lua:41-50` |

Unchanged and re-confirmed: F1, F6, F7, F8, F12, F13, F16. The comment about the comma in an
originate `{…}` block that §6.6 cites as `outbound.go:293` is now at `outbound.go:290-292`.

---

## 1. What phase 1 is not

Left out on purpose, each a candidate for a later issue:

- Agent campaigns, queue targets, `AGENT_OUTBOUND` from a campaign.
- Predictive, progressive or preview pacing for agents.
- Transfer to a queue or an agent from a campaign call.
- Answering-machine detection and carrier-announcement detection (§13). Phase 1 accepts that a
  voicemail answer ends the contact as `DONE`; a later phase adds bot-side detection (§13.2).
- A provider session, or any bot resource, before the customer answers.
- Per-country number validation: a number is checked for shape, not for whether that country
  issues it.
- Scheduling across calendar dates (start / end dates); a campaign runs until it is completed or
  cancelled.
- AI disclosure. It belongs in the bot's script, not in the dialer.
- Caller-id rotation. A campaign uses one DID; if rotation is ever needed, another service
  provides it and AICC integrates with it.
- Configurable lead intents and callback rules. Fixed in phase 1 (§11), configurable later.
- Applying campaign rules (DNC, DID line limit) to calls agents dial by hand from their
  softphone: those happen in the dialplan, before AICC can refuse them (§6.6, §12).

---

## 2. Call topology

**Decision: AI outbound stays on ESL originate, not on the dialplan.** The existing `DialAI` path
(F1–F3) is kept in shape: the dialer originates the customer leg into `&park()` and bridges it to
the bot gateway on `CHANNEL_ANSWER`. The alternative, routing the call through a dialplan that
bridges to the bot, was considered and rejected (it answers the roadmap-board note "re-evaluate
whether AI outbound should go through the dialplan"). The reason is the rule below: no provider
session may exist before answer, so the dialer, not a dialplan, must decide when the bot leg is
created, reserve a bot slot first (§6.4), and attach `X-AICC-Campaign-ID` / `X-AICC-Attempt-ID`
(rule 3). A dialplan would create the bot leg at its own pace and has no access to the bot-capacity
counter or the attempt row.

```
dialer ──DialAI(CallID)──▶ originate customer leg ──▶ &park()
                                   │ CHANNEL_ANSWER
                                   ▼  reserve a bot slot (§6.4)
                     bridge ──INVITE──▶ voice.UAS ──▶ provider session opens
```

Rules:

1. **No provider session before answer.** Not at originate, not on 180/183. A pre-opened session
   is paid for, in money or in concurrency, on every call that is never answered.
2. **Ring timeout is set per attempt** as the originate's timeout, taken from the campaign's
   `ring_timeout_sec` (default 30 s), and campaign validation requires it to be **strictly below** `AnswerWindow`. With F5 as
   it stands, an answer after the window reaches a customer who hears silence; the dialer must
   make that unreachable. The single-call path has the same exposure and gets its own issue.
3. **The bridge carries the campaign.** `X-AICC-Campaign-ID` and `X-AICC-Attempt-ID` go alongside
   the existing `X-AICC-*` headers, so the orchestrator knows it is a campaign call (§10) and the
   CDR carries the campaign.
4. **A bot slot is reserved after answer and before the bridge.** If none is free the call waits
   briefly and is then treated as overflow (§6.4). An answered customer is never bridged to a bot
   that will answer 486.

---

## 3. Data model

Four new tables and two new columns. Names and columns are a sketch for the migration, not
final.

### 3.1 `campaigns`

| column | notes |
|---|---|
| `id` uuid | |
| `name` | |
| `did_id` | the outbound DID; it names the flow and the caller id. Must have a flow. |
| `status` | `DRAFT \| RUNNING \| PAUSED \| COMPLETED \| CANCELLED` |
| `pause_reason` | `MANUAL \| TRUNK_FAILURE \| PROVIDER_FAILURE \| OUTBOUND_NOT_CONFIGURED`; NULL unless `PAUSED` |
| `time_zone` | IANA name; the default for contacts that carry none (the window is evaluated per contact, §8.1) |
| `calling_window` | jsonb: days of week and one or more `[start, end)` wall-clock ranges; must lie inside the outer calling hours (§8.1) |
| `default_country_code` | used to normalise national numbers on import (§9) |
| `max_concurrent` | this campaign's share of the global caps (§6) |
| `ring_timeout_sec` | `< AnswerWindow` (§2) |
| `retry_policy` | jsonb: `maxAttempts`, `maxSystemRetries`, `intervalSec` per outcome, which outcomes retry (§7) |
| `follow_up_queue_id` | nullable; the queue a `HIGH` lead's callback is filed under (§11) |
| `created_at`, `started_at`, `finished_at` | |

### 3.2 `campaign_contacts`

| column | notes |
|---|---|
| `id` uuid | |
| `campaign_id` | |
| `number_e164` | canonical form; `UNIQUE (campaign_id, number_e164)` deduplicates within a campaign |
| `number_raw` | as imported, for the operator |
| `time_zone` | nullable IANA name; overrides the campaign's. Settled: the time zone is per contact, falling back to the campaign's |
| `slots` | jsonb, handed to the flow as `userData` |
| `status` | `ACTIVE \| DONE \| EXHAUSTED \| INVALID_NUMBER \| DNC` |
| `attempts` | attempts that reached the customer; counted against `maxAttempts` (§7) |
| `system_failures` | attempts that never reached the customer; counted against `maxSystemRetries` (§7) |
| `next_attempt_at` | when it may be dialled next; NULL unless `ACTIVE` |
| `last_outcome` | from the latest attempt |
| `lead_intent`, `lead_notes` | latest recorded lead (§11); NULL means never recorded |
| `last_attempt_id`, `last_call_id` | the latest finished attempt and its call; NULL before the first attempt finishes. Written with `last_outcome` when the latest attempt settles, so they follow every attempt while the lead is kept from the latest call that recorded one (§14). Not foreign keys, like `call_id` everywhere; migration 00042 backfilled them from the latest `FINISHED` attempt by `started_at` |

### 3.3 `campaign_attempts`

| column | notes |
|---|---|
| `id` uuid | |
| `campaign_id`, `campaign_contact_id` | |
| `number_e164` | copied from the contact; used by the cross-campaign frequency check (§5.2) |
| `call_id` | minted **before** the originate; the CDR's `call_id` |
| `leg_id` | the customer leg's `origination_uuid`, also minted before the originate |
| `originate_job_id` | the Job-UUID `bgapi` returned (§5.5) |
| `state` | `DIALING \| FINISHED` |
| `outcome` | §7; NULL while `DIALING` |
| `hangup_cause`, `talk_sec` | from the CDR, or from the job result (§5.5) |
| `is_touched` | true when the outcome counts against `maxAttempts` (§7.2), i.e. the customer was reached; read by the frequency check |
| `lead_intent`, `lead_notes` | what this call recorded |
| `started_at`, `finished_at` | |

A partial unique index `ON campaign_attempts (number_e164) WHERE state = 'DIALING'` (named `uq_campaign_attempts_dialing_number_e164`) makes "one
attempt in flight per number, across all campaigns" a database fact, not a scheduler habit. It
covers "one attempt in flight per contact" too.

### 3.4 `dnc_numbers`

Global, not per campaign: `number_e164` (PK), `source` (`MANUAL \| IMPORT \| CONVERSATION`),
`call_id` (nullable, set for `CONVERSATION`), `created_at`, `created_by`.

### 3.5 New columns on existing tables

- `cdrs.campaign_id`, nullable.
- `dids.max_outbound_channels`, nullable; NULL means no limit (§6.6).

---

## 4. State machines

### 4.1 Campaign

```
DRAFT ──start──▶ RUNNING ──pause──▶ PAUSED ──resume──▶ RUNNING
                    │                  │
                    ├──────cancel──────┴──▶ CANCELLED
                    └── no ACTIVE contact left and nothing in flight ──▶ COMPLETED
```

- `PAUSED` is entered by an operator (`MANUAL`), by a breaker (`TRUNK_FAILURE`,
  `PROVIDER_FAILURE`) or by the dialer finding the deployment unable to dial
  (`OUTBOUND_NOT_CONFIGURED`), all in §7.4. Pausing stops new attempts; attempts in flight finish normally.
- **A closed calling window is not `PAUSED`**; the campaign stays `RUNNING`. Three reasons:
  - Windows are evaluated in each contact's time zone (§8.1), so at one moment some contacts are
    inside their window and others are not. "Window closed" is a fact about contacts, which a
    campaign state cannot express.
  - `PAUSED` means "stopped by a person or a breaker; a person resumes it". A window reopens by
    itself. Folding the two together means automatic resumption either also resumes manual
    pauses, or has to tell them apart by `pause_reason` — the complexity moves, it does not go.
  - The state machine holds transitions that need a decision; idleness that happens on its own
    does not belong in it.
- So that the UI reads at a glance, a campaign also has a **read-only derived field** `activity`,
  computed by the dialer and not stored: `DIALING` (calls in flight), `WAITING_WINDOW` (every
  pending contact is outside its window), `WAITING_RETRY` (no pending contact is due yet),
  `AT_CAPACITY` (held back by a cap in §6), plus `nextDialAt`. The UI shows
  "Running · waiting for calling hours (tomorrow 09:00)".
- `CANCELLED` and `COMPLETED` are absorbing. Contacts and leads stay readable.
- Every transition goes through the dialer actor (§5): the API asks, the actor decides.

### 4.2 Contact

`ACTIVE` until:

- an attempt finishes with a terminal outcome (§7.2) → `DONE`, `INVALID_NUMBER` or `DNC`; or
- `attempts` reaches `maxAttempts`, or `system_failures` reaches `maxSystemRetries` →
  `EXHAUSTED`; `last_outcome` says which.

### 4.3 Attempt

`DIALING → FINISHED`, once. The outcome is set by the finish routine (§5.4).

---

## 5. The dialer

### 5.1 One actor

A single goroutine owns admission, in-flight counts and campaign transitions, in line with the
rule that state changes go through FSM actors. It wakes on:

- an attempt finishing (signalled after its transaction commits, §5.4);
- the earliest `next_attempt_at` across running campaigns;
- the next calling-window boundary of any running campaign;
- a bot slot or a DID line being released (§6);
- an API command (start, pause, resume, cancel, a PATCH) or a DNC insertion.

It does not poll on a tick. One timer, re-armed to the earliest of the above.

### 5.2 Admission

On each wake, for each `RUNNING` campaign in round-robin order, admit while §6 allows:

```sql
SELECT ... FROM campaign_contacts c
WHERE c.campaign_id = $1
  AND c.status = 'ACTIVE'
  AND c.next_attempt_at <= now()
  AND NOT EXISTS (SELECT 1 FROM dnc_numbers d WHERE d.number_e164 = c.number_e164)
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts a          -- any campaign: none in flight
                  WHERE a.number_e164 = c.number_e164 AND a.state = 'DIALING')
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts a          -- cross-campaign frequency
                  WHERE a.number_e164 = c.number_e164
                    AND a.campaign_id <> c.campaign_id
                    AND a.is_touched
                    AND a.started_at > now() - $frequency_window)
ORDER BY c.next_attempt_at, c.id
LIMIT $2
```

The calling-window check (§8.1) depends on each contact's time zone, so it runs in Go per contact;
the query over-fetches a small multiple of the free capacity and the actor filters.

**Cross-campaign frequency.** Once any campaign has **reached** a number (an outcome that counts
against `maxAttempts`, §7.2, marked `is_touched` on the attempt), no other campaign may dial it
within `frequencyWindow` (deployment configuration, default 24 h). A contact held back this way is
not dropped: its `next_attempt_at` moves to "last touch + `frequencyWindow`", and on to the next
window if that falls outside one. Retries within the same campaign follow its `retry_policy`. A
single `createCall` is not subject to this rule.

### 5.3 Write ahead, then dial

For each admitted contact, one transaction: mint `callId` and `legId`, insert the attempt as
`DIALING`, clear `next_attempt_at`. The counters (`attempts`, `system_failures`) are updated when
the attempt finishes (§5.4), because only then is it known whether the customer was reached.
Commit, then call `DialAI` with:

- `CallID` and the customer leg id: the `callId` and `legId` just minted;
- the number in canonical E.164 with `+` (§9.1);
- the campaign's DID;
- the contact's slots as `UserData`;
- the campaign and attempt ids.

What a synchronous error from `DialAI` means for the attempt:

| error | attempt |
|---|---|
| number on DNC (§12) | finishes `DNC_SKIPPED` |
| bad number | finishes `INVALID_NUMBER` |
| DID line limit reached (§6.6; possible only when a hand-dialled call took the last line) | **voided**: the row is deleted, nothing is counted, the contact is due again at once |
| placement unconfirmed (§5.6: the `bgapi originate` reply timed out) | **pending**: the attempt stays `DIALING` and in flight; its CDR or the backstop finishes it |
| anything else (limiter cancelled, `bgapi` send failed) | finishes `ORIGINATE_FAILED` |

Because the attempt row exists before the originate is sent, every originate has a row. Because
`DialAI` is idempotent on `CallID` (F4), re-driving an attempt never dials twice.

### 5.4 One finish routine, run in the CDR's transaction when there is a CDR

`LedgerStore.InsertCDR` (F12) already enqueues webhooks in the CDR's transaction. In the same
transaction, when a `campaign_attempts` row has this `call_id`, the finish routine:

1. sets the attempt `FINISHED` with `outcome`, `hangup_cause`, `talk_sec` (§7);
2. sets the attempt's `is_touched` from the outcome class, and increments the contact's `attempts`
   or `system_failures`;
3. updates the contact's `last_outcome` and `status`, and sets `next_attempt_at` from the retry
   policy;
4. applies the side effects of the attempt's final `lead_intent`: a DNC row, a callback (§11).

The attempt therefore never disagrees with the ledger. Endings with no CDR — a synchronous error
(§5.3), an originate that never made a channel (§5.5), reconciliation (§8.2) — run the same
routine in a transaction of their own.

When a fuller CDR revision replaces one (09-webhooks §2.1), the attempt's outcome fields follow the
latest revision; the contact is rescheduled only if this is still its latest attempt.

### 5.5 Consuming the originate's job result (`BACKGROUND_JOB`)

F11: an originate that fails before a channel exists (gateway down, dial string refused) leaves no
channel, no hangup and no CDR. Decided: subscribe to `BACKGROUND_JOB`, so `ORIGINATE_FAILED`
carries the real cause instead of being inferred from a timeout.

- **Correlation.** `Originate` hands back the Job-UUID `BgAPI` returns (F13); it is stored in
  `campaign_attempts.originate_job_id`. The outbound layer keeps a Job-UUID → `callId` map and
  dispatches each `BACKGROUND_JOB` through it.
- **What the result says.** For `originate … &park()`, `-ERR <CAUSE>` is a failure before the leg
  was up, `CAUSE` being a Q.850 name (`USER_BUSY`, `NO_ANSWER`, `GATEWAY_DOWN`, …); `+OK <uuid>`
  is success. Whether early media alone yields `+OK` on this path is measured in the live probe
  before it is relied on; nothing below depends on `+OK`.
- **One source per attempt:**
  - the channel existed (the registry saw `CHANNEL_CREATE` for this `legId`): only the CDR
    finishes the attempt; the job result is logged and cross-checked;
  - the channel never existed: `-ERR <CAUSE>` finishes the attempt at once as `ORIGINATE_FAILED`
    with that cause as `hangup_cause`.
- **Backstop.** No job result and no CDR by `ring_timeout_sec` + 30 s means events were lost (an ESL
  disconnect, say); the attempt goes to reconciliation (§8.2).
- The single-call `DialAI` gets the same: its pre-channel failures gain a cause too.

### 5.6 An originate the switch did not confirm (#221)

The ESL client gives a command 10 s for its reply. A stalled switch (seen live on 2026-10-15 and
2026-10-19, under heavy host load) reads the `bgapi originate` and answers it a minute or two later,
then runs it. The timeout is therefore not "not taken": it is `esl.ErrTimeout`, distinct from
`esl.ErrDown`, and an originate that ends with it is an **unconfirmed placement**
(`outbound.ErrPlacementUnconfirmed`, `internal/outbound/unconfirmed.go`). Decided 2026-10-21:

- **What the ESL client reports (#302).** One deadline, the command timeout counted from the call,
  bounds a command's turn to write, its write (`SetWriteDeadline`: a switch that stops reading fills
  the socket buffers and blocks the write) and its reply, so no caller waits longer whatever the
  switch does. `esl.ErrTimeout` is any command some of whose bytes reached the connection without a
  reply: the reply timed out, the link closed after the write (a bgapi job the switch accepted runs
  whatever happens to the socket), or the write was cut off. A cut-off or failed write closes the
  link before the next command may write, since a half-written command leaves the stream unframed;
  the `Link` reconnects. `esl.ErrDown` is only a command none of whose bytes were written: the link
  was closed before its turn, it waited out its timeout for a turn, or the kernel took none of its
  bytes. Only that answers `503 SWITCH_DOWN`.

- **Nothing is undone.** The line, the business data and the continuation armed for the answer (the
  bridge to the bot, or click-to-dial's transfer to the destination) stay held, and the placement
  stays tracked under the leg id it minted, for the answer window plus the job margin
  (`AnswerWindow + jobMargin`, 60 s + 30 s by default) counted from the timeout.
- **The API answers `202 Accepted`** with the same body as `201`: the customer may still be called.
  The contract says so on `createCall`. A retry with the same `callId` answers `200 isDuplicate`; a
  new `callId` is a second call.
- **Idempotency covers it, and concurrency.** A `callId` is claimed for one placement from just after
  the do-not-call check until the placement's ending is settled: the job's result, the window, or
  the placement-failure CDR having been written. With the claim held, the registry and the ledger
  are asked again, so a request that waited while another placed and settled the same `callId`
  finds it. Every placement in progress, confirmed or not, is
  therefore answered as already placed, and two concurrent requests with one `callId` dial once.
- **Reconciled from what the switch does:**
  - its channel appears within the window: that is the call. Its answer window restarts at the
    channel's creation, because the switch rings it from then (`originate_timeout`) and the window
    armed at the timeout may be mostly spent; answered, it is bridged like any call, and its CDR
    comes from its events;
  - its job result arrives: the bgapi reply that named the Job-UUID never came, so the result is
    matched by the `origination_uuid` in the job's `Job-Command-Arg` (`SwitchEvent.JobChannelID`).
    A failure before any channel is a placement failure with the switch's cause (§5.5), written at
    once. The leg is registered before the originate is sent, so a result is matched whenever it
    arrives, also before the originate has returned or before the placement is held as
    unconfirmed (#302): it is kept on the placement and applied as soon as the placement is held,
    or, for an originate whose reply did arrive, when the reply's job is attached. It is applied once;
  - nothing within the window, and no result from the switch: the placement-failure CDR
    (`tech.isPlacementFailure`) with `hangupCause` `PLACEMENT_UNCONFIRMED`, a platform word, written
    when the window ends. The line and the business data are given back. Either placement-failure
    row is superseded by the record of a late channel (next point), as by any fuller row.
  - the window is decided under the service's lock, after asking the registry whether the call is
    live: the dispatch loop gives each event to the coordinator before this service, so a call can
    be live a moment before `noteChannel` has seen its leg. Such a leg is adopted, its answer window
    counted from then.
- **A channel after the window** is hung up the moment it is created (`ALLOTTED_TIMEOUT`, the same
  word as a late answer), not adopted. By then the call is recorded as failed, a campaign may have
  scheduled the contact again, and a leg nobody will bridge must not sit in `park()` on a person.
  The leg id is remembered for 10 minutes (at most 4,096 of them). A late leg created while the
  link was down is ended the same way by the reconcile that follows the reconnect, never re-armed;
  a switch later than 10 minutes is left to that reconcile's ordinary rules. The channel's own CDR then replaces the placement
  failure, as any fuller row does. This is the one case the owner's decision did not settle; it was
  the smallest safe choice.
- **Campaigns.** `ClassifyDialError` maps the unconfirmed error to *pending*, not `ORIGINATE_FAILED`.
  The flight stays, and the backstop (`flight.nextCheck`) settles it. The dialer's `IsLive` also
  answers true for a call the outbound layer is still placing (`outbound.Service.IsPlacing`), so the
  backstop at ring timeout + 30 s waits for the window rather than settling `UNKNOWN` an attempt
  whose customer may be ringing; once the window has passed, the attempt is finished from the CDR
  (`ORIGINATE_FAILED` from the placement-failure row, or the real outcome), or `UNKNOWN` if no row
  could be written.

Not covered, and follow-ups: an HTTP access log, and a `GET /calls/{callId}`, which the contract
does not have: a client polls `GET /calls` or the event stream while the call is live and
`GET /cdrs/{callId}` once it has ended. The other commands are §5.7.

### 5.7 Every other command the switch did not confirm (#303)

A stalled switch runs a `uuid_kill` or a `uuid_transfer` late exactly as it runs an originate late,
so `esl.ErrTimeout` from any of them is "sent, not confirmed", never a failure the switch decided,
and nothing is retried blindly.

- **The API.** Every call operation answered through `callResult` (answer, hold, retrieve, mute,
  unmute, hangup, transfer, DTMF, monitor) answers `202`, as it does for success; the contract's 202
  says the outcome arrives as an event. `esl.ErrDown` keeps `503 SWITCH_DOWN`, a switch refusal
  (`-ERR …`) keeps `500`.
- **One transfer of a leg at a time** (`internal/telephony/transfer.go`). The leg is claimed in its
  call's actor before `uuid_transfer` is sent, by the agent's `transferCall` and by the bot's
  handover alike (the bot's switch routes its transfer through `Coordinator.TransferLeg`). A second
  transfer while the claim stands answers `409 TRANSFER_IN_PROGRESS`, nothing sent. The switch
  answering releases it; an unanswered one keeps it until the leg's events show it moved (unbridge,
  bridge, queue join, hangup) or a minute has passed (`transferInFlightFor`).
- **The bot's handover** (`internal/aicall/handover.go`). An unconfirmed transfer does not close the
  bot leg (that raced the late transfer, and with the teardown rule already restored could hang the
  caller up). The session is hushed both ways and every other ending locked; the bot leg ending
  settles it; after 20 s (`handoverWait`) the switch is asked whether the caller's channel exists.
  It answers commands in order on a link, so a bot leg still up then means the transfer did not run:
  the conversation goes on, the teardown rule goes back to the bot bridge's, and `transfer_to_agent`
  is refused (`TRANSFER_FAILED`) for the rest of the call, so no second transfer can race a first
  that runs after all. The flow goes back to the phase `transfer_to_agent` was called in
  (`Engine.ReturnTo`: no visit counted, no `announce` said again, the silence and
  turns-without-a-tool counts started over), not the terminal phase the transfer led to, where no
  rule fires and neither the time limit nor `closingTarget` would ever end the call. The settling
  goroutine hands the failure to the call's own (`drive`), which moves the flow back, re-pins the
  standing instructions (instructions only), then lifts the hush and tells the model the transfer
  failed so it offers a message. A provider failure after that sends no rescue transfer: the bot
  leg closes unmarked and the dialplan's fallback takes the caller, as for any bot that vanished.
  The time limit's own handover ends the call instead (`SESSION_LIMIT`). A switch that answers
  nothing after three questions leaves the caller to the dialplan.
- **The platform's own endings** (`telephony.EndLeg`: overflow, late answer, recovery, the
  supervisor's previous monitoring leg, a bridge that never ran). The kill is sent once; one the
  switch did not answer is followed after 20 s (`HangupCheckAfter`) by `uuid_exists`, and only a leg
  the switch still has is stamped and killed a second time.
- **The bridge to the bot** (`internal/outbound/bridge.go`). The slot reservation is extended before
  the command over its timeout and a 20 s window after it (`bridgeUnconfirmedFor`), since a late
  INVITE would otherwise find it expired. The bot leg's `CHANNEL_CREATE` (or the customer's bridge
  or hangup) settles it; the INVITE consumes the reservation as usual. When the window ends, the
  reservation is withdrawn (`BotSlots.Withdraw`): one the INVITE consumed means the bridge ran; one
  still held means it did not, the key is refused to any later INVITE (`480`) for 10 minutes, and
  the customer leg is hung up with `hangupCause` `BOT_BRIDGE_UNCONFIRMED`, a platform word stamped
  like `OVERFLOW`. A campaign attempt so ended is `BOT_FAILED`.

---

## 6. Capacity

### 6.1 Four limits

- **Bot concurrency — hard, and shared with inbound.** Whether the bot is a cloud provider
  (per-account concurrency quota) or a self-hosted cascade-realtime-gateway (its concurrency
  limit), it is counted in concurrent sessions and cannot be raised at will. It is the binding
  resource for outbound.
  - **One number:** `AICC_BOT_MAX_CALLS` (F17), set at deployment to no more than the provider's
    or gateway's limit. The dialer has no bot cap of its own, so two numbers cannot drift apart.
  - **Inbound comes first.** Bot-first means every inbound call takes a bot too, and a campaign
    must not take them all. `inboundReserve` (deployment configuration, default
    `max(2, 10% × AICC_BOT_MAX_CALLS)`) is kept for inbound. `botInUse` counts every slot in use —
    inbound, outbound, and reserved but not yet bridged (§6.4). As inbound grows, `botInUse` rises
    and campaigns stop admitting new calls; calls in flight are not touched.
- **DID outbound lines** (§6.6): what the carrier sells per number, e.g. at most 10 outbound calls
  on 95555. Separate from inbound lines.
- **Ringing legs:** `maxRinging`, bounded by trunk channels and FreeSBC limits.
- **Per campaign:** `max_concurrent`.

The token bucket (F6) keeps pacing originates per second, unchanged.

### 6.2 Why a ringing leg must not hold a bot slot

Reserving a slot for every ringing leg (`talking + ringing < cap`) wastes exactly the scarce
resource.

[INFERENCE] Rough figures: answer rate 20 %, an unanswered call rings 30 s, an answered one rings
8 s and talks 90 s. An attempt holds a slot for `0.8 × 30 + 0.2 × (8 + 90) ≈ 43.6 s` on average, of
which `0.2 × 90 = 18 s` is conversation — **about 41 % bot utilisation**, and less the lower the
answer rate.

### 6.3 Admission by expected answers

```
admit iff  ringing < maxRinging
       and campaign.inFlight < campaign.max_concurrent
       and didOutboundInUse(d) < d.max_outbound_channels                          -- §6.6
       and botInUse + Σ_campaigns (p̂_c × ringing_c) + 1 ≤ AICC_BOT_MAX_CALLS − inboundReserve
```

- `p̂_c` is a **conservative upper bound** on campaign `c`'s answer rate: the Wilson 95 % upper
  bound over its last 200 finished attempts that rang. "Answered" here means any outcome after an
  answer: `ANSWERED`, `BOT_FAILED`, `OVERFLOW`.
- **Cold start, `p̂ = 1`:** below 50 such attempts, the rule is "every ringing leg holds a slot" —
  no over-dial. As samples accumulate it tightens to the real answer rate. A new campaign needs no
  tuning.
- The sum runs over the ringing legs of every running campaign, since they compete for the same
  bot capacity.

**As built (S4).**

- `botInUse` is the slots in use, the slots reserved for answered legs that have not bridged yet, and
  the answered legs waiting in the overflow hold for one (`voice.Usage.Waiting`). The last term closes
  the gap in which a leg that had stopped ringing and not yet reserved was counted nowhere.
- The reserve is one number with two uses. `AICC_BOT_INBOUND_RESERVE` set explicitly is both what
  campaign admission keeps free and what every outbound reservation respects. Unset, campaign
  admission keeps `max(2, 10 % x AICC_BOT_MAX_CALLS)` free (never so many that no slot is left for
  outbound), while an admitted call that answers may still take any free slot, because refusing it
  would lose the lead the admission rule already paid for.
- A sample enters a campaign's answer-rate window when its ring timeout (plus 3 s) is over, whichever
  way the ring went, and the window orders samples by when the attempt was dialled. Entering at the
  answer, or at the end of the conversation, makes the window lag or lead its own cohort: answers are
  known seconds after the dial and non-answers only at the timeout, so a burst larger than the window
  fed in arrival order leaves the window holding nothing but non-answers and the dialer plans its way
  into overflow (found in the capacity simulation, `internal/dialer/sim_test.go`). `TRANSIENT`,
  `ORIGINATE_FAILED`, `INVALID_NUMBER` and `DNC_SKIPPED` never rang and `UNKNOWN` left no trace, so none
  is a sample.
- After a restart the window is rebuilt from the campaign's last 600 finished attempts, then kept by
  each finish; no admission reads the database.

### 6.4 Take a slot on answer; wait briefly if there is none

The slot is taken **after answer and before the bridge**, not discovered full when the INVITE
reaches the UAS.

1. The customer leg answers; the armed continuation asks the bot-capacity counter for a slot and
   reserves it under `callId`. When the UAS receives the INVITE for that `callId`, it consumes the
   reservation and does not compete with inbound.
2. A slot is free: bridge at once, as today.
3. No slot is free: the customer leg stays parked for up to `overflowHold` (default 1500 ms). As
   soon as any bot session ends and frees a slot, bridge. At 200 concurrent sessions averaging
   90 s, a slot frees about every 0.45 s [INFERENCE], so most overflows clear within the hold.
4. Still none: hang up, with no announcement; the outcome is `OVERFLOW` (§7). AICC has no TTS, so
   an announcement would have to be an uploaded file; phase 1 does without.

[FACT] Today a full UAS answers 486 (F17) and a failed `DialAI` bridge is only logged.
[INFERENCE] So a single AI outbound call answered while the bots are full is silence for the
customer. The reservation in step 1 closes that gap as well.

### 6.5 Holding overflow down

Over-dialing costs overflow: a customer answers and no bot comes. Since an answered contact is
never redialled (§7.2), every overflow is a lost lead, and it must be held very low,
deterministically.

- Per campaign, track the overflow rate over the last 200 answered calls.
- Above 1 %: `p̂_c` uses the 99 % upper bound instead of 95 %.
- Above 3 %: the campaign falls back to `p̂ = 1` (no over-dial) until the rolling rate is back
  under 1 %.
- Both thresholds are deployment configuration; these are the defaults.

**As built (S4).** The overflow window is fed at the answer and turned into an overflow 1.5 s later
if no bot came. It starts without a minimum sample count: one overflow among the first few answers
is above both rates and plans for every answer, which is the safe direction; it leaves the stopped
state when the rate is under the tighten rate (`AICC_OVERFLOW_TIGHTEN_RATE`,
`AICC_OVERFLOW_STOP_RATE`, 1 % and 3 %). Because a campaign that keeps the bot nearly full has few legs
ringing at any moment (its ringing population is the free capacity divided by the answer rate), a
shift of the answer rate alone seldom causes overflow; a stale history, a cut quota or an inbound
surge does, and those are what the simulation uses.

Compliance note [ASSUMPTION]: rules such as the US FTC TSR set a 2-second and a 3 % limit on
abandoned calls, and whether an AI answering counts as "connected to a sales representative" is
unsettled. An `overflowHold` of 1500 ms and a 3 % ceiling follow the strictest reading; a customer
targeting a given market confirms with its own compliance team.

### 6.6 DID outbound line limit

A carrier sells lines per number: a DID (95555, say) carries at most N simultaneous outbound
calls. [FACT] AICC does not know N today (F18). [INFERENCE] Above it the carrier refuses, the
refusal is classified `TRANSIENT` here, and a run of them would wrongly trip the trunk breaker
(§7.4). The dialer must know N at admission.

**Counting basis (confirmed by the owner, 2026-10-11):** inbound and outbound lines are counted
separately and never take from each other. Only outbound is limited; inbound is neither counted
nor reserved for.

**Not like a bot slot.** A carrier line is taken when the INVITE goes out (ringing) and held until
hangup, so it is counted as **ringing + talking**, without `p̂`, and never over-dialled.

**One column on `dids`:** `max_outbound_channels`, nullable, NULL meaning no limit. It is a fact of
the number's contract, so it lives with the DID in AICC, maintained through `/dids` and the SPA —
unlike a gateway, which is switch configuration. FreeSBC may later mirror the limit as defence in
depth; the source of truth is AICC.

**`didOutboundInUse(d)` is held by the outbound layer, in two parts:**

1. **Calls AICC places (reserved).** `DialAI` (single calls and campaigns) and click-to-dial
   reserve a line under `callId` **before** the originate, released when the call ends (its last
   leg hangs up) or the originate fails. Reserving before the originate means originates sent in
   the same instant cannot all slip past the limit before their `CHANNEL_CREATE` arrives. A
   click-to-dial reserves from the moment the agent's leg rings, a few seconds before the carrier
   line is actually taken — conservative, and acceptable since agent phones auto-answer.
2. **Calls AICC does not place (observed).** An agent dialling by hand from the softphone goes out
   through the dialplan. The registry counts an outbound leg that carries `aicc_did` and whose
   `aicc_call_id` holds no reservation, and uncounts it on hangup.

**Today the customer leg of a click-to-dial or a hand-dialled call carries no `aicc_did` (F20,
F21), so part 2 sees nothing.** The fix goes into the repository's `aicc_outbound.lua`, not into
`Dial()` and not into the deployment's dialplan:

- it is the one place on the dialplan's outbound path that decides which number the far end sees,
  it already holds `did`, and both click-to-dial and hand-dialled calls pass through it;
- before the bridge it runs `session:setVariable("aicc_did", did)` and appends `aicc_did` to
  `export_vars`, so the trunk leg inherits it. This goes **before** the script's early return for a
  recording already started. Setting `export_vars` from Lua is not subject to the comma problem of
  an originate `{…}` block that the comment at `outbound.go:293` describes;
- the deployment's dialplan needs no change, and the repository does not depend on a deployment
  file. A deployment must already call `aicc_outbound.lua` before its bridge; recording requires
  it.

**Where it is enforced:** in `outbound.Service`, like DNC.

- **Dialer:** checked at admission (§6.3). Being held back is not an attempt; the campaign's
  `activity` is `AT_CAPACITY`. Campaigns sharing a DID share its limit, round-robin.
- **Single `createCall` and click-to-dial:** **409 `DID_CHANNELS_EXHAUSTED`**, no CDR.
- **Hand-dialled calls:** counted, not refused. They happen in the dialplan, where AICC has no
  chance to refuse first; above the limit the carrier refuses. Accepted for phase 1.

**Residual race:** a hand-dialled call and an AICC call taking the last line in the same instant —
the carrier refuses one, which shows as `TRANSIENT` and is retried by the normal rules. Rare
enough not to lock for.

---

## 7. Outcomes and retries

### 7.1 The rule in one breath

When a call ends, its outcome answers three questions: **is this number dialled again; if so,
after how long; and did this call use up one of the customer's chances?**

- **Two budgets, counted separately:**
  - `maxAttempts` (default 3): **how many times the customer's phone may be rung.** Only calls
    that reached the customer count: rang, busy, rejected, answered, or impossible to tell.
  - `maxSystemRetries` (default 5): **how many times our or the network's failures are retried.**
    These calls never reached the customer's phone and must not use up the customer's chances, but
    they are bounded so a number that always fails is not retried forever.
- **Retry time = this call's end + the interval for its outcome.** If that falls outside the
  contact's window, it moves to the start of the next window; it is never dropped.
- **When the retry time comes, the contact is back in the pool**, queued with new contacts by
  `next_attempt_at` (§5.2).

### 7.2 Outcomes and default policy

The outcome is derived from `hangup_cause` — the CDR's, or the job result's for a failure before
any channel (§5.5) — not from `releaseReason` alone, because `FAILED` folds a vacant number
(terminal) together with a network fault (retryable).

| outcome | typical causes | dialled again? | default interval | budget |
|---|---|---|---|---|
| `ANSWERED` | answered, any talk time | no — contact `DONE` | — | `maxAttempts` |
| `BOT_FAILED` | answered; the bot side failed before the flow ended, or the bridge to it never ran (`BOT_BRIDGE_UNCONFIRMED`, §5.7) | no — contact `DONE` | — | `maxAttempts` |
| `OVERFLOW` | answered; no bot slot after `overflowHold` (§6.4) | no — contact `DONE` | — | `maxAttempts` |
| `NO_ANSWER` | `NO_ANSWER`, `NO_USER_RESPONSE`, our ring timeout | yes | 2 h | `maxAttempts` |
| `BUSY` | `USER_BUSY` | yes | 30 min | `maxAttempts` |
| `REJECTED` | `CALL_REJECTED` | yes | 24 h | `maxAttempts` |
| `UNKNOWN` | reconciliation could not tell (§8.2) | yes | 24 h | `maxAttempts` |
| `TRANSIENT` | `NORMAL_TEMPORARY_FAILURE`, `NETWORK_OUT_OF_ORDER`, `RECOVERY_ON_TIMER_EXPIRE`, `SWITCH_CONGESTION`, `NORMAL_CIRCUIT_CONGESTION` | yes | 5, 15, 45 min … (×3 each time, capped at 4 h) | `maxSystemRetries` |
| `ORIGINATE_FAILED` | failure before any channel (§5.5), e.g. `GATEWAY_DOWN` | yes | as `TRANSIENT` | `maxSystemRetries` |
| `INVALID_NUMBER` | `UNALLOCATED_NUMBER`, `INVALID_NUMBER_FORMAT`, `NUMBER_CHANGED` | no — contact `INVALID_NUMBER` | — | — |
| `DNC_SKIPPED` | on DNC at admission or inside `DialAI` | no — contact `DNC` | — | — |

- Which outcomes retry, their intervals and both budgets are the campaign's `retry_policy`; the
  table holds the defaults. The answered outcomes are not configurable.
- **An answered number is never dialled again (owner's decision).** A customer who has picked up
  is not called back by the dialer whatever happened — the alternative is harassment complaints.
  This covers every answered outcome: `ANSWERED` (including a hang-up within a second, a voicemail,
  a call where `record_lead` was never called), `BOT_FAILED` and `OVERFLOW`. The price is that a
  bot failure or an overflow loses the lead outright, which is why the overflow control (§6.5) and
  the provider breaker (§7.4) are required. `talk_sec` and `lead_intent` (possibly NULL) are kept
  for reporting.
- `UNKNOWN` counts against `maxAttempts` with a 24 h interval: the call may have rung or even been
  answered, so one call too few beats one too many.
- The cause → outcome mapping is a table in code with a test per cause. An unmapped cause becomes
  `TRANSIENT`, logs a WARN and increments a metric, so new causes surface; it stays bounded by
  `maxSystemRetries`.

### 7.3 Example

Window 09:00–18:00, `maxAttempts = 3`:

| time | what happened | counts | next |
|---|---|---|---|
| Mon 10:00 | rang 30 s, nobody answered: `NO_ANSWER` | attempts = 1 | 12:00 |
| Mon 12:00 | gateway unreachable: `ORIGINATE_FAILED` | system_failures = 1, attempts unchanged | 12:05 |
| Mon 12:05 | `BUSY` | attempts = 2 | 12:35 |
| Mon 12:35 | declined: `REJECTED` | attempts = 3 | `maxAttempts` reached — contact `EXHAUSTED` |

Had the 12:05 attempt instead been a `NO_ANSWER` at 17:50, the next time would compute to 19:50,
outside the window, and move to Tuesday 09:00.

### 7.4 Breakers

- **Trunk breaker:** `N` consecutive attempts in a campaign (default 20) ending `TRANSIENT` or
  `ORIGINATE_FAILED` → `PAUSED / TRUNK_FAILURE`. Without it, a trunk outage spends the whole list's
  system retries in minutes.
- **Provider breaker:** `M` consecutive answered calls (default 5) ending `BOT_FAILED` →
  `PAUSED / PROVIDER_FAILURE`. Answered contacts are not redialled, so a provider outage that does
  not stop the campaign burns one lead per answered call.
- **No outbound endpoint (decided 2026-10-21, #293 review).** A deployment with an empty
  `AICC_OUTBOUND_ENDPOINT` cannot place any call, so the first attempt the placer refuses
  (`ErrNoOutboundEndpoint`) is voided — no attempt row, no retry budget, the contact due again as
  it was — and the campaign is paused at once as `PAUSED / OUTBOUND_NOT_CONFIGURED`, a reason of
  its own because the fix is a setting, not the carrier `TRUNK_FAILURE` sends an operator to. One
  attempt is enough evidence: waiting for `N` of them would spend the list's system retries on a
  setting. Whatever the pause does (it can fail on a database error), the campaign admits nothing
  more in that pass, and the next pass tries the pause again: the voided contact is due again, and
  a pass that kept admitting would refuse it for ever.
- **Start and Resume refuse early.** Both ask `outbound.Service.CanPlaceAI` (the one answer; the
  campaign service keeps no copy of the setting) and answer `409 OUTBOUND_NOT_CONFIGURED`, the
  campaign staying `DRAFT` or `PAUSED`, after the status check (`CAMPAIGN_INVALID_TRANSITION`
  first). Without that a resume would report `RUNNING` for the few seconds until the dialer
  paused the campaign again. The dialer's rule above still covers a campaign that was `RUNNING`
  when the application came back without an endpoint.
- Both are resumed by an operator.

---

## 8. Time and recovery

### 8.1 Calling hours

- **Outer calling hours, not overridable:** deployment configuration `AICC_OUTBOUND_HOURS`,
  default 08:00–21:00 in the callee's local time, every day. A campaign's `calling_window` must lie
  wholly inside it; this is checked on create and on every change, and refused otherwise
  (`CAMPAIGN_WINDOW_OUTSIDE_LIMIT`). The dialer checks again at admission, so a call goes out only
  when both allow it — including after the outer hours are narrowed under an existing campaign.
  The outer hours bind campaigns only: a single call does not know the callee's time zone.
- Evaluated in the contact's time zone, falling back to the campaign's (settled: the time zone is
  per contact, not per campaign). Legal calling hours are the callee's local time, and a US list
  spans zones.
- A window is wall-clock time and may cross midnight (a range starting at 22:00 and ending at 02:00
  belongs to the day it starts on), if the outer hours allow it.
- When a window closes, attempts in flight finish; no new one starts.
- Daylight saving: boundaries are computed for each day with `time.LoadLocation`, never by adding
  24 h.

### 8.2 Restart: adopting the calls still live

FreeSWITCH and the application are two processes; calls do not end when the application restarts.
Decided: **on an application restart (and on every ESL reconnect) the registry adopts the channels
still live in FreeSWITCH.** This is prerequisite work with its own issue (P1, §17); it also closes
the same gap for the single-call `DialAI`. It implements an existing design, not a new one:
`docs/design/01-telephony.md` §6 already specifies adopt-on-start-and-reconnect, and nothing calls
`ShowChannels()` yet (F15).

The gaps today (F14, F15):

- an AI outbound leg awaiting answer is armed only in memory; if the customer answers after a
  restart, nobody bridges the bot and the customer hears silence;
- `ShowChannels()` exists and nothing calls it; the registry does not know the channels that
  predate a restart.

Adoption, run before the dialer admits anything:

1. `show channels as json`; for each channel carrying `aicc_call_id`, rebuild the call actor.
2. For each campaign customer leg (matched to a `DIALING` attempt by `legId`):

   | channel state | action | attempt |
   |---|---|---|
   | still ringing | re-arm, with what is left of the answer window measured from the original originate | finishes normally, from the CDR |
   | answered but still parked, not bridged (answered during the restart) | hang up now: the customer has been talking into silence, and a bot arriving late is worse | `BOT_FAILED` |
   | bridged to a bot | the bot was in-process (`voice.UAS`) and went with the restart; hang up the customer leg | `BOT_FAILED` |

3. For each `DIALING` attempt with no channel:
   - a CDR exists: finish from it (normally §5.4 already has);
   - none: finish as `UNKNOWN`, counted against `maxAttempts` (§7), retried after the normal
     interval, never at once.

What is guaranteed, and what is not:

- **Guaranteed:**
  - every originate has an attempt row (§5.3);
  - a number never has two attempts in flight (§3.3);
  - an attempt is never dialled twice (F4);
  - a call ringing at the restart reaches the bot when answered;
  - no answered customer leg is left without a bot after a restart.
- **Not achievable:** "every attempt's outcome is known". A call that started and ended while the
  application was down leaves no trace in FreeSWITCH; it can only be `UNKNOWN`.

This replaces #89's acceptance line "no contact is dialled twice and no attempt is lost", which
cannot be met as written.

---

## 9. Numbers and import

### 9.1 Canonical form

Every number the dialer or DNC compares is canonical E.164 (`+` and 8–15 digits):

1. strip spaces, dashes, dots and brackets;
2. keep a leading `+`; a leading `00` becomes `+`;
3. otherwise drop one leading trunk `0` and prefix `+<default_country_code>`;
4. the result must match `^\+[1-9][0-9]{7,14}$`, else the row is rejected.

With country code 86, `+8613800138000` and `138 0013 8000` are one contact, and a DNC entry typed
either way matches both.

**Dial string (decided):** `isDialable` (F10) is widened to allow **one leading `+`**, nothing
else. A single `createCall` may still pass a national `to`, and is dialled as sent.

**What the trunk is sent (decided by the owner, 2026-10-12, after a live finding).** The draft
assumed FreeSBC expects E.164 and also accepts national form. On the development trunk it does
not: `+8618688886666` and `8618688886666` are both answered `480 Temporarily Unavailable`, and
`18688886666` connects. Storage and comparison stay canonical E.164 whatever the trunk wants; only
the string handed to the switch changes, by the deployment setting `AICC_OUTBOUND_DIAL_FORM`:

- `E164` (default): `+<cc><number>`;
- `E164_NO_PLUS`: `<cc><number>`;
- `NATIONAL`: the number without `+<cc>`, using the campaign's `defaultCountryCode`, else
  `AICC_DEFAULT_COUNTRY_CODE`. A number from another country is dialled as E.164, with one WARN
  per campaign.

The attempt and the contact keep `number_e164`; the CDR's `to` is the dialled string. A trunk
prefix such as a landline area code's leading `0` is not restored (known limit). A single
`createCall` is not converted.

### 9.2 Import

`POST /campaigns/{campaignId}/contacts` takes a JSON array; the SPA parses the file in the browser.
CSV (`csv.ts`) and Excel workbooks (`xlsx.ts`) both arrive as the row model in `table.ts`, so the
mapping, preview, chunking and per-row report are the same for either.

- `.xlsx` is read with `read-excel-file` (MIT), imported only once a workbook is chosen. The first
  sheet is used; when the workbook has several, the operator picks one.
- A cell is the value the spreadsheet holds, not a float that lost it: text keeps its own
  characters (a leading zero stays part of it), a number keeps its digits (never `1.38e10`), a date
  is `yyyy-mm-dd`, a formula is its cached result. Cell *display* formats are not applied.
- A `.xlsx` that cannot be read — a corrupt archive, a legacy `.xls` — is reported and imports
  nothing. `.xls` itself is out of scope.
- The result is per row: imported, merged as a duplicate, or rejected with an error code
  (`NUMBER_EMPTY`, `NUMBER_INVALID`, `SLOT_UNKNOWN`, `SLOT_TOO_LONG`, …).
- One bad row never fails the import.
- Slot names must be ones the flow declares; values are length-capped.
- Import is allowed in `DRAFT`, `RUNNING` and `PAUSED`.

---

## 10. A campaign call has no way to a human

- **At creation:** a campaign whose DID's flow can transfer is refused
  (`CAMPAIGN_FLOW_HAS_TRANSFER`), and so is publishing a version that introduces a transfer into a
  flow a non-terminal campaign uses.
- **At runtime,** for a call carrying `X-AICC-Campaign-ID`:
  - the built-in tools offered are `record_lead` (§11) and `hangup`, plus the flow's own HTTP
    tools. `transfer_to_agent` is not offered, and neither is `take_message`: in a campaign,
    callbacks come only from `HIGH` leads (§11), and `take_message` would file one for any call;
  - a provider failure takes the no-fallback branch of `rescueCaller` (F7) whatever the DID's
    fallback queue says, and the attempt finishes `BOT_FAILED`.
  - the session limit (`global.maxDurationSec`, design 02) never hands a campaign call to the
    DID's queue: at 100 % the call ends through `global.closingTarget`. This third way to a
    person was missing from the first draft (found while building #163).

---

## 11. Leads

The model judges; code records.

- **A new built-in tool, `record_lead`,** alongside `take_message`, `transfer_to_agent` and
  `hangup` (F16), callable **at any time** in a campaign call and bound to no flow node. Arguments:
  - `intent`: `HIGH | MEDIUM | LOW | NONE | DO_NOT_CALL`, validated as an enum; anything else is
    returned to the model as a tool error;
  - `notes`: free text, length-capped.
- **Callable many times; the last call wins.** A customer's stance moves during a call ("I'll think
  about it", then "send me the offer"), so each call overwrites the attempt's `lead_intent` /
  `lead_notes` and returns success to the model.
- **Side effects run once, when the call ends,** in the attempt's finish transaction (§5.4), from
  the final value:
  - `DO_NOT_CALL`: the number goes into `dnc_numbers` with `source = CONVERSATION`;
  - `HIGH`: a `callbacks` row (F9) with `phone_number`, `call_id`,
    `queue_id = follow_up_queue_id`, and the notes as `message`.

  A model changing its mind mid-call therefore leaves no duplicate or stale callback, and nothing
  needs undoing. A DNC that takes effect at the end of the call rather than during it changes
  nothing: the call is not hung up for it, and an answered contact is not redialled anyway.
- **A phase may declare the lead** (`"lead": {"intent": …, "notes": …}`, #352), which the
  engine records when a campaign call enters that phase, through the same write: one more write,
  ordered with the model's `record_lead` calls, and the last one wins. It exists for the flow that
  classifies the customer and closes in the same move, where the model has no later turn in which
  to call `record_lead`. Any phase may declare one, and entering it again writes again; `notes` is
  rendered from the slots at entry and capped like the tool's. On a call that is not a campaign
  call the declaration is ignored. Intent and slot references are checked when the flow loads
  (`docs/flow-authoring.md` section 2.7.1).
- **Copied to the contact** when the attempt finishes (latest wins). A call that recorded no lead,
  by `record_lead` or by a phase, leaves `lead_intent` NULL, which is not the same as `NONE`.
- **Human follow-up** is the existing claim / complete / release flow plus click-to-dial. Phase 1
  builds nothing else for it.
- **Fixed in phase 1:** the intent enum, and "only `HIGH` files a callback". Both become
  configurable later. Intent is a contract enum, not built on dispositions, which are read-only
  today.
- `record_lead` is not offered outside campaign calls.

---

## 12. Do-not-call

- **One choke point:** `outbound.Service` checks DNC in **both** `DialAI` and the click-to-dial
  path, so no path AICC places a call through dials a DNC number, single calls included. The dialer
  also filters at admission (§5.2) so as not to spend capacity on them. Calls agents dial by hand
  from the softphone go through the dialplan and are not checked (§1).
- **A single `createCall` to a DNC number** returns **422 `NUMBER_ON_DNC`** and writes no CDR: the
  CDR is the ledger of calls placed, and this one was not. (Settled; this was one of #89's open
  questions.)
- **A number added while its campaign runs:** an attempt not yet admitted is skipped
  (`DNC_SKIPPED`); one already ringing or talking is not hung up, and no retry follows because the
  contact becomes `DNC`.
- **Comparing a single call's number (decided by the owner, 2026-10-12; the default revised
  2026-10-21, #293).** DNC entries are canonical E.164 (§9.1). A single call's `to` is
  canonicalised with the deployment setting `AICC_DEFAULT_COUNTRY_CODE` before the check. The
  binary's default is `1`: a national number is never ambiguous, and a deployment outside +1 sets
  its own. The deploy stack keeps the `86` it has always set — an upgrade must not change how an
  existing deployment's national numbers match the list — and the development `.env` sets `86`
  too. The value cannot be blanked — `AICC_DEFAULT_COUNTRY_CODE=` reads as unset and the default
  applies — so a national `to` is always canonicalised, while a `+` or `00` number is compared as
  written. There is no unset state left, which is why `aicc doctor` no longer warns about one.

---

## 13. Latency, and what to measure first

### 13.1 Time to the first word

No bot resource exists before answer (§2), so what the customer hears first waits on the whole
chain. It is measured per stage on the first real campaign, never as one number:

1. `CHANNEL_ANSWER` → bridge INVITE received by `voice.UAS`
2. INVITE → provider session ready (WebSocket connect and session configuration)
3. session ready → first bot audio frame sent

If the total is too long, the candidate that keeps "no session before answer" is to play the flow's
`announce` greeting from cached audio on answer while the session opens in parallel, then write the
greeting into the conversation as an assistant item. It is not designed here: it moves barge-in
during the greeting into the switch.

### 13.2 Measured before anything is built for them

- **The share of answered calls that reach a voicemail or a carrier announcement.** They spend
  provider minutes and, with answered numbers never redialled, end the contact. Many carriers in
  China answer 200 OK and play the vacant-number announcement, which `hangup_cause` does not show.
  Accepted for phase 1 (owner, 2026-10-11). A later phase adds **bot-side detection**: the bot
  reports that it reached a machine rather than a person, and that becomes an answered outcome of
  its own, the one answered outcome a campaign may redial. Phase 1 leaves room for it without
  building any of it: outcomes are an additive enum, and the "never redial after answer" rule is
  stated per outcome (§7.2). The measurement above sizes the work.
- **Answer rate by caller id.** Caller-id reputation and attestation sit at the trunk, outside
  AICC.

---

## 14. Contract, configuration and surface (sketch)

Contract first in `docs/openapi.json`; final shapes come with the PR.

**API:**

- `POST/GET /campaigns`; `GET/PATCH/DELETE /campaigns/{campaignId}`. In `DRAFT` every field may
  change; in `RUNNING` / `PAUSED` only `maxConcurrent`, `callingWindow` and `retryPolicy`, affecting
  later admissions and not calls in flight; anything else is `CAMPAIGN_FIELD_LOCKED`. `DELETE` only
  in `DRAFT`.
- `POST /campaigns/{campaignId}/start | pause | resume | cancel`
- `POST/GET /campaigns/{campaignId}/contacts` (filter by `status`, `leadIntent`). A contact carries
  `lastCallId` and `lastAttemptId`, required and `null` until its first attempt finishes (§3.2).
- `GET /callbacks?callId=` (#188): only the callbacks one call filed. A campaign call files at most
  one, so a `HIGH` lead resolves to its callback with the contact's `lastCallId`, never by comparing
  phone numbers: one number can hold callbacks from several calls and campaigns, and `numberRaw` and
  `phoneNumber` need not share a format.
- `GET /campaigns/{campaignId}/attempts`
- `GET /campaigns/settings` (`campaigns:read`): the rules a write is held to, so a form does not
  mirror them. `outboundHours` is `AICC_OUTBOUND_HOURS` as a `CampaignTimeRange` (callee-local; a
  range whose end is before its start crosses midnight, and a window range is inside it only within
  the same stretch). `defaults` is what a create applies to what it leaves out (`maxConcurrent`,
  `ringTimeoutSec`, the full `retryPolicy`). `limits` are inclusive: `maxConcurrent` 1 to 100000,
  `ringTimeoutSec` from 5 to the largest whole number of seconds below the 60-second answer window
  (59). One source: the service builds the response from the constants and the environment its
  validators read (`Service.Settings`), and a test creates at each advertised edge and one past it.
  The server still enforces every rule; the response only lets a client pre-validate.
- `POST/GET/DELETE /dnc-numbers`
- `/dids`: a DID gains `maxOutboundChannels` (§6.6) and the read-only `isCampaignReady`: enabled,
  `allowOutbound`, a published flow, and that flow cannot transfer (`flow.CanTransfer`). It is
  `DIDFlow.CheckForCampaign() == nil`, the same function create and start apply, so the flag and the
  refusal (`CAMPAIGN_DID_NOT_OUTBOUND`, `CAMPAIGN_FLOW_HAS_TRANSFER`, `DID_DISABLED`,
  `DID_HAS_NO_PUBLISHED_FLOW`) cannot disagree. It is computed from the published revision on every
  read (two queries for the whole list, not one per number), so republishing flips it; a client
  value in a write body is ignored. A number whose published revision cannot be loaded reads as not
  ready.

**CDR, webhooks, SSE:**

- The CDR gains `campaignId`; webhook filters accept `campaignId`.
- SSE `CAMPAIGN_UPDATED` carries status, `activity`, `nextDialAt` and counters, coalesced to at most
  one per second per campaign. It is also published when a campaign is created (status `DRAFT`,
  zero contacts): nothing existed and now a `DRAFT` does, which is a state change, so a second open
  list learns of it without a refetch. `Service.Create` tells the same reporter every other change
  goes through (`WithCreatedHook`), after the row is stored; a create that is refused stores
  nothing and publishes nothing. The window opens at the create, so the event follows the `201`
  response by up to `ReportInterval` (1 s), never precedes it, and a campaign started inside that
  second is announced once, as `RUNNING`.

**Naming (checked against `07-naming.md`):** the `CAMPAIGN_*` SSE event type is a new scope
prefix and joins the catalogue in `04-api-sse.md` §4 when the contract lands. Durations that reach
the contract carry their unit: the draft's `overflowHold`, `frequencyWindow` and `ringTimeout` are
`overflowHoldMs`, `frequencyWindowSec` and `ringTimeoutSec` in JSON (they are deployment
configuration today, so only the env names `AICC_OVERFLOW_HOLD` and the like appear). `callId`,
`legId`, `attemptId`, `campaignId`, `leadIntent` and `maxOutboundChannels` follow the
`call_id` / `leg_id` / `attempt_id` / `campaign_id` / `lead_intent` / `max_outbound_channels` columns.

**Error codes:** `NUMBER_ON_DNC`, `DID_CHANNELS_EXHAUSTED`, `CAMPAIGN_FLOW_HAS_TRANSFER`,
`CAMPAIGN_RING_TIMEOUT_TOO_LONG`, `CAMPAIGN_WINDOW_OUTSIDE_LIMIT`, `CAMPAIGN_FIELD_LOCKED`,
`CAMPAIGN_INVALID_TRANSITION`, and the per-row import codes.

**Deployment configuration** (names final in the PR, registered in `.env.example`):

| setting | default | § |
|---|---|---|
| `AICC_OUTBOUND_HOURS` | `08:00-21:00` callee-local | 8.1 |
| `AICC_CAMPAIGN_FREQUENCY_WINDOW` | `24h` | 5.2 |
| `AICC_BOT_INBOUND_RESERVE` | `max(2, 10%)` of `AICC_BOT_MAX_CALLS` | 6.1 |
| `AICC_OUTBOUND_MAX_RINGING` | sized to the trunk | 6.1 |
| `AICC_OVERFLOW_HOLD` | `1500ms` | 6.4 |
| `AICC_OVERFLOW_TIGHTEN_RATE` / `AICC_OVERFLOW_STOP_RATE` | `1%` / `3%` | 6.5 |
| `AICC_TRUNK_BREAKER_COUNT` / `AICC_PROVIDER_BREAKER_COUNT` | `20` / `5` | 7.4 |

**Metrics:**

- `campaign_attempts_total{outcome}`
- `dialer_in_flight{kind=ringing|talking}`
- `dialer_admission_blocked_total{reason}`
- `bot_slots{state=in_use|reserved}`
- `did_outbound_in_use{did}`
- `campaign_answer_rate_ucb{campaign}`, `campaign_overflow_total`, `outbound_overflow_hold_seconds`
- `outbound_first_audio_seconds{stage}`

**SPA (in phase 1):**

- campaign list; create and edit (the same field locks as the API); start / pause / resume /
  cancel;
- contact import from CSV or Excel: parsed in the browser, sent to the import API, the per-row
  result shown;
- live progress (SSE `CAMPAIGN_UPDATED`, including `activity` and `nextDialAt`);
- leads: filtered by intent, a `HIGH` lead linked to its callback (#188). The reader decision is
  the second of the issue's two options: the lead row links to the call page
  (`/admin/cdr/$callId`, ADMIN and SUPERVISOR, `getCDR` on `history:read:all`) by `lastCallId`, only
  when `lastOutcome` says the call rang (not `DNC_SKIPPED`, `INVALID_NUMBER`, `ORIGINATE_FAILED` or
  `UNKNOWN`), and shows the callback's status as text, resolved with `GET /callbacks?callId=`. The
  callbacks screen keeps its `AGENT` guard because it is an agent's work queue (claim, complete,
  release), not because of the API: `listCallbacks` is `calls:read:own` from role `NONE` and filters
  nothing by caller, so a supervisor's session and a key with `campaigns:read` + `calls:read:own`
  read the same rows.
  - **A retry after a lead.** The link follows the latest attempt; the lead is kept from the latest
    call that recorded one (§11). After a first call that recorded `HIGH` and a retry that recorded
    `LOW`, the contact is `LOW` and links to the second call; the callback the first call filed is
    left as it is, neither removed nor filed again, because callbacks are filed once per call and
    never withdrawn. A retry can follow a lead only when the first attempt was not seen to be
    answered: reconciliation finished it `UNKNOWN` with no CDR (§8.2).
  - **Known gap.** Such an `UNKNOWN`-reconciled attempt that recorded a lead can be redialled, and
    when the retry records no lead the contact keeps `HIGH` while `lastCallId` names the retry, so a
    `HIGH` row can lose its callback link until that is fixed (#264).
- DNC list: view, add, remove;
- the DID form gains `maxOutboundChannels`.

Nothing the contract does not offer.

---

## 15. Prerequisites and decisions

**Prerequisites** (done before campaigns, each its own issue; the numbers are in §17):

- **P0, #62 (done):** campaigns make `DialAI` the main outbound path, and its INVITE carries no
  `X-FSBC-Out`. Approach decided (2026-10-11, option 2): `AICC_OUTBOUND_SIP_HEADERS`
  (`Name=value;…`), sent as `sip_h_<Name>` on the `DialAI` originate only; `AICC_OUTBOUND_ENDPOINT`
  must be `sofia/…` with exactly one `%s`, checked at startup; strict parsing, a `;` in a value fails
  at startup. Every campaign attempt and retry goes through `DialAI` and so carries the headers;
  re-arming after a restart and an overflow hang-up send no new INVITE. Nothing more is needed on
  the campaign side.
- **P1, #157. Registry adoption of live channels (§8.2):** also closes the single-call gap where a call
  answered after a restart is silence (F14).
- **P2, #156. Consume `BACKGROUND_JOB` (§5.5):** single calls gain a cause for pre-channel failures too.
- **P3, #158. Bot slot reserved after answer, before the bridge (§6.4):** also closes the single-call gap
  where a call answered while the bots are full is silence.
- **P5, #159. `isDialable` accepts a leading `+` (§9.1).**
- **P6, #160. `aicc_outbound.lua` sets and exports `aicc_did` on the outbound leg (§6.6).**
  Built: the script stamps and appends to `export_vars` before its early return; `telephony.Normalize`
  exposes the leg's value as `SwitchEvent.LegDID` and keeps it out of `Bot.DID` on an observed trunk
  leg (outbound, not the bot leg, not `DIAL_AI`), so a human call's CDR from/to/DID is unchanged.
  Needed only if the per-DID line limit is in phase 1; cuttable.
- **P4, #155. Single-call ring timeout below `AnswerWindow` (§2).**

**Decided by the owner, 2026-10-11:**

- The trunk expects E.164 and accepts national form; campaigns dial `+` E.164 (§9.1).
- Consume `BACKGROUND_JOB`; `ORIGINATE_FAILED` carries the real cause (§5.5).
- On restart the registry adopts the calls still live in FreeSWITCH (§8.2).
- `record_lead` is a built-in tool callable at any time (§11).
- Bot resources are counted in concurrency — a hard ceiling for cloud providers and self-hosted
  gateways alike — so phase 1 over-dials by expected answers (§6).
- An answered number is never redialled: `ANSWERED`, `BOT_FAILED`, `OVERFLOW` (§7.2). A voicemail
  answer therefore ends the contact in phase 1; bot-side machine detection comes later (§13.2).
- The intent enum is fixed and only `HIGH` files a callback in phase 1; configurable later (§11).
- Outer calling hours that a campaign cannot override (§8.1).
- Cross-campaign frequency: a number is reached by at most one campaign per 24 h (§5.2).
- Each DID has an outbound line limit; inbound and outbound lines are counted separately and never
  take from each other; counted as ringing + talking; every path AICC places calls through is bound
  by it (§6.6).
- Overflow hangs up with no announcement (§6.4).
- A do-not-call hit on a single `createCall` is 422 `NUMBER_ON_DNC` with no CDR (§12).
- The calling window's time zone is per contact, falling back to the campaign's (§8.1).
- AI outbound stays on ESL originate (`DialAI`), not on the dialplan (§2).
- Phase 1 includes the SPA, campaign creation and import included (§14).
- A running campaign may change only `maxConcurrent`, `callingWindow` and `retryPolicy` (§14).
- No caller-id rotation; if ever needed, another service provides it and AICC integrates (§1).
- AI disclosure is out of this task and belongs in the bot's script (§1).

---

## 16. Verification

#89's matrix (*Test data and verification*) applies, with two amendments:

- the restart case is checked against the guarantees of §8.2, not "no attempt lost";
- the agent-campaign cases are dropped.

Added by this design:

- **Outcomes:** the cause → outcome table, one test per row and one for an unmapped cause.
- **No redial after answer:** every answered outcome — a hang-up within a second, `BOT_FAILED`,
  `OVERFLOW`, no lead recorded — leaves the contact `DONE` with no further attempt.
- **Budgets:** `TRANSIENT` / `ORIGINATE_FAILED` do not consume `maxAttempts`; reaching
  `maxSystemRetries` makes the contact `EXHAUSTED`; a retry falling outside the window moves to the
  next window's start, including across midnight and on a daylight-saving changeover day.
- **Breakers:** the trunk breaker trips on the `N`th failure and not the `N − 1`th; the provider
  breaker trips on the `M`th consecutive `BOT_FAILED`.
- **Synchronous `DialAI` errors** map as in §5.3; a voided attempt leaves no row and no count.
- **`BACKGROUND_JOB`:** a pre-channel `-ERR <CAUSE>` finishes the attempt at once with that cause;
  when a channel existed only the CDR finishes it, and the job result causes no second finish.
- **Restart adoption:** a call ringing at the restart reaches the bot when answered; a leg answered
  during the restart is hung up as `BOT_FAILED`; a leg bridged to a bot is hung up as
  `BOT_FAILED`; no channel and no CDR gives `UNKNOWN`; nothing is redialled at once.
- **Over-dial:** with too few samples `p̂ = 1` and a fake dialer that answers every call produces no
  overflow; at a known answer rate (20 %) bot utilisation is well above the no-over-dial rule and
  overflow stays at or below 1 %; above 3 % the campaign falls back to `p̂ = 1` and recovers below
  1 %.
- **Slot reservation:** the UAS consumes the reservation for its `callId` and does not compete with
  inbound; a slot freed within `overflowHold` is bridged; otherwise the leg is hung up with no
  announcement as `OVERFLOW`.
- **Inbound first:** with inbound holding everything outside `inboundReserve`, campaigns admit
  nothing new and no inbound call is refused.
- **DID lines:** with `max_outbound_channels = 3`, outbound ringing + talking on that DID never
  exceeds 3, also when two campaigns and single calls share it; inbound does not affect outbound
  admission; originates sent in the same instant do not slip past the limit; single calls and
  click-to-dial over the limit get 409 `DID_CHANNELS_EXHAUSTED`; a reservation is released on
  originate failure, on an unanswered hang-up and on an answered hang-up, with no leak.
- **`aicc_outbound.lua`:** the trunk leg of a click-to-dial and of a hand-dialled call carries
  `aicc_did`, also on the early-return path for a recording already started; a hand-dialled call
  is counted in `didOutboundInUse`; a click-to-dial is not counted twice.
- **Cross-campaign frequency:** after A reaches a number, B does not dial it for 24 h and its
  `next_attempt_at` moves to the touch + 24 h; an `ORIGINATE_FAILED` in A does not hold B back; two
  campaigns admitting one number at once — exactly one succeeds (partial unique index).
- **Outer hours:** a `calling_window` outside them is refused on create and on change; narrowing
  the outer hours still binds an existing campaign at admission.
- **PATCH while running:** a locked field is refused; a new `maxConcurrent` affects only later
  admissions.
- **Ring timeout** at or above `AnswerWindow` is refused.
- **Leads:** `DO_NOT_CALL` and `HIGH` write their DNC and callback rows in the attempt's
  transaction, and nothing when the lead write fails; `record_lead` `HIGH` then `MEDIUM` leaves
  `MEDIUM` and no callback; `MEDIUM` then `HIGH` files exactly one callback; a campaign call is not
  offered `take_message` or `transfer_to_agent`.
- **Activity:** with every contact outside its window, `activity` is `WAITING_WINDOW` and the
  status stays `RUNNING`.

---

## 17. Delivery slices

Added when the draft was committed (#154). Every slice and prerequisite is its own issue on
Project #3 and links to the section it implements. The prerequisites are each useful without
campaigns; none needs one.

**Prerequisites** (§15):

| ID | Issue | What | Section |
|---|---|---|---|
| P0 | #62 | Outbound SIP headers on the `DialAI` INVITE (done) | §15 |
| P1 | #157 | Registry adoption of live channels on start and reconnect | §8.2 |
| P2 | #156 | Consume `BACKGROUND_JOB`; originate failures get a cause | §5.5 |
| P3 | #158 | Bot slot reserved on answer, before the bridge | §6.4 |
| P4 | #155 | Single-call ring timeout below `AnswerWindow` | §2 |
| P5 | #159 | `isDialable` accepts a leading `+` | §9.1 |
| P6 | #160 | `aicc_outbound.lua` stamps `aicc_did`; cuttable, needed only for the DID line limit | §6.6 |

**Slices:**

| Slice | Issue | Content | Depends on |
|---|---|---|---|
| S1 | #162 | DNC list (`dnc_numbers`, `/dnc-numbers`) and the check in `outbound.Service` for `DialAI` and click-to-dial; 422 `NUMBER_ON_DNC`; E.164 canonicalisation helper | P5 |
| S2 | #163 | Campaign and contact model, migrations, API, import (§3, §9, §14) | S1 (canonicaliser, `DNC` contact status) |
| S3 | #164 | Dialer core: actor, admission, write-ahead attempts, finish routine in the CDR transaction, outcomes and retries, breakers, calling hours (§4-§5, §7, §8.1) | S2, P0, P1, P2, P4 |
| S4 | #165 | Capacity: expected-answer admission, `inboundReserve`, overflow control, DID line limit (§6) | S3, P3, P6 (DID line part only) |
| S5 | #166 | `record_lead`, leads, DNC-by-conversation, callback on `HIGH` (§10, §11) | S2, S3, S1 |
| S6 | #167 | Reporting: SSE `CAMPAIGN_UPDATED`, CDR `campaignId`, webhook filter, metrics, SPA screens (§14) | S2-S5 |

S5 and S6 can run in parallel once S3 has landed. #158 (P3) is built as its own change rather
than folding into S4.

Follow-up outside the campaign work: #168, sending an inbound AI call whose bot session went
with a restart to the DID's fallback queue instead of hanging it up (§8.2 hangs it up; owner,
2026-10-12).
