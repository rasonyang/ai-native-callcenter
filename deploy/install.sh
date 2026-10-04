#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# One-line installer for the AI-native call center.
#
#   Linux:  curl -fsSL <release>/install.sh | sudo sh -s -- --provider qwen
#   macOS:  curl -fsSL <release>/install.sh | sh -s -- --provider openai
#
# where <release> is https://github.com/rasonyang/ai-native-callcenter/
# releases/download/<tag>. It checks the host, downloads the release's deploy
# bundle and verifies its checksum, writes .env with generated secrets, starts
# the stack with the published images and runs `aicc doctor` inside it. A
# failed check stops it before anything is started or written. It sends
# nothing anywhere except the downloads it names.
#
# Run it again to repair or restart: it adds only what .env lacks, never
# regenerates a secret, and leaves unchanged containers alone. --help lists the
# rest. POSIX sh, so that `| sh` works wherever sh is dash, bash or busybox.

AICC_RELEASE='@RELEASE_TAG@'

REPO_URL='https://github.com/rasonyang/ai-native-callcenter'
EXTENSION_URL='https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp'
MIN_ENGINE='24.0.0'
MIN_COMPOSE='2.24.4'
MIN_COLIMA='0.9.0'
# 4 GiB as a runtime reports it: the VM's kernel keeps some for itself, so a
# 4 GiB Docker Desktop shows a little less in `docker info`.
MIN_MEM_BYTES=3972844748
MIN_CPUS=4

# Keys the installer owns in .env. Everything else in the file is the
# operator's and is never touched.
SECRET_KEYS='POSTGRES_PASSWORD ESL_PASSWORD LUA_PASSWORD AICC_SEED_PASSWORD'

# ---------------------------------------------------------------- output ----

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

pass() { printf 'PASS %s\n' "$*"; }

# preflight_fail TAG message [fix...]: a stable tag an operator can search for,
# what is wrong, and each command that fixes it. Nothing has been written or
# started when this runs.
preflight_fail() {
    printf 'FAIL %s: %s\n' "$1" "$2" >&2
    shift 2
    for _pf_fix in "$@"; do
        printf '  fix: %s\n' "$_pf_fix" >&2
    done
    say "Nothing was installed or started." >&2
    exit 1
}

usage() {
    cat <<'EOF'
Install, upgrade or remove the AI-native call center on this host.

Usage: install.sh [options]

  --provider P        the voice provider: openai, qwen, gateway, doubao,
                      gemini or none (none: the bot never answers; the human
                      path works)
  --external-ip IP    the address phones reach this host at (default: the
                      address of the default route's interface); on a rerun,
                      rewrites the address in .env
  --version TAG       the release to install (default: this script's release)
  --no-demo           do not seed the demo dataset (AICC_SEED empty)
  --upgrade           back up the database, then move an install to --version
  --uninstall         stop and remove the containers; keep data, .env, backups
  --purge             with --uninstall: also delete the data and the install
                      directory
  --check             run the preflight checks only; change nothing
  --install-docker    Linux: install Docker Engine with get.docker.com first
  --yes               never prompt: accept confirmations, fail on a missing value
  -h, --help          this text

The provider key comes from the environment (OPENAI_API_KEY, ALIYUN_API_KEY
for qwen, DOUBAO_API_KEY, GEMINI_API_KEY, REALTIME_API_KEY for gateway, which
also needs AICC_PROVIDER_ENDPOINT) or a prompt. With sudo, keep it with
`sudo -E`.

Installs into /opt/aicc on Linux (as root) and ~/.aicc on macOS (not as root).
EOF
}

# ----------------------------------------------------------------- input ----

# can_prompt: a terminal is there to ask and --yes was not given. Input is read
# from /dev/tty because under `curl | sh` stdin is the script.
can_prompt() {
    [ "$ASSUME_YES" = 0 ] || return 1
    (exec </dev/tty) 2>/dev/null
}

ask() {
    printf '%s' "$1" >/dev/tty
    REPLY=''
    IFS= read -r REPLY </dev/tty || true
}

ask_secret() {
    printf '%s' "$1" >/dev/tty
    REPLY=''
    stty -echo </dev/tty 2>/dev/null || true
    trap 'stty echo </dev/tty 2>/dev/null; exit 130' INT TERM
    IFS= read -r REPLY </dev/tty || true
    stty echo </dev/tty 2>/dev/null || true
    trap - INT TERM
    printf '\n' >/dev/tty
}

# confirm question hint: yes under --yes, the answer at a terminal, and a
# failure that names --yes without one.
confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    can_prompt || die "$1: no terminal to confirm at; rerun with --yes"
    ask "$1 [y/N] "
    case $REPLY in
    y | Y | yes | YES) return 0 ;;
    esac
    return 1
}

# value_ok: a value compose's .env parser and the stack take unchanged. No
# quoting, so no whitespace, quotes, $, backslash, backtick or #.
value_ok() {
    [ -n "$1" ] || return 1
    case $1 in
    *[[:space:]\'\"\$\\\`\#]*) return 1 ;;
    esac
    return 0
}

is_ipv4() {
    printf '%s\n' "$1" | awk -F. 'NF != 4 { exit 1 }
        { for (i = 1; i <= 4; i++) if ($i !~ /^[0-9]+$/ || $i > 255) exit 1 }'
}

# ---------------------------------------------------------- pure helpers ----
# These take their input as arguments and print their answer, so that
# install_test.sh can feed them fixtures.

# version_norm: v1.2.3-rc.1+x -> 1.2.3
version_norm() {
    printf '%s\n' "$1" | sed -e 's/^[vV]//' -e 's/[-+].*$//'
}

# version_ge A B: A >= B, numerically on up to three components; missing ones
# are 0, so 24 == 24.0.0.
version_ge() {
    awk -v a="$(version_norm "$1")" -v b="$(version_norm "$2")" 'BEGIN {
        na = split(a, x, "."); nb = split(b, y, ".")
        for (i = 1; i <= 3; i++) {
            p = (i <= na) ? x[i] + 0 : 0; q = (i <= nb) ? y[i] + 0 : 0
            if (p > q) exit 0
            if (p < q) exit 1
        }
        exit 0
    }'
}

# ip_excluded: addresses that are never the one phones reach: loopback,
# link-local, and 198.18.0.0/15, where TUN proxies (mihomo's Meta) live.
ip_excluded() {
    case $1 in
    127.* | 169.254.* | 198.18.* | 198.19.* | '') return 0 ;;
    esac
    return 1
}

