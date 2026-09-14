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
