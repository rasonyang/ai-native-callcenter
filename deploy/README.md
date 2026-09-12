# Running it

The whole product on one machine: PostgreSQL, FreeSWITCH and the application,
with a demo dataset already in the database. It needs Docker, a Qwen key for the
bot to speak, and the address phones reach this host at. Nothing else.

## Start it

**1. Docker.**

```sh
curl -fsSL https://get.docker.com | sh
```

Or `sudo apt install docker.io docker-compose-v2` on Ubuntu. A `permission
denied` later means `sudo usermod -aG docker $USER` and a new login.

**2. Get the stack.** The checkout is what compose builds and mounts.

```sh
git clone --depth 1 https://github.com/rasonyang/ai-native-callcenter
cd ai-native-callcenter/deploy
cp .env.example .env
```

**3. Fill in `.env`.** Two lines; everything else has a default and every
password is `aicc@123`.

```ini
FS_EXTERNAL_IP=<the address phones reach this host at>
ALIYUN_API_KEY=<key>
```

Leave `FS_EXTERNAL_IP` empty and compose refuses to start, naming it: nothing
inside a container can work out the host's address, and a phone given the wrong
one registers and hears nothing. Outside mainland China set
`AICC_PROVIDER=openai` and `OPENAI_API_KEY` instead. With no key at all,
everything works except the bot's voice.

**4. Start it.** The first run builds the application from this checkout, which
takes a few minutes; afterwards the stack starts in seconds.

```sh
docker compose up -d
docker compose ps -a            # three running, lua-role exited (0)
docker compose logs aicc | grep 'esl connected'
# {"level":"INFO","msg":"esl connected","addr":"freeswitch:18021"}
```

A non-zero `lua-role` means the application never migrated, so read
`docker compose logs aicc`.

**5. Open it.** `http://<host>:8080`, and sign in as `admin` / `aicc@123`.

## What is seeded

`AICC_SEED` defaults to `demo`, so a first start fills an empty database. The
seeded accounts have their password and role reset on every boot; everything
else is left as it is.

| | |
|---|---|
| Accounts | `admin` (administrator), `supervisor` (supervisor), `wei` / `amy` / `ben` (agents), password `aicc@123` |
| Extensions | `amy` 1000, `wei` 1001, `ben` 1002, SIP password `aicc@123` |
| Queues | `support-en` on 7001, `support-zh` on 7002; `wei` and `amy` staff the first, `ben` the second |
| Customers | Eighteen numbers a SIP phone may register as: 13800000001–13800000009 and (212) 555-0101 – (212) 555-0109, password `aicc@123`, at `<FS_EXTERNAL_IP>:5060` with `<FS_EXTERNAL_IP>` as the domain. Until a phone registers as one, that number does not exist to the switch |
| History | Seven deterministic days of calls, queue events and presence, so the wallboard and the reports are not empty |

Six published bilingual flows, each on an English, a Chinese and a US number:

| Flow | English | Chinese | US |
|---|---|---|---|
| NovaNet support | 95001 | 95002 | 800-555-0199 |
| StarCom mobile after-sales | 95011 | 95012 | 800-555-0191 |
| NovaNet broadband plan change | 95021 | 95022 | 800-555-0192 |
| NovaNet early collections | 95031 | 95032 | 800-555-0193 |
| StarCom field-service appointment | 95041 | 95042 | 800-555-0194 |
| StarCom lead qualification | 95051 | 95052 | 800-555-0195 |

800-555-0199 is the main line, and the number the platform shows when it calls
out. The number picks the flow and the language; the flow carries both personas.
Five of the six fetch their facts from a business backend, so without
`AICC_BOT_BACKEND_BASE` pointing at a running `cmd/aicc-mockbackend` every one
of their tools fails and the bot offers a transfer instead, which is correct
behaviour ([docs/dev-stack.md](../docs/dev-stack.md)).