# linux_pick_address ROUTES ADDRS: the host's LAN address from
# `ip -4 route show default table main` and `ip -4 -o addr show scope global`.
# Default routes in metric order; the route's src, else its device's first
# global address; anything ip_excluded is skipped. Never `ip route get`: a TUN
# proxy captures it and answers with its own address.
linux_pick_address() {
    printf '%s\n' "$1" | awk '$1 == "default" {
        dev = ""; src = ""; metric = 0
        for (i = 2; i < NF; i++) {
            if ($i == "dev") dev = $(i + 1)
            if ($i == "src") src = $(i + 1)
            if ($i == "metric") metric = $(i + 1) + 0
        }
        if (dev != "") print metric, NR, dev, (src == "" ? "-" : src)
    }' | sort -n -k1,1 -k2,2 | while read -r _lp_metric _lp_nr _lp_dev _lp_src; do
        if [ "$_lp_src" != "-" ] && ! ip_excluded "$_lp_src"; then
            printf '%s\n' "$_lp_src"
            continue
        fi
        printf '%s\n' "$2" | awk -v d="$_lp_dev" '$2 == d && $3 == "inet" {
            split($4, a, "/"); print a[1] }' | while read -r _lp_a; do
            ip_excluded "$_lp_a" || printf '%s\n' "$_lp_a"
        done
    done | head -n 1
}

# macos_candidate_ifaces ROUTE HWPORTS: interfaces to take the address of, in
# order, from `route -n get default` and `networksetup -listallhardwareports`.
# A default route through a tunnel (a VPN or a TUN proxy) is not the LAN, so
# the hardware ports follow it.
macos_candidate_ifaces() {
    _mc_if=$(printf '%s\n' "$1" | awk '$1 == "interface:" { print $2; exit }')
    case $_mc_if in
    '' | utun* | ipsec* | ppp* | gif* | stf* | tun* | tap*) ;;
    *) printf '%s\n' "$_mc_if" ;;
    esac
    printf '%s\n' "$2" | awk '$1 == "Device:" { print $2 }' | grep -vx "${_mc_if:-/}" || true
}

# yaml_top VALUE-OF KEY FILE: a top-level scalar from a flat YAML file such as
# colima.yaml, without quotes or a trailing comment.
yaml_top() {
    [ -f "$2" ] || return 0
    awk -v k="$1" 'index($0, k ":") == 1 {
        v = substr($0, length(k) + 2); sub(/[ \t]+#.*$/, "", v)
        gsub(/^[ \t]+|[ \t]+$/, "", v); print v; exit }' "$2" | tr -d "\"'"
}

# json_field KEY JSON: a string or number field of flat JSON, as colima prints.
json_field() {
    printf '%s\n' "$2" | tr -d '\n' |
        sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\{0,1\}\([^\",}]*\).*/\1/p"
}

# random_secret: 32 characters of [A-Za-z0-9]. The switch writes passwords
# into XML and the database URL quotes them, so the charset stays plain.
random_secret() {
    LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | dd bs=32 count=1 2>/dev/null
    printf '\n'
}

# linux_port_conflicts SS: host ports this stack binds that `ss -Hltnu`
# already shows in use, as proto/port.
linux_port_conflicts() {
    printf '%s\n' "$1" | awk -v tcp="$LINUX_TCP_PORTS" -v udp="$LINUX_UDP_PORTS" '
        BEGIN { nt = split(tcp, T, " "); nu = split(udp, U, " ") }
        NF >= 5 {
            p = $5; sub(/.*:/, "", p); p += 0
            if ($1 == "tcp") for (i = 1; i <= nt; i++) if (p == T[i] + 0) hit["tcp/" p] = 1
            if ($1 == "udp") for (i = 1; i <= nu; i++) {
                n = split(U[i], r, "-"); lo = r[1] + 0; hi = (n > 1) ? r[2] + 0 : lo
                if (p >= lo && p <= hi) hit["udp/" p] = 1
            }
        }
        END { for (k in hit) print k }' | sort
}

# ------------------------------------------------------------------ .env ----

env_has() { [ -f "$1" ] && grep -q "^$2=" "$1"; }

env_get() {
    [ -f "$1" ] || return 0
    sed -n "s/^$2=//p" "$1" | tail -n 1
}

# env_set FILE KEY VALUE: one line for KEY, in place of any it had; appended
# when there was none. The file stays 0600.
env_set() {
    _es_tmp="$1.tmp.$$"
    (
        umask 077
        [ -f "$1" ] || : >"$1"
        awk -v k="$2" -v v="$3" 'BEGIN { done = 0 }
            index($0, k "=") == 1 { if (!done) { print k "=" v; done = 1 }; next }
            { print }
            END { if (!done) print k "=" v }' "$1" >"$_es_tmp"
    ) && mv "$_es_tmp" "$1" && chmod 600 "$1"
}

env_add_missing() {
    env_has "$1" "$2" || env_set "$1" "$2" "$3"
}

# env_merge FILE: writes what this run decided into .env. A missing key is
# added; an existing one is kept, secrets above all. The addresses are
# rewritten only when REWRITE_ADDRESS=1 (an explicit --external-ip), the
# compose file list only when REWRITE_COMPOSE=1 (that, or an --upgrade, whose
# bundle may name other files). Reads OS,
# COMPOSE_FILE_WANT, EXTERNAL_IP, LOCAL_IP, SEED, PROVIDER, PROVIDER_KEY_VAR,
# PROVIDER_KEY, PROVIDER_ENDPOINT, RTP_START_WANT, RTP_END_WANT.
env_merge() {
    (
        umask 077
        [ -f "$1" ] || printf '# Written by install.sh. Secrets are generated once and never rewritten.\n' >"$1"
    )
    chmod 600 "$1"
    if [ "$REWRITE_COMPOSE" = 1 ] && [ "$(env_get "$1" COMPOSE_FILE)" != "$COMPOSE_FILE_WANT" ]; then
        env_set "$1" COMPOSE_FILE "$COMPOSE_FILE_WANT"
    fi
    if [ "$REWRITE_ADDRESS" = 1 ]; then
        [ "$(env_get "$1" FS_EXTERNAL_IP)" = "$EXTERNAL_IP" ] || env_set "$1" FS_EXTERNAL_IP "$EXTERNAL_IP"
        if [ "$OS" = linux ] && [ "$(env_get "$1" FS_LOCAL_IP)" != "$LOCAL_IP" ]; then
            env_set "$1" FS_LOCAL_IP "$LOCAL_IP"
        fi
    fi
    env_add_missing "$1" COMPOSE_FILE "$COMPOSE_FILE_WANT"
    env_add_missing "$1" FS_EXTERNAL_IP "$EXTERNAL_IP"
    if [ "$OS" = linux ]; then env_add_missing "$1" FS_LOCAL_IP "$LOCAL_IP"; fi
    for _em_key in $SECRET_KEYS; do
        if ! env_has "$1" "$_em_key"; then
            _em_secret=$(random_secret)
            [ "${#_em_secret}" = 32 ] || die "could not generate a secret from /dev/urandom"
            env_set "$1" "$_em_key" "$_em_secret"
        fi
    done
    env_add_missing "$1" AICC_SEED "$SEED"
    env_add_missing "$1" RTP_START "$RTP_START_WANT"
    env_add_missing "$1" RTP_END "$RTP_END_WANT"
    if [ "$OS" = linux ]; then env_add_missing "$1" PG_HOST_PORT 15432; fi
    if [ "$PROVIDER" = none ]; then
        env_add_missing "$1" AICC_BOT_ENABLED false
    else
        env_add_missing "$1" AICC_PROVIDER "$PROVIDER"
        # An empty key is as good as none, so it is filled like a missing one.
        [ -n "$(env_get "$1" "$PROVIDER_KEY_VAR")" ] || env_set "$1" "$PROVIDER_KEY_VAR" "$PROVIDER_KEY"
        if [ "$PROVIDER" = gateway ] && [ -z "$(env_get "$1" AICC_PROVIDER_ENDPOINT)" ]; then
            env_set "$1" AICC_PROVIDER_ENDPOINT "$PROVIDER_ENDPOINT"
        fi
    fi
    return 0
}

# ------------------------------------------------------------- arguments ----

parse_args() {
    PROVIDER_FLAG=''
    EXTERNAL_IP_FLAG=''
    VERSION_FLAG=''
    MODE=install
    PURGE=0
    ASSUME_YES=0
    INSTALL_DOCKER=0
    NO_DEMO=0
    CHECK_ONLY=0
    ORIG_ARGS="$*"
    while [ $# -gt 0 ]; do
        case $1 in
        --provider)
            [ $# -ge 2 ] || die "--provider needs a value"
            PROVIDER_FLAG=$2
            shift
            ;;
        --provider=*) PROVIDER_FLAG=${1#*=} ;;
        --external-ip)
            [ $# -ge 2 ] || die "--external-ip needs a value"
            EXTERNAL_IP_FLAG=$2
            shift
            ;;
        --external-ip=*) EXTERNAL_IP_FLAG=${1#*=} ;;
        --version)
            [ $# -ge 2 ] || die "--version needs a value"
            VERSION_FLAG=$2
            shift
            ;;
        --version=*) VERSION_FLAG=${1#*=} ;;
        --upgrade) MODE=upgrade ;;
        --uninstall) MODE=uninstall ;;
        --purge) PURGE=1 ;;
        --yes | -y) ASSUME_YES=1 ;;
        --install-docker) INSTALL_DOCKER=1 ;;
        --no-demo) NO_DEMO=1 ;;
        --check) CHECK_ONLY=1 ;;
        -h | --help)
            usage
            exit 0
            ;;
        *) die "unknown option: $1 (see --help)" ;;
        esac
        shift
    done
    case $PROVIDER_FLAG in
    '' | openai | qwen | gateway | doubao | gemini | none) ;;
    *) die "--provider must be openai, qwen, gateway, doubao, gemini or none, got '$PROVIDER_FLAG'" ;;
    esac
    if [ -n "$EXTERNAL_IP_FLAG" ] && ! is_ipv4 "$EXTERNAL_IP_FLAG"; then
        die "--external-ip must be an IPv4 address, got '$EXTERNAL_IP_FLAG'"
    fi
    [ "$PURGE" = 0 ] || [ "$MODE" = uninstall ] || die "--purge goes with --uninstall"
    if [ -n "$VERSION_FLAG" ] && ! value_ok "$VERSION_FLAG"; then
        die "--version: '$VERSION_FLAG' is not a tag"
    fi
    return 0
}

