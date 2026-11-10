#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The end-to-end environment: up, smoke, down. See deploy/e2e/README.md.
#
#   deploy/e2e/e2e.sh up      fresh database, application built from this tree
#   deploy/e2e/e2e.sh smoke   place one AI outbound call and check the result
#   deploy/e2e/e2e.sh campaign  run the campaign scenarios, probes and invariants (#245, #246)
#   deploy/e2e/e2e.sh capacity  run the capacity scenarios K1, K2 and K3, each in an application start of its own (#247, #459)
#   deploy/e2e/e2e.sh down    collect logs, remove containers, volumes, network
#   deploy/e2e/e2e.sh config  print the resolved compose configuration
#
# E2E_OUTBOUND_DIAL_FORM (E164, E164_NO_PLUS or NATIONAL; default E164) is the
# application's AICC_OUTBOUND_DIAL_FORM and picks the simulator's scenario file.
# `up` fixes it for the environment: every later command uses the form `up`
# recorded, and refuses a different explicit value (README.md, "Dial forms").
#
# COMPOSE_PROJECT_NAME picks the project (default: derived from this checkout's
# path, so two worktrees get two environments). E2E_NET_PREFIX picks the /24
# (default: derived from the project name, 10.64-127.x.y).
set -eEuo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
E2E="$ROOT/deploy/e2e"

h="$(printf '%s' "$ROOT" | cksum | cut -d' ' -f1)"
PROJECT="${COMPOSE_PROJECT_NAME:-aice2e-$h}"
if [ -n "${COMPOSE_PROJECT_NAME:-}" ]; then h="$(printf '%s' "$PROJECT" | cksum | cut -d' ' -f1)"; fi
NET="${E2E_NET_PREFIX:-10.$((64 + h % 64)).$(((h / 64) % 256))}"

WORK="$ROOT/bin/e2e/$PROJECT"
RUN="$WORK/run"
TOOLS="$WORK/tools"
APP_IP="$NET.10"; FS_IP="$NET.11"; MOCK_IP="$NET.12"; CALLEE_IP="$NET.13"
TO_NUMBER=13800000001
# The called-number prefix of the one gateway (destination-prefix, #531): the
# simulator expects it on every INVITE and the smoke checks it never reaches
# the application's records. Lower case, letters and digits mixed; the value
# is defined here and nowhere else.
CALLED_PREFIX=a1b2cc
# The dial form the environment was started with, written by `up` and removed
# by `down`.
FORM_FILE="$WORK/dial-form"
DIAL_FORM=E164
SCENARIO_FILE=scenario.json

# The application's bot capacity, overflow hold and bot RTP range for its next
# start; empty is the application's default. Only cmd_capacity changes them,
# and nothing from the caller's shell reaches them.
APP_BOT_MAX_CALLS=""; APP_BOT_INBOUND_RESERVE=""; APP_OVERFLOW_HOLD=""
APP_BOT_RTP_PORT_LOW=""; APP_BOT_RTP_PORT_HIGH=""

# Compose runs with a scrubbed environment: nothing from the caller's shell
# (provider keys, AICC_* settings, a stray .env) can reach an interpolation.
# Only the Docker client's own variables and the harness's are passed.
compose() {
  env -i PATH="$PATH" HOME="$HOME" \
    ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} ${DOCKER_CONTEXT:+DOCKER_CONTEXT="$DOCKER_CONTEXT"} \
    ${DOCKER_CONFIG:+DOCKER_CONFIG="$DOCKER_CONFIG"} ${AICC_FS_IMAGE:+AICC_FS_IMAGE="$AICC_FS_IMAGE"} \
    E2E_PROJECT="$PROJECT" E2E_RUN_DIR="$RUN" E2E_SUBNET="$NET.0/24" \
    E2E_APP_IP="$APP_IP" E2E_FS_IP="$FS_IP" E2E_MOCK_IP="$MOCK_IP" E2E_CALLEESIM_IP="$CALLEE_IP" \
    E2E_UID="$(id -u)" E2E_GID="$(id -g)" FS_EXTERNAL_IP="$FS_IP" \
    ${E2E_OUTBOUND_ENDPOINT:+E2E_OUTBOUND_ENDPOINT="$E2E_OUTBOUND_ENDPOINT"} \
    E2E_OUTBOUND_DIAL_FORM="$DIAL_FORM" E2E_SCENARIO_FILE="$SCENARIO_FILE" E2E_CALLED_PREFIX="$CALLED_PREFIX" \
    E2E_BOT_MAX_CALLS="$APP_BOT_MAX_CALLS" E2E_BOT_INBOUND_RESERVE="$APP_BOT_INBOUND_RESERVE" \
    E2E_OVERFLOW_HOLD="$APP_OVERFLOW_HOLD" \
    E2E_BOT_RTP_PORT_LOW="$APP_BOT_RTP_PORT_LOW" E2E_BOT_RTP_PORT_HIGH="$APP_BOT_RTP_PORT_HIGH" \
    docker compose -p "$PROJECT" --project-directory "$ROOT/deploy" \
    --env-file "$E2E/e2e.env" -f "$ROOT/deploy/docker-compose.yml" -f "$E2E/docker-compose.yml" "$@"
}

