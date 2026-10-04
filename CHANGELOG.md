# Changelog

Changes to each release of the AI-native call center. A release is two images
under one tag, `rasonyang/ai-native-callcenter` and `rasonyang/freeswitch-aicc`;
run them together. From v0.2.0 on, the GitHub release of the same tag also
carries the one-line installer's files.

## v0.3.0 - 2026-10-04

### Upgrade notes

- Existing flows without `maxDurationSec` now end AI calls after 15 minutes.
  Set `"maxDurationSec": 0` in a flow's `global` to keep the old behaviour.
- Upgrade both images together. The switch image adds a dialplan route and
  `aicc_did_route.lua` for an agent's call to a platform number, and sends the
  call's type to the bot as `X-AICC-Call-Type`, which the application now
  reads. The application adds migration 00034 (`unbacked_claims` on the CDR);
  migrations run at application startup.
- API contract: the CDR gains the optional `unbackedClaims` array and the
  `UnbackedClaim` enum. Nothing was removed.
- A queue call now rings the agent's phone for 30 seconds instead of 15
  before it counts as missed.

### Added

- Deploy: the one-line installer accepts `--provider gemini`, the fifth
  engine the application already answers on. Its key, `GEMINI_API_KEY`, is
  read from the environment like the others'.
- Flow DSL: `global.maxDurationSec`, the call's time limit counted from the
  bot answering. Unset (or `null`) means 900 seconds, `0` turns it off, and any
  other value must be between 60 and 3600. Gemini's own provider session cap
  (about ten minutes) comes before the 900 s default, so a gemini flow should set
  a lower limit (docs/provider-extension.md). At 80% of the limit the bot is told to
  wrap up. At the limit the call is transferred to the number's queue with
  reason `SESSION_LIMIT` when that queue is enabled; otherwise it moves to
  `global.closingTarget`, or hangs up after a goodbye (at once, with no
  goodbye asked for, when the bot never gave up the floor). The wrap-up steer
  counts the time left when it is rendered, and offers a transfer to a person
  only when that queue is open. The CDR records hangup
  cause `SESSION_LIMIT` and does not count the call as contained. New metric
  `aicc_bot_session_limits_total{outcome}`.
- AI calls: the bot is told, as a platform rule beside the confidentiality
  rule, never to say it is transferring the caller, looking something up,
  saving a message or ending the call unless it calls that tool in the same
  reply. A claim made anyway is detected from the bot's transcript, logged,
  counted in the new metric `aicc_bot_unbacked_claims_total{claim,provider}`
  and recorded on the CDR as `unbackedClaims` (`TRANSFER`, `LOOKUP`,
  `MESSAGE`, `FAREWELL`; database column `unbacked_claims`, migration 00034).
  It changes no call behaviour and not `isContained`; a containment figure
  that should not count a caller told something that never happened can
  leave those calls out (#44).

### Changed

- Telephony: a call the queue delivers rings the agent for 30 seconds (five
  rings on the browser phone), up from 15. Agents were missing calls they
  were reaching for, and one miss benches them. Benching after one missed
  call is unchanged.
- Click-to-dial types a call by its callee: a provisioned extension or a
  queue's number is `INTERNAL`, any other number (a DID or an external
  number) is `OUTBOUND`. This replaces a four-digit length rule. When the
  lookup fails the dial is refused rather than typed by a guess.
- Demo flows: the repair-status flow looks up a clearly spoken repair number
  at once instead of asking the caller to confirm it in the same turn; an
  unclear number is still read back first.

### Fixed