# ------------------------------------------------------------------ host ----

detect_platform() {
    case $(uname -s) in
    Linux) OS=linux ;;
    Darwin) OS=macos ;;
    *) die "unsupported operating system: $(uname -s) (Linux and macOS are supported)" ;;
    esac
    case $(uname -m) in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m) (amd64 and arm64 are supported)" ;;
    esac
    if [ "$OS" = linux ]; then
        DIR=/opt/aicc
        COMPOSE_FILE_WANT=docker-compose.yml:compose.release.yml:compose.linux.yml
        RTP_START_WANT=16384
        RTP_END_WANT=29999
    else
        DIR="$HOME/.aicc"
        COMPOSE_FILE_WANT=docker-compose.yml:compose.release.yml:compose.macos.yml
        RTP_START_WANT=16384
        RTP_END_WANT=16583
    fi
    ENV_FILE="$DIR/.env"
}

script_url() {
    case $AICC_RELEASE in
    @*) printf '%s/releases/latest/download/install.sh' "$REPO_URL" ;;
    *) printf '%s/releases/download/%s/install.sh' "$REPO_URL" "$AICC_RELEASE" ;;
    esac
}

check_privileges() {
    if [ "$OS" = linux ] && [ "$(id -u)" != 0 ]; then
        die "installing into $DIR needs root; rerun with: curl -fsSL $(script_url) | sudo sh -s -- $ORIG_ARGS"
    fi
    if [ "$OS" = macos ] && [ "$(id -u)" = 0 ]; then
        die "on macOS run this as yourself, not as root: it installs into your home directory (~/.aicc)"
    fi
    return 0
}

# resolve_tag: the release this run installs, and what is installed now.
resolve_tag() {
    INSTALLED=''
    [ -f "$DIR/VERSION" ] && INSTALLED=$(head -n 1 "$DIR/VERSION")
    TAG=$VERSION_FLAG
    if [ -z "$TAG" ]; then
        case $AICC_RELEASE in
        @*) TAG=$INSTALLED ;;
        *) TAG=$AICC_RELEASE ;;
        esac
    fi
    [ -n "$TAG" ] || die "this script is not stamped with a release: pass --version <tag>"
    if [ "$MODE" = upgrade ]; then
        [ -n "$INSTALLED" ] || die "--upgrade needs an existing install; $DIR/VERSION is missing"
        [ "$TAG" != "$INSTALLED" ] || die "$INSTALLED is already installed; rerun without --upgrade to repair it"
        version_ge "$TAG" "$INSTALLED" ||
            die "refusing to downgrade from $INSTALLED to $TAG: the database has migrated past what $TAG knows"
    elif [ -n "$INSTALLED" ] && [ "$TAG" != "$INSTALLED" ]; then
        die "$INSTALLED is installed in $DIR; to move to $TAG rerun with --upgrade --version $TAG"
    fi
    return 0
}

# ------------------------------------------------------------- preflight ----

compose_version() {
    docker compose version 2>/dev/null | head -n 1 |
        sed -n 's/.*[Vv]ersion v\{0,1\}\([0-9][0-9.]*\).*/\1/p'
}