# resolve_dial_form sets DIAL_FORM and SCENARIO_FILE. The caller's
# E2E_OUTBOUND_DIAL_FORM must be a form (empty is unset); `up` takes it (E164
# when unset), every other command takes the form `up` recorded and refuses a
# different explicit one, since the application was started in the recorded
# form and the scenario file the simulator mounted follows it. `down` never
# refuses: it has to be able to clean up whatever is there.
resolve_dial_form() {
  local cmd="$1" want="${E2E_OUTBOUND_DIAL_FORM:-}" rec=""
  case "$want" in
    ""|E164|E164_NO_PLUS|NATIONAL) ;;
    *) echo "E2E_OUTBOUND_DIAL_FORM must be one of E164, E164_NO_PLUS, NATIONAL, got \"$want\"" >&2; exit 2 ;;
  esac
  [ ! -f "$FORM_FILE" ] || rec="$(cat "$FORM_FILE")"
  case "$rec" in
    ""|E164|E164_NO_PLUS|NATIONAL) ;;
    *) echo "$FORM_FILE holds \"$rec\", not a dial form; run: make e2e-down" >&2; exit 1 ;;
  esac
  if [ "$cmd" = up ] || [ -z "$rec" ]; then
    DIAL_FORM="${want:-E164}"
  else
    if [ -n "$want" ] && [ "$want" != "$rec" ] && [ "$cmd" != down ]; then
      echo "E2E_OUTBOUND_DIAL_FORM=$want, but the environment is up with $rec; run: make e2e-down, then start it again with the form you want" >&2
      exit 2
    fi
    DIAL_FORM="$rec"
  fi
  if [ "$DIAL_FORM" = E164 ]; then SCENARIO_FILE=scenario.json; else SCENARIO_FILE="scenario.$DIAL_FORM.json"; fi
}

env_value() { sed -n "s/^$1=//p" "$E2E/e2e.env"; }
say() { printf '[e2e] %s\n' "$*" >&2; }

daemon_arch() {
  case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64) echo amd64 ;;
    *) echo "unsupported daemon architecture" >&2; exit 1 ;;
  esac
}

build_tools() {
  local arch; arch="$(daemon_arch)"
  mkdir -p "$TOOLS"
  say "building the harness binaries for linux/$arch"
  for cmd in aicc-calleesim aicc-mockprovider aicc-e2e; do
    (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags '-s -w' -o "$TOOLS/$cmd" "./cmd/$cmd")
  done
  # The campaign tests are Go tests under the build tag e2e, compiled into a
  # static test binary that runs inside the network like the smoke driver.
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags e2e -trimpath -ldflags '-s -w' -o "$TOOLS/aicc-e2e-campaign.test" ./internal/e2e)
  docker build -q -t "aicc-e2e-tools:$PROJECT" -f "$E2E/Dockerfile.tools" "$TOOLS" >/dev/null
  # The guard runs on the host, before anything starts.
  (cd "$ROOT" && go build -o "$WORK/aicc-e2e-host" ./cmd/aicc-e2e)
}

