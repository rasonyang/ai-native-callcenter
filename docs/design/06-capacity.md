# Design 06 — Capacity Estimates & Load Test Plan

Scope: the single `aicc` Go process. PostgreSQL and FreeSWITCH are external (their impact noted in §5).

**Everything below is an unvalidated engineering estimate.** It exists to size the design and to give the load tests something to aim at — not to describe what the software does. None of it has been measured, none of it is published outside this design set, and no figure from it belongs in a README, a deployment guide or a release note until the campaign in §7 has actually run. Anything that then misses its estimate is a design bug.

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

## 4. External limit: the provider's quota is a rate

*Rewritten for #604 (2026-11-20). The original said that 200 simultaneous realtime sessions per account were above the vendors' default quotas and had to be raised commercially, and named a per-queue overflow path for a quota rejection. Neither held: the documented quota is a rate, not a population, and the code has no such path (a refused session is `BOT_START_FAILED` and the rescue below).*

**What the vendors limit.** For Qwen-Audio-Realtime over WebSocket the vendor's rate-limit page ([Model Studio rate limits](https://help.aliyun.com/zh/model-studio/rate-limit), "Qwen-Audio-Realtime") states three things: RPM counts **connection establishments** (the turns inside one connection do not count, a reconnect does), there is **no separate limit on connections held at once**, and TPM (input and output tokens together) applies as for any model. For `qwen-audio-3.1-realtime-flash` in the Beijing region that is **60 connects and 100,000 tokens per minute**. Doubao's documentation states the same shape per application id: 60 `session.create` and 100k tokens per minute ([doubao-findings §9](doubao-findings.md)). OpenAI's limits depend on the account's usage tier, and a gateway's on its deployment. So the sizing question for the AI leg is how many sessions a busy minute *opens* and how many tokens it *spends*, not how many run at once: a campaign can sit far below `AICC_BOT_MAX_CALLS` with every concurrency number healthy and still be refused at the door, because 60 connects a minute is one a second. Whichever of the two caps binds first depends on how many tokens a call spends, which only the token counter below can say for a deployment.

