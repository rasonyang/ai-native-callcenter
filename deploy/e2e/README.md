# End-to-end environment

An isolated stack for automated outbound-call tests: PostgreSQL, the switch
image, the application built from this checkout, `aicc-mockprovider` (the voice
provider) and `aicc-calleesim` (the called party). The test drivers
(`cmd/aicc-e2e` for the smoke, a Go test binary for the campaign tests) run
inside the same network and talk to the application only through the public
API, with a scoped API key.

```sh
make e2e-up        # fresh database, application built from the working tree
make e2e-smoke     # one AI outbound call; checks the callee's record, the CDR, the webhook and the event stream
make e2e-campaign  # two campaigns (US, CN): answered and unanswered outcomes, SIP probes, run-wide invariants
make e2e-capacity  # bot-slot overflow (K1), the DID outbound line limit (K2) and a refused bot leg (K3), each in its own application start
make e2e-down      # collect logs, remove containers, volumes and the network
```

`E2E_OUTBOUND_DIAL_FORM=NATIONAL make e2e-up` starts the environment with the
application dialling campaign contacts in that form (`E164`, the default,
`E164_NO_PLUS` or `NATIONAL`); see "Dial forms".

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
- That gateway carries `destination-prefix` `a1b2cc`, so every call of every
  run is dialled with the prefix in front of the Request-URI and To users, as a
  carrier that requires one would see it (`deploy/carrier.md`, "A called-number
  prefix"). The prefix is one constant in `e2e.sh` (`CALLED_PREFIX`), rendered
  into the gateway file and passed to the simulator (`-called-prefix`) and the
  driver (`E2E_CALLED_PREFIX`). The dial string has no prefix. One shared
  gateway rather than a second one: the smoke, the campaigns and the capacity
  scenarios, in every dial form, all dial through it, so every check that the
  CDR's dialled number equals the number sent is also a leak check, and
  `AICC_OUTBOUND_ENDPOINT`, which is deployment-wide, stays a single pinned
  value.
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
3. Before the call it creates a webhook subscription for the DID through the
   public API, pointing at an HTTP receiver the driver runs itself on the
   network (its address is the one the route to the application uses; the
   webhook sender has no destination restriction, and the subscription is
   deleted afterwards), and opens `GET /events` as the administrator, which
   sees every event.
4. It asserts: exactly one new dialog in the simulator's record, for that
   number, answered with 200 and acknowledged, with downlink audio above
   silence; exactly one CDR for the DID, and exactly one more row in the ledger,
   for that call, typed `OUTBOUND` and with an answer time.
5. It asserts the called-number prefix stayed on the wire (the simulator
   removes it before its scenario lookup and fails an INVITE without it):
   the INVITE's Request-URI user and To user are the prefix plus the dialled
   number; the CDR's `toNumber`, and the `toNumber` of every webhook delivery
   for the call, is the dialled number; no delivery body and no event carries
   the prefix in any field, in either case. The stream is held until the CDR
   and the webhook are final, and the check is not vacuous: it also asserts
   that at least one `PARTY_*` and one `CALL_*` event of the call were seen.
   The summary's `prefix` block records the users, the delivery count and the
   event counts.

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
| C07 | 180, never answered | `NO_ANSWER`; CANCEL 3.75 to 6 s after the INVITE (the 5 s ring timeout, read on the simulator's monotonic clock); no provider session |
| C08 | 183 with early media, never answered | `NO_ANSWER`; CANCEL; no provider session |
| C09 | number on the do-not-call list | `DNC_SKIPPED`, cause `NUMBER_ON_DNC`; contact `DNC`; zero INVITEs |
| C10 | 180, then 486; the driver lists the number while the retry waits | attempt 1 `BUSY` (`USER_BUSY`), attempt 2 `DNC_SKIPPED` (`NUMBER_ON_DNC`); contact `DNC`; exactly one INVITE, one CDR (attempt 1's), none for the skipped attempt |
| P1–P5 | 180, then 480, 408, 503, 484, 410 | recorded, and held to design 10 §7.2's cause table |
| C01 | 180, then 200 after 2 s, a tone on the uplink; the bot records `record_lead` `HIGH` and ends the call | `ANSWERED`; lead `HIGH` on the attempt and the contact; exactly one callback, for that call and number, the notes as its message; contact `DONE`; the caller's BYE |
| C02 | 200, then the customer's BYE at 1 s | `ANSWERED`; contact `DONE`, no lead, no callback; no further INVITE to the number for the rest of the run |
| C04 | 486 on attempt 1, 200 on attempt 2 (BYE at 3 s) | `BUSY` (`USER_BUSY`), then `ANSWERED`; exactly two INVITEs, at least the 60 s interval apart (less the 1 s INVITE slack, on the simulator's monotonic clock); contact `DONE` with 2 attempts |

C07's bound is read from the INVITE to the CANCEL on the simulator's
monotonic clock (`afterInviteMs` on the dialog's `end`, `cancelAfterInviteMs`
on the attempt), not from two wall-clock stamps: see "Clocks". The switch
checks the ring timeout in whole seconds from the start of the originate, so a
5 s ring timeout rings for more than 4 s and at most 5 s; the INVITE leaves
slightly after the originate begins, so the lower bound is the 5 s timeout
less the 1 s tolerance less a 0.25 s allowance for that lag, which is 3.75 s,
and the upper bound is the timeout plus the tolerance, 6 s. The report also
holds the wall-clock readings of the same ring (`cancelAfterInviteWallMs`,
`cancelAfterStartMs`); the gap between the monotonic and the wall value is how
a step of the VM's wall clock shows up.

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

C10 lists a number while its campaign runs, which C09 (listed before the
campaign starts, so never selected) does not. The callee refuses attempt 1 with
486, which schedules the retry 60 s later. As soon as the driver has seen the
contact right after that attempt, with its `nextAttemptAt` set, it posts the
canonical number to `POST /dnc-numbers`, reads `GET /dnc-numbers` back and
checks the number is on it. It does this for each country on its own, while
the other cases carry on. The retry falls due 60 s after the callee's final
response (the 486) on the simulator's clock. The case fails when the listing
may have been posted before that response: the driver keeps what it had read
before it posted (`firstAttemptBeforeListing` and `contactBeforeListing` in the
report's `listedMidRun`), and the case requires that to be attempt 1,
`FINISHED` as `BUSY`/`USER_BUSY`, the switch's reading of the 486 the
simulator recorded (see "Clocks"). It also fails when the API answered the
listing only after the retry fell due (it would test nothing), and otherwise
asserts that the attempts are exactly `BUSY`/`USER_BUSY` then `DNC_SKIPPED`/`NUMBER_ON_DNC`
(the second started after the listing), that the contact is `DNC` having spent
one attempt, that the simulator saw one INVITE for the number, that attempt 1
has one CDR with the dialled string and the skipped attempt none. The platform
skips a listed contact on the dialer's next loop, not when its retry falls due
(design 10 section 12), and removing the number later does not revive it, so
the number is removed from the list when the run ends, like C09's.
`aicc_campaign_attempts_total{outcome="DNC_SKIPPED"}` moves by the skipped
attempts of C09 and C10 in both countries (the run-wide "every attempt
finished once" invariant).

A control number listed mid-run and removed again before its retry would not
be dialled again: the skip is immediate, so its contact is skipped as soon as
it is listed, and the removal does not revive it. C10's control is C03 instead: the same callee behaviour (180, then 486), the same
retry interval, never listed. C10's assessment requires C03 of the same country
to have been dialled twice, the second INVITE at or after the moment C10's
retry fell due (the callee's 486 plus 60 s; give or take the INVITE slack), and the simulator's record that
counts C10's INVITEs to have been taken after that moment, both read on the
simulator's monotonic clock. So C10 cannot pass because the platform dialled
nothing from then on.

### Clocks

The driver, the callee simulator and the dialer stamp events on one clock, the
VM's wall clock. That clock steps: on Colima `lima-guestagent` sets it from the
host every 10 s, by as much as a few seconds, so a duration or an order taken
from two of its stamps can be wrong by that much (#506, #515). So the checks
read it as follows.

- **Between two events the simulator recorded: its monotonic clock.** Beside
  each wall-clock stamp, the simulator's record carries the same moment on its
  monotonic clock: `afterInviteMs` on the dialog's `final` and `end` (time
  since the INVITE), and `inviteAfterStartMs` on the dialog and `afterStartMs`
  on `final` and `end` (time since the simulator started, one timeline for
  every dialog it saw). The record's `startedAt` and `afterStartMs` say when
  the simulator started and when the record was taken. C07's ring, C04's
  INVITE gap, C10's control INVITE and the time C10's record was taken
  (against C10's retry falling due), K1's and K3's preconditions, K2's peak of
  dialogs up at once and K3's times from the answer to the BYE
  (`answerToEndMs`) read these.
- **Between two of the driver's own moments: its monotonic clock.** K3's
  holder seen on the bot before the refused calls were placed reads
  `holderOnBotAfterRunStartMs` and `placedAfterRunStartMs` (time since the
  run's `startedAt`).
- **C10's listing against the callee's 486, and against the INVITE: no
  clock.** The driver and the simulator are two processes. The driver posts
  the listing only after it has read attempt 1 `FINISHED`, and the platform
  finishes a refused attempt from the switch's hangup, whose cause the switch
  has only once the callee's response arrived. The listing keeps what the
  driver had read (`firstAttemptBeforeListing`, `contactBeforeListing`), and
  the check holds it to that: attempt 1, `FINISHED`, `BUSY`/`USER_BUSY`, with
  the simulator's record showing the 486. The wall clock's reading of the
  same order is printed in the check, not compared.
- **The rest: the wall clock the simulator shares with the driver and the
  dialer**, each with a slack or margin of its own (#515 lists them): when an
  INVITE is the attempt's (between the attempt's start and the next attempt's
  start), how long a contact was watched (from the end of the attempt's
  dialog), whether C10's listing was answered before its retry fell due, and
  whether C10's skipped attempt started after the listing.

The reports keep the wall-clock readings beside the monotonic ones
(`cancelAfterInviteWallMs`, `inviteGapWallMs`, `answerToEndWallMs`, and the
wall clock's reading in each check's text), so a step shows up as the gap
between the two. Where the wall clock does not step, the two agree.

An attempt's `finishedAt`, a contact's `nextAttemptAt` and the CDR times are
stamped from the switch's event timestamps instead. The switch follows the
kernel's monotonic clock and does not step, so against the VM's wall clock it
sits at an offset that moves as the wall clock does: on one environment it ran
0.25 s ahead right after `make e2e-up` and 6.5 s ahead five runs later. A check
that ordered one clock against the other would pass or fail with the
environment's age, so the suite anchors on the simulator, as above. The checks
that must read switch time (that `nextAttemptAt` is the attempt's end plus the
interval, that the retry did not start before `nextAttemptAt`, and the retry
interval compared across countries and tabulated in `probes.md`) carry a
`[switch time]` marker in their text. The report measures the offset for every
attempt that has a dialog (`switchOffsetMs` in `report.json`, and the "Switch
clock" table in `probes.md`: per country the smallest and largest offset; it
includes the signalling latency). No tolerance is added for the offset or for
a step of the wall clock: neither is bounded, so a fixed one would only move
the failure later.

### Dial forms

The campaign dialer writes each contact's number in the deployment's
`AICC_OUTBOUND_DIAL_FORM` with the campaign's own country code
(`internal/dialer/admit.go`), and that string is the Request-URI user part the
simulator matches and the CDR's dialled number. The form is a run parameter:

```sh
make e2e-up                                        # E164 (the default)
E2E_OUTBOUND_DIAL_FORM=E164_NO_PLUS make e2e-up
E2E_OUTBOUND_DIAL_FORM=NATIONAL make e2e-up
make e2e-campaign                                  # runs in the form `up` started
```

| form | US campaign (country code 1), C03 | CN campaign (country code 86), C03 |
| --- | --- | --- |
| `E164` | `+12025550103` | `+8613988886603` |
| `E164_NO_PLUS` | `12025550103` | `8613988886603` |
| `NATIONAL` | `2025550103` | `13988886603` |

The e2e deployment's own country code is 86, so `NATIONAL` dials the US
numbers with a code that differs from the deployment's and the Chinese ones
with the same code, both sides of #477. In every form the attempt and the
contact keep the canonical `+` number; only the dial string follows the form.

- `E2E_OUTBOUND_DIAL_FORM` is checked first thing by `e2e.sh`: a value other
  than `E164`, `E164_NO_PLUS` and `NATIONAL` stops it with a message naming the
  three, before anything is built or started.
- `up` fixes the form for the environment and records it in
  `bin/e2e/<project>/dial-form` (removed by `down`). Every other command
  (`smoke`, `campaign`, `capacity`, `config`) runs in the recorded form, so
  `e2e-capacity`'s application restarts keep it. An explicit
  `E2E_OUTBOUND_DIAL_FORM` that differs from the recorded one is refused ("the
  environment is up with X; run: make e2e-down"); `down` ignores it, since it
  must always be able to clean up. To change the form, `make e2e-down`, then
  `up` again.
- The simulator's scenario follows the form. `scenario.json` is the one file
  edited by hand and is the `E164` file. `scenario.E164_NO_PLUS.json` and
  `scenario.NATIONAL.json` are generated from it by `ScenarioForForm`
  (`internal/e2e/scenario_forms.go`): the key of every campaign contact (the
  campaign table, K1 and K2's campaigns, K3's campaign) becomes the string
  the dialer produces in that form. Every other key stays as it is: the
  smoke's `13800000001` and the direct calls (K1's slot holders, K3's calls)
  are placed through `POST /calls`, which dials its number exactly as sent, in
  every form. The generator refuses a key it cannot classify and two keys that
  would become one. After editing `scenario.json` or a case table, regenerate:

  ```sh
  go test ./internal/e2e -run TestScenarioForms -update
  ```

  `go test ./internal/e2e` fails, naming that command, when a generated file
  differs from what the generator makes of `scenario.json`. The compose
  overlay mounts the file of the form for both the simulator and the driver.
- The driver reads the form from the resolved configuration and refuses to
  start when the scenario it was given does not hold every case's number in
  that form with the case's responses; the refusal names the form
  (`AICC_OUTBOUND_DIAL_FORM=NATIONAL; ...`).
- The unit tests (`go test -race ./internal/e2e/`) run every test that depends
  on a number's form once per form, each against that form's file, and assert
  the three forms' Request-URI user parts literally. A run in any form also
  holds that no INVITE of one campaign carries the other country's number.

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
| K3 | `AICC_BOT_RTP_PORT_LOW=26000`, `AICC_BOT_RTP_PORT_HIGH=26001` (exactly one RTP pair, just above the switch's range); a single AI call from its own DID holds the pair; with it up, R1, R2 and the R3 campaign are placed, then, after the holder ended, N1 | all 200 after 300 ms: holder `+1 202 555 0151`, BYE 28 s later; R1 `…0161`, BYE 8 s later; R2 `…0162` and R3 `…0163`, BYE 20 s later unless the platform hangs up first; N1 `…0171`, BYE 6 s later | the bot refuses the INVITE (503, `NORMAL_TEMPORARY_FAILURE`) of every case placed while the pair is held. R1, a single AI call on a DID with an enabled fallback queue (no agents): the customer is not dropped, the callee's BYE comes at 8 s ±1 s, the switch log shows the transfer to the queue's extension. R2, a single AI call, no fallback queue, language `zh`: the platform's BYE within 14 s (6 s, plus up to 8 s of the prompt once the switch image ships it), cause `NORMAL_TEMPORARY_FAILURE`, and the switch log names `…/aicc/zh/bot-unavailable.wav` with `${sounds_dir}` expanded. R3, a campaign of one contact on a DID that has a fallback queue: the platform's BYE within 6 s, no transfer to the queue in the switch log, no prompt, attempt `BOT_FAILED`, contact `DONE` after one attempt and one INVITE. All three CDRs: answered, no `BOT` leg, `userData.botReason` `BOT_LEG_FAILED`. N1: answered, a `BOT` leg, downlink above silence, no `botReason` |

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
holders answered before it and still up after the last overflow, read on the
simulator's monotonic clock) and fails when it did not hold, rather than
report a split it did not set up. Which contact gets the slot is a race between three answers; the scenario asserts
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
rather than report a split it did not set up. The order of the calls' events
is read on the simulator's monotonic clock, and the holder seen on the bot
against the refused calls' placing on the driver's; R1's 8 s ±1 s, R2's 14 s
and R3's 6 s run from the 200 OK to the BYE on the simulator's monotonic clock
(`answerToEndMs`), with the wall clock's reading beside them
(`answerToEndWallMs`). See "Clocks". The three refused cases are
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
first INVITE (on the simulator's wall clock), each `OVERFLOW` leg's hold (200
OK to the platform's BYE, on its monotonic clock, the wall clock's reading
beside it) next to the configured hold, the spread of the three answers, the
application's own mean hold (`aicc_bot_overflow_hold_ms`), the line gauge's
sampled peak, and whether a campaign read `AT_CAPACITY`. K2's peak of dialogs
up at once is read on the simulator's monotonic clock: a step of the wall
clock between one dialog's end and the next one's INVITE would otherwise count
a line handed on as two lines up at once.

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
| `../dial-form` (in `bin/e2e/<project>/`) | the dial form `up` started the environment with |
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
