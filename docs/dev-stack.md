# Running it locally

What has to be up before a call can be placed, in the order it has to come up,
and the two tools that make a bot walkthrough mean anything.

`CLAUDE.md` lists the commands; this is the runbook that says which of them you
need, when, and what breaks if you skip one.

## What has to be running

| | Where | Started by |
|---|---|---|
| PostgreSQL 18 | `127.0.0.1:5432` | `deploy/dev/docker-compose.yml` |
| SeaweedFS (recordings) | filer `127.0.0.1:8888`, S3 `127.0.0.1:8333` | the same compose file |
| FreeSWITCH | ESL `127.0.0.1:18021`, SIP `5060` | a native install on the development box; started outside this repo |
| The application | HTTP/SPA `:8080` | `/tmp/aicc` |

Order matters only at the first hop: the app runs its migrations at startup and
will not come up without the database. FreeSWITCH can start before or after —
the ESL link reconnects on its own — but the human path is dead until it does.

**FreeSWITCH is not this repository's to start — but its configuration is this
repository's.** The process is a native install on this box, started and stopped
outside the repository (`pgrep -fl freeswitch`;
`/usr/local/freeswitch/bin/freeswitch -nc -nonat`). What it reads is ours:
`freeswitch/conf/` is the complete configuration tree and `freeswitch/scripts/`
the Lua that serves the directory and the queues from PostgreSQL, and
[`freeswitch/README.md`](../freeswitch/README.md) §2 says how to install both
onto a native switch and which placeholders a copy leaves to fill in. The demo
runs the same tree inside this repository's own image; the dev box does not run
that container.

One thing on this box is *not* in `freeswitch/`: the simulated PSTN trunk. It
belongs to a deployment rather than to the product, so it lives in
[`deploy/dev/freeswitch/`](../deploy/dev/freeswitch/README.md) with its own
installer — a dialplan fragment for the `aicc` include seam, the gateway that
names the peer, and the `vars.xml` lines both need.

### Ports this stack expects to own

```
:8080              application HTTP + embedded SPA
:6060              SIP UAS for the AI leg (UDP)
40000–40999        RTP for the AI leg (even/odd pairs)
127.0.0.1:8090     transcription ingest WebSocket (mod_audio_stream connects here)
127.0.0.1:9090     Prometheus metrics
127.0.0.1:5432     PostgreSQL
127.0.0.1:8888     SeaweedFS filer      (FreeSWITCH records straight to it)
127.0.0.1:8333     SeaweedFS S3         (the application reads from it)
127.0.0.1:18021    FreeSWITCH ESL       (not the stock 8021)
```

`127.0.0.1:9090` is worth remembering: it is the metrics listener's default and
also the port most other services reach for first. Two processes can bind it on
different address families and both appear to work, which is a confusing state
to debug — move the metrics listener with `AICC_METRICS_ADDR` rather than
leaving the ambiguity.

## Bringing it up

```sh
docker compose -f deploy/dev/docker-compose.yml up -d   # PostgreSQL + SeaweedFS
cp .env.example .env                                    # then uncomment what changes
go build -o /tmp/aicc ./cmd/aicc && /tmp/aicc
```

`.env.example` is the registry of every setting with its real default. An empty
value means *unset* there, so a non-empty default cannot be blanked by leaving
the variable empty — the setting's own comment says what to write instead.

Logs go to stdout **and** to `logs/aicc-<starttime>.log`. Read the file when
analysing a run; it is the only copy that survives the terminal.

An empty database needs a first account, and a DID needs a flow:

```sh
/tmp/aicc useradd -username admin -password … -role ADMIN
/tmp/aicc flowadd -file internal/seed/flows/novanet_support.json -did 95001
```

Or take the whole demo dataset instead — accounts, queues, six published
bilingual flows, each behind an English and a Chinese number (95001/95002 …
95051/95052), and a week of history — with `AICC_SEED=demo` on one boot.
`AICC_SEED=fresh` removes exactly that again. Existing data always wins, so a
second boot with `demo` changes nothing.

## The business backend a bot walkthrough needs

The reference flows in `internal/seed/flows` reach a backend for their facts: a
repair order, an overdue bill, an appointment slot. With no backend configured
every one of those tools fails — which is the **correct** behaviour, because a
bot must not invent facts, and equally useless for judging whether the bot can
hold a conversation.

`cmd/aicc-mockbackend` supplies the facts:

```sh
go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770
AICC_BOT_BACKEND_BASE=http://127.0.0.1:8770 /tmp/aicc   # or set it in .env
```

Its fixtures are the reference ones, so a walkthrough has a determinate right
answer — RMA1001 is in repair, 92223333 is eight days overdue, 10086002 is
inside its contract and cannot change plan. That is what lets a reader tell
"the bot said the wrong thing" from "the bot said what the data says". State
lives in memory and only where a flow observes its own writes (an appointment
booked, rescheduled, then confirmed); restarting resets it, which is what a
walkthrough wants.

The address comes only from `AICC_BOT_BACKEND_BASE`. A flow's `apiBaseEnv`
field does not take effect.

**A failing tool is not a broken bot.** `connection refused` on
`127.0.0.1:8770` means this process is not running; the bot handling it
gracefully and offering something else is the flow design working, not a defect
to chase.

## Load-testing tools

`cmd/aicc-mockprovider` is a Realtime *server*, reached through the same
endpoint override a real deployment would use, and `cmd/aicc-loadgen` places
the calls. Both, and the plan they serve, are in `docs/load-tests.md`. Their
numbers are not results — see the performance-claim directive in `CLAUDE.md`.

## Pointing the AI leg at a Realtime gateway

A gateway is a provider like any other (`docs/provider-extension.md`):

```sh
AICC_PROVIDER=gateway
AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9090/v1/realtime   # empty keeps :8080
REALTIME_API_KEY=…                # the credential that gateway accepts
AICC_TRANSCRIBE_PROVIDER=qwen     # say it: the gateway is not a recogniser
```

Two things bite here. The gateway is a conversation endpoint and not a
recognition client, so `AICC_TRANSCRIBE_PROVIDER` has to be named explicitly or
the server refuses to start looking for one that does not exist. And a gateway
on `9090` collides with the metrics listener above — move one of them.

Switching back is not only `AICC_PROVIDER`: leave `AICC_PROVIDER_ENDPOINT` set
and the next provider will faithfully dial the gateway that is no longer there.
