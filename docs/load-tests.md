# Load tests

The capacity budget claims 200 concurrent AI calls and 50 agents on 8 cores and
16 GB ([design 06](design/06-capacity.md)). These are the runs that check it,
in the order they are worth running: each one adds a component and keeps the
previous one's criteria.

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

`AICC_METRICS_ADDR` exposes the capacity budget's own numbers (design 06 §6):

| Metric | Reads on |
|---|---|
| `aicc_calls_active{kind}` | `SWITCH` is every call the switch carries, `BOT` the subset the model answers; they overlap |
| `aicc_rtp_late_ticks_total` / `aicc_rtp_frames_sent_total` | The ratio is the late-tick rate the budget bounds at 0.1% |
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
| **L1 micro** | `go test -run XXX -bench . -benchmem ./internal/media/ ./internal/aicall/` | 0 allocs/op on every frame path | **Passing**, and enforced by CI on every change |
| **L2 synthetic 200** | loadgen ×200 → aicc + mock, 30 min | CPU < 50% of 8 cores; RSS < 1 GB; late ticks < 0.1%; goroutines return to baseline | **Passing** — see below |
| **L3 FreeSWITCH interop** | FS `originate` loop → gateway → aicc ×200 + mock | L2's criteria, plus no ESL event lag > 250 ms and the switch inside design 06 §5 | Not yet run |
| **L4 real providers** | 20 real OpenAI or Qwen calls, 1 h soak | Turn latency ≤ 1.2 s p50 and ≤ 2 s p95; no unexplained WebSocket drops | Not yet run at soak length; the latency gate itself was measured live in M3 (p50 ≈ 1.23 s) |
| **L5 acceptance** | 200 AI (mock) + 50 sipp agent calls + 60 SSE clients + report queries, 2 h | Everything above, plus no slow-consumer disconnects and CDR/report p95 < 500 ms | Not yet run |

L3 to L5 need hardware this was not run on — L3 and L5 in particular want the
8-core target machine rather than a developer laptop, and L5 needs `sipp` and a
FreeSWITCH that is not also the development switch. They are runbooks below,
not results.

## L2 — 200 synthetic calls

**Result (2026-08-16, Apple M-series, 16 cores / 128 GB, macOS): passing.**

Read the CPU figure against the budget's *absolute* allowance rather than as a
percentage: 50% of 8 cores is 4 cores busy, and that is the number to beat on
any machine.

| | Budget | Measured |
|---|---|---|
| Concurrent calls | 200 | 200, sustained for 30 minutes |
| Calls completed | — | 1,600 placed, 1,600 answered, 0 failed |
| CPU | < 4 cores | **0.51 cores** average, 1.32 peak |
| RSS | < 1 GB | **356 MB** peak |
| Late downlink frames | < 0.1% | **0.009%** of 18.6 M frames received |
| Goroutines after | back to baseline | 16 before, 17 after |
| Call setup | — | 0 ms p50, 2 ms p95 |
| First audio | — | 20 ms p50, 22 ms p95 (against the mock) |
| Turns served | — | 37,286, none abandoned |

Each of the 200 slots recycled its call every four minutes, so the run is 1,600
complete setups and teardowns with their ledger writes — which is what makes
this a leak test and not a snapshot. The process uses about an eighth of its
CPU budget and a third of its memory budget at the full concurrency target.

Two things this run produced beyond the numbers:

* **It found a real bug.** About 1% of turns were being reported as abandoned
  mid-sentence while the model had finished cleanly — a dropped completion
  signal inside a burst of audio deltas. Fixed, and the numbers above are from
  the fixed build. [m5-findings §1](design/m5-findings.md).
* **An open item.** 100 of the 1,600 calls (6%) were ended by the UAS's
  dead-media watchdog after ~28 s of silence, while the generator went on
  sending uplink successfully. It costs no pass criterion — the calls are
  counted, the late-frame rate is inside budget — but the two sides disagree
  about where the audio went, and that is worth resolving before L3 builds on
  this harness. [m5-findings §5](design/m5-findings.md).

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
* **`-total` longer than `-duration` recycles calls.** That is what makes L2 a
  leak test: 200 slots turning over every four minutes for half an hour is
  about 1,500 complete setups and teardowns, each with its ledger write.

## L3 — through FreeSWITCH

Same mock provider, but the calls arrive the way real ones do.

```sh
# On the switch, in a loop, 200 times with a stagger:
fs_cli -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://240000)"
```

Adds to L2's criteria:

* ESL event lag under 250 ms — the application logs it, and a growing lag means
  the event loop is behind the switch.
* The switch itself inside design 06 §5: no transcoding on bot legs (they are
  PCMU-pinned), and `max-sessions` / `sessions-per-second` not reached.

What this stage catches that L2 cannot: the dialplan, the gateway, the
correlation headers, the CDR assembler's ownership rule, and recording — none
of which the load generator exercises.

## L4 — real providers

Twenty concurrent calls, one hour, against the real endpoint. Not two hundred:
the point is fidelity, not volume, and the account's concurrency quota is an
external limit that no amount of local capacity substitutes for.

* `aicc_turn_latency_ms` p50 ≤ 1.2 s and p95 ≤ 2 s (A3). Note what the
  histogram measures: it starts at `speech_stopped`, *after* the VAD's silence
  hold, so a 500 ms hold means ≤ 700 ms on the histogram.
* `aicc_provider_ws_errors_total` flat. A vendor dropping sockets under
  sustained load is exactly what an hour finds and ten minutes does not.

M3 measured the latency gate live at p50 ≈ 1.23 s end to end against Qwen, on
the line with the budget and inside it with the silence hold at 400 ms. The
soak has not been run.

## L5 — acceptance

Everything at once, for two hours: 200 AI calls on the mock, 50 agent calls
driven by `sipp` against real extensions, 60 browser sessions on the event
stream, and the reports being queried throughout.

* Every L2 and L3 criterion.
* No slow-consumer disconnects on the event stream. The hub drops a subscriber
  that cannot keep up, which is correct behaviour and a failure of this stage.
* CDR and report queries p95 under 500 ms while the ledger is being written to
  at full rate.
