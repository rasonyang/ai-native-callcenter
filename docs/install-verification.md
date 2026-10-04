# Install verification checklist

The manual checklist a release runs before it is announced: the one-line
installer on three clean targets, through a live call. Each step has a pass
criterion; a step passes only when its criterion is met exactly. This document
holds the steps and the criteria only. Record the results (versions, outputs,
pass or fail, elapsed times) in the run log of the release, not here.

Targets:

- **A.** A fresh Ubuntu Server 24.04 VM, x86_64, Docker Engine installed with
  `--install-docker` or from docs.docker.com.
- **B.** macOS 12 Monterey on Intel with Docker Desktop.
- **C.** macOS 12 Monterey on Intel with Colima.

Each target also needs a second machine on the same LAN with Chrome and the
[web-sip-phone](https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp)
extension (steps 4 to 6), and a way to change the target's LAN address
(step 9).

Throughout, `<ip>` is the target's LAN address, `<dir>` is `/opt/aicc` (A) or
`~/.aicc` (B, C), `<tag>` is the release under test and `<prev>` the release
before it. On A, run the installer with `sudo`; on B and C, without.

## Runtimes for macOS 12 on Intel

Current releases of both runtimes have left macOS 12 behind, so B and C pin
older or source-built versions. Researched on 2026-09-28; sources are listed
at the end of this section.

**B. Docker Desktop 4.41.2 (build 191736)**, the last release whose minimum
is macOS 12 (4.42.0 raised it to macOS 13.3). It bundles Docker Engine 28.1.1
and Compose v2.35.1, both above the installer's minimums (24.0.0 and 2.24.4).

```sh
curl -fLo Docker.dmg https://desktop.docker.com/mac/main/amd64/191736/Docker.dmg
echo "51a14a53808659f02b48f571dcf0e3cdb03a7e69cc51cc9ecb519bf6b10403df  Docker.dmg" | shasum -a 256 -c
sudo hdiutil attach Docker.dmg
sudo /Volumes/Docker/Docker.app/Contents/MacOS/install --accept-license
sudo hdiutil detach /Volumes/Docker
```

Turn off automatic updates in Docker Desktop's settings: every newer release
refuses macOS 12. Set Settings > Resources to at least 4 CPUs and 4 GB.

**C. Colima 0.10.3 with Lima 2.2.0 and QEMU 11.1.1, from MacPorts; docker CLI
29.8.1 and Compose v5.5.1 as static binaries.**

- `--vm-type vz` needs macOS 13 or later, so the VM type is `qemu`.
- The `grpc` port forwarder exists since Colima 0.9.0.
- Colima's FAQ states it requires macOS 13 or newer; target C is outside
  what Colima supports, which is what this target checks.
- Homebrew treats Intel Macs as Tier 3 and publishes no Monterey bottles for
  qemu, lima or colima. MacPorts builds all three on macOS 12 x86_64.
- The official release binaries declare newer minimums (colima 0.10.3:
  macOS 15.0; limactl 2.2.0: macOS 26.0). Confirm before the run: whether
  they start on macOS 12. Use the MacPorts builds.

```sh
# MacPorts for Monterey: https://www.macports.org/install.php
sudo port install colima          # also installs lima and qemu
port installed colima lima qemu   # confirm before the run: colima 0.10.3, lima 2.2.0, qemu 11.1.1
curl -fsSL https://download.docker.com/mac/static/stable/x86_64/docker-29.8.1.tgz | tar xz
sudo install docker/docker /usr/local/bin/docker
mkdir -p ~/.docker/cli-plugins
curl -fsSLo ~/.docker/cli-plugins/docker-compose \
    https://github.com/docker/compose/releases/download/v5.5.1/docker-compose-darwin-x86_64
chmod +x ~/.docker/cli-plugins/docker-compose
docker compose version            # confirm before the run: the CLI finds the plugin here
colima start --vm-type qemu --port-forwarder grpc --cpu 4 --memory 4
```

Sources:

- Docker Desktop 4.41.2 appcast (minimum macOS 12.0.0):
  https://desktop.docker.com/mac/main/amd64/191736/appcast.xml
