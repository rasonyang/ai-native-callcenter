# AI Outbound Campaigns — Design

Date: 2026-10-11, committed 2026-10-12 · Status: **built** — phase 1 shipped as slices S1–S6
(§17): `internal/campaign`, `internal/dialer`, migrations 00036–00040/00042/00045, the contract operations
(`/campaigns*`, `/dnc-numbers`, `CAMPAIGN_UPDATED`), the SPA screens, and the metrics with the
`aicc_` prefix the rest of the instrument set uses. The design body below is the record of why;
where it says "sketch" or "final shapes come with the PR", the contract is now the answer. The
facts in §0 were re-verified against `main` at `83e26a3`, so their line numbers are pinned to that
commit and will not resolve on today's tree · Issue:
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
   The campaign's value always applies: `AICC_OUTBOUND_RING_TIMEOUT` is only the default of a call
   placed without a ring timeout of its own (a single AI call, a click-to-dial), and the dialer
   always sends one. The `ai outbound placed` log line carries the value sent (`ringTimeoutSec`).
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
| `retry_at` | the time the retry policy gave it: the last call's end plus the outcome's interval, before the calling window moved it into `next_attempt_at` (§7.3), which is never earlier. Written by the settle, NULL'd by the claim and by a void, left alone by every deferral (a calling window's, the frequency hold's, the daily cap's). NULL means no policy time (never tried, voided, in flight): due as soon as a window allows. NULL unless `ACTIVE`. The re-arm after a window change starts from it (§8.1). Migration 00050 gave each waiting contact with a history its due time at the upgrade, never earlier than its policy's. Not an API field |
| `is_frequency_held` | true while `next_attempt_at` is where the cross-campaign frequency hold put it (§5.2); set only by that deferral and cleared by every other write of `next_attempt_at`. Read by the re-arm when the number is put on the list of test numbers (§3.4a) |
| `is_daily_cap_held` | the same for the daily dial cap (§5.2): true while `next_attempt_at` is where the cap put it. At most one of the two marks is set (a CHECK); each is cleared by every write of `next_attempt_at` that is not its own deferral, and the re-arm lifts either |
| `last_outcome` | from the latest attempt |
| `lead_intent`, `lead_notes` | the recorded lead (§11); NULL means never recorded. A contact with one is never dialled again (§7.2) |
| `last_attempt_id`, `last_call_id` | the latest finished attempt and its call; NULL before the first attempt finishes. Written with `last_outcome` when the latest attempt settles, so they follow every attempt; once a call records a lead there is no later attempt (§7.2), so the pair names that call (§14). Not foreign keys, like `call_id` everywhere; migration 00042 backfilled them from the latest `FINISHED` attempt by `started_at` |

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
| `sip_status` | the far end's final SIP status, from the CDR's `tech.sipStatus`; NULL when the switch reported none (a job result never has one, and attempts finished before migration 00052 have none). Tells a line's refusal from a callee's 603 under `CALL_REJECTED` (§7.2) |
| `is_touched` | true when the outcome counts against `maxAttempts` (§7.2), i.e. the customer was reached; read by the frequency check |
| `is_test_number` | true when the number was on the list of test numbers (§3.4a) at the moment the row was written. History: delisting the number later does not change it. Lets the outcome summary leave test calls out (§14) |
| `lead_intent`, `lead_notes` | what this call recorded |
| `started_at`, `finished_at` | |

A partial unique index `ON campaign_attempts (number_e164) WHERE state = 'DIALING'` (named `uq_campaign_attempts_dialing_number_e164`) makes "one
attempt in flight per number, across all campaigns" a database fact, not a scheduler habit. It
covers "one attempt in flight per contact" too.

### 3.4 `dnc_numbers`

Global, not per campaign: `number_e164` (PK), `source` (`MANUAL \| IMPORT \| CONVERSATION`),
`call_id` (nullable, set for `CONVERSATION`), `created_at`, `created_by`.

### 3.4a `test_numbers`

The numbers a team tests its campaigns on, exempt from the cross-campaign frequency window and
the daily dial cap (§5.2) and from nothing else: `number_e164` (PK, canonical, the same CHECK as `dnc_numbers`),
`note` (optional, at most 200 characters), `created_at`, `created_by` (null when an API key added
it). Modelled on `dnc_numbers`: global, `/test-numbers`, idempotent add, canonicalised with the
deployment's default country code. Every entry gives up a protection that exists for customers, so
the list holds at most 100 numbers (`store.MaxTestNumbers`): a new number on a full list is
refused `409 TEST_NUMBER_LIMIT_REACHED`, a number already listed is not an error. Adds are
serialised by a transaction-scoped advisory lock taken before the size check, so two adds racing
for the last place cannot both land.

The exemption is this platform's alone. A carrier line's own limits on calls to one callee (a cap
per callee per day or hour) still apply to a test number unless the carrier exempts it too
(`deploy/carrier.md`): on a line that refuses the 4th call of the day, a test number's 4th dial is
sent and refused.

### 3.5 New columns on existing tables

- `cdrs.campaign_id`, nullable.
- `cdrs.to_number_e164`, nullable: the canonical callee (`phonenum.Canonical` with the deployment's
  default country code) of an `OUTBOUND` row that has one, written by the ledger on insert and on
  a revision; NULL for every other call and for a dialled string that does not canonicalise.
  `cdrs.to_number` is the string that was dialled, national or E.164, so the daily dial cap (§5.2)
  counts ad-hoc calls by this column. Not backfilled: ad-hoc dials made before migration 00049 are
  not counted. Not an API field.
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
  `WAITING_FREQUENCY` (nothing is in flight, and the earliest pending contact is held by another
  campaign's call within the frequency window, §5.2; the campaign has made no attempt for it),
  `WAITING_DAILY_CAP` (nothing is in flight, and the earliest pending contact is held by the
  daily dial cap, §5.2; the campaign has made no attempt for it),
  `AT_CAPACITY` (held back by a cap in §6), plus `nextDialAt`. When a closed calling window,
  the frequency hold and the daily cap apply together, the activity names whichever ends latest
  (the hold end, last touch + window, against the window's next opening, against the cap's
  release time); a tie goes to `WAITING_DAILY_CAP`, then `WAITING_FREQUENCY`, the reasons an
  operator can act on. The cap is judged from the facts (the number's dials today against the
  cap), not from the contact's mark: after the contact's local midnight the mark may still be set
  while the cap no longer binds. A window of 0 never yields it. A contact's own retry time later
  than both is `WAITING_RETRY`. The UI shows
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
                    AND a.started_at > now() - $frequency_window
                    AND NOT EXISTS (SELECT 1 FROM test_numbers t   -- except a test number
                                    WHERE t.number_e164 = c.number_e164))
