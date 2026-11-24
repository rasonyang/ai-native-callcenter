---
name: aicc
description: Use a running AI Native Call Center deployment — sign in, issue API keys, author and verify a call flow, publish it, place a test call, and read CDRs and the event stream — through its REST API and the aicc CLI. Use when a task touches call flows, the call platform's API, integrations, campaigns or a deployed stack.
---

# Using an AI Native Call Center deployment

One Go binary serves the REST API, the SSE event stream and the whole web
interface; PostgreSQL and FreeSWITCH run beside it in containers. **The API is
the product**: everything the interface can do is an operation of the OpenAPI
contract the deployment itself serves at `GET /api/v1/openapi.json`, and the
contract is right whenever a screen and it disagree.

This skill covers *using* a deployment. Installing one is
`docs/agent-install.md` in the source repository
(github.com/rasonyang/ai-native-callcenter).

## Where things are

- API base: `http://<host-address>:8080/api/v1`. The address is `FS_EXTERNAL_IP`
  and the port `HTTP_PORT` in the install directory's `.env`; the install
  directory is `/opt/aicc` on Linux (root-owned — every read and command goes
  through `sudo`) and `~/.aicc` on macOS.
- The `aicc` binary runs inside the `aicc` container. From the install
  directory: `sudo docker compose exec aicc aicc <subcommand>` on Linux, the
  same without `sudo` on macOS. Subcommands: `doctor`, `flowadd`, `flowsim`,
  `useradd`, `passwd`, `skill`, `version`, `transcribe-recordings`.
- Sign-in on a demo-seeded install: `admin` with the `AICC_SEED_PASSWORD` from
  `.env` (`sudo grep AICC_SEED_PASSWORD /opt/aicc/.env` on Linux, `grep
  AICC_SEED_PASSWORD ~/.aicc/.env` on macOS). The same password opens the demo
  agents (`amy`, `ben`, `wei`) and `supervisor`.

## API credentials

- A session cookie (a person's browser) and `Authorization: Bearer <key>` are
  equal credentials. Authorization is scopes; each operation's `security`
  block in the contract names them, and a key missing one gets
  `403 INSUFFICIENT_SCOPE`.
- Issue a key with `POST /api/v1/api-keys` (itself needing `keys:manage`),
  body `{"name": "…", "scopes": ["…"]}`; the secret is returned once and
  cannot be read back. Store it; never print it again, and never put a key in
  a place a transcript or log keeps.
- `x-body-scopes` in the contract adds a scope when a request body names a
  value — `POST /calls` with `kind: AI_OUTBOUND` needs `calls:create:ai` on
  top of `calls:create`.
- To act for one agent, add `X-AICC-Agent-ID: <agentId>` on a key request
  (refused on a session: a signed-in account acts as itself).

## Author, verify and publish a flow

- The DSL is `docs/flow-authoring.md` in the source repository; working
  bilingual flows are `internal/seed/flows/` and the annotated reference flow
  is `docs/examples/flows/knowledge_support.json`. Unknown fields are load
  errors, so write only the fields the document declares.
- Verify before publishing:
  `aicc flowsim -flow flow.json -scenario scenarios.json` replays scripted
  scenarios on the real engine and runtime, with the flow's HTTP tools served
  by an in-process mock backend — no database, switch or model. Scenario and
  `expect` fields are in docs/flow-authoring.md section 8; `-v` prints every
  step. `aicc flowsim -tier 2` puts models in the loop and spends money — see
  "Needs a person" below.
- Publish with `aicc flowadd -file flow.json -did <number>`: it loads,
  publishes and points a number at the flow (a number it creates is inbound
  only; rerunning with the same slug updates and republishes). Through the
  API: `POST /api/v1/flows` (`{slug, name, spec}`) stores a draft,
  `PUT /api/v1/flows/{flowId}` replaces it, `POST
  /api/v1/flows/{flowId}/publish` puts it live. Calls already running keep the
  revision they started on; a publish appends one.
- A number's `language` sets the call's language (greeting and prompt); the
  flow's `global.voice` sets the bot's voice. A call's language never selects
  a provider.

## Place a test call

- The normal path is a phone call: sign in as an agent (the browser phone is
  the web-sip-phone Chrome extension, with the host's address in its Allow
  Sites), then dial `95001` (English) or `95002` (Chinese). Asking the bot for
  a person exercises the transfer into a queue. The demo's simulated customer
  numbers answer too.
- Through the API: `POST /api/v1/calls` (createCall). `kind: AI_OUTBOUND`
  starts the bot on whoever answers — it needs an outbound endpoint
  (`AICC_OUTBOUND_ENDPOINT`, which a default install does not have),
  `allowOutbound` on the DID and the `calls:create:ai` scope.
  `kind: AGENT_OUTBOUND` (the default) is click-to-dial: the named extension's
  phone rings first. Mint a UUIDv7 `callId` so a retry answers `isDuplicate`
  instead of dialling twice.
- From the switch, with no browser — on the install directory:

  ```sh
  fs() { sudo docker compose exec -T freeswitch sh -c "fs_cli -H 127.0.0.1 -P 18021 -p \"\$FS_ESL_PASSWORD\" -x \"$1\""; }   # macOS: without sudo
  fs 'originate {origination_caller_id_number=1001,aicc_extension=1001}loopback/95001/aicc &playback(silence_stream://20000)'
  ```

## Read what happened

- Live: `GET /api/v1/events` is the one ordered SSE stream (`SseEvent`
  envelope; `Last-Event-ID` resumes, `SYSTEM_RESET` means refetch state over
  REST). Leg events are `PARTY_*`, call-scoped ones `CALL_*`; a leg event goes
  only to the agent whose leg it is, so an API key sees only broadcast events
  unless it holds `calls:read:all` or carries `X-AICC-Agent-ID`. Reference:
  `docs/events.md`.
- Afterwards: `GET /api/v1/cdrs` and `/cdrs/{callId}` — one CDR per
  conversation, bot phase and human phase together. The transcript is
  `/calls/{callId}/transcript`; recordings are listed by
  `/calls/{callId}/recordings` and fetched from `/recordings/{recordingId}/audio`.
- Health: `aicc doctor` — each check prints PASS/FAIL/SKIP and a `fix:` line.
  Logs: `docker compose logs aicc freeswitch` from the install directory
  (with `sudo` on Linux).

## Needs a person — stop and ask

- Every bot call, and `aicc flowsim -tier 2` (which needs `AICC_LIVE_FLOWSIM=1`
  and `ALIYUN_API_KEY` in the environment and never runs without them), spends
  the provider's quota. Ask before repeated, load or soak calls.
- A real outbound call rings a real telephone. Test calls go to `95001`/`95002`,
  an agent's softphone or the demo's seeded customer numbers; ask before
  dialling anything else, and never point a campaign at numbers the person has
  not named test numbers.
- Never change `.env`, secrets, the host's network or firewall, or another
  service's ports on your own — state what is needed and let the person
  decide.
