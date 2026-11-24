# Install AICC — instructions for a coding agent

This document tells a coding agent (Claude Code, Codex, OpenCode, pi) how to
install the AI Native Call Center on the person's machine with its one-line
installer, verify the result and hand over a working deployment. Work it top
to bottom. Every choice the installer needs is a question for the person —
asked once, with its default — never a guess and never a value invented to
keep moving. When something outside the install has to change (a port, the
firewall, Docker), that is the person's decision to make, not yours: see
[When to stop and ask](#when-to-stop-and-ask).

## What the installer does

`deploy/install.sh`, fetched from a release. It checks the host first and a
failed check stops it before anything is written or started, prints
`FAIL <tag>` with a `fix:` line for each; then it downloads the release's
deploy bundle and verifies its checksum, writes `.env` with generated
secrets, starts the published container images (PostgreSQL, FreeSWITCH, the
application) and finishes by running `aicc doctor` inside the stack. POSIX
sh, run through `curl | sh`.

- **Linux:** installs into `/opt/aicc`, as root. Every command goes through
  `sudo`, and `.env` is root-owned mode 0600.
- **macOS:** installs into `~/.aicc`, as the person — it refuses to run as
  root. Needs Docker Desktop or Colima (with the `grpc` port forwarder).

The script to run is `releases/latest/download/install.sh` for the latest
full release, or `releases/download/<tag>/install.sh` for a specific one:

```text
https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh
```

## 1. Preflight — `--check`, changes nothing

Run the checks before asking anything; their results shape the questions.

```sh
# Linux
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo sh -s -- --check

# macOS
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sh -s -- --check
```

`Preflight passed. Nothing was changed.` means the host is ready. A `FAIL`
line names a stable tag and its `fix:` — most are answered by
[Failures](#failures). The preflight's address check also prints the LAN
address it inferred (`PASS address 192.168.x.y (inferred 192.168.x.y)`); you
need that line for the address question below.

## 2. Ask the person — each question once, with its default

Ask these together, not one per message.

1. **Voice provider** — `--provider openai|grok|qwen|gateway|doubao|none`.
   No default; the first install chooses, a rerun keeps what `.env` names.
   `qwen` or `doubao` inside mainland China, `openai` or `grok` elsewhere,
   `gateway` for the person's own Realtime gateway (which also needs
   `AICC_PROVIDER_ENDPOINT=ws://…` or `wss://…` exported the same way the key
   is), `none` for no bot — the human path works, no key is needed and calls
   to the bot numbers go to their queues.
2. **The external address** — `--external-ip`. Default: the address the
   preflight inferred. It must be one of *this host's own* addresses: its LAN
   address, or a loopback an edge SBC on this host forwards to (e.g.
   `127.0.0.2`). A NAT's address, a public address or another machine's is
   refused (`PREFLIGHT_EXTERNAL_IP_NOT_ON_HOST`). Ask the person to confirm
   the inferred address; when the host has several, or a VPN/TUN proxy is in
   play, ask which one phones on the LAN reach.
3. **Demo data** — default yes: the demo seed installs a team, two queues,
   published bilingual flows, a week of history and the `admin` account.
   `--no-demo` seeds nothing — the right answer for production, and it means
   step 6 creates the first administrator with `aicc useradd`.
4. **Docker (Linux only)** — if `command -v docker` failed in the preflight:
   may the agent install Docker Engine? `--install-docker` downloads
   get.docker.com and runs it as root; the downloaded script stays at
   `/opt/aicc/get-docker.sh` for the person to read first. If not, the person
   installs Docker themselves and you start over at `--check`.
   On macOS a missing or unsuitable runtime is the person's to install
   (Docker Desktop, or `brew install colima docker docker-compose` and
   `colima start --port-forwarder grpc --cpu 4 --memory 4`).
5. **sudo (Linux only)** — can the person sudo on this host, and will they be
   present to type a password if `sudo` asks? The whole install runs as
   `sudo sh`; if `sudo -n true` fails and the person cannot type a password
   where you run commands, they must run the install line themselves — your
   job narrows to handing them the exact line and reading the result.

## 3. The provider key — never through the conversation

The key must reach the installer without ever appearing in a message, on a
command line (it would land in the transcript and in `ps`), in a file you
write or read, or in a log. There are two sanctioned channels; choose per
tool and say which you are using.

1. **The person puts it in the provider's variable first** (preferred).
   The variable is the vendor's own, never an `AICC_*` setting:

   | Provider | Variable |
   |---|---|
   | `openai` | `OPENAI_API_KEY` |
   | `grok` | `XAI_API_KEY` |
   | `qwen` | `ALIYUN_API_KEY` |
   | `doubao` | `DOUBAO_API_KEY` |
   | `gateway` | `REALTIME_API_KEY` |

   The person enters the export themselves so your commands inherit it:
   the `!` shell prefix in Claude Code, Codex and OpenCode runs a command in
   the session's shell (`! export ALIYUN_API_KEY=…`); pi accepts the same.
   Check that it landed without ever printing the value:
   `[ -n "$ALIYUN_API_KEY" ] && echo set`. If your tool cannot carry their
   export into your next command, ask them to write the export line into a
   file you may source but never open or print — they create it
   (`umask 077; printf 'export ALIYUN_API_KEY=%s\n' '<key>' > /tmp/aicc-key.env`),
   you source it inside the install run and delete it when the run ends.
2. **The installer's hidden prompt.** When the person can be at the terminal
   for the run — the tool hands over mid-command, or the person pastes the
   install line into their own shell — omit `--yes` and pass every other
   answer as a flag. The only prompt left is `PROVIDER_API_KEY (input
   hidden)`, which reads `/dev/tty` and echoes nothing. An install with
   `--install-docker` also asks them to confirm running get.docker.com.

   If neither channel works for your tool, hand the person the full install
   line to run in their own terminal; you still own preflight, the questions
   and verification.

On Linux the key survives `sudo` only when it is exported *and* `sudo -E`
keeps the environment. `sudo sh` without `-E` drops it, and the installer
stops with `no key for <provider>`.

Never: ask the person to paste the key into the conversation; pass it as an
argument; write it into a file yourself; echo, grep or log it; put it in
`.env` by hand (the installer writes it there itself, mode 0600). If a wrong
key is already in `.env`, see the provider-key row of [Failures](#failures).

## 4. Install — non-interactively

With the answers as flags, `--yes` accepts confirmations and fails on a
missing value rather than prompting:

```sh
# Linux — sudo -E keeps the exported provider key
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sudo -E sh -s -- --provider qwen --external-ip 192.168.1.20 --yes

# macOS
curl -fsSL https://github.com/rasonyang/ai-native-callcenter/releases/latest/download/install.sh | sh -s -- --provider openai --yes
```

Add `--no-demo` when the person declined the demo data, and `--install-docker`
only after they agreed to it. The run ends with `aicc doctor` inside the
stack, then `The call center is running.` and `Installed <tag> in <dir>` —
the first install also prints the admin password once.

## 5. Verify

The installer already ran `aicc doctor --wait 5m`. To re-check later, from
the install directory:

```sh
sudo docker compose exec aicc aicc doctor --host-addrs <ip>   # Linux; macOS without sudo
```

No `FAIL` line and exit 0 is a pass. Then register the skill (next section)
and hand over: the web interface is `http://<address>:8080`; sign in as
`admin` with `AICC_SEED_PASSWORD` (the first install printed it; afterwards
`sudo grep AICC_SEED_PASSWORD /opt/aicc/.env` on Linux, `grep
AICC_SEED_PASSWORD ~/.aicc/.env` on macOS); the browser phone is the
[web-sip-phone](https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp)
extension with the host's address in its Allow Sites; dial `95001` (English)
or `95002` (Chinese) for the bot. A `--no-demo` install has no accounts —
create the first administrator:

```sh
cd /opt/aicc && sudo docker compose exec aicc aicc useradd -username admin -password '<password>' -role ADMIN   # macOS: cd ~/.aicc, no sudo
```

## 6. Register the skill

`aicc skill` prints the SKILL.md embedded in the installed binary, so the
skill always matches the release. Write it where the agent's tool reads
skills — the directory named `aicc` under the tool's skills directory:

| Tool | User-level location |
|---|---|
| Claude Code | `~/.claude/skills/aicc/SKILL.md` |
| Codex | `${CODEX_HOME:-$HOME/.codex}/skills/aicc/SKILL.md` |
| OpenCode | `~/.config/opencode/skills/aicc/SKILL.md` |
| pi | `~/.pi/agent/skills/aicc/SKILL.md` |

`~/.agents/skills/aicc/SKILL.md` is the shared location several of these
tools also scan (OpenCode and pi read it; a project-level `.agents/skills/`
works the same way inside a repository). Project-scoped locations —
`.claude/skills/`, `.opencode/skills/`, `.pi/skills/` — register the skill for
one checkout instead of every project.

```sh
mkdir -p ~/.claude/skills/aicc
cd /opt/aicc && sudo docker compose exec -T aicc aicc skill > ~/.claude/skills/aicc/SKILL.md
# macOS: cd ~/.aicc, docker compose exec -T aicc aicc skill, no sudo
```

`-T` matters: over a PTY the output gains carriage returns, and the file
must be the embedded one byte for byte. On a source checkout the binary is
`bin/aicc skill` directly. A new session picks the skill up; verify the tool
lists `aicc` (pi: `/skill:aicc`; OpenCode: `@aicc`; Claude Code and Codex
advertise it by name).

## 7. Upgrade and remove

- **Upgrade:** the newer release's script with `--upgrade` backs up the
  database to `<dir>/backups/` then moves the install to `--version <tag>`
  (default: the script's own release). Downgrades are refused. Rerunning the
  same release repairs and restarts; it never regenerates a secret and never
  replaces a non-empty key.
- **Changing provider or key:** edit `.env` — `AICC_PROVIDER` and the
  provider's key variable (drop `AICC_BOT_ENABLED=false` when turning the bot
  on) — then rerun the installer, or `docker compose up -d` in `<dir>`. The
  person edits secrets, or tells you the file may be edited; the key itself
  still never goes through the conversation.
- **Uninstall:** `--uninstall` stops and removes the containers and keeps
  the data volumes, `.env` and backups; `--uninstall --purge` also deletes
  them and the install directory. `--yes` answers the purge's confirmation —
  never pass it unless the person explicitly asked for the data to go.

## Failures

Each `FAIL` line already prints its fix; this table is the context around
running it. One attempted fix per failure — a failure that survives its
documented fix is reported, not retried.

| Symptom | Tag | What to do |
|---|---|---|
| `docker is not installed` | `PREFLIGHT_DOCKER_MISSING` | Linux: ask, then `--install-docker`, or the person installs Docker Engine. macOS: the person installs Docker Desktop or Colima. |
| `the docker daemon is not reachable` | `PREFLIGHT_DOCKER_DAEMON_DOWN` | `sudo systemctl start docker` (Linux); open Docker Desktop; `colima start --port-forwarder grpc --cpu 4 --memory 4`. |
| `docker is the snap package` / rootless / too old, compose missing or too old | `PREFLIGHT_DOCKER_SNAP`, `PREFLIGHT_DOCKER_ROOTLESS`, `PREFLIGHT_DOCKER_TOO_OLD`, `PREFLIGHT_COMPOSE_MISSING`, `PREFLIGHT_COMPOSE_TOO_OLD` | The `fix:` line is the fix; every one changes what runs on the person's host — ask before running it. |
| ports are in use | `PREFLIGHT_PORT_IN_USE` | The fix line names the ports; `sudo ss -Hltnup` (Linux) / `lsof -nP -i<spec>` (macOS) names the holder. **Stop:** ask the person before anything that is not this stack is stopped or moved. |
| an address the host does not own | `PREFLIGHT_EXTERNAL_IP_NOT_ON_HOST` | The address must be one of this host's own — ask the person which of its addresses phones reach. A moved host also shows `FAIL external_ip` in `aicc doctor`: rerun with `--external-ip <new-ip>`. |
| the registry cannot be reached, or a tag it does not have | `PREFLIGHT_IMAGE_UNAVAILABLE`, `docker compose pull failed` | `no such manifest` is a wrong tag — check `--version`. Any other failure defers to after the pull; a pull that then fails means the host cannot reach the registry (mirror, proxy, firewall) — report it; the network is the person's to change. |
| `no key for <provider>` | (installer error) | The variable was not in the installer's environment: export missing, or `sudo` without `-E`. Back to [the key channel](#3-the-provider-key--never-through-the-conversation). |
| `aicc doctor` FAIL `provider_key` / `provider_session` | (doctor) | The key in `.env` is wrong, expired or out of quota: the person refreshes the variable or edits `.env`, then `cd <dir> && sudo docker compose up -d` and re-run doctor. Never ask for the key in chat. |
| an active firewall refuses the stack's ports | `PREFLIGHT_FIREWALL` | The fix line is the rules the stack needs. **Stop:** opening ports changes the person's firewall — present the rule set and let them run or approve it. |
| too few CPUs / memory, Colima forwarder or `--network-address` | `PREFLIGHT_RESOURCES`, `PREFLIGHT_COLIMA_PORT_FORWARDER`, `PREFLIGHT_COLIMA_NETWORK_ADDRESS` | The person's runtime settings to change; the fix line prints the restart command. |
| another `aicc` compose project or a leftover volume | `PREFLIGHT_STACK_COLLISION` | Another install lives here. **Stop:** never `down` or remove a stack or volume that is not this install's; report what holds the name. |

## When to stop and ask

- **Never change the person's network, firewall or another service's ports
  on your own.** A `fix:` line that touches them is a proposal to read back,
  not a command to run.
- **Never stop, kill or uninstall something that is not this install** — a
  process holding a port, another compose project, a volume left by an old
  one.
- **Never let the provider key through the conversation**, a command line, a
  file you write or a log — [the two channels](#3-the-provider-key--never-through-the-conversation)
  exist precisely so it never does.
- **Never `--purge` unprompted**, and never add `--yes` to it: that erases the
  database, the recordings and the install directory.
- **Do not loop a failure.** Run the documented fix once; if the same check
  fails again, report the FAIL line and what was tried — especially
  `provider_session`, whose retries spend real money on the provider.
- **Ask before Docker itself changes** (`--install-docker`, removing a snap,
  switching a rootless daemon back to rootful): it is the person's host, and
  other things may run on it.
