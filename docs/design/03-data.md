# Design 03 — Data Model, Migrations, Storage

Naming here follows [07-naming.md](07-naming.md) (mandatory): tables plural snake_case, columns singular snake_case, `xxx_at` timestamptz, `xxx_sec`/`xxx_ms` durations, `is_`/`has_` booleans, enum columns store SCREAMING_SNAKE strings byte-identical to JSON, jsonb keys lowerCamelCase, `idx_`/`uq_`/`fk_` constraint names.

## 1. Ownership & tooling

One PostgreSQL 18 instance. App database `aicc` with schemas: `public` (app tables, owned by Go migrations) and `luacc` (**views only** — the FreeSWITCH/Lua read contract). mod_callcenter's runtime tables live in a **dedicated database `aicc_fs`** in the same instance (M0 finding: the module creates unqualified `public` tables named `agents`/`tiers`/`members`, which would collide with ours in a shared database; created/managed by FS, never by us). Migrations: **goose** (embedded SQL, run automatically at startup, `goose up` CLI escape hatch). All DB access through sqlc-generated code (`internal/store/queries/*.sql` → one generated package); no raw SQL in services.

## 2. Tables (condensed; authoritative DDL lives in migrations)

**Identity & agents**
```sql
users(id uuid pk, username citext unique, password_hash text, display_name text,
      role varchar check in ('AGENT','SUPERVISOR','ADMIN'),
      status varchar check in ('ACTIVE','SUSPENDED') default 'ACTIVE',
      created_at, last_login_at)
sessions(id uuid pk, user_id fk, token_hash bytea, ip inet, created_at, expires_at)   -- revocable cookie sessions
agents(id uuid pk, user_id uuid unique fk, wrap_up_time_sec int default 30,
       is_auto_answer bool default false, default_extension_id uuid null fk)
agent_states(agent_id pk fk, state varchar check in ('LOGGED_OUT','NOT_READY','READY'),
             reason varchar null,          -- 'LOGIN','BREAK','LUNCH','TRAINING','AFTER_CALL_WORK','SYSTEM','SUPERVISOR'
             extension_number text null, entered_at timestamptz, wrap_up_ends_at timestamptz null)  -- THE row, written in-request
agent_state_logs(id bigserial, agent_id, state, reason, entered_at, exited_at)        -- occupancy/reports
```

**Telephony config** (fields feeding Lua/callcenter marked ★)
```sql
extensions(id uuid pk, number text unique ★, kind varchar check in ('AGENT','BOT','PLAIN') ★,
           password text ★,               -- write-only via API; read by the Lua directory handler
           display_name text, is_enabled bool ★, created_at)
queues(id uuid pk, ext_number text unique ★,                 -- dialable 7xxx
       name text unique ★, display_name text,
       strategy varchar ★ default 'LONGEST_IDLE_AGENT'      -- our enum; Lua/Go map to callcenter tokens at the boundary
         check in ('LONGEST_IDLE_AGENT','ROUND_ROBIN','TOP_DOWN','AGENT_WITH_LEAST_TALK_TIME',
                   'AGENT_WITH_FEWEST_CALLS','RANDOM'),
       moh_sound text ★, max_wait_sec int ★, max_wait_no_agent_sec int ★,
       announce_sound text ★, announce_frequency_sec int ★,
       tier_rules jsonb ★,                                   -- {"isApplied":bool,"waitSec":int,"noAgentSkip":bool}
       discard_abandoned_after_sec int ★, is_abandoned_resume_allowed bool ★,
       sla_threshold_sec int default 20, is_recording_enabled bool default true ★,
       hours jsonb ★,                                        -- [{"weekday":1,"open":"09:00","close":"18:00"}]
       overflow jsonb ★,                                     -- {"type":"BOT_FLOW"|"ANNOUNCE_HANGUP"|"FORWARD", …camelCase…}
       rona_delay_sec int default 10, is_enabled bool ★)
queue_agents(queue_id fk, agent_id fk, level int default 1, position int default 1, pk(queue_id,agent_id))
dids(id uuid pk, number text unique ★, language varchar ★,   -- BCP 47 lowercase 'en'/'zh' (external standard, see 07 §7)
     flow_id uuid NOT NULL,                                   -- every external number answers with a bot flow
     fallback_queue_id uuid null ★,                           -- only for "the bot cannot run": provider outage, capacity
     is_recording_enabled bool ★, description text, is_enabled bool ★)
trunks(id uuid pk, name text unique,
       direction varchar check in ('INBOUND','OUTBOUND','BIDIRECTIONAL'),
       max_channels int, config jsonb,                        -- camelCase keys: proxy, isRegister, username, …
       is_enabled bool)                                       -- rendered to sofia gateway include + rescan (01 §7)
```