install_docker() {
    [ "$OS" = linux ] || die "--install-docker is for Linux; on macOS install Docker Desktop or Colima yourself"
    command -v curl >/dev/null 2>&1 || die "--install-docker needs curl"
    mkdir -p "$DIR"
    curl -fsSL https://get.docker.com -o "$DIR/get-docker.sh" || die "could not download https://get.docker.com"
    say "Docker's install script is at $DIR/get-docker.sh; read it before it runs."
    confirm "Run $DIR/get-docker.sh as root to install Docker Engine?" || die "Docker Engine was not installed"
    sh "$DIR/get-docker.sh" || die "get-docker.sh failed"
}

docker_down_fix() {
    if [ "$OS" = linux ]; then
        printf 'sudo systemctl start docker'
        return
    fi
    _dd_ep=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
    case ${DOCKER_HOST:-$_dd_ep} in
    *colima*) printf 'colima start --port-forwarder grpc --cpu 4 --memory 4' ;;
    *) printf 'open Docker Desktop (open -a Docker) and wait until it says it is running' ;;
    esac
}

preflight_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        if [ "$INSTALL_DOCKER" = 1 ]; then
            install_docker
        elif [ "$OS" = linux ]; then
            preflight_fail PREFLIGHT_DOCKER_MISSING "docker is not installed" \
                "install Docker Engine (https://docs.docker.com/engine/install/), or rerun with --install-docker"
        else
            preflight_fail PREFLIGHT_DOCKER_MISSING "docker is not installed" \
                "install Docker Desktop (https://docs.docker.com/desktop/setup/install/mac-install/)" \
                "or Colima: brew install colima docker docker-compose && colima start --port-forwarder grpc --cpu 4 --memory 4"
        fi
    fi
    if [ "$OS" = linux ] && [ "$(command -v docker)" = /snap/bin/docker ]; then
        preflight_fail PREFLIGHT_DOCKER_SNAP "docker is the snap package, whose confinement cannot mount files from $DIR" \
            "sudo snap remove docker, then install Docker Engine from https://docs.docker.com/engine/install/"
    fi
    if ! ENGINE=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || [ -z "$ENGINE" ]; then
        preflight_fail PREFLIGHT_DOCKER_DAEMON_DOWN "the docker daemon is not reachable" "$(docker_down_fix)"
    fi
    version_ge "$ENGINE" "$MIN_ENGINE" ||
        preflight_fail PREFLIGHT_DOCKER_TOO_OLD "Docker Engine $ENGINE is older than $MIN_ENGINE" \
            "upgrade Docker (https://docs.docker.com/engine/install/)"
    pass "docker engine $ENGINE"
    COMPOSE=$(compose_version)
    [ -n "$COMPOSE" ] ||
        preflight_fail PREFLIGHT_COMPOSE_MISSING "the docker compose plugin is not installed" \
            "install docker-compose-plugin (https://docs.docker.com/compose/install/linux/)"
    version_ge "$COMPOSE" "$MIN_COMPOSE" ||
        preflight_fail PREFLIGHT_COMPOSE_TOO_OLD "Docker Compose $COMPOSE is older than $MIN_COMPOSE (needed for !reset)" \
            "upgrade the compose plugin (https://docs.docker.com/compose/install/linux/)"
    pass "docker compose $COMPOSE"
    DOCKER_NAME=$(docker info --format '{{.Name}}' 2>/dev/null || true)
    DOCKER_OSNAME=$(docker info --format '{{.OperatingSystem}}' 2>/dev/null || true)
    if [ "$OS" = linux ]; then
        case $(docker info --format '{{json .SecurityOptions}}' 2>/dev/null) in
        *rootless*)
            preflight_fail PREFLIGHT_DOCKER_ROOTLESS "docker runs rootless: host networking would not bind this host's addresses and SIP needs ports below 1024 on no-one's behalf" \
                "use the rootful daemon: unset DOCKER_HOST and sudo systemctl start docker"
            ;;
        esac
        case $(docker info --format '{{.DockerRootDir}}' 2>/dev/null) in
        */snap/*)
            preflight_fail PREFLIGHT_DOCKER_SNAP "docker is the snap package, whose confinement cannot mount files from $DIR" \
                "sudo snap remove docker, then install Docker Engine from https://docs.docker.com/engine/install/"
            ;;
        esac
    fi
    return 0
}

preflight_macos_runtime() {
    case $DOCKER_OSNAME in
    *"Docker Desktop"*)
            pass "runtime Docker Desktop"
        _pm_cpus=$(docker info --format '{{.NCPU}}' 2>/dev/null || echo 0)
        _pm_mem=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
        if [ "$_pm_cpus" -lt "$MIN_CPUS" ] || [ "$_pm_mem" -lt "$MIN_MEM_BYTES" ]; then
            preflight_fail PREFLIGHT_RESOURCES "Docker Desktop has $_pm_cpus CPUs and $((_pm_mem / 1048576)) MiB; the stack needs 4 CPUs and 4 GiB" \
                "Docker Desktop > Settings > Resources: CPU limit 4 or more, Memory limit 4 GB or more, then Apply & restart"
        fi
        pass "resources $_pm_cpus CPUs, $((_pm_mem / 1048576)) MiB"
        return 0
        ;;
    esac
    case $DOCKER_NAME in
    colima) COLIMA_PROFILE=default ;;
    colima-*) COLIMA_PROFILE=${DOCKER_NAME#colima-} ;;
    *)
        preflight_fail PREFLIGHT_RUNTIME_UNSUPPORTED "the container runtime '$DOCKER_OSNAME' ($DOCKER_NAME) is not supported on macOS; Docker Desktop and Colima are" \
            "switch docker to Docker Desktop or Colima (OrbStack, Podman and Rancher Desktop are not supported)"
        ;;
    esac
    if [ "$COLIMA_PROFILE" = default ]; then _pm_p=''; else _pm_p=" -p $COLIMA_PROFILE"; fi
    command -v colima >/dev/null 2>&1 ||
        preflight_fail PREFLIGHT_COLIMA_CLI_MISSING "docker runs on Colima but the colima command is not on PATH" "brew install colima"
    _pm_ver=$(colima version 2>/dev/null | awk 'NR == 1 { print $3 }')
    version_ge "${_pm_ver:-0}" "$MIN_COLIMA" ||
        preflight_fail PREFLIGHT_COLIMA_TOO_OLD "Colima ${_pm_ver:-unknown} is older than $MIN_COLIMA" "brew upgrade colima"
    pass "runtime Colima $_pm_ver (profile $COLIMA_PROFILE)"
    _pm_yaml="${COLIMA_HOME:-$HOME/.colima}/$COLIMA_PROFILE/colima.yaml"
    if [ "$COLIMA_PROFILE" = default ]; then
        _pm_status=$(colima status --json 2>/dev/null || true)
    else
        _pm_status=$(colima status -p "$COLIMA_PROFILE" --json 2>/dev/null || true)
    fi
    _pm_cpus=$(json_field cpu "$_pm_status")
    _pm_mem=$(json_field memory "$_pm_status")
    [ -n "$_pm_cpus" ] || _pm_cpus=$(yaml_top cpu "$_pm_yaml")
    if [ -z "$_pm_mem" ]; then
        _pm_mem=$(yaml_top memory "$_pm_yaml")
        if [ -n "$_pm_mem" ]; then _pm_mem=$((_pm_mem * 1073741824)); fi
    fi
    _pm_cpus=${_pm_cpus:-0}
    _pm_mem=${_pm_mem:-0}
    # The fix keeps whichever of the user's values is already larger.
    _pm_fcpu=$MIN_CPUS
    if [ "$_pm_cpus" -gt "$_pm_fcpu" ]; then _pm_fcpu=$_pm_cpus; fi
    _pm_fmem=4
    if [ "$((_pm_mem / 1073741824))" -gt "$_pm_fmem" ]; then _pm_fmem=$((_pm_mem / 1073741824)); fi
    _pm_restart="colima stop$_pm_p && colima start$_pm_p --port-forwarder grpc --cpu $_pm_fcpu --memory $_pm_fmem"
    _pm_fwd=$(yaml_top portForwarder "$_pm_yaml")
    [ "$_pm_fwd" = grpc ] ||
        preflight_fail PREFLIGHT_COLIMA_PORT_FORWARDER "Colima forwards ports with '${_pm_fwd:-ssh}' ($_pm_yaml): it does not forward UDP, so SIP and RTP never reach the switch" \
            "$_pm_restart"
    pass "colima port forwarder grpc"
    if [ -n "$(json_field address "$_pm_status")" ]; then
        preflight_fail PREFLIGHT_COLIMA_NETWORK_ADDRESS "Colima was started with --network-address, which this stack does not support" \
            "colima stop$_pm_p && colima start$_pm_p --network-address=false --port-forwarder grpc --cpu $_pm_fcpu --memory $_pm_fmem"
    fi
    if [ "$_pm_cpus" -lt "$MIN_CPUS" ] || [ "$_pm_mem" -lt "$MIN_MEM_BYTES" ]; then
        preflight_fail PREFLIGHT_RESOURCES "Colima has $_pm_cpus CPUs and $((_pm_mem / 1048576)) MiB; the stack needs 4 CPUs and 4 GiB" \
            "$_pm_restart"
    fi
    pass "resources $_pm_cpus CPUs, $((_pm_mem / 1048576)) MiB"
}

# image_arch_ok IMAGE: the image runs natively here. A local image (an
# override built on this host, or one already pulled) is judged by itself;
# otherwise the registry's manifest list must name this architecture. Never
# emulated: an amd64 switch under Rosetta or QEMU is not something to debug a
# call on.
image_arch_ok() {
    if _ia_have=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$1" 2>/dev/null); then
        [ "$_ia_have" = "linux/$ARCH" ] ||
            preflight_fail PREFLIGHT_IMAGE_ARCH "$1 on this host is $_ia_have, not linux/$ARCH" \
                "docker image rm $1 (it is pulled again for linux/$ARCH)"
        pass "image $1 (local, linux/$ARCH)"
        return 0
    fi
    _ia_manifest=$(docker manifest inspect "$1" 2>/dev/null) ||
        preflight_fail PREFLIGHT_IMAGE_UNAVAILABLE "cannot read the manifest of $1 from its registry" \
            "check the tag and this host's access to Docker Hub (docker manifest inspect $1)"
    if printf '%s\n' "$_ia_manifest" | tr -d ' \t\n' | grep -q "\"architecture\":\"$ARCH\""; then
        pass "image $1 (linux/$ARCH)"
        return 0
    fi
    preflight_fail PREFLIGHT_IMAGE_ARCH "$1 is not published for linux/$ARCH" \
        "run on an amd64 or arm64 host the release is built for"
}

preflight_images() {
    APP_IMAGE=${AICC_IMAGE:-$(env_get "$ENV_FILE" AICC_IMAGE)}
    APP_IMAGE=${APP_IMAGE:-rasonyang/ai-native-callcenter:$TAG}
    FS_IMAGE=${AICC_FS_IMAGE:-$(env_get "$ENV_FILE" AICC_FS_IMAGE)}
    FS_IMAGE=${FS_IMAGE:-rasonyang/freeswitch-aicc:$TAG}
    for _pi_image in "$APP_IMAGE" "$FS_IMAGE" postgres:18; do
        image_arch_ok "$_pi_image"
    done
}

# preflight_collision: another stack named aicc would be taken over by ours:
# compose keys containers and volumes on the project name, not the directory.
preflight_collision() {
    _pc_other=$(docker ps -a --filter label=com.docker.compose.project=aicc \
        --format '{{.Label "com.docker.compose.project.working_dir"}}' 2>/dev/null |
        sort -u | grep -vx "$DIR" || true)
    if [ -n "$_pc_other" ]; then
        preflight_fail PREFLIGHT_STACK_COLLISION "a compose project named aicc already exists here, from $_pc_other; installing would take over its containers and its database" \
            "stop it there: cd $_pc_other && docker compose down (add -v only if its data can go)"
    fi
    if docker volume inspect aicc_postgres-data >/dev/null 2>&1 && [ ! -f "$ENV_FILE" ]; then
        preflight_fail PREFLIGHT_STACK_COLLISION "the volume aicc_postgres-data exists but $DIR holds no install: it belongs to another aicc stack, and its database has other passwords" \
            "if its data can go: docker volume rm aicc_postgres-data; otherwise remove that stack first"
    fi
    pass "no other aicc stack"
}

our_stack_running() {
    [ -n "$(docker ps --filter label=com.docker.compose.project=aicc \
        --filter "label=com.docker.compose.project.working_dir=$DIR" -q 2>/dev/null)" ]
}

preflight_ports() {
    if our_stack_running; then
        pass "ports (this install is running; not checked)"
        return 0
    fi
    _pp_http=$(env_get "$ENV_FILE" HTTP_PORT)
    _pp_http=${_pp_http:-8080}
    _pp_rs=$(env_get "$ENV_FILE" RTP_START)
    _pp_re=$(env_get "$ENV_FILE" RTP_END)
    _pp_rs=${_pp_rs:-$RTP_START_WANT}
    _pp_re=${_pp_re:-$RTP_END_WANT}
    if [ "$OS" = linux ]; then
        _pp_pg=$(env_get "$ENV_FILE" PG_HOST_PORT)
        LINUX_TCP_PORTS="$_pp_http 5060 5066 5080 7443 8090 9090 18021 ${_pp_pg:-15432}"
        LINUX_UDP_PORTS="5060 5080 6060 $_pp_rs-$_pp_re 30000-30999"
        command -v ss >/dev/null 2>&1 || die "ss (iproute2) is needed to check ports"
        _pp_busy=$(linux_port_conflicts "$(ss -Hltnu 2>/dev/null)")
        _pp_ports=$(printf '%s\n' "$_pp_busy" | sed 's|.*/||' | paste -s -d'|' -)
        _pp_busy=$(printf '%s\n' "$_pp_busy" | paste -s -d' ' -)
        _pp_fix="stop what holds them (sudo ss -Hltnup | grep -E ':($_pp_ports)[[:space:]]'), then rerun"
    else
        _pp_busy=''
        for _pp_spec in "TCP:$_pp_http" TCP:5060 TCP:5066 UDP:5060 "UDP:$_pp_rs-$_pp_re"; do
            if [ -n "$(lsof -nP -i"$_pp_spec" -t 2>/dev/null)" ]; then
                _pp_busy="$_pp_busy $_pp_spec"
            fi
        done
        _pp_busy=${_pp_busy# }
        _pp_fix="stop what holds them (lsof -nP -i<one of the above>), then rerun"
    fi
    if [ -n "$_pp_busy" ]; then
        preflight_fail PREFLIGHT_PORT_IN_USE "ports this stack binds are in use: $_pp_busy" "$_pp_fix"
    fi
    pass "ports free"
}

