#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Unit tests for install.sh's pure functions, fed with fixtures:
#
#   sh deploy/install_test.sh
#
# POSIX sh, so it runs under dash, bash, busybox and macOS's /bin/sh alike. It
# sources install.sh with AICC_INSTALL_LIB=1, which defines the functions
# without running main, and touches nothing outside a temporary directory.

here=$(cd "$(dirname "$0")" && pwd)
AICC_INSTALL_LIB=1
# shellcheck source=deploy/install.sh
. "$here/install.sh"

failures=0
tests=0
TMP=$(mktemp -d "${TMPDIR:-/tmp}/aicc-install-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

eq() { # name got want
    tests=$((tests + 1))
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        failures=$((failures + 1))
        printf 'FAIL %s\n  got:  %s\n  want: %s\n' "$1" "$2" "$3"
    fi
}

ok() { # name command...
    _name=$1
    shift
    if "$@"; then eq "$_name" yes yes; else eq "$_name" no yes; fi
}

notok() {
    _name=$1
    shift
    if "$@"; then eq "$_name" yes no; else eq "$_name" no no; fi
}

# ------------------------------------------------------------ versions ----

ok 'engine 24.0.7 >= 24' version_ge 24.0.7 24
ok 'engine 24 >= 24.0.0' version_ge 24 24.0.0
notok 'engine 23.0.6 < 24' version_ge 23.0.6 24.0.0
ok 'engine 29.8.0 >= 24' version_ge 29.8.0 24.0.0
notok 'compose 2.24.3 < 2.24.4' version_ge 2.24.3 2.24.4
ok 'compose 2.24.4 >= 2.24.4' version_ge 2.24.4 2.24.4
ok 'compose v2.29.1 >= 2.24.4' version_ge v2.29.1 2.24.4
ok 'compose 5.1.4 >= 2.24.4' version_ge 5.1.4 2.24.4
ok 'compose v5.5.1 >= 2.24.4' version_ge v5.5.1 2.24.4
notok 'compose 2.3.10 < 2.24.4 (numeric, not lexical)' version_ge 2.3.10 2.24.4
ok 'colima 0.10.3 >= 0.9.0 (numeric, not lexical)' version_ge 0.10.3 0.9.0
notok 'colima 0.8.4 < 0.9.0' version_ge 0.8.4 0.9.0
ok 'tag v0.2.0 >= v0.1.1' version_ge v0.2.0 v0.1.1
notok 'tag v0.1.0 < v0.1.1 (downgrade)' version_ge v0.1.0 v0.1.1
ok 'prerelease suffix ignored' version_ge v0.0.1-dev v0.0.0-dev
eq 'version_norm strips v and suffix' "$(version_norm v1.2.3-rc.1+meta)" 1.2.3

# ------------------------------------------------------ Linux address ----

# The test VM: a DHCP LAN route and mihomo's TUN device Meta on 198.18.0.1/30.
vm_routes='default via 192.168.31.1 dev enp0s1 proto dhcp src 192.168.31.111 metric 100'
vm_addrs='2: enp0s1    inet 192.168.31.111/24 metric 100 brd 192.168.31.255 scope global dynamic enp0s1\       valid_lft 3000sec preferred_lft 3000sec
3: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\       valid_lft forever preferred_lft forever
5: Meta    inet 198.18.0.1/30 brd 198.18.0.3 scope global Meta\       valid_lft forever preferred_lft forever'
eq 'linux: VM default route' "$(linux_pick_address "$vm_routes" "$vm_addrs")" 192.168.31.111

# A TUN proxy that also installs a default route with a lower metric, and one
# without src: both must be passed over for the LAN.
tun_routes='default dev Meta scope link metric 1
default via 198.18.0.2 dev Meta src 198.18.0.1
default via 192.168.31.1 dev enp0s1 proto dhcp src 192.168.31.111 metric 100'
eq 'linux: TUN default routes skipped' "$(linux_pick_address "$tun_routes" "$vm_addrs")" 192.168.31.111

# No src on the route: the device's global address.
nosrc_routes='default via 10.0.0.1 dev eth0 proto static'
nosrc_addrs='2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0\       valid_lft forever preferred_lft forever'
eq 'linux: no src, device address' "$(linux_pick_address "$nosrc_routes" "$nosrc_addrs")" 10.0.0.5

# Two uplinks: the lower metric wins.
two_routes='default via 10.0.0.1 dev eth1 src 10.0.0.9 metric 600
default via 192.168.1.1 dev eth0 src 192.168.1.20 metric 100'
eq 'linux: lowest metric' "$(linux_pick_address "$two_routes" '')" 192.168.1.20
eq 'linux: nothing usable' "$(linux_pick_address 'default dev Meta scope link' "$vm_addrs")" ''

# ------------------------------------------------------ macOS address ----

hwports='Hardware Port: Ethernet
Device: en0
Ethernet Address: 00:00:00:00:00:01

Hardware Port: Wi-Fi
Device: en1
Ethernet Address: 00:00:00:00:00:02

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 00:00:00:00:00:03'
utun_route='   route to: default
destination: default
       mask: default
  interface: utun4
      flags: <UP,DONE,CLONING,STATIC>'
eq 'macos: utun default falls back to hardware ports' \
    "$(macos_candidate_ifaces "$utun_route" "$hwports" | tr '\n' ' ')" 'en0 en1 bridge0 '
wifi_route='   route to: default
    gateway: 192.168.31.1
  interface: en1'
eq 'macos: a hardware default goes first, once' \
    "$(macos_candidate_ifaces "$wifi_route" "$hwports" | tr '\n' ' ')" 'en1 en0 bridge0 '

# ---------------------------------------------------------- exclusions ----

ok 'excluded: TUN 198.18.0.1' ip_excluded 198.18.0.1
ok 'excluded: 198.19.255.1' ip_excluded 198.19.255.1
ok 'excluded: loopback' ip_excluded 127.0.0.1
ok 'excluded: link-local' ip_excluded 169.254.10.2
notok 'kept: 198.20.0.1' ip_excluded 198.20.0.1
notok 'kept: 192.168.31.111' ip_excluded 192.168.31.111
ok 'ipv4 ok' is_ipv4 192.168.31.111
notok 'ipv4 octet > 255' is_ipv4 192.168.31.256
notok 'ipv4 hostname' is_ipv4 pbx.example.com

# ------------------------------------------------------------- secrets ----

s1=$(random_secret)
s2=$(random_secret)
eq 'secret is 32 characters' "${#s1}" 32
eq 'secret charset is [A-Za-z0-9]' "$(printf '%s' "$s1" | tr -d 'A-Za-z0-9')" ''
notok 'two secrets differ' [ "$s1" = "$s2" ]
ok 'value_ok plain key' value_ok 'sk-abc_DEF.123'
notok 'value_ok space' value_ok 'a b'
notok 'value_ok dollar' value_ok "a\$b"
notok 'value_ok quote' value_ok "a'b"
notok 'value_ok hash' value_ok 'a#b'
notok 'value_ok empty' value_ok ''

# -------------------------------------------------------------- colima ----

cat >"$TMP/colima.yaml" <<'EOF'
# Number of CPUs to be allocated to the virtual machine.
cpu: 8
memory: 12
# portForwarder: grpc
network:
  address: false
portForwarder: "ssh"   # the old default
EOF
eq 'colima.yaml forwarder ssh (quoted, commented)' "$(yaml_top portForwarder "$TMP/colima.yaml")" ssh
eq 'colima.yaml cpu' "$(yaml_top cpu "$TMP/colima.yaml")" 8
eq 'colima.yaml nested key is not top level' "$(yaml_top address "$TMP/colima.yaml")" ''
printf 'portForwarder: grpc\n' >"$TMP/grpc.yaml"
eq 'colima.yaml forwarder grpc' "$(yaml_top portForwarder "$TMP/grpc.yaml")" grpc
eq 'colima.yaml missing file' "$(yaml_top portForwarder "$TMP/none.yaml")" ''
status='{"display_name":"colima","arch":"aarch64","runtime":"docker","cpu":8,"memory":12884901888,"disk":107374182400}'
eq 'colima status cpu' "$(json_field cpu "$status")" 8
eq 'colima status memory' "$(json_field memory "$status")" 12884901888
eq 'colima status no address' "$(json_field address "$status")" ''
eq 'colima status address' "$(json_field address '{"cpu":4,"address":"192.168.106.2"}')" 192.168.106.2

# --------------------------------------------------------------- ports ----

LINUX_TCP_PORTS='8080 5060 5066 5080 7443 8090 9090 18021 15432'
LINUX_UDP_PORTS='5060 5080 6060 16384-29999 30000-30999'
ss_out='tcp   LISTEN 0      4096         0.0.0.0:22        0.0.0.0:*
tcp   LISTEN 0      5            0.0.0.0:8080      0.0.0.0:*
tcp   LISTEN 0      4096            [::]:5066         [::]:*
udp   UNCONN 0      0          127.0.0.53%lo:53    0.0.0.0:*
udp   UNCONN 0      0            0.0.0.0:20000     0.0.0.0:*
udp   UNCONN 0      0            0.0.0.0:40000     0.0.0.0:*'
eq 'ports in use' "$(linux_port_conflicts "$ss_out" | tr '\n' ' ')" 'tcp/5066 tcp/8080 udp/20000 '
eq 'ports free' "$(linux_port_conflicts 'tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*')" ''

# ---------------------------------------------------------------- .env ----

OS=linux
COMPOSE_FILE_WANT=docker-compose.yml:compose.release.yml:compose.linux.yml
EXTERNAL_IP=192.168.31.111
LOCAL_IP=192.168.31.111
SEED=demo
PROVIDER=qwen
PROVIDER_KEY_VAR=ALIYUN_API_KEY
PROVIDER_KEY=sk-test
PROVIDER_ENDPOINT=''
RTP_START_WANT=16384
RTP_END_WANT=29999
REWRITE_ADDRESS=0
REWRITE_COMPOSE=0

env="$TMP/fresh.env"
env_merge "$env"
eq 'fresh .env mode 0600' "$(find "$env" -perm 600)" "$env"
for key in COMPOSE_FILE FS_EXTERNAL_IP FS_LOCAL_IP POSTGRES_PASSWORD ESL_PASSWORD LUA_PASSWORD \
    AICC_SEED_PASSWORD AICC_SEED RTP_START RTP_END PG_HOST_PORT AICC_PROVIDER ALIYUN_API_KEY; do
    eq "fresh .env has $key once" "$(grep -c "^$key=" "$env")" 1
done
eq 'fresh .env seed password is 32 characters' "$(env_get "$env" AICC_SEED_PASSWORD | awk '{ print length }')" 32
notok 'fresh .env never ClueCon' grep -q ClueCon "$env"
notok 'fresh .env no transcription line' grep -q AICC_TRANSCRIPTION_ENABLED "$env"
notok 'fresh .env no bot switch for a provider' grep -q AICC_BOT_ENABLED "$env"

before=$(cat "$env")
env_merge "$env"
eq 'rerun leaves .env byte-identical' "$(cat "$env")" "$before"

# A moved host on a plain rerun: .env keeps its address.
EXTERNAL_IP=10.9.9.9
LOCAL_IP=10.9.9.9
env_merge "$env"
eq 'plain rerun keeps the address' "$(env_get "$env" FS_EXTERNAL_IP)" 192.168.31.111

# An operator's file: their lines stay, missing keys are added, secrets kept.
env="$TMP/operator.env"
cat >"$env" <<'EOF'
# mine
POSTGRES_PASSWORD=keepme123
AICC_LOG_LEVEL=debug
FS_EXTERNAL_IP=192.168.31.111
ALIYUN_API_KEY=
AICC_SEED=
EOF
EXTERNAL_IP=192.168.31.111
LOCAL_IP=192.168.31.111
env_merge "$env"
eq 'existing secret kept' "$(env_get "$env" POSTGRES_PASSWORD)" keepme123
eq 'operator line kept' "$(env_get "$env" AICC_LOG_LEVEL)" debug
eq 'operator comment kept' "$(head -n 1 "$env")" '# mine'
eq 'missing secret added' "$(env_get "$env" LUA_PASSWORD | awk '{ print length }')" 32
eq 'empty provider key filled' "$(env_get "$env" ALIYUN_API_KEY)" sk-test
eq 'empty AICC_SEED (no demo) kept' "$(grep -c '^AICC_SEED=$' "$env")" 1

# An explicit --external-ip rewrites the address keys and nothing else.
cp "$env" "$TMP/before.env"
EXTERNAL_IP=203.0.113.7
LOCAL_IP=192.168.31.112
REWRITE_ADDRESS=1
REWRITE_COMPOSE=1
env_merge "$env"
eq 'explicit address rewrites FS_EXTERNAL_IP' "$(env_get "$env" FS_EXTERNAL_IP)" 203.0.113.7
eq 'explicit address rewrites FS_LOCAL_IP' "$(env_get "$env" FS_LOCAL_IP)" 192.168.31.112
eq 'explicit address: only the address lines changed' \
    "$(diff "$TMP/before.env" "$env" | grep '^[<>]' | cut -c3- | cut -d= -f1 | sort -u | tr '\n' ' ')" \
    'FS_EXTERNAL_IP FS_LOCAL_IP '
eq 'explicit address keeps the secret' "$(env_get "$env" POSTGRES_PASSWORD)" keepme123
eq '.env stays 0600 after a rewrite' "$(find "$env" -perm 600)" "$env"
REWRITE_ADDRESS=0
REWRITE_COMPOSE=0

# Provider none: the bot is switched off and AICC_PROVIDER is never "none".
env="$TMP/none.env"
PROVIDER=none
OS=macos
env_merge "$env"
eq 'none: bot disabled' "$(env_get "$env" AICC_BOT_ENABLED)" false
notok 'none: no AICC_PROVIDER line' grep -q '^AICC_PROVIDER=' "$env"
notok 'macos: no FS_LOCAL_IP' grep -q '^FS_LOCAL_IP=' "$env"
notok 'macos: no PG_HOST_PORT' grep -q '^PG_HOST_PORT=' "$env"

# gateway: key and endpoint.
env="$TMP/gw.env"
PROVIDER=gateway
PROVIDER_KEY_VAR=REALTIME_API_KEY
PROVIDER_ENDPOINT=wss://gw.example.com/v1/realtime
env_merge "$env"
eq 'gateway: endpoint' "$(env_get "$env" AICC_PROVIDER_ENDPOINT)" wss://gw.example.com/v1/realtime
eq 'gateway: key' "$(env_get "$env" REALTIME_API_KEY)" sk-test

# gemini: key.
env="$TMP/gemini.env"
PROVIDER=gemini
PROVIDER_KEY_VAR=GEMINI_API_KEY
env_merge "$env"
eq 'gemini: provider' "$(env_get "$env" AICC_PROVIDER)" gemini
eq 'gemini: key' "$(env_get "$env" GEMINI_API_KEY)" sk-test

# env_set on a value with a slash, an @ and an =.
env_set "$env" X 'a/b@c=d'
eq 'env_set keeps the value verbatim' "$(env_get "$env" X)" 'a/b@c=d'

# --------------------------------------------------------------- arguments ----

eq 'parse_args: --provider=' "$(parse_args --provider=none --yes && echo "$PROVIDER_FLAG $ASSUME_YES")" 'none 1'
eq 'parse_args: --provider gemini' "$(parse_args --provider gemini && echo "$PROVIDER_FLAG")" gemini
eq 'provider_key_var: gemini' "$(provider_key_var gemini)" GEMINI_API_KEY
notok 'parse_args: bad provider' sh -c "AICC_INSTALL_LIB=1; . '$here/install.sh'; parse_args --provider bogus" 2>/dev/null
notok 'parse_args: bad address' sh -c "AICC_INSTALL_LIB=1; . '$here/install.sh'; parse_args --external-ip 1.2.3" 2>/dev/null
notok 'parse_args: --purge alone' sh -c "AICC_INSTALL_LIB=1; . '$here/install.sh'; parse_args --purge" 2>/dev/null

# ---------------------------------------------------------------- output ----

# The closing message: with demo data it names the seeded admin; without it,
# it says how to create the first administrator instead of printing a password
# that was never seeded.
env="$TMP/output-no-demo.env"
cat >"$env" <<'EOF'
AICC_SEED=
AICC_SEED_PASSWORD=secret
HTTP_PORT=8080
EOF
ENV_FILE="$env"
DIR=/tmp/aicc
TAG=v0.0.0
PROVIDER=none
EXTERNAL_IP=192.0.2.1
START=0
FIRST_INSTALL=1
OS=linux
final_output >"$TMP/output-no-demo.txt"
ok 'final_output: no demo names the first-admin command' grep -q 'sudo docker compose exec aicc aicc useradd' "$TMP/output-no-demo.txt"
notok 'final_output: no demo prints no seeded password' grep -q secret "$TMP/output-no-demo.txt"
notok 'final_output: no demo drops the demo dial line' grep -q 'dial 95001' "$TMP/output-no-demo.txt"
FIRST_INSTALL=0
final_output >"$TMP/output-no-demo-rerun.txt"
ok 'final_output: no demo rerun keeps the hint' grep -q 'sudo docker compose exec aicc aicc useradd' "$TMP/output-no-demo-rerun.txt"
OS=macos
final_output >"$TMP/output-no-demo-macos.txt"
notok 'final_output: macOS first-admin command has no sudo' grep -q 'sudo docker' "$TMP/output-no-demo-macos.txt"

env="$TMP/output-demo.env"
cat >"$env" <<'EOF'
AICC_SEED=demo
AICC_SEED_PASSWORD=secret
HTTP_PORT=8080
EOF
ENV_FILE="$env"
FIRST_INSTALL=1
final_output >"$TMP/output-demo.txt"
ok 'final_output: demo prints the admin password' grep -q 'admin / secret' "$TMP/output-demo.txt"
ok 'final_output: demo keeps the dial line' grep -q 'dial 95001' "$TMP/output-demo.txt"

printf '\n%d tests, %d failed\n' "$tests" "$failures"
[ "$failures" = 0 ]
