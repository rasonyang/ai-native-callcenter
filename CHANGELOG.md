# Changelog

Changes to each release of the AI-native call center. A release is two images
under one tag, `rasonyang/ai-native-callcenter` and `rasonyang/freeswitch-aicc`;
run them together. From v0.2.0 on, the GitHub release of the same tag also
carries the one-line installer's files.

## Unreleased

### Upgrade notes

- The binary's default country code is now `1` (`AICC_DEFAULT_COUNTRY_CODE`), so
  a national number is never ambiguous when nobody says otherwise. The deploy
  stack keeps `86` while its `.env` does not mention the variable, so a stack
  that never set it behaves as before. Two kinds of deployment do change: a
  native deployment that never set `AICC_DEFAULT_COUNTRY_CODE` (it used to
  refuse national numbers), and a stack whose `.env` blanks the variable
  (`AICC_DEFAULT_COUNTRY_CODE=` reaches the application as unset, so `1`
  applies, not the stack's `86`). Both now complete national numbers to `+1`
  for the do-not-call check, contact import and the bot's take_message callback
  numbers: a national `13800138000` no longer matches a list entry
  `+8613800138000` and would be dialled. Set `AICC_DEFAULT_COUNTRY_CODE` to your
  country's code. `aicc doctor` warns about exactly this (`dnc_country_code`,
  `DNC_COUNTRY_CODE_UNSET`, never a failure) when the variable is unset and the
  do-not-call list holds a number outside `+1`. (#293; thanks @javayou13131)
- API contract: `POST /calls` answers `409 OUTBOUND_NOT_CONFIGURED` instead of
  `503 STORAGE_DOWN` when `AICC_OUTBOUND_ENDPOINT` is empty, and
  `POST /campaigns/{campaignId}/start` and `.../resume` answer the same code
  instead of creating, or reporting as running, a campaign whose every attempt
  would fail; the campaign stays `DRAFT` or `PAUSED`. `CampaignPauseReason`
  gains `OUTBOUND_NOT_CONFIGURED` (a migration widens the column's CHECK; its
  rollback rewrites such rows to `TRUNK_FAILURE`), for a campaign the dialer
  stopped because the endpoint was empty. `Campaign` gains the read-only
  `flowFields`, present on a single campaign (`getCampaign` and the responses of
  create, update and every transition), absent from `listCampaigns` pages and
  when the flow could not be read, and `[]` when the number has no published
  flow or the flow reads no business data. (#293; thanks @javayou13131)
- `aicc doctor` gains `outbound_endpoint` (`OUTBOUND_ENDPOINT_UNSET`) when
  `AICC_OUTBOUND_ENDPOINT` is empty, and `outbound_gateway`
  (`OUTBOUND_GATEWAY_MISSING`) when the endpoint names a gateway the switch
  does not have, naming the ones it does. The gateway passes only when the
  switch's trunk table says it would place a call (`REGED`, or `NOREG` for one
  that never registers) and its ping status is `UP`; otherwise the warning names
  the state (`FAIL_WAIT`, or `NOREG` with the ping `DOWN`). A profile-qualified
  endpoint (`sofia/gateway/external::pstn_gateway/%s`) is read correctly. None
  changes the exit status. (#293; thanks @javayou13131)

### Added

- A flow phase can declare the campaign lead the call records when it enters
  that phase: `"lead": {"intent": "HIGH", "notes": "{slots.agree_words}"}`. A
  flow whose classification tool moves straight into the closing phase now
  records its outcome without depending on the model calling `record_lead` in
  the same response. It is one more write to the attempt's lead, ordered with
  the model's `record_lead` calls (the last one wins), made on every entry into
  the phase however the call got there, and ignored on a call that is not a
  campaign call. The callback for `HIGH` and the do-not-call entry for
  `DO_NOT_CALL` still come once, from the final lead, when the call ends. The
  intent and every `{slots.x}` in the notes are checked when the flow is saved
  (`docs/flow-authoring.md` section 2.7.1). The flow simulator records it on
  scenarios with `isCampaign: true` and checks it with `expect.lead`. (#352)

### Fixed

- `POST /flows` and `PUT /flows/{flowId}` answer a spec whose JSON has the wrong
  shape (a number for a phase `instruction`, a malformed `http.modelOnly`
  entry, a non-text `global.toolWaitLine`) with `422 VALIDATION_FAILED` and a
  `params.problems` entry saying what was expected, instead of
  `503 STORAGE_DOWN`. A body that is not JSON at all is still `400`. (#354)
- A call that ends with a tool and `hangup` in one response (`record_lead` and
  `hangup`, say) no longer ends as if the closing line had been heard when the
  model answered with a confirmation of its own, sometimes with no goodbye at
  all. On the Realtime engines the words of the turn after the closing move are
  held against the phase's line; a turn that does not say it is heard whole and
  the line is asked for once more, so a caller may hear a goodbye of the model's
  own before the line. A second miss is logged at WARN
  (`the closing line was not said; ending the call without it`) and the call
  ends. Doubao, transfers and flows without a closing line are unchanged.
  (#262)
- A refused Publish in the flow editor says what the server named, in the
  notice and in the publish dialog, as a refused Save draft already did: every
  problem when the stored draft no longer loads (`VALIDATION_FAILED`), the
  phases that need a line of their own (`TERMINAL_ANNOUNCE_REQUIRED`), and for
  a revision that can transfer while campaigns that have not finished dial with
  the flow (`CAMPAIGN_FLOW_HAS_TRANSFER`), how many campaigns and the way out:
  finish or cancel them, or remove the transfer. It used to show the error
  code's sentence alone, and for the campaign block the campaign form's advice
  to choose another number. (#361)

### Changed

- A campaign refuses to start or resume on a deployment that cannot place an
  outbound call, rather than dialling every contact into a trunk it has not been
  given. The SPA says so in English and Chinese and points at
  `deploy/carrier.md`; the deploy guide and the installer's closing output say
  it too. (#293; thanks @javayou13131)
- A campaign that is already running on a deployment with no
  `AICC_OUTBOUND_ENDPOINT` (the application restarted without it) is paused on
  the first attempt it cannot place, as `OUTBOUND_NOT_CONFIGURED`, with that
  attempt voided rather than retried. If the pause itself fails the pass stops
  admitting for that campaign and the next pass tries again, instead of
  refusing the same contacts in a loop. (#293; thanks @javayou13131)
- The contact import offers the field names the campaign's flow reads
  (`flowFields`) in a dropdown per column, and preselects a column whose header
  already names one, ignoring case; the first of two headers naming the same
  field takes it. When the flow reads no fields, only the number and the time
  zone can be mapped; when the server could not read the flow, the field name is
  typed as before. (#293; thanks @javayou13131)
- `AICC_DEFAULT_COUNTRY_CODE=86` is how a Chinese number written nationally
  (`13800138000`) is dialled as `+8613800138000`; a campaign's own
  `defaultCountryCode` does the same for one campaign, and a number written
  `+86…` needs neither.

## v0.7.0 - 2026-10-24

### Upgrade notes

- Upgrade both images together, as always: they are published under one tag
  and the deploy stack names both. The switch image has no change in this
  release. The application adds migration 00043, which runs at startup: a
  callback whose `phone_number` has no digit at all (the bot once filed the
  words "the caller's current number") gets the caller's number from its
  call's CDR, which is `to_number` on a call this platform placed to a
  customer and `from_number` on every other call. A value that holds digits,
  a callback whose call has no CDR, and a CDR with no number are left alone.
  Nothing is dropped or narrowed, and the rewrite is not undone on rollback
  (#311).
- No new settings. `.env.example` only gained a comment: the bot's
  `take_message` writes a callback number in the form `AICC_OUTBOUND_DIAL_FORM`
  names, with `AICC_DEFAULT_COUNTRY_CODE` deciding which numbers are domestic
  (#311).
- API contract: transferring a caller can now answer
  `409 TRANSFER_IN_PROGRESS` (a new code) while an earlier transfer of the same
  caller, from this operation or from the bot handing the caller over, has not
  been confirmed by the switch; it clears when the leg's events settle it or
  after a minute (#303). The `202` of every call-control operation now reads
  "sent, possibly unconfirmed": the switch accepted the command, or did not
  answer in time and may still run it, so a client must not send it again
  (#302, #303). `QueueFacts`, `CallSnapshot.queue`, `CDR.queueId`,
  `CDR.queueWaitSec`, `QueueReport.queueId`, `CDR.flowId`, `LegKind` and
  `Leg.note` have new descriptions only (#283, #313). `make api-breaking
  BASE=v0.6.0` reports no breaking change.
- Behaviour change: `POST /calls/{callId}/hangup` from a credential that is
  not an agent, on a call handed to an extension nobody is signed in at, now
  ends the call at that phone's leg. Before, it answered an error (#282).
- A CDR status changes: an agent's call to a bot number the bot never
  answered is now `NO_ANSWER`. Before, it was `ANSWERED` with a zero-second
  bot hop (#313).

### Added

- An HTTP tool that has not returned after one second makes the bot say a
  short wait line, once per batch of tools, only into silence and never as a
  question; the result is never lost, even when the caller talks over the
  line. A flow can word it with the optional bilingual `global.toolWaitLine`.
  The one-second threshold is fixed. doubao is excluded: it drops a tool
  result sent after a spoken line, so HTTP tools stay synchronous there and
  the caller hears no wait line (#316, #286).
- The mock backend gains `POST /api/kb/secure-search`, a bearer-token search
  with a configurable delay, for trying the wait line (#316).

### Changed

- A call through several queues now records one `QUEUE` leg per stay, in time
  order. The row's `queueId` and `queueWaitSec` are the first stay's, and
  `missedReason` reads the last stay (#283).
- A call that was answered and then reached nobody (an AI outbound call whose
  bridge to the bot never ran, an overflow, a recovery) keeps the leg it was
  answered on in its journey, with the span `billSec` measures, instead of a
  `DIALING` hop. A bot hop now needs a bot that answered, and the call's
  `flowId` is stamped from the moment the call is placed or the conversation
  starts (#313).
- A call whose bot leg never answered now gets its CDR row from the human
  path; before, such a call could be left without a row (#313).
- The session-limit documentation now states what the code does: a call the
  bot ends has `hangupCause=SESSION_LIMIT`, a transferred call carries
  `userData.botReason=SESSION_LIMIT` and the final leg's cause (#230).
- A non-401 failure of the session check (a 5xx, a network error) shows the
  same error page at `/` and inside the app and keeps the session, so a
  server restart does not look like a sign-out. This reverses the earlier
  rule that sent the reader to the sign-in form (#324).

### Fixed

- An agent-placed call the agent handed on recorded the hand-over
  (`NORMAL_CLEARING` from the agent's leg) as its `hangupCause`; it is now the
  cause of the leg whose release ended the call (#282).
- A call through two queues kept one set of queue facts, so the second join
  overwrote the name and join time but kept the first stay's bridge and
  cause, and the wait could come out negative (#283).
- A leg to one of our extensions read `TRUNK` in the journey when nobody was
  signed in there; it is now an `AGENT` leg labelled with the extension
  (#284).
- A command to the switch that was written and not answered was reported as a
  failure, or blocked other callers. It is now treated as sent: hangup,
  transfers (the agent's and the bot's), the platform's own endings and the
  bridge to the bot answer `202`, a second transfer is refused with
  `TRANSFER_IN_PROGRESS`, and a bot hand-over that never ran returns the flow
  to the phase it was made from instead of stranding the caller. A bridge to
  the bot that never ran ends the customer leg with
  `hangupCause=BOT_BRIDGE_UNCONFIRMED`. `ErrDown` now means nothing was
  written, and a job result that arrives before the originate returns keeps
  the switch's cause (#302, #303).
- The bot's BYE sent the Record-Route set in reverse, so behind a
  record-routing proxy the far end never saw it. It now keeps the INVITE's
  order, goes to the top of the route set (the strict-router rule applies to
  an entry without `lr`), carries `Max-Forwards: 70`, and has its own random
  Via branch. Folded Record-Route headers are read whole (#250, #325).
- A transcription tap whose stream dialled back before the attach reply was
  read was reported as `STREAM_NEVER_CONNECTED` though it had transcribed, and
  one whose attach command timed out was forgotten although the switch started
  it later. An early arrival is remembered, a timed-out attach is kept as
  sent, and every expectation is per attachment, so a detach stops exactly
  that stream (#287, #312).
- `take_message` stored whatever the model wrote as the callback number, once
  a description of the caller's number. A value that is not a dialable number
  is discarded with a warning and the caller's number is used; a valid one is
  stored normalised. The `mobile_support` flow no longer asks for the "caller's
  number" in a way that led to it, and the log no longer carries the message or
  the full number (#311).
- Opening the site root while signed in landed on the sign-in form instead of
  the role's home, because the entry route swallowed its own redirect (#296).
  The entry route and the app layout now share one session check, so a visit
  makes one `/auth/me` instead of two; signing out or a 401 clears every
  cached query of the previous user, and the event stream no longer refetches
  the session (#324).

## v0.6.0 - 2026-10-21

### Upgrade notes

- Upgrade both images together, as always: they are published under one tag
  and the deploy stack names both. The switch image itself has no functional
  change in this release; only its documentation changed (a trunk needs a
  mounted gateway file, see Fixed). The application adds migration 00042,
  which runs at startup: campaign contacts gain `last_attempt_id` and
  `last_call_id` (new columns with a CHECK that both or neither are set),
  backfilled from each contact's latest finished attempt, and `callbacks`
  gains an index on `call_id`. Nothing is dropped or narrowed.
- On every start the server now gives a phone from `AICC_EXTENSION_RANGE` to
  each AGENT or SUPERVISOR agent identity that has none, suspended accounts
  included, keeping its agent id, and mirrors the repaired agents to
  mod_callcenter. An exhausted pool is logged and does not stop the server.
  A binding cleared on purpose with `PUT /agents/{agentId}` therefore does
  not last for a call-taking account: it gets a phone again on its next save
  or the next start (#268).
- HTTP tools now connect only through the new outbound host policy, do not
  follow redirects (a 3xx fails the tool) and no longer use the
  `HTTP_PROXY`/`HTTPS_PROXY` environment. Private and loopback addresses stay
  reachable without configuration, and the host of `AICC_BOT_BACKEND_BASE` is
  always allowed, so existing flows load and send the same requests as
  before. Link-local, cloud metadata and unspecified addresses are refused
  even for that host. A tool that names a public backend with the new
  `baseUrl` needs that host in `AICC_TOOL_ALLOWED_HOSTS` (#286).
- The server now refuses to start when `AICC_TOOL_ALLOWED_HOSTS` is
  malformed, including a bare `*` or a single-label wildcard such as `*.com`.
  With the AI leg enabled, publishing a flow whose HTTP tool header references
  an unset `AICC_TOOL_SECRET_<NAME>` is refused; saving it as a draft only
  warns (#286).
- API contract: `POST /calls` can now answer `202` (same body as `201`) when
  the switch has not confirmed the placement; the customer may still be
  called, so a client must not place the call again under a new `callId`
  (a retry with the same `callId` answers `200` with `isDuplicate`). A link to
  the switch that is down answers `503 SWITCH_DOWN` there, as every other call
  operation does, instead of `502` (#221). `CampaignContact` gains
  `lastCallId` and `lastAttemptId`, and `GET /callbacks` gains a `callId`
  filter (#188). `User` gains a required `locale`, and `Identity.locale`
  (`/auth/login`, `/auth/me`) is now a string that reads `en` when the account
  never chose one, instead of `null` (#289). `PUT /users/{userId}` gives a
  call-taking account without a phone one, or answers
  `409 EXTENSION_POOL_EXHAUSTED` (#268). Hold, retrieve and transfer declare
  the `409 OPERATION_NOT_ALLOWED_FOR_CALL_TYPE` they already answered on an
  INTERNAL call (#269). A transfer requested by an agent who is not on the
  call is now refused with the declared `403 NOT_CALL_PARTY` (#239). Nothing
  was removed, and `make api-breaking` against v0.5.0 reports no breaking
  change.
- New settings, both optional: `AICC_TOOL_ALLOWED_HOSTS` (empty lists nothing)
  and the `AICC_TOOL_SECRET_<NAME>` family, one variable per secret.
  `.env.example` describes both.
- Building from source needs Go 1.27.3 or later (#294).
- The seeded `campaign_lead_followup` flow changed (see Fixed). The seed never
  replaces a flow that already exists, so a deployment seeded earlier keeps
  its published revision; load the new one from
  `internal/seed/flows/campaign_lead_followup.json` with `aicc flowadd` if you
  use it.

### Added

- HTTP tools can reach a backend of their own, authenticate to it and take
  more time. A tool's `http` block gains `baseUrl` (an absolute `http`/`https`
  URL that overrides `AICC_BOT_BACKEND_BASE` for that tool), `headers`,
  `timeoutMs` (default 5000, from 500 to 10000) and `query`, and `path` may
  reference required arguments and slots. Substituted values are escaped so a
  model-supplied value cannot change the host or leave its path segment; an
  empty, `.`, `..`, object or list value is refused before any connection, as
  a failure the model can correct. A header value references a secret as
  `{secrets.NAME}`, resolved from `AICC_TOOL_SECRET_<NAME>` when the call is
  made; the value is never stored in the flow, returned by the API, logged or
  shown to the model. `Authorization`, `Cookie` and any header whose name
  contains `auth`, `token`, `key`, `secret` or `password` must reference a
  secret, and a secret may appear only in a header value. Logs carry the path
  template and query parameter names, never rendered values. Which hosts a
  tool may reach is the deployment's outbound host policy, decided on the
  resolved address as the connection is made (`AICC_TOOL_ALLOWED_HOSTS`, see
  Upgrade notes); a refusal fails the tool and logs `http tool connection
  refused by the outbound host policy` with the host, address and rule. Every
  field is checked at load. Anyone who can write flows (`config:write`) can
  send any configured secret to any host the policy allows, so grant it only
  to people trusted with those secrets. `aicc flowsim` sends a placeholder for
  every secret. Guide: `docs/flow-authoring.md` 2.4.1 (#286, part A).
- Campaign leads link to their call by id: a campaign contact names its
  latest finished attempt and that attempt's call (`lastAttemptId`,
  `lastCallId`), and `GET /callbacks?callId=` returns the callback a HIGH lead's
  call filed. The Leads tab resolves a HIGH lead's callback this way instead
  of matching phone digits, so two campaigns reaching one number no longer
  show each other's callback, and it links to the call record only when the
  attempt rang (#188).
- "Assign a phone" on the users screen, for an AGENT or SUPERVISOR account
  that has no phone. It saves the account unchanged; `PUT /users/{userId}`
  now provisions the phone for any call-taking account without one, not only
  for a promoted administrator, and keeps the existing agent id with its
  state history and wrap-ups (#268).
- An isolated end-to-end environment for outbound calls (`deploy/e2e/`,
  `make e2e-up`, `e2e-smoke`, `e2e-campaign`, `e2e-capacity`, `e2e-down`):
  PostgreSQL, the published switch image, the application built from the
  checkout, the mock provider and a scripted callee on one internal network
  with no way out, guarded against real keys, real trunks and public
  endpoints before anything starts. It places an AI outbound call through the
  public API and checks the dialog and the CDR; runs campaigns of US and
  Chinese numbers through busy, rejected, invalid, unanswered, early-media,
  do-not-call, answered and retried cases and SIP response probes, with
  run-wide invariants (provider sessions equal answered dialogs, no redial
  after an answer, capacity released, one CDR per placed attempt); and checks
  bot-slot overflow and the DID line limit. It runs in CI on changes to the
  paths it covers. Built on two test tools that are not shipped in the
  release images or bundle: `aicc-calleesim`, a SIP callee that plays a
  scenario per number and attempt, and a scenario mode for
  `aicc-mockprovider` with scripted speech, tool calls and session end
  (#242, #243, #244, #245, #246, #247).
- `make test-stress PKG=... [RUN=...]` for contributors: runs tests with
  `-race -count=20 -cpu 1,2,8` on a few CPUs kept busy by a parallel test run
  and busy loops, as a loaded CI runner would. A change that adds or alters a
  test with goroutines, fakes, sockets or timers runs it, and
  `CONTRIBUTING.md` describes it with the injected-sleep check (#255).

### Changed

- An internal call (one extension calling another) no longer puts either
  agent into after-call work. Wrap-up starts when an agent's own answered leg
  hangs up on a call that is not INTERNAL, decided by the call type stamped at
  creation, so an internal call transferred out to a customer still owes no
  wrap-up. Calls a SIP phone dials by itself are still typed INTERNAL whatever
  they dial (#77), so until that is fixed such a call to a customer number
  opens no wrap-up either; the browser phone cannot place one (#272).
- `isAutoAnswer` on an agent now does only what its new description says: the
  phone answers a call a queue delivers without ringing. A direct dial, a
  transfer or a colleague's click-to-dial to that extension rings and is
  answered by hand; the agent's own click-to-dial still answers their phone by
  itself. Transfers used to auto-answer at every extension, flagged or not,
  only because of the leaked header fixed below (#266).
- Agent-placed calls in the CDR journey: when the agent who placed a call
  handed it on (to a colleague, a queue or an outside number) and left before
  it ended, the journey keeps an `AGENT` leg for them, timed by their own
  bridges, and every journey is now listed in the order its legs joined. A
  call the agent stayed on is still one trunk leg. The `AGENT` leg of an agent
  who dialled a bot now counts their bridged time rather than answered to
  released. The demo history gains such calls, and `AICC_SEED=fresh` removes
  contacts only for the inbound calls the seed created (#267).
- Go 1.27.3 and newer `golang.org/x/net`, `golang.org/x/crypto` and
  `google.golang.org/grpc`, for reported vulnerabilities in code the server
  calls (certificate verification, HTTP/2, `net/http`, `crypto/tls`,
  `html/template`) (#294).

### Fixed

- An AI outbound call or click-to-dial whose originate reply was late, because
  the switch had stalled, was answered `502 SWITCH_DOWN` while the switch went
  on to place it: the customer's phone rang for a call the client had been
  told failed, a retry with the same `callId` would have dialled again, a
  campaign counted `ORIGINATE_FAILED` for a ringing phone, and a click-to-dial
  agent leg could sit parked with nothing to transfer it. A late reply is now
  an unconfirmed placement: `POST /calls` answers `202`, the line, business
  data and answer handling stay held, and the placement is tracked for the
  answer window plus a margin. A channel the switch creates in that time is
  the call, handled like any other; a failure the switch reports is recorded
  with its own cause; if nothing happens, a FAILED CDR with `hangupCause`
  `PLACEMENT_UNCONFIRMED` is written. A channel created after that is hung up
  (`ALLOTTED_TIMEOUT`) and its own record supersedes the failed one.
  Concurrent requests with one `callId` dial once, and a campaign attempt in
  this state stays in flight until it can be settled from a record or as
  `UNKNOWN` (#221).
- A click-to-dial's auto-answer request (`Call-Info: answer-after=0`) no
  longer reaches the carrier or any leg dialled after the agent's: it is
  removed when the agent's own leg answers, so a call transferred to another
  extension rings there instead of being answered without ringing (#266).
- `POST /calls/{callId}/transfer` moved the customer of any call whose id an
  agent held, even one they were not on. It now requires the requesting agent
  to have a live leg on the call and answers `403 NOT_CALL_PARTY` otherwise,
  sending nothing to the switch (#239).
- Saving an account no longer clears the person's UI locale. `User` never
  returned `locale`, so a read-modify-write had nothing to send back; it now
  reads the stored value, or `en`, and the edit dialog and "Assign a phone"
  send it back (#289).
- The Realtime client no longer waits forever on a `response.create` the
  engine silently ignores, which left later lines queued and tool results
  waiting for a turn that never came. An unanswered request is given up after
  a bounded wait: a line waiting behind it is asked for instead, otherwise the
  request is retried once (a line with its direction put in the conversation),
  and a retry that is not answered either is dropped. A request that was only
  late still gets one turn. Each case logs one WARN and counts
  `aicc_provider_response_unanswered_total`. Doubao is unaffected (#231).
- A hangup that moves the call into a terminal phase with a closing line now
  waits for a later turn that played audio: an audio-less turn no longer runs
  the armed ending or counts as the line, and a repeated hangup with nothing
  said since is refused once with `CALL_ENDING`, asking for the line. A model
  that insists is hung up after that turn plays; one that never speaks ends at
  the cap with a warning (#207).
- The seeded `campaign_lead_followup` flow hears a change of mind: after a
  HIGH, MEDIUM or LOW stance the bot confirms in one sentence and asks once
  whether there is anything else, records again if the stance changes, and
  hangs up on "nothing else", a goodbye or silence. A clear no (`NONE`) and
  `DO_NOT_CALL` still end the call at once (#206).
- An audit row is no longer lost when the client disconnects after the
  handler answered: the trail, and the record of a revealed extension
  password, are written on a context detached from the request, with a
  timeout so a stalled database cannot hold the request (#273).
- Copy buttons work over plain HTTP, where the browser has no clipboard API:
  the extension SIP password, the API key shown once and the cockpit's call
  id. "Copied" appears only after a confirmed copy; otherwise the button asks
  for Ctrl+C (⌘C) and selects the value (#290).
- The "Set up your phone" card opens soon after the page loads when the page
  carries no sign of the phone extension, instead of after a full run of
  unanswered hellos, and its first step is already ticked or unticked when it
  opens. A page the extension has marked keeps the full run (#292).
- Docs: the switch's READMEs and its Docker Hub page called
  `PSTN_GATEWAY_HOST`/`PSTN_GATEWAY_PORT` the whole trunk configuration. The
  image ships no gateway: a trunk is a gateway file in
  `conf/sip_profiles/external/` and an outbound route in
  `conf/dialplan/aicc/`, and without the gateway outbound calls fail with
  `INVALID_GATEWAY` (#251).

## v0.5.0 - 2026-10-16

### Upgrade notes

- Upgrade both images together. The switch image changes `aicc_outbound.lua`
  (it stamps and exports `aicc_did` on the outbound trunk leg, which the DID
  line limit counts). The application adds migrations 00035 to 00041, which run
  at startup: post-call transcription, the do-not-call list, campaigns and
  their contacts and attempts, the CDR's `campaign_id`, the DID's
  `max_outbound_channels`, and a data correction that clears `has_recording` on
  CDR rows no stored recording backs (#196). Nothing is dropped or narrowed.
- Breaking: the Gemini provider is removed. A deployment with
  `AICC_PROVIDER=gemini` no longer starts; choose `openai`, `grok`, `qwen`,
  `gateway` or `doubao` and set that provider's key before upgrading (see
  Removed).
- Flows are re-validated at the start of every call. A flow that names a slot
  nothing in it writes no longer loads, so its calls are refused until it is
  fixed; check each custom flow with `aicc flowadd` before moving traffic (see
  Changed).
- The server now refuses to start when `AICC_OUTBOUND_ENDPOINT` is set to
  anything but a `sofia/` dial string holding exactly one `%s`, when
  `AICC_TRANSCRIBE_MODEL` names a model the qwen client does not accept, and
  when any of the new settings below is malformed or out of range.
- API contract: new operations for campaigns (`/campaigns`, `/campaigns/settings`,
  `/campaigns/{campaignId}` and its `start`, `pause`, `resume`, `cancel`,
  `contacts` and `attempts`), the do-not-call list (`/dnc-numbers`) and
  post-call transcription (`/calls/{callId}/transcription`), under the new
  scopes `campaigns:read`, `campaigns:write`, `dnc:read` and `dnc:write`. The
  CDR gains `campaignId`, the DID gains `maxOutboundChannels` and
  `isCampaignReady`, `POST /calls` gains the refusals `NUMBER_ON_DNC`,
  `DID_NOT_OUTBOUND` and `DID_CHANNELS_EXHAUSTED`, and the event stream gains
  `CAMPAIGN_UPDATED`. Nothing was removed. A call that ends without a stored
  recording no longer reports `hasRecording: true`.
- New settings, all optional: `AICC_BOT_NO_INPUT_TIMEOUT`,
  `AICC_OUTBOUND_RING_TIMEOUT`, `AICC_OUTBOUND_SIP_HEADERS`,
  `AICC_DEFAULT_COUNTRY_CODE`, `AICC_OUTBOUND_DIAL_FORM`,
  `AICC_OUTBOUND_HOURS`, `AICC_OUTBOUND_MAX_RINGING`,
  `AICC_CAMPAIGN_FREQUENCY_WINDOW`, `AICC_TRUNK_BREAKER_COUNT`,
  `AICC_PROVIDER_BREAKER_COUNT`, `AICC_BOT_INBOUND_RESERVE`,
  `AICC_OVERFLOW_HOLD`, `AICC_OVERFLOW_TIGHTEN_RATE`,
  `AICC_OVERFLOW_STOP_RATE`, the `AICC_POSTCALL_TRANSCRIBE_*` family and
  `XAI_API_KEY`. `.env.example` lists each with its default. A deployment that
  keeps a do-not-call list and dials national numbers should set
  `AICC_DEFAULT_COUNTRY_CODE`; `aicc doctor` warns when it is empty and the
  list is not.
- An outbound call that rings past the 60 second answer window is now hung up
  when it answers late, and every outbound leg rings for at most
  `AICC_OUTBOUND_RING_TIMEOUT` (default 45 s).

### Added

- Campaigns: AI outbound campaigns. A campaign dials an imported contact list
  from one DID, every answered call talks to that DID's flow, and each contact
  yields a structured lead. The customer is dialled first and the bot leg
  exists only once they answer, so an unanswered call costs no bot session.
  Campaigns are AI-only: a flow that can `transfer_to_agent` is refused on
  create and start, and while a running or paused campaign dials from its DID
  (`CAMPAIGN_FLOW_HAS_TRANSFER`, `CAMPAIGN_DID_NOT_OUTBOUND`). A campaign has a
  calling window checked against the callee's time zone (per contact, else the
  campaign's) and the deployment's outer hours, a ring timeout, a retry policy,
  and a state machine of DRAFT, RUNNING, PAUSED, COMPLETED and CANCELLED, with
  racing transitions decided by the database. The dialer writes each attempt
  before it dials, classifies the outcome (busy, no answer, answered,
  transient and originate failures, bot failures) and schedules retries.
  Consecutive trunk failures or bot failures pause a campaign
  (`AICC_TRUNK_BREAKER_COUNT`, `AICC_PROVIDER_BREAKER_COUNT`). After one
  campaign reaches a number, no other campaign dials it for
  `AICC_CAMPAIGN_FREQUENCY_WINDOW`. Contacts are imported up to 5000 per
  request with a result per row, numbers canonicalised to E.164. API:
  `/campaigns` with `campaigns:read` and `campaigns:write`; the SPA has the
  campaign, do-not-call and line-limit screens. Design:
  `docs/design/10-campaigns.md` (#89, #154, #163, #164).
- Campaigns keep bot capacity for inbound and bound the over-dial: admission
  counts the answers a campaign's ringing legs are expected to bring instead
  of one bot slot per leg, and stops short of `AICC_BOT_INBOUND_RESERVE`
  slots (unset keeps a small default share free). An answered outbound call
  that finds no free bot slot is held for `AICC_OVERFLOW_HOLD` (default
  1500 ms) and then hung up as `OVERFLOW`; a campaign whose answered calls
  overflow beyond `AICC_OVERFLOW_TIGHTEN_RATE` plans more cautiously, and above
  `AICC_OVERFLOW_STOP_RATE` stops over-dialing until the rate falls
  (`AICC_OUTBOUND_MAX_RINGING` bounds the legs ringing at once) (#158, #165).
- Do-not-call list: `GET`/`POST /dnc-numbers` and `DELETE /dnc-numbers/{number}`
  (scopes `dnc:read`, `dnc:write`). A destination on the list is refused with
  `422 NUMBER_ON_DNC` on every outbound path, click-to-dial included, and no
  CDR is written; the campaign dialer checks it before every call. A national
  number is compared after completing it with `AICC_DEFAULT_COUNTRY_CODE`;
  with that empty it cannot be matched and the call goes through, so set it on
  a deployment that keeps a list. A customer who asks not to be called is added
  by the flow (`record_lead` with `DO_NOT_CALL`) (#162).
- Flow tool `record_lead` on campaign calls: the bot records the call's
  conclusion (`intent` `HIGH`, `MEDIUM`, `LOW`, `NONE` or `DO_NOT_CALL`, and
  notes) on the call's attempt. At the end of the call `DO_NOT_CALL` adds the
  number to the do-not-call list and `HIGH` files one callback on the
  campaign's follow-up queue. A campaign call is offered neither
  `transfer_to_agent` nor `take_message`. The seed gains the
  `campaign_lead_followup` flow on 95061 (English) and 95062 (Chinese); no
  campaign is seeded (#166).
- Campaign reporting: the CDR carries `campaignId` and `GET /cdrs` filters on
  it, a webhook subscription can filter on `campaignId`, and `CAMPAIGN_UPDATED`
  events on the stream tell supervisors when a campaign moved (#167).
- `maxOutboundChannels` on a DID (`/dids`): how many outbound calls the number
  may carry at once, as the carrier sells lines per number. Unset is no limit.
  A single call or click-to-dial over the limit answers
  `409 DID_CHANNELS_EXHAUSTED` with no CDR, and a campaign waits. The switch
  stamps the DID on the trunk leg (`aicc_did`) so hand-dialled legs count too
  (#160, #165).
- The campaign contact importer in the web app reads Excel workbooks (`.xlsx`)
  as well as CSV, with the same mapping, preview and rejected-rows download
  (#192; thanks @javayou13131).
- `aicc flowsim`: replays a scripted scenario against a flow on the real flow
  engine with no telephony and no model, and `go test ./internal/flowsim` runs
  it for every seed flow. A second tier lets text models play the bot and the
  caller. Every seed flow ships with scenarios. Guide:
  `docs/flow-authoring.md` section 8 (#85).
- `AICC_OUTBOUND_RING_TIMEOUT` (default `45s`, at least `5s` and below the
  60 second answer window): how long an outbound leg, AI outbound or
  click-to-dial, rings before the switch gives up. A single call used to ring
  for as long as the carrier allowed, and an answer after the window was
  bridged to nobody and left parked; it is now hung up (`ALLOTTED_TIMEOUT`)
  (#155).
- `AICC_OUTBOUND_DIAL_FORM`: how the campaign dialer writes a number to the
  trunk, `E164` (default), `E164_NO_PLUS` or `NATIONAL`, for a trunk that
  refuses the default. Campaigns store and compare E.164 whatever it is
  (#164).
- `AICC_DEFAULT_COUNTRY_CODE` completes a national number for the do-not-call
  check and contact import. The compose stack sets it to `86`; the
  application's own default is empty. `aicc doctor` gains a `WARN` status and a
  `dnc_country_code` check that uses it; a `WARN` does not change the exit
  status (#162).
- `AICC_PROVIDER=grok`: xAI Grok Voice (`grok-voice-3.0`, key in `XAI_API_KEY`)
  as a profile on the Realtime client, with G.711 passed through untouched. The
  client now ignores the `ping` event, treats cumulative
  `...input_audio_transcription.updated` as partial text, and counts a
  `...transcription.completed` as final only when its `status` is absent or
  `completed`, so each caller utterance is one transcript line. A refused
  `response.cancel` with nothing active is no longer reported as an error.
  Grok drops a per-response instruction override once the conversation has a
  turn, so on that profile every mid-call spoken line is first put in the
  conversation as a caller message (profile trait `IgnoresOverrideOnceTalking`).
  `aicc doctor` and the installer (`--provider grok`) know the provider. Findings:
  `docs/design/grok-findings.md` (#226).
- Flow tools can keep a list across their calls: `collect` on an HTTP tool
  appends a rendered value (`{args.x}`, `{slots.y}`) to a list slot after each
  successful call for which its optional `when` condition holds, without
  duplicates and at most 20 values. `{slots.<slot>}` renders the list and
  `<slot>.count` its size, so a transition can branch on it. Checked at load
  (`docs/flow-authoring.md` 2.6.1) (#116).
- Post-call transcription of recordings with `qwen-audio-3.1-asr-flash`, off by
  default (`AICC_POSTCALL_TRANSCRIBE_ENABLED`, plus an HTTPS endpoint and
  `ALIYUN_API_KEY`). A finished call's stereo recording is split into its two
  channels, cut into clips of at most 4.5 minutes and sent to the model; the
  segments are stored apart from the live transcript and read with
  `GET /calls/{callId}/transcription`. Calls whose live transcript already
  covers both channels are skipped, and recordings outside a minimum and
  maximum length are too. A daily audio cap, retries with backoff and a
  concurrency limit bound the cost. `POST /calls/{callId}/transcription`
  (scope `quality:review`) asks for a call to be transcribed or transcribed
  again; it answers `409 TRANSCRIPTION_DISABLED` when the feature is off and
  `429 RATE_LIMITED` at the cap. `aicc transcribe-recordings` plans and runs
  the same work over existing recordings, with a dry run that prints the audio
  minutes (#27).
- `AICC_OUTBOUND_SIP_HEADERS`: `Name=value` pairs, separated by `;`, added as SIP
  headers to the AI outbound customer leg only (not click-to-dial, not the
  bridge to the bot). A deployment behind FreeSBC sets `X-FSBC-Out=1` so the AI
  leg carries the same loop marker the dialplan stamps on click-to-dial legs;
  the application names no SBC vendor itself. Names must start with `X-`, stay
  out of the `X-AICC-` namespace and be unique; values cannot contain `;`,
  braces or control characters. A malformed value refuses startup and names the
  pair (#62).
- `AICC_BOT_NO_INPUT_TIMEOUT`: how long the bot waits for the caller before a
  dead-air `NO_INPUT`, as a Go duration from `2s` to `60s` (default `8s`, the
  old fixed value; it cannot be turned off). A malformed or out-of-range value
  refuses startup (#49).
- `global.temperature` in a flow sets the model's sampling temperature, in
  `0 <= t < 2`, published and versioned with the persona like `global.voice`.
  Absent keeps the provider's default, so existing flows are unchanged. The
  qwen client sends it; openai, grok, gateway and doubao do not (grok accepts
  the field but it showed no effect) and ignore it, and the server logs that
  once. The bot detail page
  shows the value (#11).
- Slot tools: a flow tool with `"kind": "slot"` records a fact the caller
  states into a declared slot (`slot`, optional `enum`), so it survives the
  model's own context and can be read back with `{slots.<slot>}` in an
  instruction, an announce, an HTTP body or a transition condition. An empty
  value, one over 256 characters or one outside the enum is refused and
  nothing is stored. Every tool call also keeps its scalar arguments as
  `<tool>.args.<k>`, and a call placed with business data starts with it as
  `userData.<k>`. The seed `lead_qualification` flow records interest, budget
  and timeline this way. Authoring guide: `docs/flow-authoring.md` §2.8 (#13).
- `GET /campaigns/settings` (scope `campaigns:read`): the deployment's outer
  calling hours (`AICC_OUTBOUND_HOURS`), the defaults a create applies
  (`maxConcurrent`, `ringTimeoutSec`, the retry policy) and the limits the
  server enforces, built from the same values it validates against. `DID`
  gains a read-only `isCampaignReady`: enabled, allows outbound, and a
  published flow that cannot transfer, the same rule `createCampaign` applies.
  Creating a campaign now publishes one `CAMPAIGN_UPDATED` (status `DRAFT`),
  shortly after the `201`. The campaign form reads all three, and pre-validates
  the calling window and numeric limits before sending (#189).
- Authoring guide for flows (`docs/flow-authoring.md`): where business rules,
  knowledge and long operations belong, the closing rule, and how to verify a
  flow. Its worked example is now the seeded `plan_change` flow, reworked to
  match: one `plan_check` decision tool (`OK_TO_CHANGE`, `NEED_ID_VERIFY`,
  `NEED_HUMAN`, `NOT_FOUND`) replaces the account lookup and the in-contract
  prompt rule, `kb_search` answers fee and policy questions, `plan_submit`
  returns a ticket that `plan_status` follows, and a `NEED_HUMAN` or refused
  submit hands over with the decision and reason in the transfer summary. The
  mock backend serves `/api/plan/*` and `/api/kb/search` in place of
  `/api/bb/*`, with a fixture number per decision (#83).

### Changed

- Recovery: on start and on every reconnect to the switch, the application
  reads the switch's live channels and adopts the ones it does not own. A ringing
  outbound leg is re-armed for what is left of its answer window; a leg whose
  window is spent, an answered leg still parked, and a customer leg bridged to
  the bot with no session in this process are hung up. Outbound admission opens
  after the first pass, so a placement made during startup answers `503
  SWITCH_DOWN` if the switch never reports (#157).
- An originate that never made a channel is now recorded with its cause: the
  application consumes the switch's background-job result, so a refused or
  failed dial is a CDR with a reason instead of a call that silently vanished
  (#156).
- An answered AI outbound call reserves its bot slot before it is bridged, and
  waits for a free one instead of being bridged into a refused session (#158).
- Providers: the code kept only for Gemini is gone from the provider
  interface, with no change to any call: the unused tool-result hint, the
  greeting cue, semantic turn detection and its qwen `smart_turn` hold, and two
  event fields nothing read. Every call already used the fixed `server_vad` turn
  detection, and `RequiresTerminalAnnounce` and the tool-result variant of it
  became one capability (#225).
- A flow whose `{slots.x}` (in an instruction, an announce or an HTTP body) or
  transition condition names a slot nothing in the flow writes no longer
  loads. Such a reference used to render as empty or never match. The
  published revision is re-validated at the start of every call, so such a
  flow refuses its calls after the upgrade until it is fixed: check each
  custom flow with the new binary (`aicc flowadd` validates before it stores)
  before moving traffic to it. None of the seed flows is affected (#13).

- The server now refuses to start when `AICC_OUTBOUND_ENDPOINT` is set to
  anything but a `sofia/` dial string holding exactly one `%s` (loopback never
  worked for AI outbound calls). Unset is still allowed.

- Live transcription on the qwen recogniser now defaults to
  `qwen-audio-3.1-asr-flash-message` (was `qwen-audio-3.0-asr-flash-streaming`).
  The client asks this model for live partials and leaves out the parameters it
  does not support. Set `AICC_TRANSCRIBE_MODEL` to keep a `-streaming` or
  `fun-asr-*` model; their parameters are unchanged.
- The server now refuses to start when `AICC_TRANSCRIBE_MODEL` names a model
  outside the DashScope-native families on the qwen client (a typo used to fail
  only after the first call finished, with "Model not exist.").
  `qwen3-asr-flash-realtime` is refused explicitly: it speaks the OpenAI dialect.
- The embedded web app is served gzip-compressed to clients that accept it.
  The build writes a `.gz` beside each compressible file and the server picks
  it by `Accept-Encoding`; nothing is compressed at request time, and the API
  and `/events` are untouched. The index page at `/` is now `no-cache`, like
  the SPA fallback (#64).
- CDR and audit-log listings with an open time bound (`from` only, `to` only
  or neither) now use the time index under a generic query plan too. Results
  are unchanged: `from` is inclusive, `to` exclusive (#63).

### Removed

- The Gemini voice provider. `AICC_PROVIDER` now accepts `openai`, `grok`,
  `qwen`, `gateway` and `doubao`. Gemini could not apply a mid-call instruction change,
  which every other engine does at once (measured live on 2026-10-15), so the
  provider and the machinery that existed only for it are deleted: the client,
  the `PROVIDER_SESSION_EXPIRED` failure cause and its handling, the
  `aicc_provider_sessions_expired_total` metric and `AICC_GEMINI_STRICT_TESTS`.
  Migration: a deployment with `AICC_PROVIDER=gemini` no longer starts and must
  choose another value (`openai`, `grok`, `qwen`, `gateway` or `doubao`) together with
  that provider's key; `GEMINI_API_KEY` is ignored, and the installer no longer
  offers `--provider gemini`. New calls no longer end with hangup cause
  `PROVIDER_SESSION_EXPIRED`, and the metric is no longer produced; call records
  written earlier keep that cause and are still listed and reported as before.
  The evidence record is now an archive note,
  `docs/design/gemini-findings.md` (#224).

### Fixed

- Post-call transcription of a click-to-dial recording no longer swaps the
  speakers: the recording is made on the agent's leg, so the agent's speech
  is labelled as the agent's, as the live transcript labels it (#161).
- An outbound call whose callee declined (`USER_BUSY`) is recorded as
  `BUSY` instead of `NO_ANSWER`. Earlier rows are not rewritten (#180).
- The browser phone's widget is withdrawn whenever the page stops having a
  phone, not only on sign-out: an expired or revoked session, or a role change,
  used to leave a dead phone on the sign-in page or a blank tab. A page whose
  session is gone goes to `/login` (#217).
- A dialled number with a leading `+` (E.164) is accepted by click-to-dial and
  `POST /calls` instead of being refused as not dialable (#159).
- AI outbound calls (`kind: AI_OUTBOUND`, and so every campaign attempt) are
  now recorded when their DID records. The answered customer leg had been
  bridged straight to the bot with no recording started, while the CDR still
  said `hasRecording: true`. The recording now starts on that leg before the
  bridge: one stereo file, the customer on the left and the bot and then any
  agent on the right, filed under the call's start date. `hasRecording` now
  means a recording was stored. The bot ledger no longer writes the DID's
  setting, and a later CDR write cannot clear the flag (#196).
- A silent caller on an engine that takes no text cue (Doubao) is now closed. The
  reprompt after the first dead-air NO_INPUT failed, no turn followed, and
  nothing watched the silence again, so the NO_INPUT count stayed at one and
  neither the flow's rule nor `global.closingTarget` could fire (#228). A
  failed reprompt now restarts the dead-air watch.
- The phone setup card no longer stays up with "Allow this site" unticked
  when an agent reaches the cockpit by typing its address. Chrome had
  prerendered the page, and web-sip-phone up to 1.1.1 withdrew from a
  prerendered page for good. The fix is in the extension and needs
  web-sip-phone 1.1.2 or later; requirement R10 in
  `docs/web-sip-phone-protocol.md` records it (web-sip-phone#30).
- The `mobile_support` flow no longer asks for the order number again, or says
  it did not hear it, when the caller asks about "my order" after looking up
  several. It keeps the orders found, answers for the only one directly, and
  asks which one, naming only those, once two were found; a number that was
  not found is never offered (#116).
- The browser phone's widget no longer shows on administrators' pages or the
  sign-in page, where it read "Not connected / Not configured" as if a phone
  were broken. The page declares that only a page with a phone shows it; agents
  keep theirs, and a ringing or live call shows it on every tab. Needs
  web-sip-phone 1.1.1 or later; an older extension shows it everywhere, as
  before (#204).
- `POST /calls` with kind `AI_OUTBOUND` no longer presents a number that does
  not allow outbound calls as the caller id: it answers `422 DID_NOT_OUTBOUND`
  and places nothing. `aicc flowadd -did` now says that the number it creates is
  inbound only and how to open it for outbound (#179).
- AI calls: a bot no longer says its closing line and then leaves the call
  open until the dead-air timer. The seeded closing phases that asked the model
  to speak a goodbye and then call `hangup` (`lead_qualification`,
  `early_collections`, `campaign_lead_followup`) are now terminal phases with an
  `announce`, or tool-first, and `docs/flow-authoring.md` states the rule. On
  the Realtime clients one batch of tool results now asks for one response, not
  one per result (no more `Cannot create response while another response is in
  progress` WARNs); a non-terminal phase change sends the new instructions
  before the tool answer asks for a turn; the dead-air cue tells a bot that has
  closed the conversation to call `hangup` instead of repeating its question;
  and the unbacked-claim check reads "Thanks for your time", "have a great day"
  and "祝您生活愉快" as farewells. A caller who speaks over the start of the closing
  line (more than 1.5 s of it still unplayed) is now answered before the armed
  hangup or transfer runs, instead of the call ending on their question; speech
  over its last 1.5 s still ends the call at once. The grace cap covers every
  answer to such an interruption, not only the first, and a `transfer_to_agent`
  or `hangup` the armed ending makes pointless is refused (`CALL_ENDING`) so the
  model does not promise a transfer that will not happen; after such an
  interruption the model's first bare `hangup` is refused once, and only a turn
  that played audio runs the ending. A bot that says goodbye in
  words without calling `hangup`, followed by a silent caller, is closed through
  `closingTarget` instead of being asked "are you still there?". `early_collections`
  now has an owner-refuses branch in `greet`, no model-spoken goodbye in its
  `hangup` rule, and `maxTurnsWithoutTool: 10` (#147).
- A call the bot answered and finished alone had its CDR start at the bot
  session's start: `startedAt` equalled `answeredAt` and `ringSec` was 0, so an
  AI outbound call's ringing was missing from the ledger. The row now takes
  `startedAt` from the call's creation (the originate, on an outbound call) and
  `answeredAt` from the carrier-facing leg's answer, so `ringSec` and `totalSec`
  include the ringing and `billSec` runs from the carrier answer. `botSec` and
  the flow's time budget still run from the bot leg. Historical rows are not
  rewritten (#181).
- Installer: on Linux, `.env` is root-owned and mode 0600, so the closing
  message's "the password is `AICC_SEED_PASSWORD` in `/opt/aicc/.env`" led to
  a `Permission denied`. The installer now also prints the command that reads
  it, `sudo grep AICC_SEED_PASSWORD /opt/aicc/.env` on Linux and the same
  without `sudo` on macOS, and `deploy/README.md` says the same (#123, #130;
  thanks @javayou13131).
- Docs: the compose commands for an installed stack (`aicc doctor` in
  `deploy/README.md`, and those in `docs/install-verification.md`) now carry
  `sudo` on Linux, where compose cannot read the root-only `.env` without it;
  the dump check of an upgrade runs its redirect as root too. macOS keeps the
  forms without `sudo` (#131).
- Cockpit: right after signing in, the phone setup card could ask an agent to
  allow a site the extension was already allowed on. The card now opens only
  after a full run of unanswered hellos, never before the extension's first
  answer; the extension removing and re-adding its marker on the page no
  longer reads as it having gone; the page asks again on window focus and
  every 10 seconds while nothing answers, so the card leaves without a reload
  when the extension comes back. Until the first answer the phone chip says
  "Connecting…", and the header's state timer says in a tooltip that it is the
  time spent in the current state (#187).
- Live transcription on `qwen` sends the call's language as `language_hints`
  on `qwen-audio-3.1-asr-flash-message` too. Since that model became the
  default, nothing told the recogniser which language a call was in, and the
  recognised language drifted within a call (an English "Hello." stored as
  `哈喽。`) (#127).

## v0.4.0 - 2026-10-10

### Upgrade notes

- Upgrade both images together. The switch image builds `mod_audio_stream`
  from our fork (see Changed), forwards the call type on a bot overflow in
  `aicc_queue.lua`, and takes the new optional `PSTN_GATEWAY_PROXY` setting.
  The database schema is unchanged. One migration corrects data: it clears
  `cdrs.has_recording` on every call that has no stored recording, so the CDR
  list stops offering recordings that cannot be played (#196).
- API contract: `POST /agent/login` no longer accepts `extensionNumber`. The
  body is optional and a body carrying any property is refused with
  `VALIDATION_FAILED` (422); the agent is always signed in at the extension
  bound to them. `PUT /agents/{agentId}`
  that changes or clears the bound extension of a signed-in agent is now
  refused with `CONFLICT`; the agent signs out first. `Presence` gains the
  optional `sipDomain`. Nothing else was removed.
- New settings, all optional: `AICC_BOT_RTP_DEAD_TIMEOUT` (default `5s`),
  `AICC_BOT_FIRST_MEDIA_TIMEOUT` (default `30s`),
  `AICC_BOT_GREETING_MEDIA_WAIT` (unset keeps the old behaviour), and the
  switch's `PSTN_GATEWAY_PROXY`. A negative or malformed duration is refused
  at startup.
- A bot call that receives no inbound media at all is now ended after 30
  seconds, not 5. A call whose media was flowing and stopped still ends after
  5 seconds.

### Breaking (API)

- `POST /agent/login` no longer accepts `extensionNumber` (#106, recorded as
  breaking in #125). The body is optional and a body carrying any property is
  refused with 422 `VALIDATION_FAILED`; the agent is always signed in at the
  extension bound to them. An integration fixes this by dropping the field, or
  by sending no body. Signing in at an extension other than your own is gone.
  To move an agent, change the binding with `PUT /agents/{agentId}`
  (`config:write`), which is refused with 409 `CONFLICT` while the agent is
  signed in. This shipped without a deprecation release: the customer base is
  small and each existing integration was told directly.

### Added

- Deploy: `AICC_BOT_GREETING_MEDIA_WAIT` holds the bot's first turn until the
  caller's inbound media has started. Behind a carrier that opens its media
  path seconds after the answer, the greeting was spoken into a path that did
  not exist yet and the caller never heard it. The model session still opens
  at call start. Unset greets as soon as the session is ready; `0` waits for
  the first inbound RTP however long; a positive value waits for it or that
  long. The logs now record `first media` and `first bot audio`
  (`isBeforeMedia`), and the metrics `aicc_bot_first_media_ms` and
  `aicc_bot_first_audio_ms{before_media}`, whether or not the setting is
  used (#98).
- Deploy: `AICC_BOT_RTP_DEAD_TIMEOUT` and `AICC_BOT_FIRST_MEDIA_TIMEOUT`
  make the bot's two media timeouts configurable; `0` turns one off (#97).
- Switch: `PSTN_GATEWAY_PROXY` sets the host and port the outbound
  Request-URI names, separately from `PSTN_GATEWAY_HOST`/`PORT`, the next hop
  the datagram goes to. A carrier behind FreeSBC v2 needs the two to differ.
  Unset, it follows the host and port, so existing deployments are
  unchanged (#61).
- API: `Presence.sipDomain` is the SIP domain this deployment's phones
  register at, readable without minting a session (#119).

### Changed

- Agents: the phone (`isDeviceRegistered`, `deviceAccount`) is read from the
  switch at the agent's bound extension on every surface: the `AGENT_*` and
  `DEVICE_*` events, `GET /agent/presence`, `GET /auth/me` and the `GET
  /agents` roster. An agent who is signed out is told about their phone, and
  a `DEVICE_*` event is published only when the state changed. Sign-in is
  only at the bound extension (#106).
- Switch: `mod_audio_stream` is built from our fork,
  `rasonyang/mod_audio_stream`, pinned at a commit whose code is the MIT
  release we already shipped; only the submodule URL changed, so it now
  fetches `libwsc` from our fork too. The image gains `mod_audio_stream.repo`
  as a label. Upstream relicensed the module to AGPLv3 after that commit and
  the fork takes nothing from it.
- Docs: the product has no carrier-line configuration screen or API. The
  switch holds one gateway, the SBC's, set once at install; everything about
  a carrier lives in the SBC's own management. The reference dialplan notes
  that an AI outbound leg carries no `X-FSBC-Out` marker (#62).
- Docs: the installer's "not yet verified" caveats for Docker Desktop and
  Linux distributions other than Ubuntu 24.04 are gone.

### Fixed

- AI calls: a carrier that opens its media path late no longer sends every
  inbound bot call to the fallback queue. The dead-media watch used to count
  from the start of the call, so a call that had received no packet yet was
  ended at 5 seconds; it now waits up to `AICC_BOT_FIRST_MEDIA_TIMEOUT` (30
  s) and logs `media never started` (#97).
- AI calls: speech at the moment playback ends cancels the dead-air watch.
  `NO_INPUT` could be reported over a caller who had just spoken (#95).
- AI calls: when a tool result changes a value the phase's instruction
  quotes without moving the call (a second `repair_status` lookup, say), the
  instruction is pinned again before the tool answer, so the next turn is not
  worded from the previous record (#48).
- CDR: a transfer the bot accepted but never handed over (the caller hung up
  during the closing line) keeps the bot's `botSummary` and `botReason` in
  the row's user data (#57).
- CDR: an agent's call to a bot number is the agent's call. The row is
  `OUTBOUND` from the agent's extension whether the bot hangs up, transfers
  or the agent hangs up first, with the agent as primary and legs
  `AGENT, BOT`; `bill_sec` is 0 because no carrier answered. It was written
  by the bot ledger with no agent, counted as bot containment and billed from
  the bot's answer (#76, #80, #104). The call type also travels on a bot
  overflow from a queue.
- CDR: a carrier call whose caller id equals a signed-in agent's extension
  is no longer written as the agent's call, nor written twice. Who placed a
  call is read from the originating leg's own extension, not from a number
  match (#105).
- Agents: `isDeviceRegistered` was false for an agent whose phone was
  registered but who had not signed in, and a stale true survived a later
  unregister (#37, #106).
- Web: the phone setup card ticks "install the extension" as soon as the
  extension is installed, before the site is allowed, instead of telling the
  agent to install what they already have (#81).
- Web: a browser that moved to another deployment, with the agent on the
  same extension number there, mints a phone session for the new deployment
  instead of staying registered at the old one with a chip saying Voice ready
  (#119).

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
