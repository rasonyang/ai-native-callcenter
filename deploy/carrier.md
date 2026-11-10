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
- The gateway may be written with the profile that holds it, the way the switch
  prints it: `sofia/gateway/external::pstn_gateway/%s` is as good as
  `sofia/gateway/pstn_gateway/%s`.
- A campaign fills `%s` with the number in the form `AICC_OUTBOUND_DIAL_FORM`
  names (E.164 with a `+` by default); a single `POST /calls` uses the number as
  sent.
- Empty is allowed: the deployment simply takes calls in and places none.
  A campaign then refuses to start (`409 OUTBOUND_NOT_CONFIGURED`) instead of
  dialling every contact into nothing. One that is already running — the
  application restarted without the endpoint — is paused on its first attempt
  with the reason `OUTBOUND_NOT_CONFIGURED`, that attempt voided rather than
  retried, and resuming it is refused with the same code until the endpoint is
  set. Setting the endpoint and then resuming is the whole recovery.
- Recreate the application after changing it, and the switch after changing the
  gateway file: `docker compose up -d aicc` and
  `docker compose exec freeswitch fs_cli -P 18021 -p <ESL_PASSWORD> -x "sofia profile external rescan"`.

## A called-number prefix

Some trunks route a call only when the called number carries a prefix the
carrier assigned, for example `a1b2cc13800000000` for `13800000000`. Put the
prefix on the gateway, not in the dial string:

```xml
<gateway name="pstn_gateway">
  <param name="proxy" value="…"/>
  <param name="destination-prefix" value="a1b2cc"/>
</gateway>
```

- `destination-prefix` puts the string in front of the user part of the
  Request-URI and of the To header of every call dialled through that gateway.
  It changes nothing else: the leg's called number stays the plain number, so
  the API's `to`, the party numbers, every event, the CDR's `toNumber`, the
  webhook and contact matching all keep it. The prefix exists only on the SIP
  wire towards the trunk.
- `AICC_OUTBOUND_ENDPOINT` stays `sofia/gateway/pstn_gateway/%s`, and dialplan
  rules bridge `sofia/gateway/pstn_gateway/$1`. Neither carries the prefix.
  A prefix in the dial string becomes part of the leg's destination number: it
  shows up in party numbers, the CDR's `toNumber` and the webhook, and a call
  no longer matches its contact, which is compared by exact number.
- Do not put the prefix in the number given to the API either: a number with
  letters is refused, and a numeric prefix would be stored as part of the
  number.
- It applies to every outbound call through the gateway: an AI outbound call, a
  campaign attempt, an agent's outbound call and a transfer to an outside
  number.
- `AICC_OUTBOUND_DIAL_FORM` is the form the trunk expects after the prefix:
  `NATIONAL` for a trunk that takes national numbers, so a contact imported as
  `+8613900000001` goes out as `a1b2cc13900000001`.
- Apply a changed gateway between calls: `sofia profile external killgw
  pstn_gateway`, then `sofia profile external rescan`, both with
  `docker compose exec freeswitch fs_cli -P 18021 -p <ESL_PASSWORD> -x "…"`.
  Or restart the switch. A call already up keeps the form it was placed with.
- Check it on the wire: `sofia global siptrace on`, place one call, read the
  INVITE (the Request-URI and To users are the prefix plus the number), then
  `sofia global siptrace off`. Trace output is verbose; leave it on only for
  that call.
- Limits:
  - Letters and digits only. Any other character in a SIP URI user needs
    escaping.
  - Inbound is untouched. A trunk that also prefixes the numbers it sends
    needs a dialplan rule before `public/05_aicc.xml` that removes the prefix;
    that is not covered here.
