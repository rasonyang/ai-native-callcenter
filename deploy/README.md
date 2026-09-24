# Deployment guide

This stack runs the whole product on one host: PostgreSQL, FreeSWITCH and the
application, seeded with a demo dataset.

## Prerequisites

- Docker with Compose v2 (`docker compose version` works).
- A Qwen key (`ALIYUN_API_KEY`), or an OpenAI key outside mainland China.
- The address phones use to reach this host.

## Quick start

**1. Get the stack and copy the example config.**

```sh
git clone --depth 1 https://github.com/rasonyang/ai-native-callcenter
cd ai-native-callcenter/deploy
cp .env.example .env
```

Compose builds and mounts files from this checkout, so run it from `deploy/`.

**2. Set two values in `deploy/.env`.**

```ini
FS_EXTERNAL_IP=<the address phones reach this host at>
ALIYUN_API_KEY=<key>
```

- `FS_EXTERNAL_IP` is required. Compose refuses to start without it. A wrong
  value lets a phone register but it hears no audio.
- Outside mainland China, set `AICC_PROVIDER=openai` and `OPENAI_API_KEY`
  instead of `ALIYUN_API_KEY`.
- Put the key in `deploy/.env`. The `.env` at the repository root is not read
  by the stack.
- Without a key, everything works except the bot: it never answers, every call
  to a bot number goes straight to that number's queue, and the caller hears
  hold music until an agent picks up.
- Every other setting has a default. Every password is `aicc@123`. Change
  them before others can reach the host
  ([production checklist](production-checklist.md)).

**3. (Optional) Use the published images instead of building.** By default the
first start builds the application from this checkout (a few minutes; later
starts take seconds). To skip the build, add to `.env`:

```ini
AICC_IMAGE=rasonyang/ai-native-callcenter:v0.1.1
AICC_FS_IMAGE=rasonyang/freeswitch-aicc:v0.1.1
```

- The application image contains the executable with the SPA embedded;
  `docker compose pull` is all it takes to get the product onto a host.
- Use exact tags. There is no `latest`.
- Both images must carry the same release tag. The application's migrations and
  the switch's Lua scripts share the `luacc.*` contract; a mismatched pair
  breaks phone registration.

**4. Start the stack and check it.**

```sh
docker compose up -d
docker compose ps -a            # three running, lua-role exited (0)
docker compose logs aicc | grep 'esl connected'
# {"level":"INFO","msg":"esl connected","addr":"freeswitch:18021"}
docker compose logs aicc | grep -c 'API_KEY is not set'
# 0
```

- `lua-role` exited non-zero: the application never migrated. Read
  `docker compose logs aicc`.
- Count other than `0`: the provider key did not reach the application. The
  counted line names the variable. The application still starts, but the bot
  does not answer.

After any change to `.env`, run `docker compose up -d`. It recreates the
containers whose settings changed. `docker compose restart` does not re-read
`.env`.

**5. Open `http://<host>:8080`** and sign in as `admin` / `aicc@123`.

## Demo data

`AICC_SEED` defaults to `demo`, so the first start fills an empty database. On
every boot the seeded accounts get their password and role reset; nothing else
is touched.

| | |
|---|---|
| Accounts | `admin` (administrator), `supervisor` (supervisor), `wei` / `amy` / `ben` (agents). Password `aicc@123` |
| Extensions | `amy` 1000, `wei` 1001, `ben` 1002. SIP password `aicc@123`, readable through `GET /extensions/{id}/password`. A signed-in agent's browser phone gets its own credentials from the platform; the static password is for a hand-configured phone while nobody is signed in at that extension |
| Queues | `support-en` on 7001 (`wei`, `amy`), `support-zh` on 7002 (`ben`) |
| Customers | 18 numbers a SIP phone can register as: 13800000001–13800000009 and (212) 555-0101 – (212) 555-0109. Password `aicc@123`, registrar `<FS_EXTERNAL_IP>:5060`, domain `<FS_EXTERNAL_IP>`. A number is unreachable until a phone registers as it |
| History | Seven deterministic days of calls, queue events and presence, for the wallboard and reports |

Six published bilingual flows, each on an English, a Chinese and a US number:

| Flow | English | Chinese | US |
|---|---|---|---|
| NovaNet support | 95001 | 95002 | 800-555-0199 |
| StarCom mobile after-sales | 95011 | 95012 | 800-555-0191 |
| NovaNet broadband plan change | 95021 | 95022 | 800-555-0192 |
| NovaNet early collections | 95031 | 95032 | 800-555-0193 |
| StarCom field-service appointment | 95041 | 95042 | 800-555-0194 |
| StarCom lead qualification | 95051 | 95052 | 800-555-0195 |

