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

## Test numbers and the carrier's own limits

A team that tests campaigns on its own phones can put those numbers on the list of test numbers
(`/test-numbers`), and neither the cross-campaign frequency window (`AICC_CAMPAIGN_FREQUENCY_WINDOW`)
nor the daily dial cap (`AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY`, below) then holds them back.
That exemption is the application's alone. A carrier
line often limits calls to one callee by itself (a number of calls to one callee per hour or per
day), and that limit still applies to a test number unless the carrier exempts the number too: a
test campaign that dials the same few phones many times can be refused by the trunk even though the
application lets it through. In particular a test number's 4th dial of the day is sent even with the
default cap of 3, and a line that refuses the 4th call of the day refuses it. Ask the carrier to
exempt the numbers, or keep the test volume under its limit.

## Line limits to set at bring-up

Set the application's own limits to match the line's, so a campaign waits instead of being refused:

- `AICC_CAMPAIGN_FREQUENCY_WINDOW` (default `24h`): how long after one campaign reached a number no
  other campaign dials it.
- `AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY` (default `3`, `0` turns it off): the most dials to one
  callee number in the contact's local day, counting every campaign, every attempt (a no-answer, a
  retry after a system failure, an attempt in flight) and ad-hoc outbound calls. A campaign contact
  whose number has reached it is not dialled: it waits for its calling window on the next local day
  and spends no attempt. Set it to the lowest per-callee daily limit the carrier enforces; a
  development setup that dials one phone repeatedly needs `0`. Ad-hoc dials made before the cap
  existed are not counted.

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

## A carrier that ends an unanswered call itself

Some carriers give up on an unanswered call before the application's own ring
timeout (`AICC_OUTBOUND_RING_TIMEOUT`, 45 s by default, or a campaign's
`ringTimeoutSec`) and answer the call with a final `408 Request Timeout` while
the customer's phone is still ringing. The switch reports such a call with the
hangup cause `RECOVERY_ON_TIMER_EXPIRE`.

- From the release after v0.8.6, a call that rang and then ended with a 408 from
  the carrier counts as `NO_ANSWER`: a campaign retries it by the campaign's
  usual rules and counts it against `maxAttempts`, exactly like a call the
  application itself cancelled at its ring timeout. Nothing needs to be
  configured.
- In v0.8.6 and earlier such calls were treated as a system failure. A campaign
  retried them within minutes without spending `maxAttempts`, and a long run of
  them could pause the campaign as a trunk outage. The workaround was to set
  `AICC_OUTBOUND_RING_TIMEOUT` (and the campaign's ring timeout) below the
  carrier's own timer, so that the application's cancel came first. That
  workaround is no longer needed and can be removed.
- The call detail shows what the carrier did: `ringSec` counts from the first
  180 or 183 the carrier sent, and the call's `tech` carries `sipStatus` (the
  status the carrier ended the call with) and `ringStartedAt` (when it first
  rang). A 408 without any ringing before it is still treated as a failure of
  the line, since nothing shows the customer's phone was reached.
