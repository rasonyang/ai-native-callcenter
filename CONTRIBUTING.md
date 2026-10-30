# Contributing

Build, test and API-contract instructions are in the [README](README.md#building).
The rules below cover the questions that come up in issues and pull requests
more than once.

## Integrations live outside the tree

This repository is a call center: telephony, queues, the AI leg, the product
API. It is not a home for connectors to other products, and it will not merge a
pull request that adds one — no vendor-named adapter, client, interface, stub,
MCP client or new dependency for a third-party service, however optional the
flag that guards it. That rule is written down in
[`docs/provider-extension.md`](docs/provider-extension.md) for voice engines and
in [`docs/phase1-decisions.md`](docs/phase1-decisions.md) (A6) for everything
cascade-shaped, and it applies the same way to memory stores, CRMs, ticketing
systems and analytics products.

The reason is not a lack of interest. Business-system integration here is
configuration, not code, and the extension points for it already exist:

- **A flow declares HTTP tools** that call the operator's own backend at
  `AICC_BOT_BACKEND_BASE`. The request body can carry the caller number via
  `{slots.caller}`, and a tool's result can be quoted in later phase
  instructions via `{slots.<tool>.<field>}`. The `bill_lookup` tool in
  [`internal/seed/flows/early_collections.json`](internal/seed/flows/early_collections.json)
  is the "look the caller up right after the call starts" pattern; the tool
  shape is documented in [`internal/flow/spec.go`](internal/flow/spec.go).
- **Everything the call center knows is on the REST API** behind an API key:
  call state, user data, CDRs, transcripts, handoff. The contract is
  [`docs/openapi.json`](docs/openapi.json). An external service reads what it
  needs and keeps its own records on its own side.

So an integration with product X is a service you host, sitting behind that
backend URL, plus a flow that names it as a tool. Consent, retention, export and
deletion stay entirely in your product; this repository stays authoritative for
telephony only. If you build one and want people to find it, a link from your
own documentation is the right place for it.

If the tool contract cannot express something your integration needs, open an
issue describing the missing capability in vendor-neutral terms. A generic
gap in the flow DSL or the API is in scope; a hook for one product is not.

## Tests with goroutines, fakes, sockets or timers

A test that waits for another goroutine (a fake server's read loop, a reader
on a socket, a timer) can pass every time on a fast machine and still fail in
CI. The fast machine finishes the racing goroutine before the assertion, so
running the test again and again never opens the window; a 4-vCPU CI runner
busy with the whole suite does. #185, #191, #253, #254 and #256 were all found
that way, and each was a real timing assumption in the test or its harness,
not CI being slow. The fix belongs in the test, never in CI's load or timeouts.

So a pull request that adds or changes such a test runs two checks on the
packages it touches, and its description says so and names the hand-offs the
second check covered:

1. **Stress the packages the way CI loads them.**

   ```sh
   make test-stress PKG=./internal/calleesim RUN=TestCodecIsNegotiatedPerOffer
   ```

   This runs `go test -race -count=20 -cpu 1,2,8` on `PKG` while a looping
   `go test -race ./...` and one busy loop per CPU share a few CPUs with it:
   four pinned with `taskset` on Linux, the efficiency cores on Apple silicon.
   Load alone is not enough on a machine with many cores; the CPUs have to be
   few and busy, as on the runner. `PKG` is required and may name several
   packages, but never `./...`; `RUN`, `COUNT`, `CPU` and `CORES` are optional
   (see `scripts/test-stress.sh`). A pass is evidence, not proof: run it more
   than once, or raise `COUNT` for a fast test.

2. **Inject a sleep at each hand-off.** Find every point where the test
   relies on one goroutine having done something before another looks: a
   frame recorded, a reply written, a socket closed. Add a temporary
   `time.Sleep` of tens of milliseconds there, and confirm the test fails. Fix
   the test or the harness, confirm it passes with the sleep still in place,
   then remove the sleep.

## The web lockfile belongs to npm 11

Node 24 and npm 11 own `web/package-lock.json`. CI (the `web`, `contract` and
`layout` jobs) and the image's `web` stage install with them. `devEngines` in
`web/package.json` makes any other npm major stop with `EBADDEVENGINES`, so a
lock is never written or installed by the wrong one.

The reason is that npm 10 and npm 11 disagree about optional peers, and npm 10
drops the `libc` fields npm 11 writes (#389). A lock written by npm 11 can fail
`npm ci` under npm 10, and one rewritten by npm 10 loses information. After a
web dependency change, commit the lock npm 11 wrote; do not regenerate it from
scratch.
