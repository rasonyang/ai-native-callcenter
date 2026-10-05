# One-line installer

On Linux:

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo sh
```

On macOS (Colima or Docker Desktop), drop the `sudo`:

```sh
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sh
```

## What it does

The installer checks the host first, asks for the voice provider and its key,
generates every password into `.env`, starts the published images and checks
the running stack with `aicc doctor`. It then prints the address and the admin
password.

## Choosing the voice provider

The provider is chosen once, on the first install (`--provider P` or the
prompt); a later run keeps what `.env` names. Its key is the vendor's own
environment variable, not an `AICC_*` setting:

| Provider | Key |
|---|---|
| `openai` | `OPENAI_API_KEY` |
| `qwen` | `ALIYUN_API_KEY` |
| `gateway` | `REALTIME_API_KEY` and `AICC_PROVIDER_ENDPOINT=ws://…` |
| `doubao` | `DOUBAO_API_KEY` |
| `gemini` | `GEMINI_API_KEY` |

Export the key and pass `--provider` when there is no terminal to prompt at; on
Linux keep the exported key through sudo with `sudo -E`:

```sh
# Linux
export ALIYUN_API_KEY='<your-key>'
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo -E sh -s -- --provider qwen

# macOS
export ALIYUN_API_KEY='<your-key>'
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sh -s -- --provider qwen
```

`--provider none` installs with the AI leg off and no key. A rerun never
replaces a non-empty key; to change the provider or fix a key, edit `.env` in
the install directory (`/opt/aicc/.env` on Linux, `~/.aicc/.env` on macOS) and
rerun the installer, or `docker compose up -d` there. On Linux both need root:
the install directory and its `.env` belong to root, so edit with `sudo`, run
compose as `sudo docker compose`, and rerun the installer with `sudo -E` when
the new key is exported. Turning the bot back on after `--provider none` also
means removing the `AICC_BOT_ENABLED=false` line (or setting it to `true`).
`aicc doctor`'s `provider_key` and `provider_session` checks say whether the
key reached the app.

## What you get

The stack comes up seeded with a team, two queues, six published bilingual
flows (each behind an English, a Chinese and a US number, the main line being
800-555-0199), eighteen simulated customer telephones and a week of history,
so the wallboard is not empty and a softphone can ring the bot straight away.
Install the [web-sip-phone](https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp)
extension, sign in as an agent and dial 95001 (English) or 95002 (Chinese).

## Sign in

A default install seeds the demo dataset (`AICC_SEED=demo`), so it has an
account to sign in with. Open `http://<ip>:8080` and sign in as `admin`; the
password is generated with the other secrets on the first install and kept in
the install directory's `.env` as `AICC_SEED_PASSWORD` (`/opt/aicc/.env` on
Linux, `~/.aicc/.env` on macOS). The first install prints it, a rerun only
names that key. The demo agents (`amy`, `ben`, `wei`), `supervisor` and the
seeded SIP extensions and customer numbers share that password.

```sh
sudo grep AICC_SEED_PASSWORD /opt/aicc/.env    # Linux, root-owned mode 0600
grep AICC_SEED_PASSWORD ~/.aicc/.env           # macOS
```

The seed never overwrites an account that exists, so a password changed in the
app stays changed; `.env` keeps the value the seed first used.

An install made with `--no-demo` seeds no accounts. Create the first
administrator inside the application container:

```sh
# Linux: the install directory and .env belong to root
cd /opt/aicc && sudo docker compose exec aicc aicc useradd -username admin -password '<password>' -role ADMIN

# macOS
cd ~/.aicc && docker compose exec aicc aicc useradd -username admin -password '<password>' -role ADMIN
```

## Supported platforms

| OS | Arch | Runtime | Versions | What was verified |
|---|---|---|---|---|
| Ubuntu 24.04.5 LTS | x86_64 | Docker Engine | Engine 29.8.0, Compose v5.5.1 | The installer end to end (install, rerun, `--external-ip`, `--upgrade`, `--uninstall` / `--purge`, preflight failures); `aicc doctor` all PASS; a WebRTC agent on another LAN machine (Chrome 154 on macOS, web-sip-phone 1.0.7) registered over `ws://` with two-way audio to the bot on 95001 |
| macOS 26.6.2 | arm64 | Colima (vz, port forwarder `grpc`) | Colima 0.10.3, Docker 28.4.0, Compose 5.1.4 | The macOS overlay stack; `aicc doctor` all PASS; a WebRTC agent on the same host with two-way audio (switch configuration from this release); installer preflight (`--check`) only (the macOS 12.7.6 row is a full installer run) |
| macOS 12.7.6 | x86_64 | Colima (QEMU, port forwarder `grpc`) | Colima 0.10.1, Docker 29.2.1, Compose 2.40.0 | Verified 2026-10-04: the installer end to end (install with `--provider qwen` and `ALIYUN_API_KEY` in the environment, rerun, provider changed in `.env` to gemini and back; `aicc doctor` all PASS, including `provider_session` for qwen and gemini); admin login over the API; SIP over the UDP port forwarder (an OPTIONS on 5060 was answered). WebRTC audio not tested |

- Colima: 0.10.1 is the oldest version verified, and it must run the `grpc`
  port forwarder (`colima start --port-forwarder grpc`); the default `ssh`
  forwarder does not forward UDP.
- Both images are published for `linux/amd64` and `linux/arm64`. The
  installer's preflight refuses a host whose architecture an image lacks; it
  never runs an image under emulation.

Options, upgrades, removal and the manual compose install are in
[deploy/README.md](README.md).