`AICC_SEED=fresh docker compose up -d aicc` removes all of it again, the only way
back to an empty product once the volume exists; `docker compose up -d aicc` then
seeds it afresh. Only what the seeder created goes, so real calls placed against
this stack stay, and seeding again restores the accounts, queues, flows and
numbers but *not* the history, which is only ever generated into an empty ledger.

## Make a call

**1. A customer telephone.** Register any SIP softphone as `2125550101` /
`aicc@123` at `<FS_EXTERNAL_IP>:5060`, domain `<FS_EXTERNAL_IP>`. That phone is
now (212) 555-0101, and nothing works before it: a number nobody registered is
unreachable, like an unallocated number on a real trunk.

**2. wei calls the customer.** Sign in as `wei` / `aicc@123`. The agent's phone
is the [web-sip-phone](https://github.com/rasonyang/web-sip-phone) Chrome
extension: in its options page set Server to `ws://<FS_EXTERNAL_IP>:5066/`,
Account `1001`, Password `aicc@123`, and add this host under Allow Sites.
Signing in at the softphone bar takes no input and lands in Not ready, which
dialling out does not need: open the keypad, type `2125550101`, press Dial.
wei's phone is raised first and auto-answers; only then does the customer
telephone ring, showing 800-555-0199. Answer it: the call is on the wallboard
and ends as a CDR.

**3. The customer calls in.** From that telephone dial `8005550199`: the bot
answers in English as NovaNet support; ask for a person and the call lands in
`support-en`, ringing wei, once wei has picked Go ready in the bar. 95001 and
95002 are the same flow's Chinese-style hotlines; 95002 greets in Chinese.

**With no telephone at hand,** this places the same call from inside the
switch, and `-ERR UNALLOCATED_NUMBER` means no enabled number `95001` exists.

```sh
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123 \
    -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://20000)"
# +OK <uuid>
```

## What is running

`postgres` is PostgreSQL 18, holding `aicc` and mod_callcenter's own `aicc_fs`
beside it; `aicc` is the product, REST API, event stream, embedded SPA and the
SIP endpoint AI calls land on; `lua-role` runs once, creates the confined role
the switch reads the database with, and exits; `freeswitch` is FreeSWITCH
v1.11.3, this repository's own image. The volumes are `postgres-data`,
`recordings` (written by the switch, read by the application), `fs-db`,
`fs-log` and `app-logs`.

Published, on `HTTP_BIND` or `SIP_BIND`: 8080/tcp for the API, the event stream
and the interface; 5060/udp and 5060/tcp for SIP signalling; 5066/tcp for the
browser phone's WebSocket transport; 16384-16484/udp for media to a phone off
this host. ESL, the metrics listener and the AI leg's own SIP and RTP are
published nowhere.

## Reaching into it

```sh
docker compose logs -f aicc                             # the application
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123
docker compose exec postgres psql -U aicc aicc
docker compose down                                     # stop
docker compose down -v                                  # stop and forget everything
```

## What to change

| `.env` | |
|---|---|
| `FS_EXTERNAL_IP` | Required. The address phones reach this host at |
| `POSTGRES_PASSWORD`, `LUA_PASSWORD`, `ESL_PASSWORD` | All three default to `aicc@123` |
| `AICC_SEED` | `demo` unless set. Empty seeds nothing; `fresh` removes what the seed created |
| `SWITCH_DOMAIN` | The switch's domain, defaulting to `FS_EXTERNAL_IP`. The application is configured from the same value |
| `HTTP_BIND`, `HTTP_PORT`, `SIP_BIND` | Where the published ports listen |
| `RTP_START`, `RTP_END` | The media range. Configures the switch and publishes the ports together |
| `AICC_SUBNET`, `AICC_APP_IP` | The compose network and the application's fixed address in it, which the switch dials the bot at. Change together |
| `AICC_IMAGE`, `AICC_FS_IMAGE` | Unset, the application is built from this checkout. A published release tag pulls it instead; exact tags only, there is no `latest` |

Any other `AICC_*` line in the same `.env` reaches the application unchanged:
provider keys, `AICC_TRANSCRIBE_*`, `AICC_BOT_BACKEND_BASE`, `AICC_S3_*`, all
of it. The registry is the repository's own [`.env.example`](../.env.example),
and two of its semantics surprise people: an empty value means *unset*, so a
non-empty default cannot be blanked, and everything after the first `=` is the
value, comments included. The wiring keys in `docker-compose.yml` sit in
`environment:`, which wins over `env_file:`, so `.env` cannot break them. The
provider is one of those: a single provider answers every call, chosen at
startup, and a call's language never selects it ([the provider
notes](../docs/provider-extension.md)).