- Docker Desktop release notes, 4.41.0 components and 4.42.0 minimum macOS
  13.3: https://docs.docker.com/desktop/release-notes/
- Checksums of the 4.41.2 Intel image:
  https://desktop.docker.com/mac/main/amd64/191736/checksums.txt
- Lima, vz needs macOS 13:
  https://github.com/lima-vm/lima/blob/master/website/content/en/docs/config/vmtype/vz.md
- Colima 0.9.0 release notes (`--port-forwarder`):
  https://github.com/abiosoft/colima/releases/tag/v0.9.0
- Colima FAQ (macOS 13 or newer):
  https://github.com/abiosoft/colima/blob/main/docs/FAQ.md
- Colima 0.10.3 needs Lima 0.18.0 or later:
  https://github.com/abiosoft/colima/blob/v0.10.3/core/core.go
- Homebrew support tiers: https://docs.brew.sh/Installation
- MacPorts ports: https://ports.macports.org/port/colima/ ,
  https://ports.macports.org/port/lima/ , https://ports.macports.org/port/qemu/
- Docker static binaries:
  https://download.docker.com/mac/static/stable/x86_64/
- Compose v5.5.1: https://github.com/docker/compose/releases/tag/v5.5.1

## Common commands

The switch's console, from `<dir>`:

```sh
fs() { docker compose exec -T freeswitch sh -c "fs_cli -H 127.0.0.1 -P 18021 -p \"\$FS_ESL_PASSWORD\" -x \"$1\""; }
fs 'sofia status profile internal reg'
```

`aicc doctor`, from `<dir>`:

```sh
docker compose exec aicc aicc doctor --host-addrs <ip>
```

The installer, from the release under test (the URL of a pre-release, since
`releases/latest` never points at one):

```sh
URL=https://github.com/rasonyang/ai-native-callcenter/releases/download/<tag>/install.sh
curl -fsSL "$URL" | sudo sh -s -- --provider qwen     # A; B and C without sudo
```

Export the provider key first (`ALIYUN_API_KEY` for qwen; keep it through
`sudo` with `sudo -E`), or type it at the prompt.

## Steps

Run every step on each target, in order.

### 1. One-liner from a clean state

Start from a host with no `<dir>`, no container or volume whose compose
project is `aicc` (`docker ps -a --filter label=com.docker.compose.project=aicc`
and `docker volume ls --filter label=com.docker.compose.project=aicc` print
nothing) and, on A, no Docker at all (then add `--install-docker`).

```sh
time (curl -fsSL "$URL" | sudo sh -s -- --provider qwen)
```

Pass:

- The preflight prints only `PASS` lines, the installer ends with
  `The call center is running.` and `Installed <tag> in <dir> in <n>s.`, and
  exits 0.
- `ls -l <dir>/.env` shows mode `-rw-------`.
- `curl -fsS http://<ip>:8080/` from the second machine returns the web page.

Record in the run log: the elapsed time from the command's start to the
`Installed …` line (the installer prints it after `aicc doctor` has seen
`/readyz` report ready), and the `time` output.

### 2. `aicc doctor`

```sh
docker compose exec aicc aicc doctor --host-addrs <ip>; echo "exit $?"
```

Pass: no `FAIL` line, exit status 0. `SKIP` is allowed only for
`provider_session` when run with `--skip-provider`.

### 3. Agent on the same host

On the target itself, in Chrome with web-sip-phone installed: add `<ip>` to
the extension's Allow Sites, open `http://<ip>:8080`, sign in as `wei`
(password: `AICC_SEED_PASSWORD` in `<dir>/.env`), allow the microphone, press
**Sign in** on the phone bar.

Pass:

- `fs 'sofia status profile internal reg'` lists wei's extension (`1001`)
  as `Registered`.
