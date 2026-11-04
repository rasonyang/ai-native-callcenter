# End-to-end environment

An isolated stack for automated outbound-call tests: PostgreSQL, the switch
image, the application built from this checkout, `aicc-mockprovider` (the voice
provider) and `aicc-calleesim` (the called party). The test drivers
(`cmd/aicc-e2e` for the smoke, a Go test binary for the campaign tests) run
inside the same network and talk to the application only through the public
API, with a scoped API key.

```sh
make e2e-up        # fresh database, application built from the working tree
make e2e-smoke     # one AI outbound call; checks the callee's record and the CDR
make e2e-campaign  # two campaigns (US, CN): answered and unanswered outcomes, SIP probes, run-wide invariants
make e2e-capacity  # bot-slot overflow (K1), the DID outbound line limit (K2) and a refused bot leg (K3), each in its own application start
make e2e-down      # collect logs, remove containers, volumes and the network
```

Needs Docker with Compose v2.24 or later and Go. Nothing else on the host:
the harness binaries are cross-compiled statically for the Docker daemon's
architecture and run in an image with no shell and no libraries.

## Why it cannot dial a real number

The isolation is a property of the network, not of configuration.

- Every service is on one compose network with `internal: true`
  (`docker-compose.yml`, bottom). A container on it has no route out: no
  container can reach a trunk, a provider, a registry or a name server.
- No service publishes a port, uses host networking or joins another network.
  The `deploy/compose.linux.yml` and `deploy/compose.macos.yml` overlays are not
  used.
- The switch has one extra gateway, `e2e_calleesim`, whose only peer is the
  simulator's fixed address (`calleesim-gateway.xml.in`, rendered into the run
  directory and mounted as that one file; the directory is never mounted, so
  `aicc_bot.xml` stays). Nothing from `deploy/dev/freeswitch`, the development
  machine's trunk, is mounted.
- `AICC_OUTBOUND_ENDPOINT` is `sofia/gateway/e2e_calleesim/%s`. The provider is
  the mock, reached by endpoint, with the placeholder key `not-a-key`; no real
  key is passed. It plays `provider-scenario.json` (see "The mock provider's
  scripts").
- The base file's `deploy/.env` is not read (`env_file: !reset []`, and
  `e2e.sh` runs Compose with a scrubbed environment and its own
  `--env-file deploy/e2e/e2e.env`).

On top of that the harness refuses a run (`internal/e2e/guard.go`):

- before anything starts, `e2e.sh` resolves the configuration
  (`docker compose config`) and `aicc-e2e guard` rejects it unless the network
  is internal and alone, no service publishes a port, `AICC_OUTBOUND_ENDPOINT`
  is exactly the simulator's gateway and the provider is the mock;
- the smoke repeats that check from inside, then asks the switch for
  `sofia status` and rejects it unless its only gateways are `aicc_bot` and
  `e2e_calleesim`.

`E2E_OUTBOUND_ENDPOINT=sofia/gateway/other/%s make e2e-up` is the way to see the
refusal.

## Several worktrees at once

The project name is derived from the checkout's path
(`aice2e-<number>`), and so is the `/24` the network uses (`10.64-127.x.y`), so
two worktrees get two environments that share nothing. Volumes, containers and
the network carry the project name; `e2e-down` removes only those and never
touches the `aicc` project of the demo stack. Pin either one yourself with
`COMPOSE_PROJECT_NAME` and `E2E_NET_PREFIX` (for example `10.77.3`).

Addresses on the network, from the prefix: `.10` application, `.11` switch,
`.12` mock provider, `.13` callee simulator.

## What the smoke checks

1. The driver signs in as the administrator the environment creates, mints an
   API key with only the scopes it needs, and uses that key for everything
   after.
2. It publishes a one-phase flow (`flow.json`) and creates a DID that allows
   outbound calls, then places `POST /calls` with `kind: AI_OUTBOUND` to
   `13800000001`, the number `scenario.json` answers with 200.
3. It asserts: exactly one new dialog in the simulator's record, for that
   number, answered with 200 and acknowledged, with downlink audio above
   silence; exactly one CDR for the DID, and exactly one more row in the ledger,
   for that call, typed `OUTBOUND` and with an answer time.

Every equality is paired with a count, so an empty result fails. Latency
(`firstAudioAfterAnswer`, `inviteToAnswer`, `placeToInvite`) is written to the
summary and never asserted. The smoke provisions its own flow and DID with a
random number, so it can be repeated against a running environment; `make
e2e-up` is what gives a fresh database.

## Campaign tests