ORDER BY c.next_attempt_at, c.id
LIMIT $2
```

The calling-window check (§8.1) depends on each contact's time zone, so it runs in Go per contact;
the query over-fetches a small multiple of the free capacity and the actor filters.
The daily dial cap below is judged in Go too, after the window and in one batched query, because its
day is the contact's local day.

**Cross-campaign frequency.** Once any campaign has **reached** a number (an outcome that counts
against `maxAttempts`, §7.2, marked `is_touched` on the attempt), no other campaign may dial it
within `frequencyWindow` (deployment configuration, default 24 h). A contact held back this way is
not dropped: its `next_attempt_at` moves to "last touch + `frequencyWindow`", and on to the next
window if that falls outside one. Retries within the same campaign follow its `retry_policy`. A
single `createCall` is not subject to this rule.

**Test numbers.** A number on the list of test numbers (§3.4a) is exempt from this check, and only
from this check: it is still never dialled when on the do-not-call list, still skipped while any
attempt to it is in flight, still held to the calling window and `AICC_OUTBOUND_HOURS`, and its
campaign's own `maxAttempts` and retry policy are unchanged. Putting a number on the list makes
the contacts that were waiting out the window for it due again at once (the dialer is told, as for
a do-not-call add; the re-arm is also run when the dialer starts and when a campaign starts or
resumes, so a listing made while the campaign was paused, or lost to a restart, is not forgotten):
the contacts of `RUNNING` campaigns with that number that the hold itself
deferred. The hold marks what it defers (`campaign_contacts.is_frequency_held`, set only where
`HeldByFrequency`'s deferral moves the due time and cleared by every other write of it: the claim,
the settle, a calling-window deferral, a re-arm); a contact waiting on its own retry policy carries
no mark and keeps its time, however it compares with the other campaign's touch. Taking the number off the list
holds it again from then on, by the touches inside the window. The contact listing carries
`frequencyHoldEndsAt`, computed at read time from the same facts (an ACTIVE contact, another
campaign's touch inside the deployment's window, number not listed) and only while the hold binds:
the contact is marked, or its due time is not later than the hold's end (a retry time past it is
the campaign's own and is shown as such): "held by another campaign's
call until …".

**Daily dial cap.** A deployment-wide cap (`AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY`, default
3, `0` off, range 0 to 100) on dials to one callee number per local day: the 4th dial of the day is
not sent. It is the deployment's, never a campaign's: the cap belongs to the carrier line.

- *What counts* for number N on the contact's local day, from the day's start to now: every
  `campaign_attempts` row for N, from any campaign, that is in flight (`DIALING`) or whose
  outcome spends a budget (`campaign.ClassOf` is not `ClassNone`, so not `DNC_SKIPPED` and not
  `INVALID_NUMBER`), `ORIGINATE_FAILED` and `TRANSIENT` included (the dialer cannot tell whether
  the carrier passed the INVITE on to the phone, and counting is the safe side), except
  `LINE_REJECTED` (§7.2): the line answered the INVITE itself, so the call certainly never reached
  a phone (owner's decision, #702). The rule is `campaign.CountsTowardsDailyCap`. A voided attempt
  is a deleted row and
  never counts. And every ad-hoc call: a `cdrs` row with `call_type = 'OUTBOUND'`, no `campaign_id`
  and `to_number_e164 = N`. A campaign call's CDR carries its campaign, so it is not counted twice.
  A number on the list of test numbers is not counted at all (the query leaves it out).
- *The day* is the contact's local calendar day in its effective zone: the zone of the schedule
  `scheduleFor` returns (the contact's own, else the campaign's), UTC when there is none
  (`Schedule.DayStart`).
- *Admission.* After the calling-window judgement of a due batch, one batched query
  (`DialsTodayByNumber`, the numbers and each one's day start as parallel arrays) counts the
  in-window contacts. Those at or over the cap are deferred to their release time and marked
  `is_daily_cap_held` (`DeferContacts`, hold kind `DAILY_CAP`); the rest are ready, and the refill
  loop goes on as after a window deferral. No attempt is written: `attempts` and
  `system_failures` do not move (§7). Begin and the claim are unchanged; two campaigns racing for
  the same number are still serialised by the one-`DIALING`-per-number index, and the loser is
  counted again on its next pass.
- *Release time* (`Schedule.DailyCapRelease`): the start of the contact's calling window on the
  next local day, that is the next local midnight moved to the schedule's first opening from there
  (the next local midnight itself when there is no window or no opening).
- *Test numbers.* A number on the list is exempt exactly where the frequency check exempts it. The
  re-arm run for a number that joins the list (`RearmHeldContacts`) lifts a contact carrying either
  mark, and only a marked one, never by inferring from attempt times; it runs when
  `AICC_CAMPAIGN_FREQUENCY_WINDOW` or the cap is on. Taking a number off the list needs nothing:
  the next admission counts today's dials and holds it until the next day's window. The carrier's
  own limit still applies to a test number's 4th dial.
- *Activity.* `WAITING_DAILY_CAP` with `nextDialAt` the release time, as §4.1 says. Like the
  frequency hold it binds only when the contact's own `next_attempt_at` is not later than the
  release time (one second of slack). A contact whose own retry time falls before the release is
  deferred to the release (with the cap's mark) by the dialer as soon as it sees it capped, in
  the pass that reports the activity and not when the retry time comes, so `nextDialAt` is the
  release time from the first report (at most 20 contacts per pass; the rest follow on the next
  pass). A retry time after the release is never moved.
- Dials made before migration 00049 as ad-hoc calls are not counted; campaign attempts always
  were.

### 5.3 Write ahead, then dial

For each admitted contact, one transaction: mint `callId` and `legId`, insert the attempt as
`DIALING`, clear `next_attempt_at`. The counters (`attempts`, `system_failures`) are updated when
the attempt finishes (§5.4), because only then is it known whether the customer was reached.
Commit, then call `DialAI` with:

- `CallID` and the customer leg id: the `callId` and `legId` just minted;
- the number to dial, in the trunk's form (`AICC_OUTBOUND_DIAL_FORM`), and the stored canonical
  E.164 number with `+` (§9.1) as `CanonicalTo`, which the do-not-call check uses (§12);
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
  After a restart that memory is gone, so `noteChannel` ends one more kind of leg the same way
  (`ALLOTTED_TIMEOUT`, at its `CHANNEL_CREATE`): a created leg stamped `aicc_placed_by=DIAL_AI` that
  this process neither placed (`legJobs`), nor holds armed (`pending`, which is also what a leg the
  cold-start or reconnect listing re-armed, and an unconfirmed placement the window adopted, sit
  in), nor remembers as late. Nothing would bridge it, so a customer answering it would hear
  silence (#573). The same rule ends, in a running process, a late leg whose `CHANNEL_CREATE` comes
  after its 10 minutes of memory, which used to be left parked. The attempt is not consulted: a leg
  the listing did re-arm is still answered and bridged, and its real CDR revises the attempt as
  before (§7, lead intent).
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
- **A bot leg that is refused or fails before media** (the bot's UAS answers 503/486/480, the
  gateway is down, or the leg answers and `startMedia` fails). The bridge carries an inline
  continuation (design 02 section 1): a campaign call is hung up with the switch's cause and
  **no prompt**, never sent to a queue. For the pre-media failure `OnCallFailed` runs `rescue`,
  which only stamps the customer's channel and ends the bot leg, so `hangup_after_bridge` hangs the
  customer up. Either way the CDR carries `userData.botReason=BOT_LEG_FAILED` beside the switch's
  `hangupCause`, and the attempt is `BOT_FAILED`. A leg that answered and never started leaves the
  row to the human path (#334), whose `hangupCause` is then `BOT_START_FAILED` when the customer was
  not moved to a queue; a lost ACK, where no rescue runs and nothing is stamped, ends the same way,
  and the attempt is `BOT_FAILED` on that cause too.

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

**As built (#604): a fifth limit, the provider's rate.** The first bullet's premise, that a cloud
provider's quota is counted in concurrent sessions, does not hold for the providers documented so
far: Qwen-Audio-Realtime limits connection establishments and tokens per minute and states no limit
on sessions held at once, and doubao limits `session.create` and tokens per minute
([06-capacity §4](06-capacity.md)). `AICC_BOT_MAX_CALLS` still bounds the sessions this process
runs; what the provider takes is a rate, and every answered call opens one session at the answer.
The provider's per-minute quota (`AICC_PROVIDER_SESSION_RPM`, `AICC_PROVIDER_TOKEN_RPM`, defaulting
to the profile's documented numbers) is a fifth limit, applied in §6.3. It is shared with inbound
the way bot concurrency is: inbound sessions spend it and are never held for it.

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
  `LINE_REJECTED`, `ORIGINATE_FAILED`, `INVALID_NUMBER` and `DNC_SKIPPED` never reached a phone and
  `UNKNOWN` left no trace, so none is a sample.
- After a restart the window is rebuilt from the campaign's last 600 finished attempts, then kept by
  each finish; no admission reads the database.

**As built (#604): the provider's rate.** After the four conditions above, admission applies the
provider's quota per minute (`internal/dialer/provider.go`, `providerHold`):

```
and now ≥ lastQuotaRefusal + 60 s
and starts60 + n ≤ SessionRPM                                  -- when SessionRPM > 0
and tokens60 + n × tokens60 / max(1, botSessionsInUse) ≤ TokenRPM   -- when TokenRPM > 0
where n = pendingAnswered + Σ_campaigns (p̂_c × ringing_c) + 1
```

- `starts60` and `tokens60` are the last 60 s of the process's rate meter
  (`internal/provider/ratemeter`): every provider session start, counted before it dials, and every
  response's reported tokens, inbound sessions included. An event counts while `now < t + 60 s`.
  `lastQuotaRefusal` is the latest time the provider said its quota was spent: a refused start
  (`provider.ErrQuotaExceeded`: an HTTP 429, or vendor text that says quota, rate limit or
  throttling) or such an error mid-call, from any call.
- `Σ p̂_c × ringing_c` is the expected-answer sum the bot rule already computes, because each answer
  opens a session. `pendingAnswered` is the answered legs holding a reserved slot or waiting in the
  overflow hold (`voice.Usage.Reserved + Waiting`): answered, their session not yet started, so in
  neither the meter nor the sum — the gap the bot rule's `Waiting` term closes, closed here too.
- `tokens60 / max(1, botSessionsInUse)` is what a session in use cost over the last minute, so the
  token condition asks whether the minute ahead, the sessions running plus the ones about to open,
  fits. With no tokens counted yet it admits; the session condition is what guards a cold burst, and
  the refusal backoff what bounds an estimate that ran short while sessions were younger than a
  minute.
- The backoff applies whenever the meter is wired, whatever the limits, 0 included: a refusal is
  the provider's word, not an estimate. A limit of 0 (the profile has no documented number, or the
  deployment set it off) turns its condition off.
- A call held for one of these is counted as `aicc_dialer_admission_blocked_total{reason}` with
  `PROVIDER_SESSION_RATE`, `PROVIDER_TOKEN_RATE` or `PROVIDER_QUOTA_BACKOFF`; the campaign's activity
  is `AT_CAPACITY`, as for every other limit (no contract change). The dialer logs the rule starting
  to hold, changing reason and letting calls through again, once per change. A hold that ends by
  time alone (the refusal's minute, the oldest start or token leaving the window) asks for a pass at
  that moment, because no event may arrive to wake the dialer then.
- Inbound calls are never held. A quota refusal is still a `BOT_FAILED` outcome and still counts
  towards the provider breaker (§7.4); what changed is that admission stops feeding it.
- The capacity simulation has a provider with a per-minute quota (`internal/dialer/provider_test.go`):
  a connect-rate case and a token-rate case, each run with and without the rule, the rule keeping
  refusals near zero (a run of answers above `p̂_c` can still put one start over) while the rate is
  used.

**As built (#644): a limit is a count over the vendor's period.** A vendor can state its quota over a
period shorter than a minute (3 session starts per 1 s and 60,000 tokens per 12 s, read on one
deployment after a quota raise). Each limit is now a count and a period (`AICC_PROVIDER_SESSION_LIMIT`
/ `_PERIOD`, `AICC_PROVIDER_TOKEN_LIMIT` / `_PERIOD`; the `_RPM` settings are the same count over
60 s), and the rule above becomes:

```
and now ≥ lastQuotaRefusal + backoff
and starts_W + n ≤ SessionLimit × W / SessionPeriod            -- when SessionLimit > 0
and tokens_T + n × tokens_T / max(1, botSessionsInUse) ≤ TokenLimit   -- when TokenLimit > 0
where n = pendingAnswered + startsWaiting + Σ_campaigns (p̂_c × ringing_c) + 1
      W = max(SessionPeriod, 10 s), T = TokenPeriod
      backoff = the longer period of the limits in force; 60 s when neither is
