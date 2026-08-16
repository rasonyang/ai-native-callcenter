# The demo stack

PostgreSQL, FreeSWITCH and the application, seeded and wired to each other.
One command from a clone to a call.

```sh
cd deploy/demo
cp .env.example .env        # optional; every default is already the one used
docker-compose up -d        # builds the application image on first run
```

Then open <http://127.0.0.1:8080> and sign in as `admin` / `demo1234`.

The first boot builds the frontend and the Go binary, which takes a few
minutes. Afterwards the stack starts in seconds.

## What is running

| Container | What it is |
|---|---|
| `aicc-demo-postgres` | PostgreSQL 18, with the `aicc` database and mod_callcenter's own `aicc_fs` beside it |
| `aicc-demo-app` | The product: REST API, event stream, embedded SPA, and the SIP endpoint the AI calls land on |
| `aicc-demo-lua-role` | Runs once, creates the confined role the switch reads the database with, exits |
| `aicc-demo-freeswitch` | FreeSWITCH 1.10.12, configured from this repository at every boot |

Only the screens are published, on `127.0.0.1:8080`. Everything an AI call
touches — the number, the dialplan, the gateway, the SIP leg, the RTP — stays
inside the compose network, so the demo needs nothing from the host but Docker.

## What is seeded

`AICC_SEED=demo` fills an empty database (design 03 §6). Existing data always
wins: a second boot changes nothing.

| | |
|---|---|
| Accounts | `admin` (administrator), `sam` (supervisor), `amy` / `ben` / `cara` (agents) — password `demo1234` |
| Extensions | 1000, 1001, 1002, SIP password `demo1234` |
| Queues | `support-en` on 7001, `support-zh` on 7002 |
| Flow | `novanet_support`, published, bilingual |
| Numbers | 95001 answers in English, 95002 in Chinese; both fall back to the queue of their language |
| History | Seven deterministic days of calls, queue events and presence, so the wallboard and the reports are not empty |

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
4. Register extension 1000 with password `demo1234` against the host, then
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

* The FreeSWITCH image is a third-party build (`dheaps/freeswitch`, pinned by
  digest) chosen because it carries `mod_callcenter`, `mod_lua` and
  `mod_pgsql`. It is **amd64 only** — on Apple Silicon it runs under emulation,
  which is fine for a demo and not for load.
* It is FreeSWITCH 1.10.12, where `uuid-version` does not exist yet, so channel
  identifiers are UUIDv4 rather than v7. Nothing depends on their ordering. The
  development box runs 1.11.1, which does honour it.