`make e2e-campaign` runs the tests of `internal/e2e/campaign*.go` (issues
#245 and #246). They are Go tests under the build tag `e2e`: `e2e.sh up`
compiles them into a static test binary (`go test -c -tags e2e`) that goes into
the harness image and runs with `docker compose run`, inside the internal
network, like the smoke driver. They need an environment that is up; like the
smoke, they can be repeated against a running one, and `make e2e-up` is what
gives a fresh one. `e2e.sh campaign` restarts the simulator first: it answers a
number's n-th INVITE with the n-th behaviour of its scenario, counted from its
start, so C04 (busy, then answered) needs one that has not seen its number, and
the driver refuses one that has.

Two campaigns, one per country, are created and started together, each with
its own DID (the caller id in `From`) and every case of the table below:

- US: `+1 202 555 01xx`, the fictional range;
- CN: `+86 139 8888 66xx`, 11 national digits, a range reserved for this
  harness.

Numbers are never mixed within a campaign, and no number of either range may
be replaced by one a real trunk rings.

| case | callee (`scenario.json`) | asserted |
| --- | --- | --- |
| C03 | 180, then 486 | `BUSY`, cause `USER_BUSY` |
| C05 | 180, then 603 | `REJECTED`, cause `CALL_REJECTED` |
| C06 | 180, then 404 | `INVALID_NUMBER`, cause `UNALLOCATED_NUMBER`; contact `INVALID_NUMBER`; no further INVITE |
| C07 | 180, never answered | `NO_ANSWER`; CANCEL at the 5 s ring timeout ±1 s; no provider session |
| C08 | 183 with early media, never answered | `NO_ANSWER`; CANCEL; no provider session |
| C09 | number on the do-not-call list | `DNC_SKIPPED`, cause `NUMBER_ON_DNC`; contact `DNC`; zero INVITEs |
| P1–P5 | 180, then 480, 408, 503, 484, 410 | recorded, and held to design 10 §7.2's cause table |
| C01 | 180, then 200 after 2 s, a tone on the uplink; the bot records `record_lead` `HIGH` and ends the call | `ANSWERED`; lead `HIGH` on the attempt and the contact; exactly one callback, for that call and number, the notes as its message; contact `DONE`; the caller's BYE |
| C02 | 200, then the customer's BYE at 1 s | `ANSWERED`; contact `DONE`, no lead, no callback; no further INVITE to the number for the rest of the run |
| C04 | 486 on attempt 1, 200 on attempt 2 (BYE at 3 s) | `BUSY` (`USER_BUSY`), then `ANSWERED`; exactly two INVITEs, at least the 60 s interval apart; contact `DONE` with 2 attempts |

C07's bound is read from the attempt's `startedAt`, which the dialer writes
just before it sends the originate whose timeout the ring timeout is. The
switch checks that timeout in whole seconds, so a 5 s ring timeout rings for
more than 4 s and at most 5 s; measured from the INVITE, a ring that began
late in a second can read just under 4 s although nothing is wrong. Both
times are in the report (`cancelAfterStartMs`, `cancelAfterInviteMs`).

The policy is the validated minimum: ring timeout 5 s, every interval and the
transient backoff 60 s, `maxAttempts` and `maxSystemRetries` 2, calling window
00:00–24:00 every day. So a retried outcome is dialled exactly twice and ends
`EXHAUSTED`, whichever budget it spends, and a terminal one is dialled once. A
run lasts a little over a minute, most of it the wait for the retries.

For every case and both countries the run asserts the attempt's outcome and
hangup cause; the contact's status, `attempts`, `systemFailures` and
`nextAttemptAt` right after the first attempt (end + 60 s, or none) and at the
end; that the retry did not start early; one CDR per dialled attempt with the
same cause, the dialled string and no answer; and the simulator's INVITEs for
the number. Over the run: every dialog belongs to one contact and came from its
own campaign's caller id, no INVITE of one campaign carries the other country's
number, the simulator saw nothing its scenario does not know, and the mock
provider's scripts did what they should (one tool call per scripted answered
call, each answered; none failed or went unclaimed). The two countries must
come out identical on every column of the measured table; a difference fails
the run. Every equality is paired with a count. Latency is reported, never
asserted.

For an answered case the run also asserts, per attempt: one CDR, answered,
typed `OUTBOUND`, with a `BOT` leg; the simulator's 200 acknowledged; who sent
the BYE; and downlink audio above silence. The time from the 200 OK to the
first downlink frame above silence is reported per answered call (`probes.md`,
"First audio"), with no threshold.

