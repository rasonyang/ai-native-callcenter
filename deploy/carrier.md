# Connecting your own carrier

This applies to the compose stack in [the deployment guide](README.md). Paths
are relative to `deploy/`.

Two directories are mounted into the switch:

- `freeswitch/dialplan/`: rules of the `aicc` context.
- `freeswitch/directory/`: SIP users the switch knows outside the database.

They currently hold a simulated public network, which is what lets a softphone
act as a customer. For a real deployment, replace them with your own trunk
files and mount your gateway into `conf/sip_profiles/external/` as a third
mount. `deploy/dev/freeswitch/` is a worked example;
[freeswitch/README.md](../freeswitch/README.md) §3 explains the layout.

## Telling the application to dial through it

A gateway the switch holds is not yet a gateway the application can place an AI
outbound call through: `AICC_OUTBOUND_ENDPOINT` is the dial string it renders
each number into, and it is a line in `deploy/.env`:

```ini
AICC_OUTBOUND_ENDPOINT=sofia/gateway/pstn_gateway/%s
```

- `pstn_gateway` is the `<gateway name="…">` in the file you mounted — not the
  filename, not the carrier's hostname. A name the switch does not have is a
  campaign that rings nothing; `aicc doctor` warns (`outbound_gateway`,
  `OUTBOUND_GATEWAY_MISSING`) and lists the gateways that do exist.
- The single `%s` is where the number goes. The value must start with `sofia/`
  and carry exactly one `%s`, or the application refuses to start. A direct
  endpoint (`sofia/sip:…/%s`) works too, and then there is no gateway to check.
- The customer sees the campaign's DID as the caller id; the switch's own
  `effective_caller_id_number` does not apply on this path, because an AI
  outbound call is originated straight to the endpoint rather than through a
  dialplan route.
- Empty is allowed: the deployment simply takes calls in and places none.
  A campaign then refuses to start (`409 OUTBOUND_NOT_CONFIGURED`) instead of
  dialling every contact into nothing, and one that is already running — after
  a resume, or after the application restarted — is paused on its first
  attempt with `TRUNK_FAILURE`, that attempt voided rather than retried.
  Fixing the endpoint and resuming is the whole recovery.
- Recreate the application after changing it, and the switch after changing the
  gateway file: `docker compose up -d aicc` and
  `docker compose exec freeswitch fs_cli -x "sofia profile external rescan"`.
