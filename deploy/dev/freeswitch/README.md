# This machine's FreeSWITCH overlay

The switch configuration the product ships lives in `freeswitch/` and goes into
the container image. This directory holds what *this development laptop* adds
on top of it: a route out to the simulated PSTN peer, and the gateway
definition that names that peer.

It is version controlled so it stops being rebuilt from memory — it has been
reconstructed by hand at least twice — and it is kept out of `freeswitch/` so
it is never shipped.

## Why it is not in `freeswitch/`

Two reasons, and they are the same reason twice.

The image has to be deployment-neutral. Everything here is about one peer at
one address on one office LAN. A `pstn_gateway` in the shipped configuration
would be a gateway every deployment inherits and none of them wants, pointed at
a host only this building has.

And the trunk is the switch's to hold, not the product's. That is what
`internal/store/migrations/00021_the_trunk_is_the_switchs_to_hold.sql` settled
when it dropped the `trunks` table: a gateway is defined in the switch's own
sofia profile XML and read when that profile loads, so a row in the database
could only ever change half of it. The other half is a file on the host. This
directory is that file, under version control, which is as close as the
repository can get to owning it without pretending it owns the switch.

What the product reads from the database — accounts, dialplan for aicc
extensions, queues — is unaffected. This is the part that was never a database
question.

## What is in here

| File | Installed as | What it is |
| --- | --- | --- |
| `dialplan/aicc/00_pstn_gateway.xml` | `conf/dialplan/aicc/00_pstn_gateway.xml` | The outbound route: any 7-to-16-digit number, optionally `+` prefixed, bridged to the trunk with `effective_caller_id_number` passed through as the outbound caller id. |
| `sip_profiles/external/pstn_gateway.xml` | `conf/sip_profiles/external/pstn_gateway.xml` | The gateway itself. NOREG, no ping, `outbound-proxy` pinned. |
| `vars.d.example.xml` | nothing — copied by hand | The `X-PRE-PROCESS` lines `vars.xml` needs, with placeholder values. |
| `install.sh` | — | Copies the two files in and reloads the switch. |

The dialplan file is an `<include>` fragment, not a `<context>`. It has to be.
`freeswitch/conf/dialplan/aicc.xml` ends with

```xml
<X-PRE-PROCESS cmd="include" data="aicc/*.xml"/>
```

and that include point is where a deployment adds what only it has. Two files
that both open `<context name="aicc">` do not merge: the first is used and the
second is silently ignored, so its rules are visible to `xml_locate` and
unreachable to a call. That cost an afternoon of `NO_ROUTE_DESTINATION` once
already. Anything added here goes in as `<include><extension>…`.

## Install

1. Add the variables. `install.sh` does not touch `vars.xml`, because
   `vars.xml` holds database passwords and nothing automated should be writing
   over it. Open `vars.d.example.xml`, copy the `X-PRE-PROCESS` lines into the
   `<include>` in `/usr/local/freeswitch/conf/vars.xml`, and fill in the real
   values.

   `aicc_bot_host` must be `$${local_ip_v4}` — never `127.0.0.1`, never
   `localhost`, and never with a `sip:` scheme in the value. The Go process
   binds the LAN address and a LAN-bound macOS socket cannot send to loopback,
   so a loopback target gives you a gateway that reports UP and calls that never
   arrive.

2. Run the installer.

   ```sh
   deploy/dev/freeswitch/install.sh
   # or, elsewhere:
   FS_ROOT=/opt/freeswitch ESL_PASSWORD=secret deploy/dev/freeswitch/install.sh
   ```

   It refuses to run if the destination is not a FreeSWITCH conf tree, creates
   `dialplan/aicc/` if it is absent, backs up any file it is about to overwrite
   as `<name>.bak-<timestamp>`, and then runs `reloadxml` and
   `sofia profile external rescan` through `fs_cli` on `127.0.0.1:18021`
   (password `ClueCon` unless `ESL_PASSWORD` says otherwise). The rescan is not
   optional: `reloadxml` re-reads the XML without rebuilding the profile's
   gateways, so a changed proxy address would sit in the file and not in the
   switch.

## Verify

The gateway first:

```sh
/usr/local/freeswitch/bin/fs_cli -x "sofia status gateway pstn_gateway"
```

`State NOREG` and `Status UP` is correct and is what a healthy trunk looks like
here. NOREG is not a failure: this is an IP trunk, we never register, and a
NOREG gateway is dialable as it stands. `Proxy` should show the address you put
in `pstn_gateway_host`, and `Context public`.

Then place a real call. Dial an external number from the browser phone and read
`/usr/local/freeswitch/log/freeswitch.log`. A working call shows, in order:

```
Dialplan: sofia/internal/…@… Action bridge({absolute_codec_string=PCMU,PCMA,
  origination_caller_id_number=${trunk_clid},…}sofia/gateway/pstn_gateway/1888…)
EXECUTE [depth=0] … bridge({…origination_caller_id_number=95555,
  origination_caller_id_name=95555}sofia/gateway/pstn_gateway/1888…)
send … INVITE sip:1888…@<peer>:5080
recv … SIP/2.0 100 Trying
recv … SIP/2.0 180 Ringing
recv … SIP/2.0 200 OK
[DEBUG] switch_core_media.c:3730 Set Codec sofia/external/1888… PCMU/8000 20 ms
```

Three things in that sequence are the ones worth checking:

- The `EXECUTE` line shows `trunk_clid` already resolved to a number. If it
  still reads `${trunk_clid}` the variable was never set, and the peer will see
  whatever the softphone sent.
- The number in `origination_caller_id_number` is the one the platform chose
  for this call, not `pstn_gateway_caller_id`. The fallback only applies to a
  leg that carries none.
- `Set Codec … PCMU` on the `sofia/external/…` leg. The codec pin rides the
  bridge string; without it `$${global_codec_prefs}` opens with OPUS, the peer
  answers 488, and the call never rings.

If the ACK or the BYE appears to go nowhere and the call tears down with
`SIP;cause=408 "ACK Timeout"` after a run of BYE retransmissions, the
`outbound-proxy` line in the gateway is missing or wrong. The peer advertises a
public address in its Via and Contact rather than the LAN address it actually
occupies, and in-dialog requests follow the Contact. The header comment in
`sip_profiles/external/pstn_gateway.xml` has the whole finding.

## Superseded files still on the live box

Two earlier versions of this route are still installed on this machine. They
are dead, and they are not in this directory. Remove them by hand — the
installer deliberately does not, because deleting files nobody asked it to
delete is not a thing an installer should do.

- `conf/dialplan/default/00_pstn_gateway.xml` — the original route, matching
  only the ten test numbers `18688886660`–`18688886669` and pinning the caller
  id to `$${pstn_gateway_caller_id}`. Superseded by the file in this directory,
  which lives in the `aicc` context, matches the general pattern, and passes the
  platform's caller id through.
- `conf/dialplan/public/00_pstn_gateway.xml` — a bridge from `public` back into
  `default` for phones that landed in the wrong context, matching the same ten
  numbers. Dead since the internal profile's context became `aicc` and
  `internal_auth_calls` became `true`: a registered phone is challenged, the
  directory is consulted, and its user context applies, so no agent call lands
  in `public` for this rule to rescue.
