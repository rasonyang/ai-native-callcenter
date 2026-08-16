# Load tests

How this project intends to find out what it costs to run. The stages are in
the order they are worth running: each adds a component and keeps the previous
one's criteria.

**Status: nothing here has been run as a benchmark, and this project publishes
no performance figure.** What exists today is the harness and this plan. A
shakedown run was done while building the harness — it is what found the bug in
[m5-findings §1](design/m5-findings.md) — but it was aimed at exercising the
code, not at measuring it, and its numbers are not results and are not quoted
as any.

Thresholds are deliberately absent below. Deciding what "good" is belongs to
the campaign, against the machine it actually runs on, and a number written
down beforehand has a way of becoming a claim before anything has measured it.

## The harness

Two commands, both built from this repository and neither shipped in a release.

**`aicc-mockprovider`** answers as a voice provider. It speaks the Realtime
protocol over a WebSocket, so the application reaches it the way it reaches any
provider — by endpoint — and runs its real client against it, base64 audio path
and all. Nothing in the application knows it is under test, and 200
conversations cost nothing.

```sh
go run ./cmd/aicc-mockprovider -addr 127.0.0.1:9099 \
    -turn-every 10s -turn-audio 5s -first-audio 400ms -speech-before 300ms
```

**`aicc-loadgen`** is the switch's side of the bot leg: an INVITE with the
`X-AICC-*` headers the dialplan puts on one, a PCMU offer, 20 ms frames uplink,
and a stopwatch on every frame downlink.

```sh
go run ./cmd/aicc-loadgen -target 127.0.0.1:6060 -did 95001 \
    -calls 200 -ramp 60s -duration 240s -total 30m
```

Point the application at the mock and let it run:

```sh
AICC_PROVIDER=openai \
AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9099/v1/realtime \
OPENAI_API_KEY=not-a-key \
./aicc
```

> Design 06 §7 described the mock as an in-process `VoiceSession` fake. A fake
> at that seam would skip the WebSocket, the JSON and the base64 — the layers a
> load test exists to stress — and the endpoint override is already a supported
> way to reach anything speaking the protocol. The mock is a server instead.

## What to watch while it runs

`AICC_METRICS_ADDR` exposes what the process knows about itself (design 06 §6):

| Metric | Reads on |
|---|---|
| `aicc_calls_active{kind}` | `SWITCH` is every call the switch carries, `BOT` the subset the model answers; they overlap |
| `aicc_rtp_late_ticks_total` / `aicc_rtp_frames_sent_total` | The ratio is the late-tick rate — send ticks the caller would hear as a stutter |
| `aicc_jitter_lost_total` / `_dropped_total` / `_filled_total` | Inbound media health |
| `aicc_turn_latency_ms{provider}` | Caller stopped speaking to first reply frame |
| `aicc_provider_first_audio_ms{provider}` | The provider's share of that |
| `aicc_provider_ws_errors_total` | Sessions that ended on an error |
| `go_goroutines`, `go_memstats_heap_inuse_bytes` | Leaks, and the RSS budget |

Goroutines are the leak detector: they must come back to their pre-load
baseline within a minute of the last call ending.

## Stages

| Stage | Setup | Pass criteria | State |
|---|---|---|---|
| Stage | Setup | What it establishes | State |
|---|---|---|---|
| **L1 micro** | `go test -run XXX -bench . -benchmem ./internal/media/ ./internal/aicall/` | Frame paths allocate nothing per frame | The one stage that runs continuously: CI holds it on every change |
| **L2 synthetic** | loadgen → aicc + mock provider, 30 min | What the process alone costs per call, and whether it lets go of anything | Harness ready, not run |
| **L3 FreeSWITCH interop** | FS `originate` loop → gateway → aicc + mock | The same with the switch in the path: dialplan, gateway, correlation headers, CDRs, recording | Not run |
| **L4 real providers** | Real OpenAI or Qwen calls, 1 h soak | Turn latency against a real vendor, and whether it holds a socket for an hour | Not run |
| **L5 acceptance** | AI calls + `sipp` agent calls + event-stream clients + report queries | Everything at once, which is the only configuration a deployment is ever in | Not run |

Each needs a machine this project does not have to hand: a host that is not a
developer laptop, a FreeSWITCH that is not also the development switch, `sipp`,
and for L4 real provider credit. Running them anywhere else and publishing the
numbers would produce a figure that flatters or maligns the software for
reasons that have nothing to do with it.

## L2 — the process on its own

Concurrent calls against the application and the mock provider, no switch in
the path. It bounds the cost of the process itself, and because each slot
recycles its call it is a leak test as much as a load test: half an hour at
four-minute calls is over a thousand complete setups and teardowns, each with
its ledger write.

What to record: CPU as *cores busy* rather than as a percentage of whatever
machine it ran on, peak RSS, the late-frame rate the generator reports, and
goroutines before and after. Report the machine alongside them, always.

### Running it

```sh
# PostgreSQL up, and a number with a published flow behind it (AICC_SEED=demo
# provides 95001). FreeSWITCH is not involved: the load generator is the switch.
go run ./cmd/aicc-mockprovider -addr 127.0.0.1:9099 \
    -turn-every 10s -turn-audio 5s -first-audio 400ms -speech-before 300ms &

AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9099/v1/realtime OPENAI_API_KEY=not-a-key \
AICC_BOT_MAX_CALLS=250 AICC_ESL_ADDR=127.0.0.1:1 ./aicc &

go run ./cmd/aicc-loadgen -target 127.0.0.1:6060 -did 95001 \
    -calls 200 -ramp 60s -duration 240s -total 30m
```

Two things to get right or the run measures the wrong thing:

* **Keep each call shorter than the flow's turn budget.** The demo flow allows
  30 turns; at a turn every 10 seconds a 240-second call uses 24 of them. A
  longer call ends on the flow rather than on the load generator, and the run
  quietly becomes a test of something else.
* **`-total` longer than `-duration` recycles calls.** That is what makes this
  a leak test rather than a snapshot: every slot turns over repeatedly, each
  turnover a complete setup and teardown with its ledger write.

## L3 — through FreeSWITCH

Same mock provider, but the calls arrive the way real ones do.

```sh
# On the switch, in a loop, 200 times with a stagger:
fs_cli -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://240000)"
```

Adds to what L2 records:

* ESL event lag — the application logs it, and a growing lag means the event
  loop is falling behind the switch.
* The switch's own cost: whether bot legs stay PCMU-pinned and untranscoded,
  and how close `max-sessions` / `sessions-per-second` come to their limits.

What this stage catches that L2 cannot: the dialplan, the gateway, the
correlation headers, the CDR assembler's ownership rule, and recording — none
of which the load generator exercises.

## L4 — real providers

A modest number of calls for an hour against the real endpoint. The point is
fidelity, not volume — and the account's concurrency quota is an external limit
that no amount of local capacity substitutes for.

* `aicc_turn_latency_ms`, with the caveat about what it measures: the window
  starts at `speech_stopped`, *after* the VAD's silence hold, so the hold has
  to be added back before comparing it to what a caller experiences.
* `aicc_provider_ws_errors_total`. A vendor dropping sockets under sustained
  load is exactly what an hour finds and ten minutes does not.

## L5 — acceptance

Everything at once, for hours: AI calls on the mock, agent calls driven by
`sipp` against real extensions, browser sessions on the event stream, and the
reports being queried throughout.

* Everything L2 and L3 record.
* Slow-consumer disconnects on the event stream. The hub drops a subscriber
  that cannot keep up, which is correct behaviour and a signal that this
  configuration has found a limit.
* CDR and report latency while the ledger is being written to at full rate.
