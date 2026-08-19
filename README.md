# AI Native Call Center

An open-source call center where the AI is the default answer, not an add-on.

Calls arrive at FreeSWITCH and are answered by a voice model over a
speech-to-speech connection the application terminates itself. The model talks;
a flow steers the conversation and decides when a person is needed. When one
is, the caller is transferred into a real queue where real agents are waiting
with a browser softphone. Everything the two halves do lands in one call
record.

It is **one Go binary** — REST API, event stream and the whole web interface
inside it — plus PostgreSQL and FreeSWITCH.

[简体中文](README.zh-CN.md) · [Deploying](deploy/README.md) ·
[Design](docs/design/00-overview.md)

## Try it

```sh
git clone https://github.com/rasonyang/ai-native-callcenter
cd ai-native-callcenter/deploy/demo
docker-compose up -d
```

<http://127.0.0.1:8080>, sign in as `admin` / `aicc@12345`. The database, the
switch and the application come up together, seeded with a team, two queues, a
published bilingual flow behind two numbers, and a week of history so the
wallboard is not empty. Put an `OPENAI_API_KEY` in `.env` and it answers the
phone. [More about the demo](deploy/demo/README.md).

## What it does

**Answers with a model, not a menu.** The AI leg is a SIP endpoint inside the
application: FreeSWITCH bridges the caller to it and audio goes straight to the
provider — G.711 passed through byte for byte where the provider accepts it, so
nothing decodes or resamples on the way.

**Steers without scripting the conversation.** The model owns the dialogue; the
flow owns the phase. A phase carries instructions and a list of tools the model
may use; transitions fire on tool results. The built-in tools may *refuse* —
"the queue is closed" is something to talk about, not an error — and the
persona, the rules and the bot's voice are published and versioned together.

**Hands over to people properly.** Transfers go into `mod_callcenter` queues
with the caller's context attached, so the agent's screen has already popped
when the phone rings. Agents work in the browser: presence, softphone bar,
callbacks. Supervisors get a live wallboard, the queue view and the roster.

**Keeps one record per conversation.** A call that a bot answered, handed to a
queue and an agent finished is one CDR with one transcript and one recording —
not three fragments. Recordings go to a filesystem or to any S3-compatible
store.

**Speaks two languages, and admits which provider it runs.** English and
Chinese throughout, interface and bot. One provider answers every call in a
deployment, chosen at startup: `qwen` inside mainland China, `openai`
elsewhere. A call's language never selects it.

## How it fits together

```
                    ┌──────────── one Go binary ────────────┐
  caller ──▶ FreeSWITCH ──▶ SIP UAS ──▶ provider (Realtime, speech-to-speech)
                 │            │
                 │            └─ flow engine: phases, tools, transfers
                 │
                 ├─ mod_callcenter queues ──▶ agents (browser softphone)
                 │
                 └─ ESL ──▶ call registry ──▶ REST + SSE ──▶ web interface
                                                    │
                                              PostgreSQL
```

FreeSWITCH reads its directory, its dialplan and its queues *from the
database*, through Lua. Adding an extension, a queue or a number is a database
change; the switch is configured once and never edited again.

Two design notes worth knowing before reading the code:

* The domain model is Genesys-lineage. A **call** aggregates **parties**; leg
  events are `PARTY_*`, call-scoped ones are `CALL_*`.
* Every live call is an actor — one goroutine as its sole mutator, snapshots by
  mailbox. There are no locks around call state because there is no shared call
  state.

The full design is in [`docs/design/`](docs/design/), starting with
[the overview](docs/design/00-overview.md).

## Building

```sh
make dev-up            # PostgreSQL in Docker
cd web && npm install && npm run build && cd ..
make build             # bin/aicc, with the interface embedded
./bin/aicc useradd -username admin -password '…' -role ADMIN
./bin/aicc
```

For frontend work, `make web-dev` runs Vite on 5173 against the API on 8080.

```sh
go test -race ./...    # always -race; it has caught real bugs here
make lint              # go vet, gofmt, oxlint
make api-check         # the API contract gate
```

The HTTP API is spec-first: [`docs/openapi.json`](docs/openapi.json) is the
single source of truth, and the Go server and the TypeScript client are
generated from it. Edit the contract, run `make api-generate`, then implement.
Never the other way round.

## Extending it

* **A new voice provider** is a profile, not a client:
  [`docs/provider-extension.md`](docs/provider-extension.md).
* **A new flow** is a JSON document validated at load — see
  [`internal/seed/flows/`](internal/seed/flows/) for a working bilingual one.
* **A new screen** follows the design system in
  [`web/CLAUDE.md`](web/CLAUDE.md), which is binding rather than advisory.

## Status

The human path, the AI path, the product surface and the packaging are built
and verified against live FreeSWITCH and live providers.

Performance is not yet a claim this project makes. The design has a capacity
budget and a latency target ([design 06](docs/design/06-capacity.md)), and the
harness to test them against is in the repository
([docs/load-tests.md](docs/load-tests.md)) — but the benchmark campaign itself
is still to come, so treat the budget as an intention rather than a
measurement.

What is deliberately *not* here, and will not be: any cascaded
ASR + LLM + TTS pipeline inside this process. That composition belongs in a
separate service speaking the same protocol.

## License

Apache-2.0. See [LICENSE](LICENSE).