The deployment's `AICC_OUTBOUND_DIAL_FORM` is `E164` (the default, spelt out in
the compose overlay): the switch is handed `+12025550103` and `+8613988886603`,
which is the Request-URI user part the simulator matches. The driver reads the
dial form from the resolved configuration and refuses to start when
`scenario.json` does not hold every case's number in that form with the case's
responses.

### The mock provider's scripts

The mock runs in scenario mode (`provider-scenario.json`). It picks a script
per session by a substring of the session's standing instructions, because
nothing else in the provider protocol tells one call from another. A contact's
own data reaches those instructions through the flow: `flow.json`'s entry
instruction ends in `E2E script: {slots.userData.e2eScript}.`, and the driver
imports C01's contact with the slot `e2eScript=C01_LEAD_HIGH` (a contact's
slots become the call's `userData`). So C01 runs in the same campaign and flow
as every other case, and only its calls render `E2E script: C01_LEAD_HIGH.`,
which selects the script that speaks, calls `record_lead` (`HIGH`), waits for
the result and ends the session. Every other call, the smoke's included,
renders `E2E script: .` and gets the default script: one spoken turn, no tool,
no ending, so the callee hangs up. The driver refuses a run in which the
scenario, the flow and the cases disagree.

### Run-wide invariants

Checks that hold for any run, rather than for one case, plug in as
`AfterRunChecks` (`internal/e2e/campaign_checks.go`, `campaign_invariants.go`).
They read the application's metrics (`GET /metrics` on the ops listener,
`:9090`, unauthenticated, the listener the driver already polls for
`/readyz`), the mock's `GET /_stats`, the simulator's record and the API:

| invariant | how it is observed |
| --- | --- |
| provider sessions started = answered dialogs; none on a 180 or 183 | the mock's session count and the application's `aicc_provider_sessions_started_total` both moved by the number of dialogs the simulator answered with 200 (itself held to the cases); no session is live; every answered call's CDR has a `BOT` leg and no CDR of a call that only rang has one |
| every answered number gets exactly one answered INVITE, and none after it | the simulator's dialogs per number (C04's earlier, refused INVITE comes before its answer) |
| bot slots and DID outbound lines back to zero | `aicc_bot_slots{state="IN_USE"\|"RESERVED"}` and `aicc_did_outbound_in_use{did=<each campaign's DID>}` exist and are 0 |
| every attempt finished exactly once | each attempt appears once in the API, `FINISHED` with an outcome and an end; `aicc_campaign_attempts_total`, which counts a finish when it first commits and never a revision, moved by exactly the attempts, per outcome |
| every attempt that created a channel has exactly one CDR carrying `campaignId` | the CDRs listed by the campaign's DID (not by campaign, so a row without its campaign id is still found): one per attempt that was placed, each with the campaign's id, also when read by call id; no CDR for a skipped attempt, none the campaign does not own |

The counters move on hand-offs the API does not wait for (the dialer counts a
finish after its transaction commits; a slot or a line is given back when the
switch reports the hangup), so the driver waits up to 30 s for them to catch up
with what the run shows, then 3 s more, and judges that reading
(`countersSettledMs` in the report).

A run writes `campaign-<time>/report.json` (every assertion, per region, case
and attempt, with the simulator's dialog and the CDRs) and `probes.md` (the
measured table: SIP response → `hangup_cause` → outcome → retry interval and
budget, then the first-audio times). On a failure, `bundles/<region>-<case>-<n>/` holds that attempt, its
dialog, and the lines of the application log and the switch log that name its
call id, leg id or SIP Call-ID, sliced by `e2e.sh` from `logs/aicc.log` and
`logs/freeswitch-file.log` (the switch's own log file, which prefixes a call's
lines with its channel uuid, the leg id).

## Capacity tests

