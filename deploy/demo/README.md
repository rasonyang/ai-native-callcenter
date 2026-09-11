# The demo stack

PostgreSQL, FreeSWITCH and the application, seeded and wired to each other.
One command from a clone to a call.

```sh
cd deploy/demo
cp .env.example .env        # optional; every default is already the one used
docker-compose up -d        # builds the application image on first run
```

Then open <http://127.0.0.1:8080> and sign in as `admin` / `aicc@12345`.

The first boot builds the frontend and the Go binary, which takes a few
minutes. Afterwards the stack starts in seconds.

## What is running

| Container | What it is |
|---|---|
| `aicc-demo-postgres` | PostgreSQL 18, with the `aicc` database and mod_callcenter's own `aicc_fs` beside it |
| `aicc-demo-app` | The product: REST API, event stream, embedded SPA, and the SIP endpoint the AI calls land on |
| `aicc-demo-lua-role` | Runs once, creates the confined role the switch reads the database with, exits |
| `aicc-demo-freeswitch` | FreeSWITCH v1.11.3 — this repository's own image, configuration and all |

Only the screens are published, on `127.0.0.1:8080`. Everything an AI call
touches — the number, the dialplan, the gateway, the SIP leg, the RTP — stays
inside the compose network, so the demo needs nothing from the host but Docker.

## What is seeded

`AICC_SEED=demo` fills an empty database (design 03 §6). Existing data wins
everywhere but one: the seeded accounts have their password and role **reset**
on every boot, so "run the seed" is always the answer to "I cannot sign in".
Everything else — queues, numbers, an agent's phone, their staffing, the
history — is left exactly as it is.

| | |
|---|---|
| Accounts | `admin` (administrator), `supervisor` (supervisor), `wei` / `amy` / `ben` (agents) — password `aicc@12345` |
| Extensions | `wei` 1001, `amy` 1000, `ben` 1002 — SIP password `aicc@12345` |
| Staffing | `wei` and `amy` on `support-en`, `ben` on `support-zh`, so a queued call actually reaches somebody |
| Queues | `support-en` on 7001, `support-zh` on 7002 |
| Flows | Six, all published and bilingual — the Nth answers on 950N1 in English and 950N2 in Chinese, each falling back to the queue of its language |
| Numbers | `novanet_support` 95001 / 95002 · `mobile_support` 95011 / 95012 · `plan_change` 95021 / 95022 · `early_collections` 95031 / 95032 · `field_service_appointment` 95041 / 95042 · `lead_qualification` 95051 / 95052 (English first) |
| History | Seven deterministic days of calls, queue events and presence, so the wallboard and the reports are not empty |

The five business flows fetch their facts from a backend, so without
`AICC_BOT_BACKEND_BASE` pointing at a running `cmd/aicc-mockbackend` every one
of their tools fails and the bot offers a transfer to a human instead — the
correct behaviour, because a bot must not invent a repair order or an overdue
bill (see [docs/dev-stack.md](../../docs/dev-stack.md)).

`AICC_SEED=fresh` removes all of that again — the only way back to an empty
product once the volume exists:

```sh
AICC_SEED=fresh docker-compose up -d aicc     # clears it
docker-compose up -d aicc                     # seeds it again
```

Only what the seeder created is removed; real calls placed against the demo
stay. That also means re-seeding after a reset restores the accounts, queues,
flow and numbers but *not* the seven days of history — history is only ever
generated into an empty ledger.

## Placing a call

Without a provider key the switch, the flow and the ledger all work; only the
bot has nothing to say. Set `OPENAI_API_KEY` (or `ALIYUN_API_KEY` with
`AICC_PROVIDER=qwen`) in `.env` and restart `aicc` to hear it answer.

From inside the stack:

```sh
docker exec aicc-demo-freeswitch fs_cli -H 127.0.0.1 -P 18021 -p ClueCon \
    -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://20000)"
```

The call appears live on the wallboard, and as a CDR with a recording when it
ends.

From a real softphone, which needs media to reach off this machine:

1. Set `FS_EXTERNAL_IP` in `.env` to the host's LAN address.
2. Uncomment the RTP port range in `docker-compose.yml`.
3. Set `SIP_BIND=0.0.0.0` if the phone is on another machine.
4. Register extension 1000 with password `aicc@12345` against the host, then
   dial 95001.

## Reaching into it

```sh
docker-compose logs -f aicc                                   # the application
docker exec aicc-demo-freeswitch fs_cli -H 127.0.0.1 -P 18021 -p ClueCon
docker exec -it aicc-demo-postgres psql -U aicc aicc
docker-compose down                                           # stop
docker-compose down -v                                        # stop and forget everything
```

## What this is not

A deployment. The passwords are in this file, the database has no TLS, and the
event socket trusts the whole private network. Read [../README.md](../README.md)
before putting any of it somewhere other people can reach.

Two more things worth knowing:

* The FreeSWITCH image is built from this repository — `freeswitch/`, which
  holds the whole configuration tree and the Dockerfile that compiles
  FreeSWITCH v1.11.3 around it. The published tag is
  `rasonyang/freeswitch-aicc:v1.11.3` and it is multi-arch,
  `linux/amd64` and `linux/arm64`, so nothing here runs under emulation. Set
  `AICC_FS_IMAGE` to try a locally built one (`make fs-image`).
* Live transcription of the human phase is possible in the demo: the image
  carries `mod_audio_stream`. It stays **off** by default, because turning it
  on also needs a recogniser credential and the transcribe settings
  (`AICC_TRANSCRIPTION_ENABLED`, `AICC_STREAM_PUBLIC_URL`,
  `AICC_STREAM_SECRET`, `AICC_TRANSCRIBE_*` — the repository's own
  [`.env.example`](../../.env.example) is the registry), and the demo has to
  work with none of them. With it true and the module missing, the switch
  refuses to start rather than leaving every transcript panel saying
  "Connecting…" for ever.