# render writes the resolved configuration and the one gateway file, then
# refuses to go on when the configuration is not the isolated environment.
render() {
  mkdir -p "$RUN"
  sed -e "s/@CALLEESIM_IP@/$CALLEE_IP/" -e "s/@CALLED_PREFIX@/$CALLED_PREFIX/" "$E2E/calleesim-gateway.xml.in" > "$RUN/e2e_calleesim.xml"
  compose config --format json > "$RUN/compose.json"
  "$WORK/aicc-e2e-host" guard -compose-config "$RUN/compose.json" >&2
}

collect_logs() {
  mkdir -p "$RUN/logs"
  local svc
  for svc in aicc freeswitch calleesim mockprovider postgres lua-role; do
    compose logs --no-color --timestamps "$svc" > "$RUN/logs/$svc.log" 2>&1 || true
  done
  compose exec -T freeswitch fs_cli -p "$(env_value ESL_PASSWORD)" -P 18021 -x 'sofia status' > "$RUN/logs/sofia-status.txt" 2>&1 || true
  # The switch's own log file prefixes each line of a call with its channel
  # uuid (the leg id), which the console output does not: the failure bundles
  # are sliced from it.
  compose exec -T freeswitch cat /usr/local/freeswitch/log/freeswitch.log > "$RUN/logs/freeswitch-file.log" 2>/dev/null || true
}

