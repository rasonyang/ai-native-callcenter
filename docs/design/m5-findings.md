# M5 findings — packaging, and what running it two hundred times over showed

The evidence record for the packaging milestone, in the same role
[`m0-findings.md`](m0-findings.md) and [`m4-cleanup-findings.md`](m4-cleanup-findings.md)
play for theirs. Everything here was found by running the thing, not by reading
it. Where a design document said otherwise, it has been amended in place — 03
§6 for the seed, 06 §8 for the capacity metrics and the harness.

## 1. The load test found a bug in the product

At 200 concurrent calls, about **1% of turns were closed out as "the provider
stopped partway through speaking" while the provider had finished cleanly**.
Fixed; the reasoning is worth keeping because the failed assumption is a
tempting one.

A turn arrives in a burst — fifty audio deltas back to back. The response
watchdog is driven by progress signals sent over a small channel that the read
loop is never allowed to block on, so signals are dropped when the watchdog is
momentarily behind. The comment on that channel promised a dropped signal
"only costs a spurious timeout, never a wrong one". It does not: the
*completion* signal rides in the same burst, and when it is the one dropped,
the timer stays armed on a response that already ended and fires two seconds
later.

The watchdog now reads whether a response is open from state rather than
inferring it from having seen every signal. Verified by re-running the same
load: 51 abandonments before, none after, over 2,403 turns.

Two things about how this was found are worth noting. It only appears under
burst delivery — with deltas paced at real time the rate is exactly zero, which
is why no test had caught it. And it was invisible in production terms: the
caller still heard the audio, and only the turn bookkeeping was wrong, which is
precisely the class of bug that a synthetic run at scale finds and a phone call
does not.

Two earlier hypotheses were wrong and were reverted rather than left in: that
the send queue's backpressure was blocking the read loop (the queue holds 30
seconds and drops rather than blocks), and that the stall clock should start at
the socket rather than at the audio. Both produced plausible-looking fixes that
did not move the number. What moved it was instrumenting the abandonment with
`sinceLastFrameMs` and `isHandingOff` and reading what came back.

## 2. L2 — 200 synthetic calls, 30 minutes

Run on an Apple M-series laptop (16 cores, 128 GB) rather than the 8-core
target, so the CPU figure is read against the budget's absolute allowance —
50% of 8 cores is 4 cores busy — not as a percentage of this machine.

| | Budget | Measured |
|---|---|---|
| Concurrent calls | 200 | 200, sustained |
| Calls completed | — | 1,600 placed, 1,600 answered, 0 failed |
| CPU | < 4 cores | **0.51 cores** average, 1.32 peak |
| RSS | < 1 GB | **356 MB** peak |
| Late downlink frames | < 0.1% | **0.009%** of 18.6 M frames received |
| Goroutines after | back to baseline | 16 before, 17 after |
| Call setup | — | 0 ms p50, 2 ms p95 |
| First audio | — | 20 ms p50, 22 ms p95 (mock provider) |
| Turns served | — | 37,286, none abandoned |

Each of the 200 slots recycled its call every four minutes, so the run is 1,600
complete setups and teardowns with their ledger writes — which is what makes it
a leak test rather than a snapshot.

The margin is large enough to be worth stating plainly: the process uses about
an eighth of its CPU budget and a third of its memory budget at the full
concurrency target. The parts of the budget still unproven are the ones L3 to
L5 cover — FreeSWITCH in the path, real providers, and everything at once.

### Measuring it wrong first

The first CPU figure was 4.2 cores for ten calls, which is absurd on its face
and was: `ps -o time=` prints `M:SS.ss` on macOS, and the parser read the
minutes field as hours. A number that surprising is a bug in the measurement
until proven otherwise.

## 3. The demo stack

A stock third-party FreeSWITCH image (`dheaps/freeswitch`, pinned by digest,
1.10.12, carrying `mod_callcenter`/`mod_lua`/`mod_pgsql`) turned into ours at
every boot by an entrypoint hook. Six things had to be found by running it.

**`install` does not exist in that image's busybox** — and a missing command
does not reliably abort a sourced `ash` subshell under `set -e`. The hook
printed "switch configuration applied", FreeSWITCH started, the modules loaded,
and *nothing had been copied*: the switch ran the vanilla configuration and
looked healthy. The hook now uses `cp` and verifies every file it claims to
have installed, failing the boot if one is missing. A configuration step that
can silently do nothing is worse than one that crashes.

**`listen-ip ::` fails on a compose network.** mod_event_socket answers with
"Cannot get information about IP address ::", ends its thread, and the port is
simply never opened — no fatal error, no retry. IPv4 explicitly.