**What a refusal looks like.** Qwen accepts the upgrade (101) and then closes the socket with `1007` and "Allocated quota exceeded, please increase your quota limit." A refused upgrade (HTTP 429, what the vendors' HTTP APIs answer) is the other shape a client can meet. Either way the call is not answered by the model: the session start fails, the CDR says `BOT_START_FAILED`, an inbound call is rescued to the number's fallback queue, and an answered campaign call is hung up and counts towards the provider breaker (design 10 §7.4). That is unchanged.

**How to read a deployment's limits.**

- The vendor's document, per model and region. The profile carries the number only where that document gives it for the profile's own default model (`Profile.SessionLimit` / `SessionPeriod`, `Profile.TokenLimit` / `TokenPeriod`; table in [provider-extension.md](../provider-extension.md#quota)); any other model, region or a raised quota is the deployment's to state, each limit as a count over the vendor's period (`AICC_PROVIDER_SESSION_LIMIT` / `_PERIOD`, `AICC_PROVIDER_TOKEN_LIMIT` / `_PERIOD`), or per minute with `AICC_PROVIDER_SESSION_RPM` / `AICC_PROVIDER_TOKEN_RPM` (#644). A vendor may state a raised quota over a shorter period: one deployment's read 3 requests per 1 s and 60,000 `total_tokens` per 12 s, which per minute would be 180 and 300,000 but allows no four starts in one second.
- The upgrade response's headers. The client reads `X-RateLimit-Limit/-Remaining/-Reset` and the `x-ratelimit-{limit,remaining,reset}-{requests,tokens}` family when present. On the qwen deployment checked on 2026-11-18 none was sent: the 101 carried only `x-request-id`, and neither a 401 nor the REST API's 200 or 429 carried one either. The header cannot be the only mechanism.
- DashScope's quota endpoint. The same deployment answered `GET /api/v1/quotas?model=<model>` with its key: `request_limit` 60 per 60 s, `usage_limit` 100,000 `total_tokens` per 60 s, and a null concurrency field, which matches the document. Whether that endpoint is a stable public API is the vendor's to say; nothing here reads it.

**What the platform does.**

- *Counts.* `aicc_provider_sessions_started_total{provider}` (every session, counted before it dials), `aicc_provider_tokens_total{provider,direction}` (`response.done` usage), `aicc_provider_dial_failures_total{provider,status,reason}` (a start that failed: the HTTP status of a refused upgrade, `CLOSE_<code>`, `ERROR_EVENT`, `TIMEOUT` or `NETWORK`; reason `QUOTA_EXCEEDED`, `AUTH_REJECTED` or `OTHER`), `aicc_provider_quota_per_minute{provider,kind}` (the limits in force) and, when a vendor sends them, `aicc_provider_ratelimit_limit` / `_remaining{provider,kind}`. A refusal is logged with the upgrade's request id, the handle the vendor's support asks for; a session that opened logs it once.
- *Paces campaigns.* One rate meter per process (`internal/provider/ratemeter`) keeps session starts and tokens, each over its limit's period, for inbound and outbound sessions alike, and the time of the latest quota refusal. Campaign admission holds a call while the starts or tokens counted, plus what the legs ringing are expected to add, would pass the limits (starts judged over the session period, or over 10 s when it is shorter), and after any quota refusal, inbound ones included, for the longer period of the limits in force, 60 s when neither is (design 10 §6.3, as built for #604 and #644).
- *Paces campaign starts.* Answers arrive when callees pick up, not when the dialer admits, so admission alone cannot keep the starts of a short period inside its limit. A campaign call's session start waits, at most 2 s, FIFO, for room in the session period, and past that is made anyway (#644). Inbound calls are never held and never wait: a caller who rings while the quota is spent meets the refusal and the rescue, as before.
- *Says so.* `aicc doctor` prints the limits in force and where each came from, the request id and any rate-limit header of its probe session, and a refusal's status and reason.

## 5. FreeSWITCH machine impact (informative, per mandate)

250 concurrent calls ≈ 500 channels: bot legs PCMU-pinned (no transcode); agent legs OPUS↔PCMU transcode ≈ 0.5–1 core for 50 calls (optionally pin agent extensions to PCMU to eliminate); recording 250 streams ≈ 4MB/s sequential disk; RTP relay is kernel-bound and comfortable on a 4–8 core host; `max-sessions 1000` / `sessions-per-second 100` (D7) suffice. The image's earlier RTP range did not fit: 16384–16584 was 101 legs at one RTP/RTCP pair per leg, and 500 channels (legs) need 500 pairs, 1,000 ports. The image now ships 20000–25999 (3,000 legs), as does the Linux installer; the manual quick start (20000–20099, 50 legs) and the macOS installer (20000–20199, 100 legs) are narrower, and `RTP_START`/`RTP_END` widen them (the compose files publish what it is set to), kept disjoint from the bot's range.

## 6. Metrics (OTel; the budget's runtime enforcement)

`aicc_turn_latency_ms{provider}` (histogram, 02 §8), `aicc_calls_active{kind}`, `rtp_late_ticks_total`, `rtp_tx_underruns_total`, `jitter_filled/lost/dropped_total`, `provider_first_audio_ms`, `provider_ws_errors_total`, `esl_event_lag_ms`, `esl_link_up`, `sse_clients`, `sse_slow_disconnects_total`, `pg_persist_failures_total`, `go_goroutines/heap` (runtime), `recording_upload_failures_total`. Alert set mirrors cti-server's ops doc.

## 7. Load test & validation plan

Harness pieces (repo deliverables, also used in CI smoke): **mock provider** — a Realtime *server* the application reaches by endpoint, with scripted turns, configurable first-audio/VAD delays and zero WAN (§8 explains why it is a server rather than an in-process `VoiceSession`) — and a **UAC load generator** built on the voice package's client-side test code (INVITE + 20ms PCMU tone/pcap RTP, asserts downlink pacing).

| Stage | Setup | Pass criteria |
|---|---|---|
| L1 micro | `go test -bench` codec/resample/b64 paths | 0 allocs/op on frame paths; sinc ÷3 < 5µs/frame |
| L2 synthetic 200 | UAC ×200 → aicc + mock provider, 30min | CPU <50% of 8c; RSS <1GB; late ticks <0.1%; goroutines return to baseline (no leaks) |
| L3 FS interop | FS `originate` loop → gateway → aicc ×200 + mock provider | same as L2 + no ESL event lag >250ms; FS box within §5 envelope |
| L4 real providers | N=20 real OpenAI+Qwen calls, 1h soak | turn latency meets A3 (≤1.2s p50/≤2s p95); zero unexplained WS drops |
| L5 acceptance | 200 AI (mock) + 50 sipp agent calls + 60 SSE clients + report queries, 2h | all L2/L3 criteria + SSE zero slow-consumer disconnects + CDR/report p95 <500ms |

M0 spike (precedes all): live ESL event-shape verification, mod_callcenter odbc-dsn pgsql check, OpenAI `audio/pcmu`/`audio/pcma` bidirectional verification (drives the codec-path table in 02 §2).

## 8. M5 amendments (the plan met the machine)

**The metrics of §6 now exist.** They were designed in M0 and never built; L2's own pass criteria depend on them, so M5 implemented the ones the budget makes claims about, in `internal/obs/callmetrics.go`: `aicc_calls_active{kind}`, `aicc_rtp_frames_sent_total`, `aicc_rtp_late_ticks_total`, `aicc_jitter_{lost,dropped,filled}_total`, `aicc_provider_first_audio_ms{provider}`, `aicc_provider_ws_errors_total`, beside the `aicc_turn_latency_ms{provider}` M3 already had. Every name carries the `aicc_` prefix (§6 wrote several of them as bare shorthand). Three departures from the list worth knowing:

- **`kind` is `SWITCH` or `BOT`, and they overlap** rather than partition. A bot-answered call is on the switch too, so it appears under both; `SWITCH` is what the machine carries, `BOT` what the model answers.
- **`rtp_tx_underruns_total` is not implemented.** The RTP session tracks underruns only as a local grace counter deciding whether an utterance is still playing; it is not a persisted count, and exposing it would mean adding an atomic to the send loop for a number the late-tick rate already covers. `aicc_rtp_frames_sent_total` ships instead, because the late-tick count is meaningless without a denominator.
- The remaining §6 names (`esl_event_lag_ms`, `esl_link_up`, `sse_clients`, `sse_slow_disconnects_total`, `pg_persist_failures_total`, `recording_upload_failures_total`) are still unbuilt. They belong to L3 and L5, which have not been run.

**Metrics added after M5**, in the same file, outside §6's list: `aicc_provider_sessions_started_total{provider}` (session-creation rate, the quota vendors limit; the provider's other counters are in §4), `aicc_bot_interruptions_total{reason}` (`SPEECH` or `DTMF`; speech inside the barge-in guard is not counted), `aicc_bot_session_limits_total{outcome}` (AI calls the platform ended at the flow's `global.maxDurationSec`; `TRANSFER` or `HANGUP`), `aicc_webhook_deliveries_total{subscriptionId,outcome}` (`DELIVERED` or `FAILED`), and `aicc_transcribe_frames_sent_total` / `aicc_transcribe_frames_dropped_total{provider,speaker}` (audio handed to, or discarded before, a recognition session). `aicc_turn_unanswered_total{provider}` counts caller turns the engine never answered before the application stepped in; those turns are excluded from `aicc_turn_latency_ms` and `aicc_provider_first_audio_ms`. `aicc_turn_latency_ms` is published by `obs.RecordTurnLatency`, fed from `internal/aicall/metrics.go`.

**The mock provider is a server, not a fake.** §7 specified an in-process `VoiceSession`. That seam sits above the WebSocket, the JSON and the base64 audio path — the layers a load test exists to stress — and `AICC_PROVIDER_ENDPOINT` is already the supported way to reach anything speaking the protocol. `internal/mockprovider` is therefore a Realtime server the real client connects to, and its tests drive it with that client rather than with one written for it.

**The UAC generator carries the dialplan's headers.** `internal/loadgen` places calls straight at the SIP UAS, so it must supply the `X-AICC-*` correlation headers FreeSWITCH would have added; without them the orchestrator has no number to resolve a flow from. It measures pacing from the caller's side — the gap between arriving downlink frames — which is the property a caller actually hears, and it counts a gap of two frame intervals or more as late.

**The campaign itself is deferred.** The harness is built and the plan is written ([../load-tests.md](../load-tests.md)), but L2–L5 need the target machine, a switch that is not the development one, `sipp` and real provider credit. A shakedown run of L2's shape on a laptop is what surfaced the watchdog bug recorded in [m5-findings §1](m5-findings.md); its numbers are not results and this document's budget stays an estimate until the campaign runs.