There is no API-key setting: keys are issued through `POST /api-keys`, named
and scoped, the first with the administrator's session. A session cookie must
carry an `X-AICC-Csrf` header, any value, on anything that writes; a bearer key
need not.

## Your own carrier

The two directories mounted onto the switch are the seam: `freeswitch/dialplan/`
holds the rules of the `aicc` context, `freeswitch/directory/` the SIP users the
switch knows outside the database. Here they hold a simulated public network,
which is why a softphone can be a customer. A real deployment puts its own trunk
files there instead and mounts its gateway into `conf/sip_profiles/external/` as
a third one. `deploy/dev/freeswitch/` is a worked example of both, and
[freeswitch/README.md](../freeswitch/README.md) §3 explains the seam.

## Recordings

The `recordings` volume is shared between the switch and the application, with
`AICC_RECORDING_BACKEND=FS`: the switch writes, the application serves what it
finds. Files are keyed `recordings/YYYY/MM/DD/<call_id>.wav`, stereo, caller
left, bot or agent right. Retention is a product setting, applied by a daily job.
Any S3-compatible store works too: set `AICC_RECORDING_BACKEND=S3` and the
`AICC_S3_*` lines in `.env`, and the application uploads each recording on
hangup, clears the spool and plays it back through a presigned URL.

## Upgrading

`git pull && docker compose up -d --build`, or where `AICC_IMAGE` names a
published tag, change it and `docker compose pull && docker compose up -d`.
Migrations run at startup and are forward-only; a
single-instance advisory lock means two cannot race. The switch's configuration
is versioned with the schema, so `AICC_FS_IMAGE` does not normally move with
it; the release notes say when it does, and
[freeswitch/README.md](../freeswitch/README.md) covers installing that switch by
hand instead of pulling it.

## Before it faces anyone

- [ ] Every password changed: PostgreSQL, `aicc_lua`, ESL, and every seeded
      account. They are published in this file.
- [ ] `AICC_SEED=` (empty) in `.env`, so none of the demo dataset lands on a
      host other people can reach. Where a database already holds it,
      `AICC_SEED=fresh` once removes exactly what the seed created.
- [ ] TLS in front, which this stack does not cover. Terminate at a reverse
      proxy given the whole of `/` (API, event stream and interface are one
      origin by design) and set `AICC_SECURE_COOKIES=true`, or the session
      cookie is never sent back. That proxy needs response buffering **off** and
      a read timeout longer than a quiet stream: with buffering on, Server-Sent
      Events arrive in batches; with a short timeout, browsers reconnect forever.
- [ ] `AICC_METRICS_ADDR` reachable only inside the stack. That listener has no
      authentication of its own; the compose file publishes no port for it.
- [ ] SIP and the RTP range reachable by the phones that need them and nothing
      else, through a host firewall. The AI leg's own SIP and RTP are published
      nowhere and need no rule.
- [ ] Every issued API key named for the integration that holds it, with only
      the scopes that integration needs. A leaked key is revoked on its own.
- [ ] Provider keys in the environment, never in the database, never in a flow.
- [ ] Provider concurrency quota raised to match the traffic. It is an external
      limit and no amount of local capacity substitutes for it.