**Flows**
```sql
flows(id uuid pk, slug text unique, name text, draft_spec jsonb,
      published_revision_id uuid null, published_at, updated_at, updated_by)
flow_revisions(id uuid pk, flow_id fk, spec jsonb, note text, created_by, created_at)  -- publish = insert + point
```
Spec = **DSL v2** (v1 re-keyed to lowerCamelCase per 07 §7: `specVersion`, `initialNode`, `maxTurns`, …; enum-like values SCREAMING_SNAKE). Server-side validation mirrors the Designer's Validate (reachable nodes, defined targets, known tools, bilingual completeness).

**Calls & artifacts**
```sql
live_calls(call_id uuid pk, snapshot jsonb, updated_at)          -- recovery only; row deleted at call end
cdrs(call_id uuid pk, started_at, answered_at, ended_at,
     call_type varchar check in ('INBOUND','OUTBOUND','CONSULT','INTERNAL'),  -- Genesys-style, caller-perspective; immutable across transfers
     language varchar,                                            -- 'en'/'zh'
     from_number text, to_number text, did text, flow_id uuid, queue_id uuid,
     agent_ids uuid[], primary_agent_id uuid,
     ring_sec int, bot_sec int, queue_wait_sec int, talk_sec int, total_sec int,
     status varchar check in ('ANSWERED','NO_ANSWER','BUSY','FAILED'),
     hangup_cause text,                                           -- FS Q.850 token (already SCREAMING_SNAKE upstream)
     missed_reason varchar null check in ('SHORT_ABANDONED','ABANDONED_RINGING','ABANDONED_WAITING',
                                          'AGENTS_DID_NOT_ANSWER','NO_AVAILABLE_AGENT','OUT_OF_HOURS'),
     disposition text, is_contained bool, has_recording bool,
     user_data jsonb,      -- business context (ticketId, slots); merge-patched during the call
     tech jsonb,           -- CDR technical tab: sipCallId, codec, IPs (release cause is the hangup_cause column)
     legs jsonb)
     -- legs: [{"kind":"TRUNK"|"DIALING"|"BOT"|"QUEUE"|"AGENT","label":…,"durationSec":…,"note":…}]
  -- idx_cdrs_started_at (desc), idx_cdrs_queue_id_started_at, idx_cdrs_primary_agent_id_started_at, idx_cdrs_status
transcripts(id bigserial pk, call_id, seq int, occurred_at timestamptz,
            role varchar check in ('BOT','CALLER'),
            kind varchar check in ('TEXT','TOOL_CALL','TOOL_RESULT'), content jsonb)   -- uq_transcripts_call_id_seq
recordings(id uuid pk, call_id fk, backend varchar check in ('FS','S3'), bucket text,
           object_key text, size_bytes bigint, duration_sec int,
           format varchar default 'WAV', created_at, deleted_at)
quality_reviews(id uuid pk, recording_id fk, call_id, reviewer_id fk,
                scores jsonb, total_score smallint, notes text, created_at)
callbacks(id uuid pk, call_id, queue_id, phone_number text, message text,
          status varchar check in ('OPEN','DONE','DISMISSED'), created_at, handled_by, handled_at)
queue_events(id bigserial, occurred_at, call_id, queue_id,
             event varchar check in ('JOINED','LEFT','OFFERED','BRIDGED','ABANDONED'),
             agent_id uuid, wait_ms int)                          -- SL/abandon source
audit_logs(id bigserial, occurred_at, actor_id, action varchar,   -- 'QUEUE_UPDATED','FLOW_PUBLISHED','AGENT_FORCE_LOGOUT',…
           target_kind text, target_id text, detail jsonb, ip inet)
settings(key text pk, value jsonb, updated_at)                    -- org, locale default, retentionDays, …
seq_blocks(name text pk, value bigint)                            -- SSE hi/lo blocks (100k)
```

## 3. CDR semantics (from cti-server, extended)

`missed_reason` derived from recorded facts, precedence caller-phase-first — never parsed from cause strings. `bot_sec` / `queue_wait_sec` / `talk_sec` come from party-established and callcenter event timestamps (first-class AI/human split). `is_contained` = answered ∧ ended in bot leg ∧ no transfer ∧ no `SESSION_LIMIT` reason. `CALL_CDR` (SSE) emitted exactly once; the `cdrs` row is the ledger that also backs outbound idempotency.

## 4. The Lua contract (`luacc` views)

```sql
luacc.directory  (number, password, is_enabled, display_name, is_auto_answer)          -- from extensions ⋈ agents
luacc.dids       (number, language, is_recording_enabled, fallback_queue_ext_number, is_enabled)
                 -- the dialplan needs only: is this number ours, record it?, and where to
                 -- send the caller if the bot cannot take the call. Which flow runs is
                 -- resolved by the application, so the flow catalogue never enters the
                 -- switch contract.
luacc.queues     (name, ext_number, strategy, moh_sound, max_wait_sec, max_wait_no_agent_sec,
                  announce_sound, announce_frequency_sec, tier_rules,
                  discard_abandoned_after_sec, is_abandoned_resume_allowed,
                  is_recording_enabled, overflow, is_enabled)
```
Rules: Lua role `aicc_lua` has SELECT on `luacc.*` only; **any migration touching the base tables must re-assert these view shapes and is reviewed as a schema+Lua pair** (the shared-contract mandate). Views are versioned in the same migration files. Upstream token mapping (e.g. `LONGEST_IDLE_AGENT` → `longest-idle-agent`) happens **inside the Lua scripts/Go adapter**, never in views or app code (07 §5).