# preflight_firewall: with host networking the switch's ports are the host's,
# so an active ufw or firewalld drops phones' SIP and media unless it allows
# them. Owner decision: this is not skippable, --yes included.
FIREWALL_RULES='8080/tcp 5060/tcp 5060/udp 5066/tcp 5080/tcp 5080/udp 7443/tcp'
preflight_firewall() {
    [ "$OS" = linux ] || return 0
    _pf_rtp="$(env_get "$ENV_FILE" RTP_START)"
    _pf_rtp_end="$(env_get "$ENV_FILE" RTP_END)"
    _pf_rtp=${_pf_rtp:-$RTP_START_WANT}
    _pf_rtp_end=${_pf_rtp_end:-$RTP_END_WANT}
    _pf_missing=''
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | head -n 1 | grep -q 'Status: active'; then
        _pf_status=$(ufw status 2>/dev/null)
        for _pf_rule in $FIREWALL_RULES "$_pf_rtp:$_pf_rtp_end/udp"; do
            _pf_port=${_pf_rule%/*}
            if ! printf '%s\n' "$_pf_status" | grep -Eq "^($_pf_rule|$_pf_port)[[:space:]]+ALLOW"; then
                _pf_missing="$_pf_missing ufw allow $_pf_rule;"
            fi
        done
        [ -z "$_pf_missing" ] ||
            preflight_fail PREFLIGHT_FIREWALL "ufw is active and does not allow the stack's ports" "sudo sh -c '$_pf_missing'"
        pass "firewall (ufw allows the stack's ports)"
    fi
    if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
        for _pf_rule in $FIREWALL_RULES "$_pf_rtp-$_pf_rtp_end/udp"; do
            firewall-cmd --query-port="$_pf_rule" >/dev/null 2>&1 ||
                _pf_missing="$_pf_missing --add-port=$_pf_rule"
        done
        [ -z "$_pf_missing" ] ||
            preflight_fail PREFLIGHT_FIREWALL "firewalld is active and does not allow the stack's ports" \
                "sudo firewall-cmd --permanent$_pf_missing && sudo firewall-cmd --reload"
        pass "firewall (firewalld allows the stack's ports)"
    fi
    return 0
}

infer_address() {
    if [ "$OS" = linux ]; then
        linux_pick_address "$(ip -4 route show default table main 2>/dev/null)" \
            "$(ip -4 -o addr show scope global 2>/dev/null)"
        return 0
    fi
    for _ia_if in $(macos_candidate_ifaces "$(route -n get default 2>/dev/null)" \
        "$(networksetup -listallhardwareports 2>/dev/null)"); do
        _ia_addr=$(ipconfig getifaddr "$_ia_if" 2>/dev/null || true)
        if [ -n "$_ia_addr" ] && ! ip_excluded "$_ia_addr"; then
            printf '%s\n' "$_ia_addr"
            return 0
        fi
    done
}

host_addrs() {
    if [ "$OS" = linux ]; then
        ip -o addr show 2>/dev/null | awk '{ split($4, a, "/"); print a[1] }'
    else
        ifconfig 2>/dev/null | awk '$1 == "inet" || $1 == "inet6" { print $2 }' | sed 's/%.*//'
    fi | paste -s -d, -
}

# resolve_address: what goes into FS_EXTERNAL_IP (and FS_LOCAL_IP on Linux).
# An explicit --external-ip wins and is written; otherwise .env's value stands
# on a rerun even when the host's address has moved (doctor then says so), and
# a first install takes the inferred one.
resolve_address() {
    INFERRED=$(infer_address)
    REWRITE_ADDRESS=0
    REWRITE_COMPOSE=0
    if [ "$MODE" = upgrade ]; then REWRITE_COMPOSE=1; fi
    if [ -n "$EXTERNAL_IP_FLAG" ]; then
        EXTERNAL_IP=$EXTERNAL_IP_FLAG
        REWRITE_ADDRESS=1
        REWRITE_COMPOSE=1
    elif env_has "$ENV_FILE" FS_EXTERNAL_IP; then
        EXTERNAL_IP=$(env_get "$ENV_FILE" FS_EXTERNAL_IP)
        if [ -n "$INFERRED" ] && [ "$INFERRED" != "$EXTERNAL_IP" ]; then
            warn "this host's address looks like $INFERRED but .env says $EXTERNAL_IP; leaving .env alone (rerun with --external-ip $INFERRED to change it)"
        fi
    else
        EXTERNAL_IP=$INFERRED
    fi
    if [ -z "$EXTERNAL_IP" ]; then
        if can_prompt; then
            ask "The address phones reach this host at: "
            EXTERNAL_IP=$REPLY
        fi
        [ -n "$EXTERNAL_IP" ] || die "could not work out this host's address; pass --external-ip <ip>"
    fi
    is_ipv4 "$EXTERNAL_IP" || die "'$EXTERNAL_IP' is not an IPv4 address; pass --external-ip <ip>"
    # The switch and the bot bind the LAN address; phones may reach the host
    # at another (a NAT's), which is what --external-ip is for.
    LOCAL_IP=${INFERRED:-$EXTERNAL_IP}
    if [ "$OS" = linux ] && [ "$REWRITE_ADDRESS" = 0 ] && env_has "$ENV_FILE" FS_LOCAL_IP; then
        LOCAL_IP=$(env_get "$ENV_FILE" FS_LOCAL_IP)
    fi
    pass "address $EXTERNAL_IP${INFERRED:+ (inferred $INFERRED)}"
}

preflight() {
    for _pr_tool in tar awk sed; do
        command -v "$_pr_tool" >/dev/null 2>&1 || preflight_fail PREFLIGHT_TOOL_MISSING "$_pr_tool is not installed" "install $_pr_tool"
    done
    command -v curl >/dev/null 2>&1 || [ -n "${AICC_INSTALL_BUNDLE_DIR:-}" ] ||
        preflight_fail PREFLIGHT_TOOL_MISSING "curl is not installed" "install curl"
    command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 ||
        preflight_fail PREFLIGHT_TOOL_MISSING "neither sha256sum nor shasum is installed" "install coreutils"
    preflight_docker
    if [ "$OS" = macos ]; then preflight_macos_runtime; fi
    preflight_images
    preflight_collision
    preflight_ports
    preflight_firewall
    resolve_address
}

# ---------------------------------------------------------------- inputs ----

provider_key_var() {
    case $1 in
    openai) printf OPENAI_API_KEY ;;
    qwen) printf ALIYUN_API_KEY ;;
    gateway) printf REALTIME_API_KEY ;;
    doubao) printf DOUBAO_API_KEY ;;
    gemini) printf GEMINI_API_KEY ;;
    esac
}

