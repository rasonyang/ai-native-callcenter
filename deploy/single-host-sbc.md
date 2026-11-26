# One host with FreeSBC at the edge

This puts the whole stack and [FreeSBC](https://github.com/rasonyang/freesbc)
on one cloud host: one NIC with a private address, behind the provider's 1:1
NAT. It applies to a stack installed by [install.sh](one-line-installer.md).
Paths are relative to the install directory (`/opt/aicc`).

The switch binds a loopback address, so nothing outside the host can reach it,
and every phone, browser and carrier comes in through FreeSBC:

```
carrier / SIP phones / browser phones
        |  public address (on the NAT)
        v
this host, NIC address 10.77.0.2
  FreeSBC     udp/5060 (SIP), tcp/7443 (WSS), udp/10000-19999 (RTP)
  TLS proxy   tcp/443 -> 127.0.0.1:8080 (the web interface)

  loopback only:
  FreeSBC private side   127.0.0.3:5060
  switch                 127.0.0.2:5060 (phones), 5080 (carriers), RTP 20000-25999
  application            bot SIP 6060, bot RTP 26000-29999, web 8080
```

Verified on 2026-11-26 with v0.9.2 and FreeSBC v0.8.6 on Ubuntu 26.04.1
(x86_64, Docker Engine 29.7.2, Compose v5.5.0) with a carrier: an inbound call
answered by the bot, an AI outbound call, a transfer to an agent on a browser
phone (web-sip-phone 1.2.0, registered over FreeSBC's WSS listener), and a page
reload during a call. The addresses below are examples: `203.0.113.7` is the
public address, `10.77.0.2` the NIC's, `198.51.100.10:5060` the carrier and
`cc.example.com` the name agents open.

## What to open

Inbound, in the provider's security group and in the host firewall if one is
active:

| Port | For |
|---|---|
| 443/tcp | The web interface, through the TLS proxy |
| 5060/udp | FreeSBC, SIP for carriers and SIP phones |
| 7443/tcp | FreeSBC, WSS for browser phones |
| 10000-19999/udp | FreeSBC, media |

None of the stack's own ports (5060, 5066, 5080, 7443 on 127.0.0.2, 8080,
20000-29999) is opened. FreeSBC's ports are free to choose; its media range
must not overlap 20000-29999.

## 1. Move the switch to loopback

Install as usual, then rerun the installer with the address:

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo sh -s -- --external-ip 127.0.0.2
```

It rewrites `FS_EXTERNAL_IP` and `FS_LOCAL_IP` in `.env` and recreates the
switch. The closing message then names the host's own address for the web
interface, and says that a browser phone needs `SIP_WSS_URL` (step 4) until
it is set.

## 2. Install FreeSBC beside it

```sh
curl -fsSL https://github.com/rasonyang/freesbc/releases/latest/download/install.sh \
  | sudo FREESBC_SWITCH=127.0.0.2:5060 FREESBC_PRIVATE_IP=127.0.0.3 FREESBC_PUBLIC_IP=203.0.113.7 sh
```

Put a certificate the browser trusts in `/etc/freesbc/` (owner `root:freesbc`,
mode 0640), then edit `/etc/freesbc/freesbc.yaml`:

```yaml
public:
  ip: 203.0.113.7          # on the NAT
  bind: 10.77.0.2          # the NIC's own address
private:
  ip: 127.0.0.3
rtp: 10000-19999
tls:
  cert: /etc/freesbc/cert.pem
  key: /etc/freesbc/key.pem
edge:
  switch: ["127.0.0.2:5060"]
  switch_carrier_port: 5080
  listen:
    udp: 5060
    wss: 7443
  carriers:
    trunk: 198.51.100.10:5060
```

```sh
sudo freesbc check --deploy --local-addresses -c /etc/freesbc/freesbc.yaml
sudo systemctl enable --now freesbc
```

`check --deploy` names the kernel settings to raise. Leave FreeSBC's `admin`
section out, or give it a port other than 8080, which the application holds.

## 3. Send the trunk through FreeSBC

Follow [carrier.md](carrier.md) with these values: the gateway's next hop is
FreeSBC's private socket, and its `proxy` is the carrier entry, byte for byte
as `edge.carriers` has it, port included.

```yaml
services:
  freeswitch:
    environment:
      PSTN_GATEWAY_HOST: 127.0.0.3
      PSTN_GATEWAY_PORT: "5060"
      PSTN_GATEWAY_PROXY: 198.51.100.10:5060
```

`.env`:

```ini
AICC_OUTBOUND_ENDPOINT=sofia/gateway/pstn_gateway/%s
AICC_OUTBOUND_SIP_HEADERS=X-FSBC-Out=1
```

`aicc doctor` then reports `outbound_gateway` as `ping UP`: the ping reached
the carrier through FreeSBC.

## 4. The web interface and the browser phone

Put a TLS proxy on 443 in front of `127.0.0.1:8080`
([production checklist](production-checklist.md)), then in `.env`:

```ini
SWITCH_DOMAIN=cc.example.com
SIP_WSS_URL=wss://cc.example.com:7443/
AICC_SECURE_COOKIES=true
```

and `docker compose up -d`. Agents open `https://cc.example.com` and add that
name to the browser phone's Allow Sites.

A browser phone has one way in here, FreeSBC's WSS listener. A proxy to the
switch's own `wss-binding` carries the signalling but not the call: the switch
advertises 127.0.0.2 for media, which no browser can reach.

## Checking it

| Test | Expect |
|---|---|
| `aicc doctor --host-addrs 127.0.0.0/8` | Every check passes |
| A SIP phone registers at `203.0.113.7:5060`, domain `cc.example.com` | 401 with that domain as realm, then 200 |
| OPTIONS from an address that never registered | No answer |
| An agent dials `95001` from the keypad | The bot answers; FreeSBC logs `webrtc media established` |
| The carrier delivers a call to a number of yours | The bot answers; FreeSBC logs `proxying INVITE upstream` with `carrier=trunk` |
| `POST /calls` with `kind: AI_OUTBOUND` | FreeSBC logs `proxying INVITE to carrier`; the callee's phone rings |

`journalctl -u freesbc -f` shows each of these as it happens.

## When one host is no longer enough

Move FreeSBC to a host of its own: `private.ip` and `edge.switch` become LAN
addresses, the gateway's next hop follows, and the installer's `--external-ip`
goes back to this host's LAN address.