- After **Sign in**, in the page's DevTools console,
  `await (await fetch('/api/v1/auth/me')).json()` shows
  `isDeviceRegistered: true`. Before **Sign in** it is `false`; that is
  [#37](https://github.com/rasonyang/ai-native-callcenter/issues/37), not a
  failure.

### 4. Agent on a second LAN machine over `ws://`

On the second machine, the same as step 3, signed in as `amy` (extension
`1000`), with DevTools open on the Network tab (filter: WS) before the phone
connects.

Pass: the criteria of step 3 for `amy`, and DevTools shows a WebSocket to
`ws://<ip>:5066/` with status `101 Switching Protocols`.

### 5. Dial 95001: the bot answers

As `wei` (step 3), open the keypad on the softphone bar, type `95001`, press
Dial.

The `aicc` context routes every enabled number of the platform's own (the
Numbers page) to the same doorway a carrier's caller uses, so this is the
normal path. The same works from the switch, with no browser: originate a leg
in the `aicc` context and let it dial the number, as an agent's phone does:

```sh
fs 'originate {origination_caller_id_number=1001,aicc_extension=1001}loopback/95001/aicc &playback(silence_stream://20000)'
```

To have the switch ring wei's phone and then route the answered leg to the
bot (the header makes the browser phone answer by itself):

```sh
fs 'originate {sip_h_Call-Info=<sip:<ip>>;answer-after=0}user/1001@<ip> 95001 XML public'
```

Pass:

- The English greeting is audible in wei's browser.
- A spoken question gets a spoken answer from the bot.
- `docker compose logs aicc | grep 'ai conversation started'` shows the call
  with `"language":"en"`.
- After hanging up, the call is listed under CDRs with its transcript.

### 6. Transfer to a human; the LAN agent answers

Before the call, `amy` (step 4) selects **Go ready**; she is a member of the
queue `support-en`. Repeat step 5 and ask the bot for a person.

Pass:

- amy's browser phone rings and answers; the cockpit shows the caller.
- Two-way audio between wei (the caller, on the target) and amy (on the
  second machine): each hears the other.
- After hanging up, one CDR covers the bot phase and the human phase.

### 7. Supervisor monitor on the live call

The demo's `supervisor` account has no phone, and monitoring rings the
supervisor's own phone. Before this step, as `admin`, create an extension for
the supervisor and give `supervisor` an agent identity bound to it (the
administration screens, or `POST /api/v1/extensions` and
`POST /api/v1/agents` with `userId`, `callcenterName` and
`defaultExtensionId`). Sign in as `supervisor` in a separate Chrome profile
with web-sip-phone installed and `<ip>` allowed, and press **Sign in** on the
phone bar.

Place the call of step 6 again. While wei and amy talk, the supervisor opens
the live call and starts monitoring amy's leg in LISTEN mode, then WHISPER.

Pass:

- LISTEN: the supervisor's phone rings and answers, the supervisor hears both
  wei and amy, and neither hears the supervisor.
- WHISPER: amy hears the supervisor; wei does not.
- The call's CDR lists no party for the supervisor.

### 8. Rerun the installer

Before and after the rerun, from `<dir>`:

```sh
sha256sum .env        # macOS: shasum -a 256 .env
docker compose ps -a --format '{{.Service}} {{.ID}}' | grep -v '^lua-role ' | sort
```

Then run the step 1 command again, unchanged.

Pass: the rerun exits 0 with no preflight `FAIL`; the `.env` hash and the
container IDs are unchanged. `lua-role` is excluded: it is a one-shot
container that runs again on every `up`.

### 9. The host's address changes

Change the target's LAN address (a new DHCP reservation, or a new static
address). Then, before any rerun:

```sh
docker compose exec aicc aicc doctor --host-addrs <new-ip>
```

Pass, before: a `FAIL  external_ip  EXTERNAL_IP_NOT_ON_HOST` line and exit
status 1. A plain rerun of the installer warns that the host's address looks
different and leaves `.env` alone.

```sh
curl -fsSL "$URL" | sudo sh -s -- --external-ip <new-ip>
```