- Deploy: with `--no-demo` the installer's closing message says how to create
  the first administrator with `aicc useradd` (as root on Linux) instead of
  printing an admin password that was never seeded, and drops the demo-only
  "dial 95001" hint (#72).
- Deploy: `aicc doctor --wait` gives the switch's `aicc_bot` gateway up to 45
  seconds to come back UP after the application is recreated. A provider
  change followed by a rerun could print `FAIL BOT_GATEWAY_DOWN` for a stack
  that was healthy by the switch's next OPTIONS probe.
- Docs: `deploy/one-line-installer.md` now says where the generated admin
  password lives and how the voice provider's key is passed, kept and changed
  (#69, #70).
- AI calls: when the model calls `transfer_to_agent` or `hangup` again while
  the call's ending is already armed, the first ending stands. A repeat used
  to replace it, wait for a turn after the latest call and restart the 10 s
  cap, so the transfer ran ten seconds after the last repeat, and a `hangup`
  after a transfer could hang up a caller who had been told they were being
  put through. The repeat is answered as done and changes nothing (#25).
- CDR: a call the bot decided to transfer but never handed over (the caller
  hung up during the closing line) no longer carries the queue's `queueId`.
  The row was counted in the queue reports as a queue call answered inside
  the SLA, with a wait of 0 s, for a caller who never reached the queue (#44).
- Telephony: an agent who dialled a platform number (95001, say) from the
  browser phone was hung up with `NO_ROUTE_DESTINATION`. The call now reaches
  that number's flow, the same one a carrier's caller reaches, as one call
  with one id; the CDR row is `OUTBOUND` from the agent's extension (#74).
- Agents: an agent benched for a missed call who chooses READY is offered the
  next call at once. The switch kept them idle for up to a minute after the
  miss while callers waited (#51).
- AI calls: when the model asks for several tools in one response, each is
  judged in the phase the response was made in. The second tool used to be
  refused when the first one's result moved the phase, and the new phase's
  line pre-empted the answers still owed (#12).
- AI calls: an armed transfer or hangup whose closing line is queued behind
  other audio waits for the line to finish playing (plus 2 s) instead of
  cutting it off at the 10 s cap (#24).
- AI calls, interruptions and keypresses:
  - The tail of a response cut off by a barge-in is no longer played; a call
    used to hear 40-80 ms of the interrupted answer after every keypress.
  - A keypress while an ending is armed is recorded and leaves the closing
    line alone. It used to cut the line into fragments and delay the action.
  - A keypress takes the floor from a turn that is being generated but has
    not spoken yet, and on openai and gateway it now cancels the turn at the
    provider too (#55).
  - A text cue sent while a response holds the floor is answered when that
    response ends, instead of being refused and left unanswered (#55).
  - A function call cut off by a cancel is removed from the conversation, so
    qwen no longer says it is transferring without calling the tool.
  - The 800 ms echo guard runs from when the caller starts hearing the bot,
    so real speech over the start of a back-to-back answer is no longer
    swallowed (#58).
  - A turn flushed before any of it played is trimmed from the model's
    history. qwen accepts the trim but still behaves as if the line was
    heard (#53 stays open for qwen).
- Web: an agent without the web-sip-phone extension on the page sees the
  install wizard at once, not "Phone lost contact · reload" for several
  seconds (#75).

## v0.2.0 - 2026-09-28

### Upgrade notes

- The demo seed no longer resets existing accounts: an account or extension
  that already exists keeps its password and role on every boot.
- `AICC_SEED_PASSWORD` must not contain `& < > " '`, whitespace or control
  characters; the application refuses to start with one, because the switch
  writes the value into XML.
- Upgrade both images together. The v0.2.0 switch image changes the internal
  SIP profile's NAT handling (see Changed): every phone that is not on
  loopback is answered with `FS_EXTERNAL_IP` in the SDP, including phones on
  the switch's own LAN. On a Linux host, check that `FS_EXTERNAL_IP` is an
  address LAN phones can send RTP to.
- The macOS installer needs the v0.2.0 switch image or later. Do not pair it
  with the v0.1.1 switch image (`AICC_FS_IMAGE=rasonyang/freeswitch-aicc:v0.1.1`):
  that image answers phones behind Docker Desktop's or Colima's port
  forwarding with the container's address, and the calls have no audio.
- qwen: the default model is now `qwen-audio-3.1-realtime-plus` and the
  profile's fallback voice `longanqian_v3.1`. A deployment that sets
  `AICC_PROVIDER_MODEL` keeps its model. There is no 3.1 flash model. Live
  transcription still uses `qwen-audio-3.0-asr-flash-streaming`.
- Building from source needs Go 1.27.1 or later.

### Added

- A one-line installer, `deploy/install.sh`:
  `curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo sh`
  on Linux (into `/opt/aicc`), without `sudo` on macOS (into `~/.aicc`). It
  runs preflight checks that fail with a stable `PREFLIGHT_*` tag and a fix
  before anything is written, verifies the deploy bundle's checksum, writes
  `.env` (mode 0600) with generated secrets, starts the published images and
  runs `aicc doctor`. Reruns repair without regenerating secrets;
  `--external-ip`, `--upgrade` (with a `pg_dump` backup first),
  `--uninstall`, `--purge`, `--check` and `--no-demo` cover the rest.
- Release assets: `install.sh`, `aicc-deploy-<tag>.tar.gz` and
  `checksums.txt` on the GitHub release, built by `scripts/release-bundle.sh`.
- Compose overlays: `deploy/compose.linux.yml` runs the application and the
  switch on the host network; `deploy/compose.macos.yml` runs them in one
  shared network namespace. `deploy/compose.release.yml.in` is the release
  overlay that pins the published images.
- `aicc doctor` checks a running deployment from inside it and changes
  nothing: readiness, the switch link, the database and its migrations, the
  event socket, the SIP profiles, the bot gateway, the external address and
  the provider. Each failure carries a stable code and a fix.
- `AICC_SEED_PASSWORD`: the password the demo seed gives the accounts and
  extensions it creates, and the simulated customers' SIP password.
- `AICC_BOT_ALLOWED_PEERS`: the addresses allowed to send SIP to the bot and
  RTP/RTCP to its calls; anything else is dropped before it is parsed. Empty
  (the default) allows every peer, as before; the Linux overlay sets it.
- `/readyz` reports the database, the switch link and the migration version
  in stable body lines.
- The agent's My calls list shows the whole call's duration in a Total
  column beside the agent's talk time, which is now labelled "Agent talk".

### Changed

- The switch's internal SIP profile treats only loopback as its local
  network (`local-network-acl` `aicc_sip_local`, with
  `aggressive-nat-detection` on) and answers every other phone with
  `FS_EXTERNAL_IP`, letting RTP auto-adjust follow the address the audio
  arrives from. Before, a phone reaching the switch through Docker Desktop's
  or Colima's port forwarding arrived from the compose gateway's private
  address, was taken to be local, and was told to send its audio to the
  container's address. The external profile, which carries the `aicc_bot`
  gateway, is unchanged. Two-way audio verified live on Docker Desktop on an
  Intel Mac, on Colima with the `grpc` port forwarder (macOS overlay) and on
  a Linux host with the host-network overlay and a LAN WebRTC agent.

### Fixed

- `lua-role` could exit with "tuple concurrently deleted" on a fresh start,
  when its grants raced a migration replacing a `luacc` view. It now waits
  for the migrations to finish and checks every grant.
- Gemini: the local speech detector no longer drops a caller's speech start
  or stop while the bot's audio is being relayed, so the dead-air timer is
  cancelled when the caller talks over the bot.

### Docs and CI

- `deploy/README.md`: the git checkout names the release tag, and the guide
  covers the prerequisites, where provider keys must be set, first-start
  switch messages, a no-phone test call, the bot gateway's NOREG state,
  transcription settings and upgrading. A section covers running the stack
  on macOS, with troubleshooting for one-way and no audio.
- CI skips Markdown-only changes and builds container images only at
  release. Redundant tests were removed and the test harness consolidated.

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
