# Changelog

Changes to each release of the AI-native call center. A release is two images
under one tag, `rasonyang/ai-native-callcenter` and `rasonyang/freeswitch-aicc`;
run them together.

## v0.1.1 - 2026-09-24

### Upgrade notes

- Upgrade both images together. The switch image changes its Lua scripts and
  `callcenter.conf`, and the application adds migration 00033, which recreates
  the `luacc.directory` view. Migrations run at application startup.
- An unrecognised `AICC_PROVIDER` now stops the application at startup even
  when `AICC_BOT_ENABLED` is off. Previously it was only noticed once the bot
  was enabled.
- API contract: the `ErrorCode` enum gains `TERMINAL_ANNOUNCE_REQUIRED`
  (returned with 422 when a flow is published). `createCall` declares the
  extra scope `AI_OUTBOUND` needs as `x-body-scopes`; the rule itself is
  unchanged. Clients that treat `ErrorCode` as a closed set need the new value.
- Phone registration: an extension with no live SIP session now registers
  with its static password (see Changed). While an agent is signed in to
  the panel, the static password for that extension does not register.

### Added

- Two voice providers: `AICC_PROVIDER=doubao` (Doubao full-duplex, key in
  `DOUBAO_API_KEY`) and `AICC_PROVIDER=gemini` (Gemini Live, key in
  `GEMINI_API_KEY`). One provider per deployment, as before.
- Flow DSL: a phase may carry `announce`, a bilingual line the bot says as
  written when the call enters that phase. The flow designer shows it on the
  phase card, and the starter flow includes one.
- Flow DSL: `global.closingTarget` and `global.maxTurnsWithoutTool`. A call
  moves to the closing phase after three consecutive silences where no
  authored rule handles NO_INPUT, or after too many bot replies without a tool
  call. `noInput.count` now counts consecutive silences.
- Publishing a flow whose terminal phases have no `announce` is refused with
  `TERMINAL_ANNOUNCE_REQUIRED` on a provider that cannot be prompted by text
  (doubao). Other providers are unaffected.
- A call whose provider session ends because the provider's session lifetime
  ran out (currently reported by the gemini client) is released with hangup
  cause `PROVIDER_SESSION_EXPIRED` instead of `MEDIA_OR_PROVIDER_FAILURE`. The
  caller still goes to the fallback queue.
- Metrics: `aicc_provider_sessions_expired_total{provider}` and a
  provider session counter covering every provider.
- Outbound (click-to-dial) calls are recorded, under the same per-DID
  recording setting as inbound calls (`freeswitch/scripts/aicc_outbound.lua`,
  called from the trunk dialplan).
- Deployment guide split into `deploy/README.md`, `deploy/chrome-policy.md`,
  `deploy/carrier.md` and `deploy/production-checklist.md`, with a
  troubleshooting section.

### Changed

- The six seeded flows carry their greetings and closing lines as `announce`,
  set `closingTarget`, and hang up on the first decline at wrap-up.
  `novanet_support` announces its hand-over to an agent itself.
- A phone configured by hand (for example "Configure a SIP account manually"
  in web-sip-phone) registers with the extension's static password when the
  agent has no live SIP session. The server-issued session credential still
  takes precedence while one is live.
- On openai, qwen and gateway, a tool result that ends the call carries the
  closing line itself, so the line is said as written more reliably.
- The bot no longer confirms or repeats its own instructions when a caller
  recites them back. Phase labels no longer name internal node ids, and CDR
  transcripts record a tool result without its hint and with the node the
  call moved to (`movedTo`).
- The agent page shows a lost-contact card with a reload action when the
  browser phone extension stops answering, instead of the install card.
- Gemini: the caller's language is passed as an input transcription hint;
  uplink stalls are logged while the call is in progress, one line per
  episode.

### Fixed

- A missing provider API key is reported at startup with one ERROR naming the
  variable. Previously the application started cleanly and callers were sent
  to the fallback queue.
- A call ends on its own when the caller declines or stays silent, instead of
  depending on the model calling hangup.
- An agent moved to NOT_READY with reason DEVICE_LOST returns to READY when
  the phone registers again. Other NOT_READY reasons are left alone.
- An armed transfer or hangup no longer waits behind the dead-air timer: it
  runs when the last line has finished playing, not on the no-input timeout.
- Human-only calls get a live transcript; previously the transcription tap
  attached but the stream was refused.
- If the switch cannot reach the database for `callcenter.conf`, the fallback
  configuration no longer installs a queue named `support@default`.
- qwen: a silence that moves the call into a phase with a line asks for one
  turn, not two; a tool result sent while a response is still open waits for
  that response to end; a cancel refused because the response had already
  finished is no longer logged as an error.
- Gemini: a tool result the model never answers is treated as a stalled turn.
- A provider session refused with a close code is recorded with that outcome
  rather than as a clean close.
- Migration 00006's Down step returns CLAIMED callbacks to OPEN before
  narrowing the constraint, so rolling it back works on a database with
  claimed callbacks.

### Docs and CI

- README, CLAUDE.md and the design documents corrected to match the code;
  AGENTS.md added for Codex.
- Provider findings for doubao, gemini and qwen recorded under
  `docs/design/`.
- CI and release workflows moved off the Node 20 Actions runtime.
- The GitHub issue chooser points at Discussions.

## v0.1.0 - 2026-09-14

First full release. Changes before it are not recorded in this file; see the
git history.