Pass, after: the installer exits 0; `aicc doctor --host-addrs <new-ip>` has no
`FAIL`; with `<new-ip>` in Allow Sites (and the old address removed, see
[#38](https://github.com/rasonyang/ai-native-callcenter/issues/38)), steps 4
to 6 pass again.

### 10. Upgrade from the previous release

On a clean target (step 1's starting state), install `<prev>` with its own
script, then upgrade with the script of `<tag>`:

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/download/<prev>/install.sh | sudo sh -s -- --provider qwen
curl -fsSL "$URL" | sudo sh -s -- --upgrade
```

When `<tag>` is the first release with an installer, `<prev>` is one of its
release candidates.

Pass:

- The upgrade prints `Backing up the database to <dir>/backups/aicc-<prev>-….dump`
  and exits 0; `cat <dir>/VERSION` prints `<tag>`.
- The dump is readable:
  `docker compose exec -T postgres pg_restore -l < backups/<file>.dump | head`
  lists the archive's table of contents.
- `aicc doctor` shows `PASS  migrations  schema N, this binary's latest N`
  (the two numbers equal) and no `FAIL`.
- Steps 5 and 6 pass.

### 11. Uninstall and purge

```sh
curl -fsSL "$URL" | sudo sh -s -- --uninstall
```

Pass: no container of project `aicc` remains
(`docker ps -a --filter label=com.docker.compose.project=aicc` is empty);
`docker volume ls --filter label=com.docker.compose.project=aicc` still lists
the volumes; `<dir>/.env` and `<dir>/backups` still exist.

```sh
curl -fsSL "$URL" | sudo sh -s -- --uninstall --purge
```

Pass: after confirming, no volume of project `aicc` remains and `<dir>` does
not exist.

### 12. Preflight refusals

Each case below must stop in the preflight with `FAIL <tag>`, what is wrong
and a `fix:` line, then `Nothing was installed or started.`, exit status 1,
no container of project `aicc` and no new `<dir>`. Undo each cause before the
next case.

| Case | Targets | How to cause it | Expected tag |
|---|---|---|---|
| Runtime stopped | A, B, C | A: `sudo systemctl stop docker.socket docker`; B: quit Docker Desktop; C: `colima stop` | `PREFLIGHT_DOCKER_DAEMON_DOWN` |
| Port 8080 taken | A, B, C | `python3 -m http.server 8080` in another terminal | `PREFLIGHT_PORT_IN_USE` |
| Image for another architecture | A, B, C | `curl -fsSL "$URL" \| sudo AICC_IMAGE=arm64v8/alpine:3.20 sh -s -- --check` (an arm64-only image on an x86_64 host; on B and C without `sudo`) | `PREFLIGHT_IMAGE_ARCH` |
| Firewall without the stack's ports | A | `sudo ufw allow OpenSSH && sudo ufw enable` | `PREFLIGHT_FIREWALL` |
| Too few resources | B, C | B: Settings > Resources, 2 CPUs and 2 GB, Apply & restart; C: `colima stop && colima start --vm-type qemu --port-forwarder grpc --cpu 2 --memory 2` | `PREFLIGHT_RESOURCES` |
| Colima's `ssh` forwarder | C | `colima stop && colima start --vm-type qemu --port-forwarder ssh --cpu 4 --memory 4` | `PREFLIGHT_COLIMA_PORT_FORWARDER` |

Pass: every case shows exactly its expected tag, and the fix it prints, run as
printed, makes the next `--check` pass that check.

## Known issues to expect

- [#37](https://github.com/rasonyang/ai-native-callcenter/issues/37):
  `isDeviceRegistered` is `false` until the agent presses **Sign in** on the
  phone bar.
- [#38](https://github.com/rasonyang/ai-native-callcenter/issues/38): the
  browser phone stays registered to another deployment with the same
  extension number. Remove the old deployment's host from Allow Sites.
- [rasonyang/web-sip-phone#13](https://github.com/rasonyang/web-sip-phone/issues/13):
  Sign Out / Clear Account does not drop a provisioned credential.
- A TUN proxy (mihomo, Clash) on the target or on the client: ICE candidates
  on `198.18.0.1` are harmless, and the installer ignores TUN routes when it
  infers the address. If the inferred address is wrong, pass
  `--external-ip`.