# resolve_provider: the provider this install runs, and its key. An install
# keeps the provider its .env names; only a first install chooses.
resolve_provider() {
    _rp_have=''
    if [ "$(env_get "$ENV_FILE" AICC_BOT_ENABLED)" = false ]; then
        _rp_have=none
    elif env_has "$ENV_FILE" AICC_PROVIDER; then
        _rp_have=$(env_get "$ENV_FILE" AICC_PROVIDER)
    fi
    if [ -n "$_rp_have" ]; then
        PROVIDER=$_rp_have
        if [ -n "$PROVIDER_FLAG" ] && [ "$PROVIDER_FLAG" != "$PROVIDER" ]; then
            warn "$ENV_FILE already names provider $PROVIDER; keeping it (edit .env to change it)"
        fi
    else
        PROVIDER=$PROVIDER_FLAG
        if [ -z "$PROVIDER" ] && can_prompt; then
            ask "Voice provider (openai, qwen, gateway, doubao, gemini, none): "
            PROVIDER=$REPLY
        fi
        case $PROVIDER in
        openai | qwen | gateway | doubao | gemini | none) ;;
        '') die "no provider chosen: pass --provider openai|qwen|gateway|doubao|gemini|none" ;;
        *) die "unknown provider '$PROVIDER': pass --provider openai|qwen|gateway|doubao|gemini|none" ;;
        esac
    fi
    PROVIDER_KEY_VAR=$(provider_key_var "$PROVIDER")
    PROVIDER_KEY=''
    PROVIDER_ENDPOINT=''
    [ "$PROVIDER" = none ] && return 0
    [ -n "$PROVIDER_KEY_VAR" ] || die "$ENV_FILE names provider '$PROVIDER', which this installer does not manage"
    if [ -z "$(env_get "$ENV_FILE" "$PROVIDER_KEY_VAR")" ]; then
        eval "PROVIDER_KEY=\${$PROVIDER_KEY_VAR:-}"
        if [ -z "$PROVIDER_KEY" ] && can_prompt; then
            ask_secret "$PROVIDER_KEY_VAR (input hidden): "
            PROVIDER_KEY=$REPLY
        fi
        [ -n "$PROVIDER_KEY" ] ||
            die "no key for $PROVIDER: export $PROVIDER_KEY_VAR (with sudo, use sudo -E) or run at a terminal"
        value_ok "$PROVIDER_KEY" || die "$PROVIDER_KEY_VAR holds whitespace, a quote, \$, \\, \` or #, which .env cannot carry unquoted"
    fi
    if [ "$PROVIDER" = gateway ] && [ -z "$(env_get "$ENV_FILE" AICC_PROVIDER_ENDPOINT)" ]; then
        PROVIDER_ENDPOINT=${AICC_PROVIDER_ENDPOINT:-}
        if [ -z "$PROVIDER_ENDPOINT" ] && can_prompt; then
            ask "The gateway's Realtime URL (ws:// or wss://): "
            PROVIDER_ENDPOINT=$REPLY
        fi
        case $PROVIDER_ENDPOINT in
        ws://* | wss://*) value_ok "$PROVIDER_ENDPOINT" || die "AICC_PROVIDER_ENDPOINT cannot be carried in .env unquoted" ;;
        *) die "gateway needs its URL: export AICC_PROVIDER_ENDPOINT=ws://... or wss://..." ;;
        esac
    fi
    return 0
}

