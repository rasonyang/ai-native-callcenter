# Design 06 — Capacity Budget (200 AI calls + 50 agents on 8c/16GB) & Load Test Plan

Scope: the single `aicc` Go process. PostgreSQL and FreeSWITCH are external (their impact noted in §5). Numbers are engineering estimates to be **validated by the load tests in §7**; anything that misses its budget is a design bug.

## 1. Per-AI-call cost model

| Resource | Item | Estimate (steady) |
|---|---|---|
| Goroutines | RTP read, RTP 20ms send, RTCP send, RTCP recv, dead-monitor, DTMF pump, session actor, provider WS read, provider WS write/ping | **~9–10 / call** |
| Heap | RTP TX queue (cap 250×~180B, worst 45KB), RX queue (16KB), jitter map (≤4KB), WS+TLS buffers (~48KB), pooled codec/resample scratch (~16KB), flow/session state + live transcript (~16KB) | **~150–200KB typical, ≤350KB worst** |
| CPU | G.711 LUT both ways (~0.02% core), resample ×2 linear + ÷3 sinc (~0.04%), base64 ~0.1MB/s (~0.05%), JSON events incl. audio deltas (~0.1–0.2%, the largest item — fast-path delta extraction is the documented optimization if needed), RTP packetize + syscalls (~0.1%) | **~0.3–0.5% of one core / call** |
| Sockets/FDs | 2 UDP (RTP/RTCP) + 1 WSS | 3 |
| WAN bandwidth | Qwen: up 16k PCM16 b64 ≈ 360kbps continuous; down 24k PCM16 b64 ≈ 512kbps × ~45% talk duty ≈ 230kbps → **~0.6Mbps avg / 0.9 peak**. OpenAI (G.711 passthrough, **M0-verified for both laws**): **~0.12Mbps avg** (~5× cheaper than the Qwen path) | per call |

## 2. Process totals @ 200 AI calls (+50 agent calls, which cost the Go process only ESL events + SSE)

| Resource | Total | Budget / headroom vs 8c16GB |
|---|---|---|
| Goroutines | ~2,000 + ~100 baseline | trivial for the runtime |
| Heap/RSS | 30–70MB call state + ~20MB stacks + SSE ring (65,536 envelopes ≈ 25–50MB, size-configurable) + runtime/pgx/embedded SPA ≈ **RSS target <500MB, alert 2GB** | >85% of 16GB free |
| CPU | calls ≈ 0.6–1.0 core avg; + GC (pooling keeps churn <50MB/s), SSE fan-out (60 clients), ESL (~30 ev/s), HTTP/reports | **≤3 cores p95 target, alert 4** → ≥2× headroom |
| FDs | ~700 (600 call sockets + SSE + PG pool + listeners) | ulimit 4096 documented |
| WAN uplink | all-Qwen worst **~120Mbps avg / 180Mbps peak**; 50/50 mix ~70–100Mbps; all-OpenAI-passthrough ~25Mbps | **the #1 external sizing item** — deployment doc requires ≥200Mbps for all-Qwen fleets |
| LAN (RTP to FS) | 200×2×87kbps ≈ 35Mbps | trivial |

Design levers that keep this honest (already in 02): `sync.Pool` on every per-frame buffer, pre-encoded silence frames, passthrough path for OpenAI, prebuffer 60ms (bounds burst memory), TX blocking backpressure (bounds queue growth), per-call end-of-call health log (late ticks, jitter fills, underruns) so regressions surface per call, not per fleet.

## 3. Concurrency-cost decisions justified by this table (mandate)

- **SIP/RTP in-process (no mod_audio_fork/stream)**: keeps per-call cost at 2 UDP sockets + LUT codecs; an audio-fork design would add a second WS hop per call and put the fork cost on FreeSWITCH. Confirmed choice.
- **G.711 passthrough for OpenAI** chosen over always-PCM for ~5× WAN and ~30% CPU reduction on that path.
- **Windowed-sinc only on downsample** (quality-critical) and linear on upsample (masked by the µ-law channel) — sinc both ways would double resample CPU for inaudible gain.
- **JSON audio deltas accepted** for phase 1 (0.1–0.2%/call) with a documented fast-path escape hatch — no custom binary protocol invented.

## 4. External limit: provider concurrency quotas

200 simultaneous realtime sessions per account is above default OpenAI/DashScope quotas — must be raised commercially. Runtime behavior at quota rejection: call gets the queue's overflow treatment (`provider_unavailable` → per-queue overflow action), never a dead-air call. Preflight surfaces quota errors on the health panel.

## 5. FreeSWITCH machine impact (informative, per mandate)

250 concurrent calls ≈ 500 channels: bot legs PCMU-pinned (no transcode); agent legs OPUS↔PCMU transcode ≈ 0.5–1 core for 50 calls (optionally pin agent extensions to PCMU to eliminate); recording 250 streams ≈ 4MB/s sequential disk; RTP relay is kernel-bound and comfortable on a 4–8 core host; `max-sessions 1000` / `sessions-per-second 100` (D7) suffice. FS RTP range 16384–32768 disjoint from the bot's 40000–40999.

## 6. Metrics (OTel; the budget's runtime enforcement)

`aicc_turn_latency_ms{provider}` (histogram, 02 §8), `aicc_calls_active{kind}`, `rtp_late_ticks_total`, `rtp_tx_underruns_total`, `jitter_filled/lost/dropped_total`, `provider_first_audio_ms`, `provider_ws_errors_total`, `esl_event_lag_ms`, `esl_link_up`, `sse_clients`, `sse_slow_disconnects_total`, `pg_persist_failures_total`, `go_goroutines/heap` (runtime), `recording_upload_failures_total`. Alert set mirrors cti-server's ops doc.

## 7. Load test & validation plan

Harness pieces (repo deliverables, also used in CI smoke): **mock provider** (in-process `VoiceSession`: scripted turns, configurable first-audio/VAD delays, zero WAN) and a **UAC load generator** built on the voice package's client-side test code (INVITE + 20ms PCMU tone/pcap RTP, asserts downlink pacing).

| Stage | Setup | Pass criteria |
|---|---|---|
| L1 micro | `go test -bench` codec/resample/b64 paths | 0 allocs/op on frame paths; sinc ÷3 < 5µs/frame |
| L2 synthetic 200 | UAC ×200 → aicc + mock provider, 30min | CPU <50% of 8c; RSS <1GB; late ticks <0.1%; goroutines return to baseline (no leaks) |
| L3 FS interop | FS `originate` loop → gateway → aicc ×200 + mock provider | same as L2 + no ESL event lag >250ms; FS box within §5 envelope |
| L4 real providers | N=20 real OpenAI+Qwen calls, 1h soak | turn latency meets A3 (≤1.2s p50/≤2s p95); zero unexplained WS drops |
| L5 acceptance | 200 AI (mock) + 50 sipp agent calls + 60 SSE clients + report queries, 2h | all L2/L3 criteria + SSE zero slow-consumer disconnects + CDR/report p95 <500ms |

M0 spike (precedes all): live ESL event-shape verification, mod_callcenter odbc-dsn pgsql check, OpenAI `audio/pcmu`/`audio/pcma` bidirectional verification (drives the codec-path table in 02 §2).
