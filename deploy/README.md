# Deployment guide

This stack runs the whole product on one host: PostgreSQL, FreeSWITCH and the
application, seeded with a demo dataset.

There are two ways to install it:

- [The one-line installer](#install-with-one-command) downloads a release's
  deploy bundle, writes `.env` with generated secrets, starts the published
  images and checks the result with `aicc doctor`. Start here.
- [The manual compose quick start](#manual-install-with-compose) runs the same
  stack from a git checkout. Use it to build from source, or where the
  installer refuses the host.

The platforms verified so far are listed in the
[support matrix](one-line-installer.md#supported-platforms). On macOS, read
[Running on macOS](#running-on-macos) first: what works depends on the
container runtime.

## Install with one command

Linux (as root, into `/opt/aicc`):

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo sh
```

macOS (as yourself, into `~/.aicc`):

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sh
```

Options go after `sh -s --`, for example
`curl -fsSL …/install.sh | sudo sh -s -- --provider qwen --no-demo`.

`releases/latest` is the latest full release. A pre-release (a tag with a
hyphen, such as `v0.2.0-rc.1`) is never `latest`: download its own script
from `releases/download/<tag>/install.sh`, or pass `--version <tag>`.

**Before it changes anything, it checks the host.** Each failed check prints a
stable tag (`FAIL PREFLIGHT_…`), what is wrong and the command that fixes it,
then stops with nothing written or started. `--check` runs only these checks.
They cover: Docker Engine 24.0.0 or later and Compose 2.24.4 or later (not the
snap package, not rootless), on macOS Docker Desktop or Colima 0.9.0 or later
with the `grpc` port forwarder, at least 4 CPUs and 4 GiB for the runtime,
both images published for this host's architecture, no other stack named
`aicc`, the stack's ports free, and on Linux an active ufw or firewalld
allowing the stack's ports ([Ports and firewall](#ports-and-firewall)).

**What it asks, and what it works out itself:**

- The LAN address phones reach this host at: inferred from the default route's
  interface, skipping loopback, link-local and TUN-proxy addresses
  (198.18.0.0/15). Pass `--external-ip <ip>` when the inference is wrong or
  phones reach the host through NAT.
- The voice provider (`--provider openai|qwen|gateway|doubao|gemini|none`) and
  its key. The key is read from the environment (`OPENAI_API_KEY`,
  `ALIYUN_API_KEY` for qwen, `DOUBAO_API_KEY`, `GEMINI_API_KEY`,
  `REALTIME_API_KEY` plus `AICC_PROVIDER_ENDPOINT` for gateway) or asked for at
  the terminal with the input hidden. With `sudo`, keep an exported key with
  `sudo -E`. `none` runs without a bot: calls to the bot numbers go to their
  queues.
- Secrets: `POSTGRES_PASSWORD`, `ESL_PASSWORD`, `LUA_PASSWORD` and
  `AICC_SEED_PASSWORD` are generated (32 random characters each) and written
  to `<dir>/.env`, mode 0600. With the demo seed (the default), the installer
  prints the admin password once, on the first install; afterwards it is
  `AICC_SEED_PASSWORD` in `.env`. An install made with `--no-demo` seeds no
  account, so the installer prints the `aicc useradd` command that creates the
  first administrator instead (as root on Linux).

`--yes` never prompts: it accepts confirmations and fails on any value it
would have had to ask for.

**Options:**

| Option | Meaning |
|---|---|
| `--provider P` | `openai`, `qwen`, `gateway`, `doubao`, `gemini` or `none`. Only a first install chooses; later runs keep what `.env` names |
| `--external-ip IP` | The address phones reach this host at. On a rerun, rewrites the address in `.env` |
| `--version TAG` | The release to install. Default: the release the script came from |
| `--no-demo` | Do not seed the demo dataset (`AICC_SEED` empty). Use it for production |
| `--upgrade` | Back up the database, then move an install to `--version` |
| `--uninstall` | Stop and remove the containers; keep the data volumes, `.env` and backups |
| `--purge` | With `--uninstall`: also delete the data volumes and the install directory |
| `--check` | Run the preflight checks only; change nothing |
| `--install-docker` | Linux: install Docker Engine with `get.docker.com` first (you confirm after it is downloaded) |
| `--yes`, `-y` | Never prompt |

**Where it installs.** `/opt/aicc` on Linux (run as root), `~/.aicc` on macOS
(run as yourself, never as root). The directory holds the release's compose
files and mounted configuration, `VERSION`, `.env` and, after an upgrade,
`backups/`. Linux uses `compose.linux.yml` (host networking), macOS
`compose.macos.yml` (one shared network namespace); `COMPOSE_FILE` in `.env`
names them, so plain `docker compose …` in that directory addresses the stack.

**Running it again.**

- **Rerun** (same command): repairs or restarts the install. It adds only the
  keys `.env` lacks, never regenerates a secret, and leaves unchanged
  containers alone. A rerun never changes the address: if the host's address
  moved, it warns and `aicc doctor` fails with `EXTERNAL_IP_NOT_ON_HOST`.
- **`--external-ip <ip>`**: writes the new address into `.env` and recreates
  what depends on it.
- **`--upgrade`**, run with the new release's script or with
  `--version <tag>`: writes a `pg_dump` of the database to
  `<dir>/backups/aicc-<old tag>-<time>.dump` first, then installs the new
  bundle and images; the application migrates at startup. It refuses a
  downgrade: migrations are forward-only.
- **`--uninstall`**: removes the containers and keeps the data volumes, `.env`
  and `backups/`; rerunning the installer starts the same data again.
- **`--uninstall --purge`**: also deletes the data volumes and the install
  directory, after a confirmation.

### aicc doctor

The installer ends by running `aicc doctor` inside the application container.
Run it again at any time:

```sh
cd /opt/aicc && docker compose exec aicc aicc doctor --host-addrs <ip>   # ~/.aicc on macOS
```

`--host-addrs` is the host's address (comma-separated if several); on macOS
the container cannot see the Mac's interfaces, so without it the address check
is judged against the wrong host. Each line is `PASS`, `FAIL` or `SKIP`, the
check's name and a message; a `FAIL` also carries a code and a `fix:` line. The
exit status is 1 if and only if a check failed. `--skip-provider` leaves out
the provider session; `--json` prints the results as JSON. `--wait` keeps
asking until the application is ready and its switch link is up, and gives a
just-recreated `aicc_bot` gateway up to 45 seconds to answer again.

| Check | Code on failure |
|---|---|
| `app` | `APP_NOT_READY` |
| `switch_link` | `SWITCH_DOWN` |
| `database` | `DB_UNREACHABLE` |
| `migrations` | `DB_MIGRATIONS_PENDING` |
| `schema` | `DB_SCHEMA_NEWER` |
| `switch_reachable` | `SWITCH_UNREACHABLE` |
| `switch_auth` | `SWITCH_AUTH_REJECTED` |
| `sip_profiles` | `SWITCH_PROFILE_DOWN` |
| `bot_gateway` | `BOT_GATEWAY_DOWN` |
| `external_ip` | `EXTERNAL_IP_NOT_ON_HOST` |
| `external_ip_advertised` | `EXTERNAL_IP_STALE` |
| `provider_key` | `PROVIDER_KEY_MISSING` |
| `provider_session` | `PROVIDER_SESSION_FAILED` |

### Next: an agent's browser phone

1. Install the web-sip-phone extension from the
   [Chrome Web Store](https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp).
2. In the extension, add the host's address (`<ip>`) to its Allow Sites.
3. Open `http://<ip>:8080` and sign in as an agent (`wei`, `amy` or `ben`;
   the password is `AICC_SEED_PASSWORD` in `.env`).
4. Press **Sign in** on the phone bar. The agent's device counts as registered
   only after this ([#37](https://github.com/rasonyang/ai-native-callcenter/issues/37)).
5. Dial `95001` (English) or `95002` (Chinese) to reach the bot. The agent's
   phone is in the `aicc` dialplan context, which hands any enabled number of
   the platform's own to the same doorway a carrier's caller uses; a number
   that is not one of them is dialled out through the trunk, if there is one.

### Ports and firewall

| Overlay | Switch media (RTP) | Bot media | How ports reach the stack |
|---|---|---|---|
| `compose.linux.yml` (installer, Linux) | `16384-29999/udp` | `30000-30999/udp`, bot SIP `6060` | Host networking: the switch and the application bind the host's addresses; nothing is published |
| `compose.macos.yml` (installer, macOS) | `16384-16583/udp` (100 RTP/RTCP pairs) | not published | The `netns` container publishes 8080/tcp, 5060/udp+tcp, 5066/tcp and the RTP range |
| none (manual quick start) | `16384-16484/udp` | not published | Published by the switch and application containers |

`RTP_START` and `RTP_END` in `.env` move the switch's range; on macOS each
published port costs a forwarder in the VM, so widen it with care.

On Linux with an active ufw or firewalld, the preflight fails with
`PREFLIGHT_FIREWALL` until these are allowed (and prints the exact commands):
`8080/tcp`, `5060/tcp`, `5060/udp`, `5066/tcp`, `5080/tcp`, `5080/udp`,
`7443/tcp` and the RTP range `16384-29999/udp`. For ufw:

```sh
for r in 8080/tcp 5060/tcp 5060/udp 5066/tcp 5080/tcp 5080/udp 7443/tcp 16384:29999/udp; do sudo ufw allow "$r"; done
```

For firewalld:

```sh
sudo firewall-cmd --permanent --add-port=8080/tcp --add-port=5060/tcp --add-port=5060/udp \
    --add-port=5066/tcp --add-port=5080/tcp --add-port=5080/udp --add-port=7443/tcp \
    --add-port=16384-29999/udp && sudo firewall-cmd --reload
```

The bot's ports (6060, `30000-30999/udp`) need no rule: only the switch on the
same host sends to them, and the application accepts SIP, RTP and RTCP only
from the addresses in `AICC_BOT_ALLOWED_PEERS` (loopback and `FS_LOCAL_IP`).
The [production checklist](production-checklist.md) covers narrowing SIP and
RTP to the phones that need them.

## Manual install with compose

The fallback: the same stack from a git checkout, without the installer.

### Prerequisites

- A Linux host with [Docker Engine](https://docs.docker.com/engine/install/)
  and the Compose plugin, v2 or later (`docker compose version` works). On
  macOS, see [Running on macOS](#running-on-macos).
- A Qwen key (`ALIYUN_API_KEY`), or an OpenAI key outside mainland China.
- The address phones use to reach this host.

### Quick start

**1. Get the stack and copy the example config.**

```sh
git clone --depth 1 --branch v0.3.0 https://github.com/rasonyang/ai-native-callcenter
cd ai-native-callcenter/deploy
cp .env.example .env
```

Compose builds and mounts files from this checkout, so run it from `deploy/`.
The branch is the release tag, the same tag as `AICC_IMAGE` and
`AICC_FS_IMAGE` in step 3: the database init script, the switch's confined
role and its simulated dialplan and directory are mounted from the checkout,
and they must belong to the release the images come from.

**2. Set two values in `deploy/.env`.**

```ini
FS_EXTERNAL_IP=<the address phones reach this host at>
ALIYUN_API_KEY=<key>
```

- `FS_EXTERNAL_IP` is required. Compose refuses to start without it. A wrong
  value lets a phone register but it hears no audio.
- Outside mainland China, set `AICC_PROVIDER=openai` and `OPENAI_API_KEY`
  instead of `ALIYUN_API_KEY`.
- Put the key in `deploy/.env`. The `.env` at the repository root is not read
  by the stack, and a key exported in your shell does not reach the
  application either: compose passes provider keys to it only from
  `deploy/.env`.
- Without a key, everything works except the bot: it never answers, every call
  to a bot number goes straight to that number's queue, and the caller hears
  hold music until an agent picks up.
- Every other setting has a default. Every password is `aicc@123`. Change
  them before others can reach the host
  ([production checklist](production-checklist.md)).

**3. (Optional) Use the published images instead of building.** By default the
first start builds the application from this checkout (a few minutes; later
starts take seconds). To skip the build, uncomment these two lines in `.env`:

```ini
AICC_IMAGE=rasonyang/ai-native-callcenter:v0.3.0
AICC_FS_IMAGE=rasonyang/freeswitch-aicc:v0.3.0
```

- The application image contains the executable with the SPA embedded;
  `docker compose pull` is all it takes to get the product onto a host.
- Use exact tags. There is no `latest`.
- Both images must carry the same release tag. The application's migrations and
  the switch's Lua scripts share the `luacc.*` contract; a mismatched pair
  breaks phone registration.

**4. Start the stack and check it.**

```sh
docker compose up -d
docker compose ps -a            # three running, lua-role exited (0)
docker compose logs aicc | grep 'esl connected'
# {"level":"INFO","msg":"esl connected","addr":"freeswitch:18021"}
docker compose logs aicc | grep -c 'API_KEY is not set'
# 0
docker compose exec postgres psql -U aicc aicc -Atc \
    'select max(version_id) from goose_db_version where is_applied'
# the highest number under internal/store/migrations/ in this checkout
```

- `postgres` and `freeswitch` show `(healthy)`; `aicc` shows `Up`. It has no
  healthcheck, so `Up` is the whole of what `ps` says about it.
- `lua-role` exited non-zero: the application never migrated. Read
  `docker compose logs aicc`.
- A few `esl connect failed … connection refused` warnings before
  `esl connected` are normal on the first start: the switch starts last, after
  `lua-role`, and the application retries until it is up.
- The switch's first boot logs `[ERR]` lines such as
  `NATIVE SQL ERR [no such table: channels]` and, from mod_pgsql,
  `relation "members" does not exist`. They are expected: the switch probes
  its core tables in the new `fs-db` volume and mod_callcenter probes its
  tables in the empty `aicc_fs` database, then creates what is missing.
- Count other than `0`: the provider key did not reach the application. The
  counted line names the variable. The application still starts, but the bot
  does not answer.

After any change to `.env`, run `docker compose up -d`. It recreates the
containers whose settings changed. `docker compose restart` does not re-read
`.env`.

**5. Open `http://<host>:8080`** and sign in as `admin` / `aicc@123`.

## Running on macOS

On macOS, containers run inside a Linux VM. A port published on the Mac is
forwarded into the VM, and the phone's SIP and media reach the switch from an
address of the VM's network instead of the phone's own. The switch treats every
phone except loopback as behind NAT: it advertises `FS_EXTERNAL_IP` in the SDP
and sends its media to the address the phone's media arrives from. `network_mode: host` and macvlan networks attach to
the VM, not to the Mac's network, so they do not help.

A call that connects with no audio in one direction is the failure to look
for. On a bot number the bot leg ends after 5 s without caller audio and the
caller is moved to the number's fallback queue
([Troubleshooting](#troubleshooting)).

The one-line installer uses `compose.macos.yml` on macOS: a placeholder
container, `netns`, owns one network namespace on the compose network and
publishes every port (8080/tcp, 5060/udp+tcp, 5066/tcp and the RTP range
`16384-16583/udp`, 200 ports); the application and the switch join it. The
installer supports Docker Desktop and Colima; OrbStack, Podman and Rancher
Desktop are refused (`PREFLIGHT_RUNTIME_UNSUPPORTED`).

### Colima

Colima's default `ssh` port forwarder forwards TCP only: SIP over UDP and all
media never reach the switch. The `grpc` port forwarder forwards UDP too. It
needs Colima 0.9.0 or later (the `--port-forwarder` flag); 0.10.3 is the
oldest version verified
([support matrix](one-line-installer.md#supported-platforms)).
Start the VM with it, with at least 4 CPUs and 4 GiB:

```sh
colima start --port-forwarder grpc --cpu 4 --memory 4
```

The installer refuses a profile with the `ssh` forwarder
(`PREFLIGHT_COLIMA_PORT_FORWARDER`), with too few resources
(`PREFLIGHT_RESOURCES`), or started with `--network-address`
(`PREFLIGHT_COLIMA_NETWORK_ADDRESS`); each failure prints the `colima stop`
and `colima start` line that fixes it. Phones register at the Mac's LAN
address.

**Manual only: a VM address instead of port forwarding.** With the manual
compose quick start (not with the installer, which refuses it), the VM can get
its own address and phones point at it:

```sh
colima start --vm-type vz --network-address
colima list        # the ADDRESS column, e.g. 192.168.64.11
```

In `deploy/.env`:

```ini
FS_EXTERNAL_IP=<the ADDRESS from colima list>
```

Then follow the quick start from step 4. Phones register at
`<FS_EXTERNAL_IP>:5060` over UDP, and the web interface is at
`http://<FS_EXTERNAL_IP>:8080`. SIP and media then reach the switch with the
phone's real address, as on a Linux host.

- If the profile already runs without `--network-address`, run `colima stop`,
  then the `colima start` line above.
- The VM address is on a network shared between the Mac and its VMs. Only
  softphones on the same Mac can reach it. Phones on other hosts need the VM
  bridged onto the LAN (Colima `--network-mode bridged`, which requires
  `socket_vmnet`); this is not verified.

### Docker Desktop

The installer supports Docker Desktop (Settings > Resources: at least 4 CPUs
and 4 GB of memory). An earlier manual setup was verified on an Intel Mac with
Docker Desktop and softphones on the same Mac. For the manual quick start, in
`deploy/.env`:

```ini
FS_EXTERNAL_IP=<the Mac's own LAN address>
```

Then follow the quick start from step 4. Phones register at
`<FS_EXTERNAL_IP>:5060` over UDP, and the web interface is at
`http://<FS_EXTERNAL_IP>:8080`.

- Phones on other hosts are not verified with Docker Desktop.
- `sofia status profile internal reg` shows a Docker gateway address as the
  phone's `IP` (`10.130.0.1` or `192.168.65.1`). That is expected here.

For phones on other hosts, or for anything beyond a trial, run the stack on a
Linux host or a Linux VM with a bridged network adapter.

## Demo data

`AICC_SEED` defaults to `demo`, so the first start fills an empty database.
Later boots add only what is missing: an account or extension that already
exists keeps its password and role. `AICC_SEED_PASSWORD` (default `aicc@123`)
is the password the seed gives the accounts and extensions it creates.

| | |
|---|---|
| Accounts | `admin` (administrator), `supervisor` (supervisor), `wei` / `amy` / `ben` (agents). Password `AICC_SEED_PASSWORD` (`aicc@123`) |
| Extensions | `amy` 1000, `wei` 1001, `ben` 1002. SIP password `AICC_SEED_PASSWORD` (`aicc@123`), readable through `GET /extensions/{id}/password`. A signed-in agent's browser phone gets its own credentials from the platform; the static password is for a hand-configured phone while nobody is signed in at that extension |
| Queues | `support-en` on 7001 (`wei`, `amy`), `support-zh` on 7002 (`ben`) |
| Customers | 18 numbers a SIP phone can register as: 13800000001–13800000009 and (212) 555-0101 – (212) 555-0109. Password `AICC_SEED_PASSWORD` (`aicc@123`), registrar `<FS_EXTERNAL_IP>:5060`, domain `<FS_EXTERNAL_IP>`. A number is unreachable until a phone registers as it |
| History | Seven deterministic days of calls, queue events and presence, for the wallboard and reports |

Six published bilingual flows, each on an English, a Chinese and a US number:

| Flow | English | Chinese | US |
|---|---|---|---|
| NovaNet support | 95001 | 95002 | 800-555-0199 |
| StarCom mobile after-sales | 95011 | 95012 | 800-555-0191 |
| NovaNet broadband plan change | 95021 | 95022 | 800-555-0192 |
| NovaNet early collections | 95031 | 95032 | 800-555-0193 |
| StarCom field-service appointment | 95041 | 95042 | 800-555-0194 |
| StarCom lead qualification | 95051 | 95052 | 800-555-0195 |

- 800-555-0199 is the main line and the caller ID for outbound calls.
- The dialled number selects the flow and the language.
- Five of the six flows call a business backend. Without
  `AICC_BOT_BACKEND_BASE` pointing at a running `cmd/aicc-mockbackend`, their
  tools fail and the bot offers a transfer instead. This is expected
  ([docs/dev-stack.md](../docs/dev-stack.md)).

**Remove or re-seed the demo data:**

```sh
AICC_SEED=fresh docker compose up -d aicc   # remove what the seed created
docker compose up -d aicc                   # seed again
```

- `fresh` is the only way back to an empty product once the volume exists.
- It removes only seeded data. Real calls placed on this stack stay.
- Seeding again restores accounts, queues, flows and numbers, but not the
  history. History is only generated into an empty ledger.

## Make a test call

**1. Register a customer phone.** In any SIP softphone, register as
`2125550101` / `aicc@123` at `<FS_EXTERNAL_IP>:5060`, domain
`<FS_EXTERNAL_IP>`. The phone is now (212) 555-0101. Do this first: an
unregistered number is unreachable.

**2. Call the customer as agent wei.**

1. Install the [web-sip-phone](https://github.com/rasonyang/web-sip-phone)
   Chrome extension. It needs no configuration.
2. Sign in as `wei` / `aicc@123`. The platform issues the browser its own SIP
   credentials and passes them to the extension.
3. Follow the onboarding card in the cockpit: allow this site, allow the
   microphone. The card disappears when the phone chip in the softphone bar
   shows ready.
   For many agents, [Chrome policy](chrome-policy.md) does this instead.
4. Open the keypad, type `2125550101`, press Dial. Dialling out does not need
   *Go ready*.

wei's phone rings first and auto-answers, then the customer phone rings showing
800-555-0199. Answer it. The call appears on the wallboard and ends as a CDR.

Going *ready* requires a registered phone; an agent without one is refused.

**3. Call in as the customer.** From the customer phone, dial `8005550199`. The
bot answers in English as NovaNet support. Ask for a person: the call goes to
`support-en` and rings wei once wei has picked *Go ready*. 95001 and 95002 are
the same flow; 95002 greets in Chinese.

**Without a phone**, place the same call from inside the switch:

```sh
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123 \
    -x "originate {aicc_harness=true}loopback/95001/public &playback(silence_stream://20000)"
# +OK <uuid>
docker compose logs aicc | grep 'ai conversation started'
# {"level":"INFO","msg":"ai conversation started",…,"provider":"qwen","language":"en"}
```

- `-p` is the `ESL_PASSWORD` value; `aicc@123` is its default.
- `-ERR UNALLOCATED_NUMBER` means no enabled number `95001` exists.
- The `ai conversation started` line names the provider that answered. The
  call then appears under CDRs, and its transcript starts with the bot's
  greeting.

## What is running

| Container | Role |
|---|---|
| `postgres` | PostgreSQL 18. Holds `aicc` and mod_callcenter's `aicc_fs` |
| `aicc` | The application: REST API, event stream, embedded SPA, and the SIP endpoint for AI calls |
| `lua-role` | Runs once, creates the confined role the switch reads the database with, and exits |
| `freeswitch` | FreeSWITCH v1.11.3, this repository's own image |

Volumes: `postgres-data`, `recordings` (written by the switch, read by the
application), `fs-db`, `fs-log`, `app-logs`.

Published ports (on `HTTP_BIND` or `SIP_BIND`):

| Port | Use |
|---|---|
| 8080/tcp | API, event stream and web interface |
| 5060/udp, 5060/tcp | SIP signalling |
| 5066/tcp | Browser phone WebSocket transport |
| 16384-16484/udp | Media to phones off this host |

ESL, the metrics listener, and the AI leg's SIP and RTP are not published.

The switch reaches the AI leg through the gateway `aicc_bot`. `sofia status`,
`GET /system/health` and the administrator's Overview show it as `NOREG`. That
is by design: it is not a registering trunk, and FreeSWITCH sends calls to it
without registration. Its health is the OPTIONS ping the application answers:
`sofia status gateway aicc_bot` shows `Status UP`. `/system/health` reports
`isUp: true` for a `NOREG` trunk; it does not reflect the ping.

## Common commands

```sh
docker compose logs -f aicc                             # application logs
docker compose exec freeswitch fs_cli -P 18021 -p aicc@123   # -p is ESL_PASSWORD
docker compose exec postgres psql -U aicc aicc
docker compose down                                     # stop
docker compose down -v                                  # stop and delete all data
```

## Configuration

Stack settings in `deploy/.env`:

| Variable | Meaning |
|---|---|
| `FS_EXTERNAL_IP` | Required. The address phones reach this host at |
| `POSTGRES_PASSWORD`, `LUA_PASSWORD`, `ESL_PASSWORD` | Default `aicc@123` |
| `AICC_SEED` | Default `demo`. Empty seeds nothing; `fresh` removes what the seed created. Empty it on a host others can reach ([production checklist](production-checklist.md)) |
| `SWITCH_DOMAIN` | The switch's domain. Default `FS_EXTERNAL_IP`. The application uses the same value |
| `SIP_DOMAIN` | Digest realm and registration domain for agents' browser phones. Default `SWITCH_DOMAIN`. Leave it unless you separate the two |
| `SIP_WSS_URL` | Where the browser phone connects. Default `ws://<FS_EXTERNAL_IP>:5066/`. Behind TLS, set the proxy's `wss://…` URL ([production checklist](production-checklist.md)). It is sent to the phone, not typed into it |
| `HTTP_BIND`, `HTTP_PORT`, `SIP_BIND` | Listen addresses for the published ports |
| `RTP_START`, `RTP_END` | Media port range. Configures the switch and publishes the ports |
| `AICC_SUBNET`, `AICC_APP_IP` | The compose network and the application's fixed address in it (the switch dials the bot at this address). Change together |
| `AICC_IMAGE`, `AICC_FS_IMAGE` | Unset: build the application from this checkout. Set to `rasonyang/ai-native-callcenter:<tag>` and `rasonyang/freeswitch-aicc:<tag>` to pull. Same release tag on both (they share the `luacc.*` contract; mismatched tags break phone registration). `AICC_FS_IMAGE` defaults to the switch of this checkout's release. Exact tags only; no `latest` |

**Application settings.** Any other `AICC_*` line in `deploy/.env` is passed to
the application unchanged: provider keys, `AICC_TRANSCRIBE_*`,
`AICC_BOT_BACKEND_BASE`, `AICC_S3_*`, and so on. The full list is the
repository's [`.env.example`](../.env.example). Two rules:

- An empty value means unset, so a non-empty default cannot be blanked.
  `AICC_SEED` is the exception in this stack: compose itself reads it, so no
  line means `demo` and `AICC_SEED=` means seed nothing.
- Everything after the first `=` is the value, including comments.

The wiring keys in `docker-compose.yml` are set under `environment:`, which
overrides `env_file:`, so `.env` cannot break them.

**Transcription.** Live transcription of the human phase is off by default.
To turn it on, set in `deploy/.env`:

```ini
AICC_TRANSCRIPTION_ENABLED=true
AICC_STREAM_ADDR=0.0.0.0:8090
AICC_STREAM_PUBLIC_URL=ws://aicc:8090/stream
AICC_STREAM_SECRET=<a long random string>
# qwen only:
AICC_TRANSCRIBE_ENDPOINT=wss://<workspace-id>.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference
```

- The application refuses to start with transcription on and
  `AICC_STREAM_PUBLIC_URL` or `AICC_STREAM_SECRET` empty.
- `AICC_STREAM_ADDR` defaults to loopback, which the switch in another
  container cannot reach. The port is not published.
- On `qwen`, `AICC_TRANSCRIBE_ENDPOINT` is required: the workspace id is part
  of the hostname, so there is no default. On other providers it is optional.
- `AICC_TRANSCRIPTION_ENABLED` also tells the switch to load
  mod_audio_stream, so it must be a line in `deploy/.env`, not only in the
  application's environment.

**Voice provider.** One provider answers every call, chosen at startup. A
call's language does not select it. `AICC_PROVIDER` takes `openai`, `qwen`,
`gateway`, `doubao` or `gemini`, each with its own credential.

- `doubao` uses `DOUBAO_API_KEY` (ByteDance full-duplex dialogue API); `gemini`
  uses `GEMINI_API_KEY` (Google Live API).
- On `doubao` and `gemini`, the client pins the model, so `AICC_PROVIDER_MODEL`
  is ignored. If transcription is on, set `AICC_TRANSCRIBE_PROVIDER` (also
  required on `gateway`).
- On `doubao`, every terminal phase of a flow needs an `announce`; publishing a
  flow without one is refused.
- On `gemini`, allow outbound access to `generativelanguage.googleapis.com`,
  and give every DID a fallback queue. Gemini closes the connection when its
  session lifetime runs out and the application does not reconnect: the call is
  released with hangup cause `PROVIDER_SESSION_EXPIRED` and the caller goes to
  the queue.

See [the provider notes](../docs/provider-extension.md).

**API.** The API is at `/api/v1` on the same port.
[docs/openapi.json](../docs/openapi.json) is the contract. For example,
`POST /auth/login` with `{"username":"admin","password":"aicc@123"}` is
`http://<host>:8080/api/v1/auth/login`.

- API keys are not configured in `.env`. Issue them with `POST /api-keys`,
  named and scoped; the first one with the administrator's session.
- Requests that write with a session cookie must carry an `X-AICC-Csrf` header
  (any value). Bearer-key requests need not.

## Recordings

- Default (`AICC_RECORDING_BACKEND=FS`): the switch writes to the shared
  `recordings` volume and the application serves from it.
- Files are `recordings/YYYY/MM/DD/<call_id>.wav`, stereo: caller left, bot or
  agent right.
- Retention is a product setting, applied by a daily job.
- S3-compatible storage: set `AICC_RECORDING_BACKEND=S3` and the `AICC_S3_*`
  lines in `.env`. The application uploads each recording on hangup, clears the
  spool, and plays it back through a presigned URL.

## Upgrading

An install made by the one-line installer upgrades with `--upgrade`
([Running it again](#install-with-one-command)). For a manual install, first
move the checkout to the new release tag, because compose mounts files
from it:

```sh
git fetch --depth 1 origin tag <tag> && git checkout <tag>
```

- Published images: change `AICC_IMAGE` and `AICC_FS_IMAGE` to the same tag,
  then `docker compose pull && docker compose up -d`.
- Built from the checkout: `docker compose up -d --build`.

Migrations run at startup and are forward-only. A single-instance advisory lock
prevents two from running at once.
[freeswitch/README.md](../freeswitch/README.md) covers installing the switch by
hand instead of pulling it.

## Troubleshooting

- **A call to a bot number only plays hold music.** The bot session did not
  start, and the caller went to the number's fallback queue with nobody
  staffed. Find the reason with
  `docker compose logs aicc | grep -E 'could not run|conversation failed'`.
- **The bot speaks, then about 5 s later the call goes to a queue.** The
  caller's audio does not reach the switch, so the bot leg receives no media
  and ends. The application logs `media went dead`; the switch logs
  `aicc_inbound: bot leg vanished` and transfers the caller to the number's
  fallback queue. Check, in order
  (`docker compose exec freeswitch fs_cli -P 18021 -p aicc@123`):
  1. The `c=` line in the SDP of the switch's `200 OK` to the phone must be an
     address the phone can reach, normally `FS_EXTERNAL_IP`. In an interactive
     `fs_cli`, run `sofia profile internal siptrace on`, place the call, read
     the `200 OK` it prints, then `sofia profile internal siptrace off`. A
     container address (`10.130.x.x`) there means the internal profile treats
     the phone as local: its `local-network-acl` must be `aicc_sip_local`
     (`conf/sip_profiles/internal.xml` in the switch image).
  2. `FS_EXTERNAL_IP` is the address the phone dials.
  3. The `RTP_START`–`RTP_END` UDP range reaches the switch: open in the
     firewall on a Linux host; on a Mac, see
     [Running on macOS](#running-on-macos).

  In `sofia status profile internal reg`, a Docker gateway address as the
  phone's `IP` (`10.130.0.1` by default) means the runtime forwards the port
  through its own proxy. It is expected on Docker Desktop and is not by itself
  a fault.
- **A softphone registers but hears nothing.** Same cause in the other
  direction: the switch's media does not reach the phone, or the phone drops
  it. Check the same things as above.
- **`… API_KEY is not set` in the logs.** The key is not in the container's
  environment. Put it in `deploy/.env` and run `docker compose up -d`
  (`restart` is not enough).
- **The browser phone registers to an old deployment, or cannot register
  after moving to a new one.** The extension keeps registering to a previous
  deployment that used the same extension number
  ([#38](https://github.com/rasonyang/ai-native-callcenter/issues/38)), and
  Sign Out / Clear Account does not drop a credential the platform provisioned
  ([rasonyang/web-sip-phone#13](https://github.com/rasonyang/web-sip-phone/issues/13)).
  Remove the old deployment's host from the extension's Allow Sites, then sign
  in again on the new one.
- **A `401` or a failed handshake.** The provider rejected the key: it is wrong,
  or not enabled for the realtime model shown in the `voice provider selected`
  log line.

## More

- [chrome-policy.md](chrome-policy.md): install the browser phone and grant
  its permissions on a managed fleet, instead of the onboarding card.
- [carrier.md](carrier.md): replace the simulated public network with your own
  trunk.
- [production-checklist.md](production-checklist.md): passwords, demo data, TLS
  and firewalling before the stack faces anyone.