```

- The meter counts starts over `W` and tokens over `T`, and the drop times that wake the dialer
  follow them. A per-minute limit (the profiles' defaults, the `_RPM` settings) gives `W = T = 60 s`
  and a 60 s backoff: the rule of #604 unchanged.
- `W` is not shorter than 10 s (`ratemeter.MinAdmissionWindow`) because `n` counts every ringing
  leg's expected answer as a start to come, and those answers land over the following seconds, not
  at once. Over a 1 s window the rule would have to hold all of them inside one second and admits
  almost nothing (5 starts a minute at 3 per 1 s in the simulation); over a minute it places waves of
  answers one second cannot take (30 % of starts refused). Tokens keep their own period: they are
  spent all along by the sessions in progress.
- **The start gate.** Admission paces the calls it places, but their sessions open when callees answer,
  and callees answer together. So a campaign call's session start waits, FIFO among campaign calls,
  for room in the last `SessionPeriod` (fewer than `SessionLimit` starts in it), at most 2 s
  (`ratemeter.StartWaitBound`); past the bound it is made as before and the provider decides. A
  start is counted when it is made, in order; a call that ends while it waits leaves the queue
  without counting one. Inbound and internal calls, and AI calls placed other than by a campaign,
  are counted and never wait.
  `startsWaiting` above is the starts in that queue. The wait sits in the orchestrator just before
  the session is made (`aicall.Orchestrator.startSession`), on the call's own goroutine; the queue
  is the meter's and holds no lock while it sleeps. `aicc_provider_session_start_wait_ms{outcome}`
  records each campaign start's wait (`ROOM`, `BOUND`, `CALL_ENDED`).
- What the callee hears during the wait is the silence of an answered leg whose session is not up,
  as during the provider's setup that follows. The clocks that start at the answer run through it:
  the call's time limit (#637) and the switch's, and the opening wait of a call that listens first
  (#594), whose listener does not run yet, so a greeting said during the wait is not heard as one.
  The dead-air timer starts after the bot has spoken.
- In the simulation at 3 per 1 s and 60,000 per 12 s (bot capacity enough that the session window
  binds), admission alone leaves a share of starts refused and the start gate keeps refusals within
  the 2 % bound of the per-minute case. The rate admission reaches there is about a third of the
  quota: `n` counts each ringing leg at `p̂_c` against the 10 s window although a leg nobody answers
  rings for its whole timeout.

**As built (#640): the window between the answer and the counted start is a term of its own.**
Between the answer — when the outbound service's `onAnswer` wakes the dialer — and the moment the
call's session start is counted on the rate meter, an answered leg stood in none of the counts
either rule read: not ringing, not `Reserved`, not `Waiting`, not `InUse`, not `starts_W`. The
dialer now keeps that leg as **unstarted** (`Dialer.unstarted`, one per flight): taken as the
answer is handled, released when the orchestrator's `startSession` reports the counted start
(`aicall.OrchestratorConfig.OnProviderSessionCounted` → `Dialer.SessionCounted`) or when the leg
ends, whichever comes first (`drop`, so every ending releases it — a callee who hangs up inside
the window, a parked leg whose hold expires, a refused or failed session). A flight adopted after
a restart is never in it, because the count and the meter are both new.

Both rules use it:

- The bot rule's `botInUse` is `InUse + max(Reserved + Waiting, unstarted)`. Every reserved or
  waiting leg is an answered, uncounted dialer flight, so `unstarted` already covers them — the
  max only matters if the counter ever reports answered legs that are no flight's — and the term
  adds the windows the slot counts cannot see: before the reservation, and between the
  reservation being consumed and the start being counted, where a leg counts briefly in both
  `InUse` and `unstarted`. That one double-count is deliberate: it holds at most one extra slot
  per leg inside the window, the safe direction.
- The provider rule's `pendingAnswered` is `max(Reserved + Waiting, unstarted)` — the same legs
  the old term covered plus the two windows it could not see, including the leg whose slot is
  already taken (`InUse`) but whose session start is not yet counted.

The simulation's answer event now models the window (`startWindow`, fixed and jittered): the dialer
hears `Answered` first and the slot, the counted start and `SessionCounted` follow by the span; a
share of callees hang up inside it and release the term by ending. At 0 / 50 / 200 / 1000 ms the
connect-rate gate keeps refusals within the windowless bound over many seeds, the rate is still
used, and the term returns to the legs actually uncounted at the run's end — the leak check in
`runScenario`.

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
    These calls never reached the customer's phone (or cannot be shown to have: a carrier's 408
    or 487 with no ringing before it, §7.2) and must not use up the customer's chances, but they are
    bounded so a number that always fails is not retried forever.
- **Retry time = this call's end + the interval for its outcome.** If that falls outside the
  contact's window, it moves to the start of the next window; it is never dropped.
- **When the retry time comes, the contact is back in the pool**, queued with new contacts by
  `next_attempt_at` (§5.2).
- **A contact the daily dial cap holds spends neither budget** (§5.2): no attempt is made, so
  `attempts` and `system_failures` do not move, and its `next_attempt_at` is the next day's window
  opening. The dials that reached the cap are counted from the attempts already made, whatever
  they came to.

### 7.2 Outcomes and default policy

The outcome is derived from `hangup_cause` — the CDR's, or the job result's for a failure before
any channel (§5.5) — not from `releaseReason` alone, because `FAILED` folds a vacant number
(terminal) together with a network fault (retryable).

| outcome | typical causes | dialled again? | default interval | budget |
|---|---|---|---|---|
| `ANSWERED` | answered, any talk time | no — contact `DONE` | — | `maxAttempts` |
| `BOT_FAILED` | answered; the bot side failed before the flow ended, the bridge to it never ran (`BOT_BRIDGE_UNCONFIRMED`, §5.7), the bot leg was refused or failed before media (`userData.botReason=BOT_LEG_FAILED`, whatever the cause, §5.7), or it answered and its session never started (`hangupCause=BOT_START_FAILED`, §5.7) | no — contact `DONE` | — | `maxAttempts` |
| `OVERFLOW` | answered; no bot slot after `overflowHold` (§6.4) | no — contact `DONE` | — | `maxAttempts` |
| `NO_ANSWER` | `NO_ANSWER`, `NO_USER_RESPONSE`, our ring timeout; `RECOVERY_ON_TIMER_EXPIRE` when the far end sent a SIP 408 after the leg rang; `ORIGINATOR_CANCEL` when the far end sent a SIP 487 of its own after the leg rang (below) | yes | 2 h | `maxAttempts` |
| `BUSY` | `USER_BUSY` | yes | 30 min | `maxAttempts` |
| `REJECTED` | `CALL_REJECTED` with a SIP 603 or no status: the callee declined (below) | yes | 24 h | `maxAttempts` |
| `UNKNOWN` | reconciliation could not tell (§8.2) | yes | 24 h | `maxAttempts` |
| `TRANSIENT` | `NORMAL_TEMPORARY_FAILURE`, `NETWORK_OUT_OF_ORDER`, `RECOVERY_ON_TIMER_EXPIRE` (a 408 with no ring progress, or no 408 at all: our own recovery's reap), `SWITCH_CONGESTION`, `NORMAL_CIRCUIT_CONGESTION` | yes | 5, 15, 45 min … (×3 each time, capped at 4 h) | `maxSystemRetries` |
| `LINE_REJECTED` | `CALL_REJECTED` with any other SIP status (402, 401, 407, 403, 608): the line refused the call before it reached a phone (below) | yes | as `TRANSIENT` | `maxSystemRetries` |
| `ORIGINATE_FAILED` | failure before any channel (§5.5), e.g. `GATEWAY_DOWN` | yes | as `TRANSIENT` | `maxSystemRetries` |
| `INVALID_NUMBER` | `UNALLOCATED_NUMBER`, `INVALID_NUMBER_FORMAT`, `NUMBER_CHANGED` | no — contact `INVALID_NUMBER` | — | — |
| `DNC_SKIPPED` | on DNC at admission or inside `DialAI` | no — contact `DNC` | — | — |

- **A carrier that ends an unanswered call itself is `NO_ANSWER`, not a system failure** (#576).
  A carrier whose own ring timer is shorter than ours answers the INVITE with a final 408 while the
  customer's phone is still ringing, and the switch reports the leg's cause as
  `RECOVERY_ON_TIMER_EXPIRE`, the word recovery's reap uses too. The two are told apart by the
  CDR's `tech`: the dialled leg's `sipStatus` (the switch's `sip_term_status`) and `ringStartedAt`
  (its first 180 or 183, the earlier of `Caller-Channel-Progress-Time` and
  `Caller-Channel-Progress-Media-Time`, because a 183 with SDP sets only the second). The rule is
  `RECOVERY_ON_TIMER_EXPIRE`, a call nobody answered, `sipStatus` 408 **and** ring progress:
  `NO_ANSWER`. Ring progress alone is not enough, since our own reap (`outbound.ReconcileCause`)
  leaves no 408 and can hit a leg that is ringing. A 408 with no 180 or 183 before it stays
  `TRANSIENT`, and an answered call with this cause stays `BOT_FAILED`. `NO_ANSWER` is a customer
  outcome, so these calls also break a run of system failures for the trunk breaker (§7.4): a line
  that ends every unanswered call with 408 no longer pauses a campaign as a trunk outage.
- **A far end that ends a ringing call with a 487 of its own is `NO_ANSWER` too** (#613). Some
  networks end an unanswered INVITE with a final 487 Request Terminated instead of a 408, whatever
  ended it on their side (a ring timer, a callee who declines); when the carrier's ring timer is
  shorter than ours, ours never fires. No CANCEL is sent by either side, and the switch reports the leg's cause as
  `ORIGINATOR_CANCEL`. Our own ring timeout is `NO_ANSWER` with no `sipStatus`, so the rule is
  `ORIGINATOR_CANCEL`, a call nobody answered, `sipStatus` 487 **and** ring progress: `NO_ANSWER`,
  on `maxAttempts`, and a customer outcome for the trunk breaker. Without ring progress or without
  the 487 the cause is not a row: it is the unmapped cause below, `TRANSIENT` with the warning.
  Which side ended the leg is kept in the CDR's `tech.endedBy` (`PEER` or `PLATFORM`, mapped in
  `internal/telephony` from mod_sofia's `sip_hangup_disposition` on `CHANNEL_HANGUP_COMPLETE`; it
  can still be empty on the earlier `CHANNEL_HANGUP`). It is evidence for a person, not an input
  of the rule, so a CDR written before it was kept classifies the same.
- **A line that refuses a call is `LINE_REJECTED`, not a callee who declined** (#702). mod_sofia
  folds six final responses into the one cause `CALL_REJECTED` (`sofia_glue_sip_cause_to_freeswitch`
  in FreeSWITCH v1.11.3: 401, 402, 403, 407, 603, 608), and only the 603 Decline is the callee's.
  A carrier whose account is out of credit answers every INVITE with a 402 before any 180 or 183;
  a 401 or 407 is the line's authentication failing, a 403 the carrier refusing the call (a caller
  id it does not accept, a suspended account, screening), a 608 an intermediary refusing it by
  policy. The rule reads the CDR's `tech.sipStatus`: `CALL_REJECTED` on a call nobody answered
  with a status other than 603 is `LINE_REJECTED`, whether or not the leg rang; a 603, or no status
  at all, is `REJECTED`. A placement failure carries no status, so a `CALL_REJECTED` from a job
  result stays `REJECTED` under the customer-side rule. `LINE_REJECTED` is paid for as `TRANSIENT`:
  it spends `maxSystemRetries` on `transientBackoff`, never `maxAttempts`; `is_touched` is false,
  so it does not hold the number from other campaigns (§5.2); unlike `TRANSIENT` it is no dial of
  the number for the daily dial cap (§5.2), because it certainly never reached a phone; it is a
  system outcome for the trunk
  breaker (§7.4), so a line that refuses every call pauses the campaign; and it is in the default
  `retryOutcomes`. Migration 00052 gave every campaign whose policy retried `TRANSIENT` the value
  too, so a running campaign does not end a contact `EXHAUSTED` on its first refusal; rows recorded
  `REJECTED` before it are not rewritten. A 603 is `REJECTED` whether or not the leg rang: some
  carriers may answer their own blocks with a 603, but calling a decline a line fault redials,
  soon, someone who has just said no; `aicc_campaign_line_rejected_total{sip_status}` counts the
  refusals so real traffic can decide later. A 403 for one number (a per-number block) arrives
  scattered and does not trip the breaker, which needs a run; it spends only the bounded system
  retries. The attempt keeps the status (`campaign_attempts.sip_status`, `CampaignAttempt.sipStatus`,
  NULL when the switch reported none), so the console shows `CALL_REJECTED · 402` and a 603 row
  its 603. 607 Unwanted is not under this cause: the switch reports it as `UNWANTED`, an unmapped
  cause, `TRANSIENT` with the warning.
- Which outcomes retry, their intervals and both budgets are the campaign's `retry_policy`; the
  table holds the defaults. The answered outcomes are not configurable.
- **An answered number is never dialled again (owner's decision).** A customer who has picked up
  is not called back by the dialer whatever happened — the alternative is harassment complaints.
  This covers every answered outcome: `ANSWERED` (including a hang-up within a second, a voicemail,
  a call where `record_lead` was never called), `BOT_FAILED` and `OVERFLOW`. The price is that a
  bot failure or an overflow loses the lead outright, which is why the overflow control (§6.5) and
  the provider breaker (§7.4) are required. `talk_sec` and `lead_intent` (possibly NULL) are kept
  for reporting.
- **An attempt that recorded a lead settles its contact as answered, whatever its outcome**
  (#264). A recorded `lead_intent` of any value, `NONE` included, proves the customer talked to the
  bot even when the call's own outcome is lost (an `UNKNOWN` left by a restart, §8.2): the contact
  is `DONE`, or `DNC` for `DO_NOT_CALL`, and has no next attempt, with every outcome and however
  much of the budget is left. The rule is in `Settle`, so every finish path applies it. Counters
  are unchanged: an `UNKNOWN` still spends one of `maxAttempts`.
- `UNKNOWN` counts against `maxAttempts` with a 24 h interval: the call may have rung or even been
  answered, so one call too few beats one too many. An `UNKNOWN` attempt that recorded a lead is
  not retried (above).
- The cause → outcome mapping is a table in code with a test per cause. An unmapped cause becomes
  `TRANSIENT`, logs a WARN and increments a metric, so new causes surface; it stays bounded by
  `maxSystemRetries`. The WARN carries the `callId`, the far end's `sipStatus`, whether the leg
  rang (`hasRingProgress`) and `endedBy`, so one line says whose ending it was.

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

- **Trunk breaker:** `N` consecutive attempts in a campaign (default 20) ending `TRANSIENT`,
  `LINE_REJECTED` or `ORIGINATE_FAILED` → `PAUSED / TRUNK_FAILURE`. Without it, a trunk outage
  spends the whole list's system retries in minutes. A line that refuses every call (a carrier's
  402 for an account out of credit, §7.2) is such an outage, and `N` refusals in a row pause the
  campaign; `N` callees who decline with a 603 are `REJECTED` and never do. A far end that ends
  unanswered calls itself after ringing, with a 408 or a 487 of its own (§7.2), is not an outage:
  those calls are `NO_ANSWER` and break the run like any unanswered call. An unmapped cause still
  counts toward it as `TRANSIENT`. A revision that turns an attempt into a system outcome from a
  customer one (the job's `-ERR CALL_REJECTED` recorded `REJECTED`, then the channel's own record
  with its 402 revising it to `LINE_REJECTED`) is judged by the breaker as well, though it is not
  counted or sampled a second time.
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
- **A changed window is applied to the contacts already waiting (#616).** A contact's
  `next_attempt_at` may be where a window put it (the settle moving a retry into the window, or
  admission deferring a due contact to the next opening), so a window widened after it closed must
  judge them again, or a campaign whose window was extended at 17:35 waits for the next morning.
  The dialer re-arms a campaign when a PATCH changes its window while it is `RUNNING` (a window
  sent again in another order normalises to the same one and is no change), when it starts or
  resumes (so a window changed while `PAUSED` takes effect on resume), and for every `RUNNING`
  campaign when the dialer starts (so a changed `AICC_OUTBOUND_HOURS` takes effect after the
  restart). The re-arm takes each `ACTIVE` contact due later than now that carries neither hold
  mark, and recomputes its due time in Go, in the contact's own zone, with the schedule admission
  uses: the later of now and its `retry_at`, moved to the schedule's next opening, as the settle
  moves a retry. So no retry interval is shortened, nobody is due inside a closed window, and
  only a time that differs is written (a guarded batch: a contact claimed, settled again or held
  meanwhile is left alone). A narrowed window may move a contact later, to the opening admission
  would defer it to anyway. A contact the frequency window or the daily cap holds keeps its time
  and mark; admission judges the hold again when it is due. A contact being dialled has no due
  time and is not touched. The list is read 500 contacts at a time, at most 20 reads per pass;
  the passes that follow at once finish it. `activity` and `nextDialAt` are computed after the
  re-arm in the same pass, so they follow the change without a restart.
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
     interval, never at once. The exception is an `UNKNOWN` attempt that recorded a lead (the call
     reached the bot before the application died): it is not retried, the contact is `DONE` (or
     `DNC` for `DO_NOT_CALL`), §7.2.

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

**Amended 2026-11-20 — the switch ends what the application cannot (#637).** Adoption needs an
application to run it. Two limits now ride the customer leg itself, set on the originate, so the
switch enforces them with the application dead, restarting or stuck:

- `park_timeout=30:RECOVERY_ON_TIMER_EXPIRE`. Counted from entering park, which is the answer: a
  customer answered and never bridged to the bot is hung up after 30 s, the "answered but still
  parked" row above without anyone to run it. The bridge takes the leg out of park, the switch reads
  the variable once, and no wait of this application's (the overflow hold, at most 10 s) comes near
  it. The cause is the one recovery's reap uses, so an attempt whose CDR carries it is `BOT_FAILED`
  (§7.2, an answered leg). One that ends while no application is up leaves no CDR and finishes
  `UNKNOWN` at the next adoption.
- `execute_on_answer_aicc_max_call_duration=sched_transfer +<N> aicc_max_call_duration_bot XML aicc`,
  `N` = `AICC_BOT_MAX_CALL_DURATION` plus a 60 s grace, in whole seconds. At the answer the switch
  schedules a transfer of the leg to an extension of the `aicc` context that stamps
  `aicc_hangup_cause=MAX_CALL_DURATION` and `aicc_max_call_duration_side=BOT` and hangs up; the bot
  leg bridged to it ends with it. The flow's own time limit, inside the same cap, ends the call a
  grace earlier, so this fires only when the application's ending did not run. A handover to a
  queue replaces it with the agent cap (`aicc_queue.lua`). The application counts each such leg
  (`aicc_call_max_duration_kills_total{side}`) and logs it with the `callId`. A campaign call never
  reaches an agent and its row is the bot's ledger, which does not read the switch's cause, so the
  attempt keeps whatever its row says (or `UNKNOWN` with no row); the outcome for a switch-side kill
  is deliberately left for a later change.

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
  - a bot leg that is refused, unreachable or fails before media is hung up with no prompt, never
    sent to the DID's queue, in the inline continuation of the bridge and in `rescue` alike; the CDR
    has `userData.botReason=BOT_LEG_FAILED` and the attempt finishes `BOT_FAILED` (§5.7).
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
    `queue_id = follow_up_queue_id`, and the notes as `message`. On a call whose lead a text
    model judges (§11.1) the callback waits for the verdict and is filed only if the lead is
    still `HIGH`.

  A model changing its mind mid-call therefore leaves no duplicate or stale callback, and nothing
  needs undoing. A DNC that takes effect at the end of the call rather than during it changes
  nothing: the call is not hung up for it, and an answered contact is not redialled anyway.
- **A lead is accepted until the call has its real CDR,** not only while the attempt is
  `DIALING`. An attempt finished early (restart reconciliation as `UNKNOWN`, a placement recorded
  as failed) can still have a live call: after a restart the switch's late leg is re-armed and the
  customer answers, with the same call id. `SetLead` therefore writes to a `FINISHED` attempt as
  long as the ledger holds no CDR for the call other than a placement failure, and the real CDR
  that follows revises the attempt through the same finish routine, which files the DNC row or
  callback once. The attempt lock orders `SetLead` against the CDR insert; once the real CDR is
  in, `SetLead` reports false (#559). A late leg the restarted process sees only as a new
  `CHANNEL_CREATE` is hung up rather than parked on the customer (#573); one its listing found
  ringing is still re-armed, which is the case this paragraph covers.
- **A phase may declare the lead** (`"lead": {"intent": …, "notes": …}`, #352), which the
  engine records when a campaign call enters that phase, through the same write: one more write,
  ordered with the model's `record_lead` calls, and the last one wins, except that while the call
  is in a phase that declares a lead the model's `record_lead` is refused (answered `ok: 0`,
  nothing written, logged at Info), so the phase's outcome stands (#358). It exists for the flow
  that classifies the customer and closes in the same move, where the model has no later turn in which
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

### 11.1 A text model reads the finished call and decides the lead (#661)

The lead above is the realtime model's hearing of one moment of audio. After a campaign call
**the bot handled alone** ends with a `HIGH` or `MEDIUM` lead, one text-model job
(`internal/leadjudge`, the `AICC_TEXT_LLM_*` model, not the voice provider) reads the transcript
and gives a verdict, and the verdict decides the lead. Nobody reviews it. The rules come from
the two offline runs posted on the issue; the owner's decisions of 2026-11-24 fixed the rest.

- **Verdicts** (`CampaignLeadVerdict`, `campaign_attempts.lead_verdict`): `CONFIRMED` keeps the
  declared intent; `UNDECIDED` lowers `HIGH` to `MEDIUM` and leaves `MEDIUM`; `REFUSED`,
  `MACHINE` and `NO_EVIDENCE` leave the call with no lead (`NONE`); `NOT_JUDGED` (no usable
  verdict in time) keeps it. `PENDING` is the row while the verdict is awaited.
  `campaign.LeadAfterVerdict` is the one table.
- **Storage: the attempt row is the queue.** `declared_lead_intent` (the flow's intent, copied
  when the judgement becomes owed and never changed after), `lead_verdict`, `lead_verdict_reason`
  and `lead_judge_deadline_at` (migration 00051, a partial index on `PENDING`). `lead_intent`
  stays the effective lead, so every consumer of it (contacts, the summary, exports) needed no
  change.
- **Owed inside the CDR's transaction.** `attemptFinisher.fromCDR` marks the attempt `PENDING`
  with a deadline 30 s from the database's clock when the store judges leads
  (`Store.IsLeadJudgeEnabled`, set from wiring), the lead is `HIGH` or `MEDIUM`, and the CDR is
  answered, more than a placement failure, has a bot, and reached no queue and no agent
  (`isJudgedCall`). The call's webhook deliveries are queued in the same transaction, held
  35 s, with a payload that already says `leadVerdict: NOT_JUDGED` and the declared lead: if
  nothing ever acts, the delivery goes out by itself at the bound and is correct as it stands.
  A fuller CDR while the judgement is owed keeps the hold. Every other call (no lead, `LOW`,
  `NONE`, `DO_NOT_CALL`, unanswered, handed to an agent, not a campaign's) is untouched and
  delivered at once, with no text-model request.
- **The callback waits.** While the attempt is `PENDING`, `leadSideEffects` files no callback
  for `HIGH`; the verdict files it when the lead it leaves is still `HIGH`, `NOT_JUDGED` at the
  deadline included. `DO_NOT_CALL` is never judged and goes on the list as before. Retry and
  redial are not recomputed: the call was answered (a redial after `MACHINE` is #664's).
- **The job.** Woken by the attempt hook (`campaign.Finished.IsLeadJudgementOwed`), and by a scan
  of `PENDING` rows at start and every 5 s for crash safety. It waits 3 s for the transcript's
  last lines, which land after the CDR, then reads the `CUSTOMER` and `BOT` rows of kind `TEXT`
  only: no tool row, not even by name (the first offline run read tool arguments as the callee's
  words). A call with no callee row, or whose callee rows are only pickup greetings and acknowledgement sounds (`saysOnlySounds`; answer words such as 好 or yes are not sounds), is `NO_EVIDENCE` in code, without a request: the model read a bare sound after the bot's question as `UNDECIDED` in the live evaluation. Otherwise one
  request with JSON output `{"verdict","reason"}`, bounded by the deadline (less a 1 s margin
  for the write) and `AICC_TEXT_LLM_TIMEOUT`, shortened by `AICC_TEXT_LLM_CONTEXT_CHARS` as the
  wrap-up draft is; one retry only if time remains. A malformed, empty or unknown answer, or a
  timeout, is `NOT_JUDGED`. The prompt is the platform's, zh or en by the call's language, and
  gets the declared intent and the notes the flow rendered (a phase's `lead` or `record_lead`
  carries nothing else); the reason is one sentence in that language.
- **Applying a verdict** is one transaction, conditional on `PENDING` and a deadline not yet
  passed (no row: too late, discarded, so the record says what the receiver was sent): verdict,
  reason, effective lead; the contact's lead only while its `last_attempt_id` is still this
  attempt; the deferred callback; the payload rebuilt from the rows and the held delivery
  replaced through `ReplacePendingWebhookDelivery`, which releases it. **The sweep** turns a
  `PENDING` row past its deadline into `NOT_JUDGED` (callback filed if `HIGH`) and leaves the held
  delivery alone: it already carries that payload and releases itself at the bound. With no text
  model the service still sweeps, so a judgement an earlier run left owed is never stuck.
- **When it is on.** `AICC_LEAD_JUDGE_ENABLED` (default true) and the text model configured with
  a key. It does not need live transcription of the human phase, but it does need the caller's
  words in the bot phase's transcript, so `AICC_PROVIDER_TRANSCRIBE_MODEL=off` turns it off:
  every lead would be `NO_EVIDENCE`. `aicc_campaign_lead_verdicts_total{verdict}` counts what is
  written.
- **Known gaps.** The transcript rows carry no acknowledgement or cut flag, so the judge cannot
  be shown them. A plain affirmative to a one-breath pitch cannot be told from a lead by text
  alone; it is `UNDECIDED`, kept visible rather than dropped. `GET /cdrs` shows `PENDING` while
  the verdict is awaited; a delivery never does.

---

## 12. Do-not-call

- **One choke point:** `outbound.Service` checks DNC in **both** `DialAI` and the click-to-dial
  path, so no path AICC places a call through dials a DNC number, single calls included. The dialer
  also filters at admission (§5.2) so as not to spend capacity on them. Calls agents dial by hand
  from the softphone go through the dialplan and are not checked (§1).
- **A campaign attempt is checked against the contact's stored canonical number** (#477). The
  dialer hands `PlaceAI` that number (`AIDialRequest.CanonicalTo`) beside the dial string, and the
  last check looks the list up with it as it is. The dial string is in the trunk's form
  (`AICC_OUTBOUND_DIAL_FORM`, with the campaign's country code), so it is never canonicalised again:
  with `E164_NO_PLUS`, or `NATIONAL` under a country code other than the deployment's, that would
  look up a different number than the contact's. A `CanonicalTo` that is not canonical E.164 is
  refused (the placement fails closed and the attempt ends `ORIGINATE_FAILED`), never waved through.
  The single-call rule below is unchanged.
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
  **Phase 1 of that detection exists (#409): a machine never produces a lead.** On a call the
  platform placed to a customer, every final caller transcript is checked against a built-in zh/en
  list of machine wording (voicemail prompts, call assistants speaking of the line's owner,
  carrier announcements); a match marks the call for the rest of its life, and the lead guard
  refuses every tool call that would record `HIGH` or `MEDIUM` interest on it, so a marked call
  writes no such lead and files no callback. Marked calls are counted by kind
  (`aicc_bot_machine_answered_calls_total`), which is the false-mark count the next phase waits
  for. Design 02 §6 has the rule. Still deferred: ending a marked call (through
  `global.closingTarget`, which would hang up on a person marked by mistake), the answered outcome
  of its own and its redial (`MACHINE_ANSWERED`, contract first), and anything for a call whose
  caller side has no transcript. Until then a marked call ends as `ANSWERED`, its contact `DONE`,
  as before.
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
- `POST/GET /campaigns/{campaignId}/contacts` (filter by `status`, `leadIntent`, `lastOutcome`,
  `isTestNumber`, `phoneNumber`). A contact carries `lastCallId` and `lastAttemptId`, required and
  `null` until its first attempt finishes (§3.2).
- `phoneNumber` (#617) narrows the contacts and the attempts to one number. It is read as an
  imported row's number is (`phonenum.Canonical` with the campaign's `defaultCountryCode`, else the
  deployment's), so an operator types it as dialled at home, without a country code, and an
  integration may send E.164; the canonical result is matched exactly against `number_e164`, never
  as a prefix or suffix. A value that is not a number is `400 VALIDATION_FAILED` with
  `params.field = phoneNumber`. The lookups ride `uq_campaign_contacts_number (campaign_id,
  number_e164)` and `idx_campaign_attempts_number_started (number_e164, started_at)`.
- `GET /callbacks?callId=` (#188): only the callbacks one call filed. A campaign call files at most
  one, so a `HIGH` lead resolves to its callback with the contact's `lastCallId`, never by comparing
  phone numbers: one number can hold callbacks from several calls and campaigns, and `numberRaw` and
  `phoneNumber` need not share a format.
- `GET /campaigns/{campaignId}/attempts` (filter by `contactId`, `outcome`, `isTestNumber`,
  `phoneNumber`). An attempt carries `isTestNumber` (§3.3). `phoneNumber` matches the attempt's own
  `number_e164`, the number dialled, which is its contact's: copied when the attempt is written, and
  a contact's number never changes.
- `GET/POST /test-numbers`, `DELETE /test-numbers/{number}` (`testnumbers:read` / `testnumbers:write`,
  audited like the do-not-call list; §3.4a).
- `GET /campaigns/{campaignId}/outcome-summary` (`campaigns:read`, #476): what the calls came to,
  its own operation so that a page of campaigns pays for no aggregate. Every count is read from the
  rows when it is asked for, in one read-only snapshot; nothing is kept as a running total, so a
  revised CDR moves its attempt from one count to another once. Each breakdown lists every value of
  its enum, in order, zero included, and each count is the total of a listing filtered the same way:
  - `attemptsByOutcome`: finished attempts by `outcome` (`listCampaignAttempts?outcome=`);
  - `contactsByLastOutcome`: contacts by `lastOutcome` (`listCampaignContacts?lastOutcome=`);
  - `contactsByLeadIntent` (`listCampaignContacts?leadIntent=`);
  - `optOuts.saidInConversation`: `DNC` contacts whose call recorded a `DO_NOT_CALL` lead
    (`?status=DNC&leadIntent=DO_NOT_CALL`); `optOuts.alreadyListed`: `DNC` contacts whose last
    attempt was `DNC_SKIPPED` (`?status=DNC&lastOutcome=DNC_SKIPPED`). A contact listed by somebody
    else while its call was in flight (§12) is `DNC` and in neither, so the two add up to
    `contacts.dnc` or less;
  - `callbacksByStatus`: the callbacks the campaign's calls filed, joined through the attempts'
    `call_id` (a campaign files only `HIGH` leads' callbacks, one per call), by `OPEN`, `CLAIMED`,
    `DONE` and `DISMISSED`; each is `listCallbacks?callId=` of one attempt's call.

  An optional `isTestNumber` query parameter (absent: everything, as before) leaves test calls out
  (`false`) or keeps only them (`true`), and the same parameter filters `listCampaignAttempts` and
  `listCampaignContacts`, so every count stays the total of a listing filtered the same way. An
  attempt is flagged by its own `is_test_number`; a contact, and so its lead and opt-out, by its
  latest finished attempt's (a contact no attempt has finished for is not flagged); a callback by
  the attempt of the call that filed it.

  Only the flag on `campaign_attempts` is new: every fact is in `campaign_attempts`,
  `campaign_contacts` and `callbacks`.
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
| `AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY` | `3` (`0` off) | 5.2 |
| `AICC_BOT_INBOUND_RESERVE` | `max(2, 10%)` of `AICC_BOT_MAX_CALLS` | 6.1 |
| `AICC_OUTBOUND_MAX_RINGING` | sized to the trunk | 6.1 |
| `AICC_OVERFLOW_HOLD` | `1500ms` | 6.4 |
| `AICC_OVERFLOW_TIGHTEN_RATE` / `AICC_OVERFLOW_STOP_RATE` | `1%` / `3%` | 6.5 |
| `AICC_TRUNK_BREAKER_COUNT` / `AICC_PROVIDER_BREAKER_COUNT` | `20` / `5` | 7.4 |
| `AICC_PROVIDER_SESSION_RPM` / `AICC_PROVIDER_TOKEN_RPM` | the profile's documented default; `0` off | 6.3 |

**Metrics** (names as built — every instrument carries the `aicc_` prefix and lives in
`internal/obs/callmetrics.go`):

- `aicc_campaign_attempts_total{outcome}`
- `aicc_campaign_line_rejected_total{sip_status}`: the line's refusals (`LINE_REJECTED`, §7.2) by
  their final SIP status, counted as each is classified (#702)
- `aicc_dialer_in_flight{kind=ringing|talking}`
- `aicc_dialer_admission_blocked_total{reason}` (reasons include the provider rule's
  `PROVIDER_SESSION_RATE`, `PROVIDER_TOKEN_RATE`, `PROVIDER_QUOTA_BACKOFF`, #604)
- `aicc_bot_slots{state=in_use|reserved}`
- `aicc_did_outbound_in_use{did}`
- `aicc_campaign_answer_rate_ucb{campaign}`, `aicc_campaign_overflow_total`, `aicc_bot_overflow_hold_ms`
- `aicc_outbound_first_audio_seconds{stage}`
- and two the design did not name: `aicc_bot_overflow_total`, `aicc_campaign_unmapped_cause_total`

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
  - **No retry follows a lead.** A contact whose call recorded a lead is never dialled again
    (§7.2), whatever the call's outcome, so the lead row's `lastCallId` always names the call that
    recorded the lead and filed its callback. The link is still hidden when `lastOutcome` is
    `UNKNOWN`, because the ledger may hold no CDR for that call.
- outcomes: a tab on the campaign page reads the outcome summary while it is open, and again on
  each `CAMPAIGN_UPDATED` of that campaign;
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
- **Budgets:** `TRANSIENT` / `LINE_REJECTED` / `ORIGINATE_FAILED` do not consume `maxAttempts`; reaching
  `maxSystemRetries` makes the contact `EXHAUSTED`; a retry falling outside the window moves to the
  next window's start, including across midnight and on a daylight-saving changeover day.
- **Breakers:** the trunk breaker trips on the `N`th failure and not the `N − 1`th; the provider
  breaker trips on the `M`th consecutive `BOT_FAILED`.
- **Refused bot legs:** a campaign in which every third bot leg is refused finishes those
  attempts `BOT_FAILED` and the rest `ANSWERED`, keeps running (the refusals are not consecutive),
  and leaves nothing in flight; `M` refusals in a row pause it.
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
- **Test numbers:** after A reaches two numbers, one of them listed, B dials the listed one at once
  and holds the other until the touch + 24 h (`WAITING_FREQUENCY`, `nextDialAt` = that time);
  listing the second makes B dial it at once; a listed number on the do-not-call list is never
  dialled; a listed number that A is dialling is skipped until A's call ends; the list refuses a
  101st number and a racing add never overshoots; an attempt to a listed number is flagged and stays
  so after delisting; the outcome summary with `isTestNumber` equals the filtered listings.
- **Outer hours:** a `calling_window` outside them is refused on create and on change; narrowing
  the outer hours still binds an existing campaign at admission.
- **Window widened after it closed (#616):** window MON–FRI 09:00–17:30 in Shanghai becomes
  09:00–18:00 at 17:35: a contact never tried and a 15-minute `NO_ANSWER` retry from 17:10 are
  due at once; a 30-minute retry from 17:20 is due 17:50; a 60-minute `BUSY` from 17:25 keeps
  tomorrow 09:00; a frequency hold and a daily-cap hold keep their times and marks; a contact in
  Los Angeles keeps its own zone's opening; `EXHAUSTED` and `DNC` contacts are not written. The
  same change made while `PAUSED` takes effect on resume. Adding a day, adding a range and
  widening a window across midnight bring waiting contacts forward to the new opening; the same
  window in another order re-arms nothing; wider outer hours take effect when the dialer starts.
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
campaigns; none needs one. **All of them shipped**; the table is the mapping from section to
issue and the order they landed in, not a status list.

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