# slice_bundles copies, into every failure bundle of a campaign run, the lines
# of the application log and the switch log that name one of the bundle's ids
# (call id, leg id, SIP Call-ID; ids.txt, written by the driver).
slice_bundles() {
  local ids dir
  for ids in "$1"/bundles/*/ids.txt; do
    [ -f "$ids" ] || continue
    dir="$(dirname "$ids")"
    grep -F -f "$ids" "$RUN/logs/aicc.log" > "$dir/aicc.log" 2>/dev/null || true
    grep -F -f "$ids" "$RUN/logs/freeswitch-file.log" > "$dir/freeswitch.log" 2>/dev/null || true
  done
}

cmd_up() {
  build_tools
  say "project $PROJECT, network $NET.0/24, dial form $DIAL_FORM (scenario $SCENARIO_FILE)"
  # Refuse first, before anything of a running environment is touched.
  render
  compose down -v --remove-orphans >&2
  rm -rf "$RUN"
  render
  # From here the environment is in this form, whatever happens next.
  mkdir -p "$WORK"
  printf '%s\n' "$DIAL_FORM" > "$FORM_FILE"
  # A start that fails leaves its logs in the run directory.
  trap 'collect_logs' ERR
  say "building the application from this tree"
  local t0=$SECONDS
  compose build aicc >&2
  say "application build took $((SECONDS - t0)) s"
  say "starting"
  compose up -d --wait --wait-timeout 300 postgres lua-role aicc freeswitch mockprovider calleesim >&2
  compose exec -T aicc /usr/local/bin/aicc useradd -username admin -password "$(env_value E2E_PASSWORD)" -role ADMIN >&2
  say "up. Run directory: $RUN"
}

cmd_smoke() {
  [ -n "$(compose ps -q aicc 2>/dev/null)" ] || { say "the environment is not up; run: make e2e-up"; exit 1; }
  render
  local id out
  id="smoke-$(date -u +%Y%m%dT%H%M%SZ)"
  out="/run/e2e/$id"
  if compose run --rm -T --no-deps driver smoke \
      -api "http://$APP_IP:8080" -ready "http://$APP_IP:9090/readyz" \
      -calleesim "http://$CALLEE_IP:8771" -esl "$FS_IP:18021" \
      -compose-config /run/e2e/compose.json -username admin \
      -flow /etc/e2e/flow.json -to "$TO_NUMBER" -called-prefix "$CALLED_PREFIX" -out "$out" >&2; then
    cat "$RUN/$id/smoke.json"
    say "smoke passed. Results: $RUN/$id"
  else
    local rc=$?
    collect_logs
    [ -f "$RUN/$id/smoke.json" ] && cat "$RUN/$id/smoke.json"
    say "smoke FAILED. Results and logs: $RUN"
    exit "$rc"
  fi
}

cmd_campaign() {
  [ -n "$(compose ps -q aicc 2>/dev/null)" ] || { say "the environment is not up; run: make e2e-up"; exit 1; }
  render
  local id out t0=$SECONDS
  id="campaign-$(date -u +%Y%m%dT%H%M%SZ)"
  out="/run/e2e/$id"
  # The simulator answers a number's n-th INVITE with the n-th behaviour of
  # its scenario, counting from its start, so a case whose attempts behave
  # differently (C04: busy, then answered) needs one that has not seen the
  # number yet. The driver waits for it and refuses a simulator that has.
  compose restart calleesim >&2
  if compose run --rm -T --no-deps --entrypoint /usr/local/bin/aicc-e2e-campaign.test driver \
      -test.v -test.run '^TestCampaigns$' -test.timeout 20m \
      -e2e.api "http://$APP_IP:8080" -e2e.ready "http://$APP_IP:9090/readyz" \
      -e2e.metrics "http://$APP_IP:9090/metrics" \
      -e2e.calleesim "http://$CALLEE_IP:8771" -e2e.mockprovider "http://$MOCK_IP:9099" \
      -e2e.esl "$FS_IP:18021" -e2e.compose-config /run/e2e/compose.json \
      -e2e.scenario /etc/e2e/scenario.json -e2e.provider-scenario /etc/e2e/provider-scenario.json \
      -e2e.flow /etc/e2e/flow.json -e2e.username admin -e2e.out "$out" >&2; then
    [ -f "$RUN/$id/probes.md" ] && cat "$RUN/$id/probes.md"
    say "campaign tests passed in $((SECONDS - t0)) s. Results: $RUN/$id"
  else
    local rc=$?
    collect_logs
    slice_bundles "$RUN/$id"
    [ -f "$RUN/$id/probes.md" ] && cat "$RUN/$id/probes.md"
    say "campaign tests FAILED after $((SECONDS - t0)) s. Report, bundles and logs: $RUN"
    exit "$rc"
  fi
}

# restart_app recreates the application with the APP_* settings in effect, so
# a scenario runs in an application start of its own. The configuration is
# rendered (and refused by the guard) first: the driver checks the scenario's
# settings in that same file.
restart_app() {
  render
  local t=$SECONDS
  compose up -d --no-deps --force-recreate --wait --wait-timeout 180 aicc >&2
  say "application restarted for $1 in $((SECONDS - t)) s (AICC_BOT_MAX_CALLS='$APP_BOT_MAX_CALLS' AICC_BOT_INBOUND_RESERVE='$APP_BOT_INBOUND_RESERVE' AICC_OVERFLOW_HOLD='$APP_OVERFLOW_HOLD' AICC_BOT_RTP_PORT_LOW='$APP_BOT_RTP_PORT_LOW' AICC_BOT_RTP_PORT_HIGH='$APP_BOT_RTP_PORT_HIGH')"
}

run_capacity() {
  local sc="$1" dir="$2" t=$SECONDS
  if compose run --rm -T --no-deps --entrypoint /usr/local/bin/aicc-e2e-campaign.test driver \
      -test.v -test.run '^TestCapacity$' -test.timeout 10m -e2e.capacity "$sc" \
      -e2e.api "http://$APP_IP:8080" -e2e.ready "http://$APP_IP:9090/readyz" \
      -e2e.metrics "http://$APP_IP:9090/metrics" \
      -e2e.calleesim "http://$CALLEE_IP:8771" -e2e.mockprovider "http://$MOCK_IP:9099" \
      -e2e.esl "$FS_IP:18021" -e2e.compose-config /run/e2e/compose.json \
      -e2e.scenario /etc/e2e/scenario.json -e2e.provider-scenario /etc/e2e/provider-scenario.json \
      -e2e.flow /etc/e2e/flow.json -e2e.username admin -e2e.out "/run/e2e/$dir/$sc" >&2; then
    say "$sc passed in $((SECONDS - t)) s"
  else
    say "$sc FAILED after $((SECONDS - t)) s"
    return 1
  fi
}

# judge_switch_log is K3's second half. The switch's log is in its container,
# so it is copied out and the test binary judges it against the probes the run
# wrote into its report (the lines of each refused call's leg).
judge_switch_log() {
  local dir="$1" t=$SECONDS
  collect_logs
  if compose run --rm -T --no-deps --entrypoint /usr/local/bin/aicc-e2e-campaign.test driver \
      -test.v -test.run '^TestRefusalSwitchLog$' -test.timeout 2m \
      -e2e.out "/run/e2e/$dir/K3" -e2e.switchlog /run/e2e/logs/freeswitch-file.log >&2; then
    say "K3 switch log checks passed in $((SECONDS - t)) s"
  else
    say "K3 switch log checks FAILED after $((SECONDS - t)) s"
    return 1
  fi
}

cmd_capacity() {
  [ -n "$(compose ps -q aicc 2>/dev/null)" ] || { say "the environment is not up; run: make e2e-up"; exit 1; }
  local id rc=0 sc t0=$SECONDS
  id="capacity-$(date -u +%Y%m%dT%H%M%SZ)"
  # A fresh record: the simulator counts a number's INVITEs from its start.
  compose restart calleesim >&2
  # K1 lowers the bot capacity, K2 keeps the defaults and K3 narrows the bot's
  # RTP range to one pair; each gets an application start of its own, and the
  # application is recreated on the defaults last, as e2e.sh up started it.
  for sc in K1 K2 K3; do
    APP_BOT_MAX_CALLS=""; APP_BOT_INBOUND_RESERVE=""; APP_OVERFLOW_HOLD=""
    APP_BOT_RTP_PORT_LOW=""; APP_BOT_RTP_PORT_HIGH=""
    case "$sc" in
      K1) APP_BOT_MAX_CALLS=3; APP_BOT_INBOUND_RESERVE=0; APP_OVERFLOW_HOLD=1500ms ;;
      K2) ;;
      K3) APP_BOT_RTP_PORT_LOW=26000; APP_BOT_RTP_PORT_HIGH=26001 ;;
    esac
    if ! restart_app "$sc"; then rc=1; break; fi
    run_capacity "$sc" "$id" || rc=1
    if [ "$sc" = K3 ] && [ -f "$RUN/$id/K3/report.json" ]; then judge_switch_log "$id" || rc=1; fi
  done
  if [ -n "$APP_BOT_MAX_CALLS$APP_BOT_INBOUND_RESERVE$APP_OVERFLOW_HOLD$APP_BOT_RTP_PORT_LOW$APP_BOT_RTP_PORT_HIGH" ]; then
    APP_BOT_MAX_CALLS=""; APP_BOT_INBOUND_RESERVE=""; APP_OVERFLOW_HOLD=""
    APP_BOT_RTP_PORT_LOW=""; APP_BOT_RTP_PORT_HIGH=""
    restart_app defaults || rc=1
  fi
  for sc in K1 K2 K3; do
    if [ -f "$RUN/$id/$sc/capacity.md" ]; then cat "$RUN/$id/$sc/capacity.md"; fi
  done
  if [ "$rc" -ne 0 ]; then
    collect_logs
    say "capacity tests FAILED after $((SECONDS - t0)) s. Reports and logs: $RUN"
    exit 1
  fi
  say "capacity tests passed in $((SECONDS - t0)) s. Results: $RUN/$id"
}

cmd_down() {
  if [ -n "$(compose ps -aq 2>/dev/null)" ]; then collect_logs; fi
  compose down -v --remove-orphans >&2
  local left
  left="$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT"; \
          docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT"; \
          docker network ls -q --filter "label=com.docker.compose.project=$PROJECT")"
  [ -z "$left" ] || { say "left behind: $left"; exit 1; }
  rm -f "$FORM_FILE"
  say "down: no container, volume or network of $PROJECT remains. Logs: $RUN/logs"
}

resolve_dial_form "${1:-}"

case "${1:-}" in
  up) cmd_up ;;
  smoke) cmd_smoke ;;
  campaign) cmd_campaign ;;
  capacity) cmd_capacity ;;
  down) cmd_down ;;
  config) build_tools; render; cat "$RUN/compose.json" ;;
  *) echo "usage: $0 up|smoke|campaign|capacity|down|config" >&2; exit 2 ;;
esac