# ---------------------------------------------------------------- bundle ----

sha256_check() {
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$1" && sha256sum -c "$2")
    else
        (cd "$1" && shasum -a 256 -c "$2")
    fi
}

fetch() {
    if [ -n "${AICC_INSTALL_BUNDLE_DIR:-}" ]; then
        cp "$AICC_INSTALL_BUNDLE_DIR/$1" "$2"
    else
        curl -fsSL --retry 3 -o "$2" "$BUNDLE_BASE/$1"
    fi
}

# fetch_bundle: downloads the tag's bundle and its checksums into a temporary
# directory and verifies the bundle before anything is extracted.
fetch_bundle() {
    BUNDLE_BASE=${AICC_INSTALL_BUNDLE_URL_BASE:-$REPO_URL/releases/download/$TAG}
    BUNDLE="aicc-deploy-$TAG.tar.gz"
    WORK=$(mktemp -d "${TMPDIR:-/tmp}/aicc-install.XXXXXX") || die "mktemp failed"
    trap 'rm -rf "$WORK"' EXIT
    say "Downloading $BUNDLE from ${AICC_INSTALL_BUNDLE_DIR:-$BUNDLE_BASE}"
    fetch checksums.txt "$WORK/checksums.txt" || die "could not download checksums.txt"
    fetch "$BUNDLE" "$WORK/$BUNDLE" || die "could not download $BUNDLE"
    grep "[[:space:]]\*\{0,1\}$BUNDLE\$" "$WORK/checksums.txt" >"$WORK/bundle.sha256" ||
        die "checksums.txt does not list $BUNDLE"
    sha256_check "$WORK" bundle.sha256 >/dev/null 2>&1 ||
        die "$BUNDLE does not match its checksum; nothing was extracted"
    say "Checksum verified."
    mkdir -p "$WORK/stage"
    tar -xzf "$WORK/$BUNDLE" -C "$WORK/stage" || die "could not extract $BUNDLE"
    if [ ! -f "$WORK/stage/docker-compose.yml" ] || [ ! -f "$WORK/stage/compose.release.yml" ]; then
        die "$BUNDLE is not a deploy bundle"
    fi
}