## 5. Recording storage

Key rule (both backends): `recordings/YYYY/MM/DD/<call_id>.wav` (UTC date of call start; stereo WAV: caller left / bot+agent right via `RECORD_STEREO=true`). Dev backend `FS`: `AICC_RECORDING_DIR` root; FreeSWITCH writes directly there (`record_session` path templated by Lua) — same-host assumption documented. Prod backend `S3`: FS records to a local spool; aicc uploads on `RECORD_STOP`/hangup (minio-go, 3 retries), inserts the `recordings` row, deletes the spool file. Playback: `GET /api/v1/recordings/{id}/audio` streams (filesystem read; S3 presigned redirect when the S3 backend is configured). The docker-compose demo runs on the filesystem backend — no object store ships with it. Retention: daily job deletes storage objects + stamps `deleted_at` where `age > settings.retentionDays` (0 = keep forever); transcripts/CDRs retained independently.

## 6. Seed data (docker-compose demo)

**M4.4 implementation notes (amended in-commit):** ingestion is centralised in the telephony CDR assembler's `CallFinished` — it runs for every finished call (bot-only, human, or both in turn), stats/uploads `recordings/YYYY/MM/DD/<call_id>.wav` two seconds after retirement, books the `recordings` row and flips `cdrs.has_recording`. Absence of a file is normal (recording is per number/queue) but the skip reason is logged at Info — a silent Debug skip cost a live debugging session. The S3 backend was verified against SeaweedFS (`weed mini`, signed V4 with identities config); backend switch is config-only (`AICC_RECORDING_BACKEND=FS|S3`). ⚠ **`export_vars` contract**: `aicc_inbound.lua` must export `aicc_call_id,aicc_did,aicc_language` — `setVariable` alone does not cross a bridge, so without the export the bot leg opens a second provisional call and the minted identity never exists in the registry (found live in M4.4; the M4.3 merge preference only works when some leg actually carries the minted id).

**M4.5 amendments — one CDR per conversation (live-verified with raw ESL captures):**

- **Reidentify.** The caller's `CHANNEL_CREATE` fires *before* the dialplan runs, so the caller is always adopted under a provisional id; the minted `variable_aicc_call_id` only rides events from `CHANNEL_ANSWER` on. The coordinator therefore *reidentifies*: the first event that reveals a minted id on a provisionally-bound channel merges the provisional call into the minted one (creating it if this is the first sighting). Relying on the bridge event alone was a coin toss — chi's `CHANNEL_BRIDGE` names an arbitrary side first, and the loser produced one phantom CDR per AI call (8 rows for 3 real calls, found live in M4.5).
- **CDR ownership rule.** The bot writes the CDR for calls it finished; the switch-side assembler writes for calls a person finished — including bot calls that were handed over, which are marked by the bot-share stamp. Concretely: `CallFinished` skips `InsertCDR` when the call has a bot leg (`Party.IsBotLeg`, stamped at adoption: outbound leg whose destination equals its `aicc_did`) and no bot share. Recording ingestion still runs either way. Known gap: if the bot process dies mid-call, that call's row is lost — accepted, noted here.
- **Containment.** Reaching a terminal flow phase marks the call contained exactly like the `hangup` tool does (`markHangup` in `afterMove`); before this every farewell-concluded call reported uncontained.
- **Harness marker.** Scripted verification/seed calls originate `{aicc_harness=true}loopback/<did>/public`. Loopback copies the variable to both halves, so the coordinator ignores only the `-a` half (pure scaffolding); the `-b` half plays the caller and is tracked normally. Seed calls that should survive to the flow's farewell need real media on the caller side — `&playback(local_stream://moh)`, not `&park()` (a silent caller trips the UAS's RTP-dead watchdog in ~5s).

`deploy/seed/`: users `admin/supervisor/agent1000` (documented demo passwords, forced-change flag), extensions 1000–1009 (agents) + queue exts 7001/7002, queues `support-en`/`support-zh`, DIDs `95001→flow-en, 95002→flow-zh, 95011→queue direct`, two published demo flows (DSL v2, en+zh), and a deterministic synthetic history generator (seeded PRNG, last 7 days of `cdrs`+`queue_events`+`agent_state_logs`, no audio files) so wallboard/CDR/reports render alive. `AICC_SEED=fresh|demo` selects empty vs seeded.