**An unset `apply-inbound-acl` is not "allow".** mod_event_socket defaults to
`loopback.auto`, so the application — a different container, therefore a
different address — was met with `text/rude-rejection`. The demo defines an
`aicc_esl` list covering loopback and the private ranges; `localnet.auto` alone
would have let the application in and shut `fs_cli` out of its own container,
which is where anyone debugging this reaches for it first.

**Vanilla resolves the external addresses over STUN.** The external profile
came up advertising the site's public internet address, and the bot leg rides
that profile — so the SDP told the application to send media across the
internet to reach a container on the same host. Pinned to the container's own
address, with `FS_EXTERNAL_IP` for the case where a real phone needs the host's.

**`uuid-version` is a 1.11 parameter.** On 1.10.12 it is ignored and channel
UUIDs stay v4. Nothing in this project orders on them, only compares them, so
the demo is unaffected — but the development switch is 1.11.1 and does honour
it, and that difference should not be discovered during a debugging session.

**Recordings are written by one user and read by another.** FreeSWITCH creates
its date directories `0750` as `freeswitch`; the application runs as `nonroot`
in its own container and could not traverse them, so a recording that had been
written perfectly was booked as `stat: permission denied`. Solved by giving the
application container membership of the switch's group rather than by chasing
the mode of every new day's folder.

Verified live in the compose stack: queues and the SIP directory served from
the database (`support-en@aicc.demo`, `user_exists id 1000` true), the gateway
pinging our UAS, a call to 95001 walking dialplan → Lua → DID → flow → gateway
→ UAS → PCMU passthrough → provider, and a recording booked at hangup.

## 4. The seed had to grow up to be a demo

`dids.flow_id` is `NOT NULL`, so a number cannot exist before a flow does. The
M4 seed therefore created no numbers at all, which left a "complete" demo that
could not take a call. The flow is now embedded in the binary and published by
the seeder, with 95001 and 95002 pointing at it in their own languages. `95011
→ queue direct` from 03 §6 is not representable and is dropped.

Two smaller gaps in the same vein: the demo seeded agents only, so the
administrator's and supervisor's screens — most of the product — were
unreachable; and extension passwords were random UUIDs, so no softphone could
ever register. Both now use the documented demo credentials.

`AICC_SEED=fresh` is implemented (phase1-decisions O3's reset flag, inert until
now). It removes exactly the seeder's own footprint, so calls placed against
the demo survive it, and re-seeding afterwards restores the entities but not
the seven days of history — history is only ever generated into an empty
ledger.

## 5. Open: 6% of calls ended on the dead-media watchdog

In the final run, 100 of the 1,600 calls were ended by the UAS because no RTP
had arrived on them for around 28 seconds — while the load generator went on
calling `WriteToUDP` on those same calls successfully. Both sides believe they
were behaving; the packets went somewhere neither of them is looking.

What is known:

- It costs no pass criterion. Those calls are counted as answered, the
  late-frame rate stays at 0.009%, and the downlink shortfall it explains
  (580 k frames of 19.2 M, 3%) is consistent with 100 calls losing the tail of
  their audio.
- The generator's hangup is not the cause on its own: a test now holds it to
  hanging calls up, and the UAS reports them ended within milliseconds.
- It is clustered in four minutes of a thirty-minute run rather than spread,
  and it did not appear in the earlier run of the same shape — which was the
  run whose spurious abandonments (§1) were tearing sessions down anyway, so
  the two may well be related by masking rather than by cause.

The next diagnostic is to log the destination address each generator call is
sending to and correlate it with the UAS's port allocations across a slot
recycling: 500 RTP port pairs serving 200 slots that turn over every four
minutes is the obvious place for a stale peer to hide. Worth settling before
L3 builds on this harness.

## 6. Things left undone, deliberately

- **L3, L4 and L5 have not been run.** They need hardware this was not run on:
  the 8-core target rather than a laptop, a FreeSWITCH that is not also the
  development switch, `sipp`, and — for L4 — an hour of real provider credit.
  [`../load-tests.md`](../load-tests.md) is their runbook, and says plainly
  which stages are results and which are plans.
- **Six of 06 §6's metrics are still unbuilt** (`esl_event_lag_ms`,
  `esl_link_up`, `sse_clients`, `sse_slow_disconnects_total`,
  `pg_persist_failures_total`, `recording_upload_failures_total`). They are the
  ones L3 and L5 grade, and building them without a run to grade would be
  guessing at what they need to show. `rtp_tx_underruns_total` is not built for
  a different reason, in 06 §8.
- **The demo's FreeSWITCH image is amd64 only.** Fine under emulation for a
  demo, not for load. A second-choice image or an in-repo build is the answer
  if that ever matters.