- 800-555-0199 is the main line and the caller ID for outbound calls.
- The dialled number selects the flow and the language.
- Five of the six flows call a business backend. Without
  `AICC_BOT_BACKEND_BASE` pointing at a running `cmd/aicc-mockbackend`, their
  tools fail and the bot offers a transfer instead. This is expected
  ([docs/dev-stack.md](../docs/dev-stack.md)).

**Remove or re-seed the demo data:**

```sh
AICC_SEED=fresh docker compose up -d aicc   # remove what the seed created
docker compose up -d aicc                   # seed again
```

- `fresh` is the only way back to an empty product once the volume exists.
- It removes only seeded data. Real calls placed on this stack stay.
- Seeding again restores accounts, queues, flows and numbers, but not the
  history. History is only generated into an empty ledger.

## Make a test call

**1. Register a customer phone.** In any SIP softphone, register as
`2125550101` / `aicc@123` at `<FS_EXTERNAL_IP>:5060`, domain
`<FS_EXTERNAL_IP>`. The phone is now (212) 555-0101. Do this first: an
unregistered number is unreachable.

**2. Call the customer as agent wei.**

1. Install the [web-sip-phone](https://github.com/rasonyang/web-sip-phone)
   Chrome extension. It needs no configuration.
2. Sign in as `wei` / `aicc@123`. The platform issues the browser its own SIP
   credentials and passes them to the extension.
3. Follow the onboarding card in the cockpit: allow this site, allow the
   microphone. The card disappears when the phone chip in the softphone bar
   shows ready.
   For many agents, [Chrome policy](chrome-policy.md) does this instead.
4. Open the keypad, type `2125550101`, press Dial. Dialling out does not need
   *Go ready*.

wei's phone rings first and auto-answers, then the customer phone rings showing
800-555-0199. Answer it. The call appears on the wallboard and ends as a CDR.

Going *ready* requires a registered phone; an agent without one is refused.

**3. Call in as the customer.** From the customer phone, dial `8005550199`. The
bot answers in English as NovaNet support. Ask for a person: the call goes to
`support-en` and rings wei once wei has picked *Go ready*. 95001 and 95002 are
the same flow; 95002 greets in Chinese.

**Without a phone**, place the same call from inside the switch:

```sh
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123 \
    -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://20000)"
# +OK <uuid>
```

`-ERR UNALLOCATED_NUMBER` means no enabled number `95001` exists.

## What is running

| Container | Role |
|---|---|
| `postgres` | PostgreSQL 18. Holds `aicc` and mod_callcenter's `aicc_fs` |
| `aicc` | The application: REST API, event stream, embedded SPA, and the SIP endpoint for AI calls |
| `lua-role` | Runs once, creates the confined role the switch reads the database with, and exits |
| `freeswitch` | FreeSWITCH v1.11.3, this repository's own image |

Volumes: `postgres-data`, `recordings` (written by the switch, read by the
application), `fs-db`, `fs-log`, `app-logs`.

Published ports (on `HTTP_BIND` or `SIP_BIND`):

| Port | Use |
|---|---|
| 8080/tcp | API, event stream and web interface |
| 5060/udp, 5060/tcp | SIP signalling |
| 5066/tcp | Browser phone WebSocket transport |
| 16384-16484/udp | Media to phones off this host |

ESL, the metrics listener, and the AI leg's SIP and RTP are not published.

## Common commands

```sh
docker compose logs -f aicc                             # application logs
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123
docker compose exec postgres psql -U aicc aicc
docker compose down                                     # stop
docker compose down -v                                  # stop and delete all data
```

## Configuration

Stack settings in `deploy/.env`:

| Variable | Meaning |
|---|---|
| `FS_EXTERNAL_IP` | Required. The address phones reach this host at |
| `POSTGRES_PASSWORD`, `LUA_PASSWORD`, `ESL_PASSWORD` | Default `aicc@123` |
| `AICC_SEED` | Default `demo`. Empty seeds nothing; `fresh` removes what the seed created. Empty it on a host others can reach ([production checklist](production-checklist.md)) |
| `SWITCH_DOMAIN` | The switch's domain. Default `FS_EXTERNAL_IP`. The application uses the same value |
| `SIP_DOMAIN` | Digest realm and registration domain for agents' browser phones. Default `SWITCH_DOMAIN`. Leave it unless you separate the two |
| `SIP_WSS_URL` | Where the browser phone connects. Default `ws://<FS_EXTERNAL_IP>:5066/`. Behind TLS, set the proxy's `wss://…` URL ([production checklist](production-checklist.md)). It is sent to the phone, not typed into it |
| `HTTP_BIND`, `HTTP_PORT`, `SIP_BIND` | Listen addresses for the published ports |
| `RTP_START`, `RTP_END` | Media port range. Configures the switch and publishes the ports |
| `AICC_SUBNET`, `AICC_APP_IP` | The compose network and the application's fixed address in it (the switch dials the bot at this address). Change together |
| `AICC_IMAGE`, `AICC_FS_IMAGE` | Unset: build the application from this checkout. Set to `rasonyang/ai-native-callcenter:<tag>` and `rasonyang/freeswitch-aicc:<tag>` to pull. Same release tag on both (they share the `luacc.*` contract; mismatched tags break phone registration). `AICC_FS_IMAGE` defaults to the switch of this checkout's release. Exact tags only; no `latest` |

**Application settings.** Any other `AICC_*` line in `deploy/.env` is passed to
the application unchanged: provider keys, `AICC_TRANSCRIBE_*`,
`AICC_BOT_BACKEND_BASE`, `AICC_S3_*`, and so on. The full list is the
repository's [`.env.example`](../.env.example). Two rules:

- An empty value means unset, so a non-empty default cannot be blanked.
- Everything after the first `=` is the value, including comments.

The wiring keys in `docker-compose.yml` are set under `environment:`, which
overrides `env_file:`, so `.env` cannot break them.

**Voice provider.** One provider answers every call, chosen at startup. A
call's language does not select it. `AICC_PROVIDER` takes `openai`, `qwen`,
`gateway`, `doubao` or `gemini`, each with its own credential.

- `doubao` uses `DOUBAO_API_KEY` (ByteDance full-duplex dialogue API); `gemini`
  uses `GEMINI_API_KEY` (Google Live API).
- On `doubao` and `gemini`, the client pins the model, so `AICC_PROVIDER_MODEL`
  is ignored. If transcription is on, set `AICC_TRANSCRIBE_PROVIDER` (also
  required on `gateway`).
- On `doubao`, every terminal phase of a flow needs an `announce`; publishing a
  flow without one is refused.
- On `gemini`, allow outbound access to `generativelanguage.googleapis.com`,
  and give every DID a fallback queue. Gemini closes the connection when its
  session lifetime runs out and the application does not reconnect: the call is
  released with hangup cause `PROVIDER_SESSION_EXPIRED` and the caller goes to
  the queue.

See [the provider notes](../docs/provider-extension.md).

**API.** The API is at `/api/v1` on the same port.
[docs/openapi.json](../docs/openapi.json) is the contract. For example,
`POST /auth/login` with `{"username":"admin","password":"aicc@123"}` is
`http://<host>:8080/api/v1/auth/login`.

- API keys are not configured in `.env`. Issue them with `POST /api-keys`,
  named and scoped; the first one with the administrator's session.
- Requests that write with a session cookie must carry an `X-AICC-Csrf` header
  (any value). Bearer-key requests need not.

## Recordings

- Default (`AICC_RECORDING_BACKEND=FS`): the switch writes to the shared
  `recordings` volume and the application serves from it.
- Files are `recordings/YYYY/MM/DD/<call_id>.wav`, stereo: caller left, bot or
  agent right.
- Retention is a product setting, applied by a daily job.
- S3-compatible storage: set `AICC_RECORDING_BACKEND=S3` and the `AICC_S3_*`
  lines in `.env`. The application uploads each recording on hangup, clears the
  spool, and plays it back through a presigned URL.

## Upgrading

- Published images: change `AICC_IMAGE` to the new tag, then
  `docker compose pull && docker compose up -d`.
- Built from a checkout: `git pull && docker compose up -d --build`.

Migrations run at startup and are forward-only. A single-instance advisory lock
prevents two from running at once. `AICC_FS_IMAGE` usually does not change with
the application; the release notes say when it does.
[freeswitch/README.md](../freeswitch/README.md) covers installing the switch by
hand instead of pulling it.

## Troubleshooting

- **A call to a bot number only plays hold music.** The bot session did not
  start, and the caller went to the number's fallback queue with nobody
  staffed. Find the reason with
  `docker compose logs aicc | grep -E 'could not run|conversation failed'`.
- **`… API_KEY is not set` in the logs.** The key is not in the container's
  environment. Put it in `deploy/.env` and run `docker compose up -d`
  (`restart` is not enough).
- **A `401` or a failed handshake.** The provider rejected the key: it is wrong,
  or not enabled for the realtime model shown in the `voice provider selected`
  log line.

## More

- [chrome-policy.md](chrome-policy.md): install the browser phone and grant
  its permissions on a managed fleet, instead of the onboarding card.
- [carrier.md](carrier.md): replace the simulated public network with your own
  trunk.
- [production-checklist.md](production-checklist.md): passwords, demo data, TLS
  and firewalling before the stack faces anyone.