`make e2e-capacity` runs the tests of `internal/e2e/capacity*.go` (issue
#247), from the same test binary as the campaign tests, against an
environment that is up. Each scenario needs an application start of its own,
so the limits it lowers never reach the other runs: `e2e.sh capacity`
recreates the application container (`docker compose up --force-recreate
--no-deps aicc`, the database and the switch untouched) with the scenario's
settings, renders and guards the configuration it used, and the driver
refuses to run when that configuration does not hold the scenario's values.
K1 runs first, K2 runs on the defaults and K3 last; the application is then
recreated on the defaults, so the environment is left as `e2e.sh up` started
it. The settings come from `E2E_BOT_MAX_CALLS`, `E2E_BOT_INBOUND_RESERVE`,
`E2E_OVERFLOW_HOLD`, `E2E_BOT_RTP_PORT_LOW` and `E2E_BOT_RTP_PORT_HIGH` in the
compose overlay, which only `e2e.sh capacity` sets (empty is the application's
default). The simulator is restarted first, as for the campaign tests.

| scenario | setup | callee (`scenario.json`) | asserted |
| --- | --- | --- | --- |
| K1 | `AICC_BOT_MAX_CALLS=3`, `AICC_BOT_INBOUND_RESERVE=0`, `AICC_OVERFLOW_HOLD=1500ms`; one US campaign of three contacts, ring timeout 8 s; once all three are ringing, two single AI calls (`POST /calls`, `AI_OUTBOUND`) from a DID of their own | contacts `+1 202 555 0121`–`23`: 180, 200 at 5 s, BYE 6 s after the answer; slot holders `…0131`, `…0132`: 200 at 300 ms, BYE 12 s later | 1 `ANSWERED` and 2 `OVERFLOW`; each `OVERFLOW` leg: CDR cause `OVERFLOW`, answered, no `BOT` leg, hung up by the platform with a BYE, no downlink window above silence; the `ANSWERED` leg was heard (so the measurement works on this path); every contact `DONE`, one attempt, one INVITE (no redial); `aicc_bot_overflow_total` and `aicc_campaign_overflow_total` moved by 2 |
| K2 | the defaults; two campaigns (US, CN) at once, each on its own DID created with `maxOutboundChannels: 2`, ring timeout 8 s, `maxAttempts` 1 | six contacts per country (`…0141`–`46`, `+86 139 8888 6641`–`46`): 180, never answered | per DID, the simulator's peak of dialogs up at once is 2: never above the limit, and reaching it; all six contacts attempted once, `NO_ANSWER`, cancelled, `EXHAUSTED`; the line gauge never read above 2 in its samples; the dialer counted `DID_LINES` as a reason to hold back |
| K3 | `AICC_BOT_RTP_PORT_LOW=26000`, `AICC_BOT_RTP_PORT_HIGH=26001` (exactly one RTP pair, just above the switch's range); a single AI call from its own DID holds the pair; with it up, R1, R2 and the R3 campaign are placed, then, after the holder ended, N1 | all 200 after 300 ms: holder `+1 202 555 0151`, BYE 28 s later; R1 `…0161`, BYE 8 s later; R2 `…0162` and R3 `…0163`, BYE 20 s later unless the platform hangs up first; N1 `…0171`, BYE 6 s later | the bot refuses the INVITE (503, `NORMAL_TEMPORARY_FAILURE`) of every case placed while the pair is held. R1, a single AI call on a DID with an enabled fallback queue (no agents): the customer is not dropped, the callee's BYE comes at 8 s ±1 s, the switch log shows the transfer to the queue's extension. R2, a single AI call, no fallback queue, language `zh`: the platform's BYE within 6 s, cause `NORMAL_TEMPORARY_FAILURE`, and the switch log names `…/aicc/zh/bot-unavailable.wav` with `${sounds_dir}` expanded. R3, a campaign of one contact on a DID that has a fallback queue: the platform's BYE within 6 s, no transfer to the queue in the switch log, no prompt, attempt `BOT_FAILED`, contact `DONE` after one attempt and one INVITE. All three CDRs: answered, no `BOT` leg, `userData.botReason` `BOT_LEG_FAILED`. N1: answered, a `BOT` leg, downlink above silence, no `botReason` |

Why K1 is not "bot slot limit 1": the dialer admits a call only while slots
held, plus the expected answers of the legs ringing, plus one fit in the
capacity, and a campaign with fewer than fifty rung samples expects every
ringing leg to answer (design 10 section 6.3, `internal/dialer/admit.go`). With
one slot no second leg ever rings, whatever the callee does, so three
contacts can never answer together. K1 therefore lets the campaign put three
contacts on the line with three free slots, and then takes two slots away the
way a production deployment loses them, to calls the campaign does not see
(design 10 names an inbound surge): two single AI calls that answer at once
and stay up. When the contacts answer, one slot is free. The run first holds
itself to that order (every contact ringing before the first answer, both slot
holders answered before it and still up after the last overflow) and fails
when it did not hold, rather than report a split it did not set up. Which
contact gets the slot is a race between three answers; the scenario asserts
the split, not the winner. The one free slot is global, so K1 uses one
country: a second one would share it and neither could be held to one
`ANSWERED` and two `OVERFLOW`.

Why K3 holds the pair with a call. The UAS refuses an INVITE with 503 when it
cannot bind an RTP pair (#450), and that is the only way a bot leg is refused
that the harness can set up without breaking something else: narrow the range
to one pair, and the second call has nowhere to go. An external process
holding the port would work too, but would be another thing to start and to
tell apart from the application's own pair; a call is the way production loses
a pair, and the run can see it (the callee answered, a bot slot is in use, the
mock has a session open). The run holds itself to the order, as K1 does: the
holder answered and on the bot before the first refused call is placed, and
still up when the last refused call ended (28 s against about 8), and fails
rather than report a split it did not set up. The three refused cases are
placed together; N1 waits for the holder's end and for the slots to be free.
The bot's range is checked against the switch's (`FS_RTP_START_PORT` and
`FS_RTP_END_PORT` in the resolved configuration, else the image's
20000-25999) and the run is refused when they share a port, so the refusal
cannot come from the switch.

The switch's log is in the switch's container, so K3 is judged in two parts.
The test run writes into its report what each refused call's leg must show
(the leg's channel id, read from the live call while it is up, or the
campaign attempt's `legId`); `e2e.sh` copies the log out and a second test
(`TestRefusalSwitchLog`) reads the leg's lines in it and writes the result into
`report.json` and `capacity.md`. A leg with no lines in the log fails.

Reported, never asserted: each call's answer and end times from the run's
first INVITE, each `OVERFLOW` leg's hold (200 OK to the platform's BYE) next to
the configured hold, the spread of the three answers, the application's own
mean hold (`aicc_bot_overflow_hold_ms`), the line gauge's sampled peak, and
whether a campaign read `AT_CAPACITY`.

The run-wide invariants (`AfterRunChecks`) run on all three, with these
adaptations, each written into the check it changes:

- provider sessions: an `OVERFLOW` attempt was answered and met no bot, so it
  opens no session and its CDR must have no `BOT` leg; a direct call that was
  answered opens one. The counts are held to answered dialogs − `OVERFLOW` +
  answered direct calls. K1 has no call that only rings, so that check is
  recorded as not applicable (and fails if one did); in K2, nothing opens a
  session, and the application, started for the run, may have no series of
  its session counter, which then reads as zero;
- no INVITE after an answer: K2 answers nothing, so it must answer no number,
  and its cardinality is that it dialled;
- capacity released: K2's application start may have no bot slot series
  (nothing took a slot), which reads as zero; K1's direct calls' DID must have
  its line series back at 0 too;
- the direct calls' dialogs are not the campaigns' (`RunView.Direct`), so
  "every attempt that created a channel has one CDR" and "every dialog belongs
  to a contact" count only the campaigns' dialogs.

K3 uses the same checks with these adaptations: an attempt that finished
`BOT_FAILED` and a direct call marked as refused are answered and open no
provider session (their CDRs must have no `BOT` leg); the sessions are the
holder's and N1's.

A run writes `capacity-<time>/K1/`, `K2/` and `K3/`: `report.json` (every
assertion and every call) and `capacity.md` (the tables, also printed by
`e2e.sh`).

## Reading a run

`bin/e2e/<project>/run/` (git-ignored) holds, after a run:

| Path | Content |
| --- | --- |
| `smoke-<time>/smoke.json` | every assertion made, failures, latencies, condensed dialog and CDR |
| `smoke-<time>/calleesim-report.json` | the simulator's full record: INVITE headers, responses, energy per 100 ms |
| `smoke-<time>/cdr.json` | the CDR list for the DID |
| `campaign-<time>/report.json` | the campaign run: every assertion, per region, case and attempt |
| `campaign-<time>/probes.md` | the measured table, one block per country, and the first-audio times |
| `campaign-<time>/bundles/` | per failed attempt: attempt, dialog, CDRs and the sliced logs |
| `capacity-<time>/K1/`, `K2/`, `K3/` | each capacity scenario's `report.json` and `capacity.md` (K3's with the switch log's lines) |
| `logs/<service>.log` | each container's output, written on a failed up, smoke, campaign or capacity run and by `e2e-down` |
| `logs/freeswitch-file.log` | the switch's log file, each call's lines prefixed with its leg id |
| `logs/sofia-status.txt` | the switch's gateways |
| `compose.json` | the resolved configuration the guard checked |
| `e2e_calleesim.xml` | the rendered gateway file |
| `calleesim-final.json` | the simulator's record at shutdown |

CI (`.github/workflows/e2e.yml`) uploads this directory as an artifact when the
job fails.

## Notes

- The switch image is the one `deploy/docker-compose.yml` pins (`AICC_FS_IMAGE`
  overrides it); it is pulled, never built. The working tree's
  `freeswitch/scripts` is mounted over the image's, so a Lua change is tested
  without rebuilding. Changes to `freeswitch/conf` need a new image.
- The application image `aicc-e2e:<project>` and the harness image
  `aicc-e2e-tools:<project>` stay after `e2e-down` so the next build is warm.
  Remove them with `docker rmi` when done.