# sync_tree SRC DST: copies what differs, in place, and removes files the new
# bundle dropped from the directories it owns. In place, because the running
# containers bind-mount these files and directories: a replaced inode would
# leave them looking at the old one.
sync_tree() {
    (cd "$1" && find . -type f) | while IFS= read -r _st_f; do
        _st_f=${_st_f#./}
        mkdir -p "$2/$(dirname "$_st_f")"
        cmp -s "$1/$_st_f" "$2/$_st_f" || cp "$1/$_st_f" "$2/$_st_f"
    done
    for _st_d in postgres sql freeswitch; do
        [ -d "$2/$_st_d" ] || continue
        (cd "$2" && find "$_st_d" -type f) | while IFS= read -r _st_f; do
            [ -e "$1/$_st_f" ] || rm -f "$2/$_st_f"
        done
    done
}

install_bundle() {
    mkdir -p "$DIR"
    chmod 755 "$DIR"
    sync_tree "$WORK/stage" "$DIR"
    printf '%s\n' "$TAG" >"$DIR/VERSION"
}

# ---------------------------------------------------------------- compose ----

# dc: docker compose in the install directory, with .env deciding. The shell's
# own values for the keys the installer owns would otherwise win over .env.
dc() {
    (
        unset COMPOSE_FILE COMPOSE_PROJECT_NAME FS_EXTERNAL_IP FS_LOCAL_IP AICC_SEED \
            POSTGRES_PASSWORD ESL_PASSWORD LUA_PASSWORD AICC_SEED_PASSWORD \
            RTP_START RTP_END PG_HOST_PORT AICC_PROVIDER
        cd "$DIR" && docker compose "$@"
    )
}

logs_hint() {
    say "Logs: cd $DIR && docker compose logs --tail 100 aicc freeswitch lua-role postgres" >&2
}

backup_database() {
    dc ps --status running --services 2>/dev/null | grep -qx postgres ||
        die "--upgrade backs up the database first, but its postgres is not running: start it with cd $DIR && docker compose up -d"
    mkdir -p "$DIR/backups"
    chmod 700 "$DIR/backups"
    _bd_file="$DIR/backups/aicc-$INSTALLED-$(date -u +%Y%m%dT%H%M%SZ).dump"
    say "Backing up the database to $_bd_file"
    if ! dc exec -T postgres pg_dump -U aicc -Fc aicc >"$_bd_file" || [ ! -s "$_bd_file" ]; then
        rm -f "$_bd_file"
        die "the database backup failed; nothing was upgraded"
    fi
    chmod 600 "$_bd_file"
}

compose_up() {
    say "Pulling images"
    # missing: the tags are exact and never move, so what is here is right.
    dc pull --policy missing --quiet || {
        logs_hint
        die "docker compose pull failed"
    }
    say "Starting the stack"
    dc up -d --no-build --remove-orphans || {
        logs_hint
        die "docker compose up failed"
    }
    _cu_n=0
    until dc ps --status running --services 2>/dev/null | grep -qx aicc; do
        _cu_n=$((_cu_n + 1))
        if [ "$_cu_n" -ge 60 ]; then
            logs_hint
            die "the aicc container is not running"
        fi
        sleep 2
    done
}

run_doctor() {
    set -- doctor --wait 5m --host-addrs "$(host_addrs)"
    if [ "$SKIP_PROVIDER" = 1 ]; then set -- "$@" --skip-provider; fi
    say "Running aicc doctor (waits up to 5 minutes for the app and the switch)"
    if ! dc exec -T aicc aicc "$@"; then
        say "" >&2
        say "aicc doctor reported a failure above; each FAIL line is followed by its fix." >&2
        logs_hint
        exit 1
    fi
}

# ---------------------------------------------------------------- modes ----

do_uninstall() {
    [ -f "$DIR/docker-compose.yml" ] || die "no install in $DIR"
    if [ "$PURGE" = 1 ]; then
        confirm "Delete the database, the recordings and $DIR for good?" || die "nothing was removed"
        dc down -v --remove-orphans || die "docker compose down -v failed"
        rm -rf "$DIR"
        say "Removed the stack, its volumes and $DIR."
    else
        dc down --remove-orphans || die "docker compose down failed"
        say "Stopped and removed the containers. The data volumes, $ENV_FILE and $DIR/backups are kept."
        say "Start it again by rerunning the installer; remove everything with --uninstall --purge."
    fi
}

final_output() {
    _fo_seed=$(env_get "$ENV_FILE" AICC_SEED)
    _fo_dc='docker compose'
    [ "$OS" = linux ] && _fo_dc='sudo docker compose'
    say ""
    say "The call center is running."
    say ""
    say "  Web:        http://$EXTERNAL_IP:$(env_get "$ENV_FILE" HTTP_PORT | grep . || echo 8080)"
    if [ "$_fo_seed" = demo ]; then
        if [ "$FIRST_INSTALL" = 1 ]; then
            say "  Sign in:    admin / $(env_get "$ENV_FILE" AICC_SEED_PASSWORD)"
            say "              (also the password of the demo's agents; kept in $ENV_FILE)"
        else
            say "  Sign in:    admin; the password is AICC_SEED_PASSWORD in $ENV_FILE"
        fi
    else
        say "  Sign in:    no demo accounts were seeded; create the first one:"
        say "              cd $DIR && $_fo_dc exec aicc aicc useradd -username admin -password '<password>' -role ADMIN"
    fi
    say "  Phone:      install the browser phone, $EXTENSION_URL"
    say "              and add $EXTERNAL_IP in the extension's Allow Sites"
    if [ "$_fo_seed" = demo ]; then
        say ""
        say "Next: sign in as an agent, then dial 95001 (English) / 95002 (Chinese)."
    fi
    if [ "$PROVIDER" = none ]; then
        say "No provider: the bot will not answer; calls to 95001/95002 go to the support queues; the human path works."
    fi
    say "Installed $TAG in $DIR in $(($(date +%s) - START))s."
}

main() {
    START=$(date +%s)
    umask 022
    set -eu
    parse_args "$@"
    detect_platform
    check_privileges
    if [ "$MODE" = uninstall ]; then
        do_uninstall
        return 0
    fi
    FIRST_INSTALL=1
    [ -f "$ENV_FILE" ] && FIRST_INSTALL=0
    resolve_tag
    say "Checking this host for $TAG ($OS/$ARCH)"
    preflight
    if [ "$CHECK_ONLY" = 1 ]; then
        say "Preflight passed. Nothing was changed."
        return 0
    fi
    resolve_provider
    SEED=demo
    [ "$NO_DEMO" = 1 ] && SEED=''
    # The provider session is opened on a first install, an upgrade and an
    # address change; a plain rerun only checks that the key is there.
    SKIP_PROVIDER=1
    if [ "$FIRST_INSTALL" = 1 ] || [ "$REWRITE_COMPOSE" = 1 ]; then SKIP_PROVIDER=0; fi
    if [ "$MODE" = upgrade ]; then backup_database; fi
    fetch_bundle
    install_bundle
    env_merge "$ENV_FILE"
    compose_up
    run_doctor
    final_output
}

[ -n "${AICC_INSTALL_LIB:-}" ] || main "$@"
