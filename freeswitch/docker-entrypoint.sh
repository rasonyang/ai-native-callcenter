#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# Injects this deployment's external dependencies into the shipped
# configuration, at every container start.
#
# The image carries the configuration tree from freeswitch/conf/ in the
# repository. That tree has no site in it: no database password, no office
# subnet, no LAN address, no PSTN trunk. Everything of that kind arrives here
# as an environment variable, which is why a variable left unset can never mean
# "keep the value the build machine happened to have" — the value it would keep
# is a default that ships to everyone.
#
# So each variable below is one of three things, and the choice is stated where
# it is made:
#
#   required   the switch refuses to start without it, because a switch that
#              started would look healthy and do nothing (AICC_LUA_DSN,
#              AICC_CC_DSN)
#   defaulted  unset produces a working self-contained switch, and the default
#              is written out explicitly rather than left to whatever the
#              configuration file says
#   optional   unset leaves the shipped configuration alone, because the
#              shipped value is already a working one
#
# The environment-variable names are the ones the published image documents and
# are not changed here: FS_LOCAL_IP, FS_EXTERNAL_IP, FS_DOMAIN,
# FS_DEFAULT_PASSWORD, FS_ESL_LISTEN_IP, FS_ESL_PORT, FS_ESL_PASSWORD,
# FS_ESL_ACL, FS_CORE_DB_DSN, FS_RTP_START_PORT, FS_RTP_END_PORT,
# FS_CALLCENTER_DSN, AICC_LUA_DSN, AICC_CC_DSN, AICC_BOT_HOST, AICC_BOT_PORT,
# AICC_RECORDINGS_DIR, PSTN_GATEWAY_*, FS_S3_*.
set -euo pipefail

FS_HOME=/usr/local/freeswitch
CONF="$FS_HOME/conf"
VARS="$CONF/vars.xml"
ESL="$CONF/autoload_configs/event_socket.conf.xml"
SWITCH="$CONF/autoload_configs/switch.conf.xml"
CC="$CONF/autoload_configs/callcenter.conf.xml"
MODULES="$CONF/autoload_configs/modules.conf.xml"
HC="$CONF/autoload_configs/http_cache.conf.xml"
INJECTED=()

# esc: escape a sed replacement string (backslash, ampersand, and the |
# delimiter used throughout).
# xml: XML attribute entities, for <param value="..."> and element text, which
# are genuinely parsed as XML.
#
# vars.xml is not one of those. Its X-PRE-PROCESS set directives are read as
# raw text by the preprocessor, which does not resolve entities, so a value
# containing ' — a pgsql DSN's password='...' is the usual one — has to go in
# verbatim. set_var therefore does not xml-escape.
esc() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }
xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }

# set_var NAME VALUE — rewrite vars.xml's <X-PRE-PROCESS cmd="set"
# data="NAME=..."/>, appending it if the tree does not set NAME at all.
#
# A double quote in the value is refused rather than escaped. FreeSWITCH's
# preprocessor (switch_xml.c) finds `data=`, then the next `"`, and takes
# everything up to the following `"` — so a quote inside the value silently
# truncates it and the switch runs with half a DSN. Escaping it as &quot; would
# be worse: the preprocessor consumes the whole directive before any XML parser
# sees it and resolves no entities, so the variable would literally contain the
# six characters &quot;.
#
# That same property is why & and < are left alone here, and why `xmllint`
# considers a vars.xml holding a DSN with an ampersand malformed while
# FreeSWITCH reads it correctly. Do not "fix" that by escaping.
set_var() {
  local name="$1" val
  case "$2" in
    *'"'*)
      echo "[entrypoint] the value for $name contains a double quote, which FreeSWITCH's" >&2
      echo "             preprocessor would silently truncate the variable at. Remove it." >&2
      exit 1
      ;;
  esac
  val="$(esc "$2")"
  if grep -q "data=\"$name=" "$VARS"; then
    sed -i "s|\(<X-PRE-PROCESS cmd=\"set\" data=\"$name=\)[^\"]*\"|\1$val\"|" "$VARS"
  else
    sed -i "s|</include>|  <X-PRE-PROCESS cmd=\"set\" data=\"$name=$val\"/>\n</include>|" "$VARS"
  fi
}

# unset_var NAME — remove vars.xml's setting of NAME entirely, so FreeSWITCH's
# own value for it applies. Only useful for the handful of variables the core
# computes for itself.
unset_var() {
  sed -i "/<X-PRE-PROCESS cmd=\"set\" data=\"$1=/d" "$VARS"
}

# put_param FILE NAME VALUE — make FILE carry exactly one <param name="NAME"/>
# with this value.
#
# Rewriting in place is not enough: vanilla ships several of these parameters
# commented out, and a sed that edits the value inside the comment leaves the
# parameter just as inactive as it was while reporting success. Both the live
# line and a self-contained commented-out one are removed first, then one line
# is inserted after the opening <settings>. A commented line is only removed
# when the whole comment is on it, so a parameter mentioned inside a multi-line
# explanation is left alone.
put_param() {
  local file="$1" name="$2" val; val="$(esc "$(xml "$3")")"
  sed -i \
    -e "/^[[:space:]]*<param name=\"$name\"/d" \
    -e "/^[[:space:]]*<!--[[:space:]]*<param name=\"$name\"[^>]*\/>[[:space:]]*-->[[:space:]]*$/d" \
    "$file"
  sed -i "0,/<settings>/s|<settings>|<settings>\n    <param name=\"$name\" value=\"$val\"/>|" "$file"
}

note() { INJECTED+=("$1"); }

# Every file this script edits has to be there. A sed against a missing path
# under `set -e` aborts with a message about a file, which is a poor way to
# learn that the configuration tree in the image is not the one expected.
for f in "$VARS" "$ESL" "$SWITCH" "$CC" "$MODULES" "$HC"; do
  [ -f "$f" ] || { echo "[entrypoint] $f is missing; this is not an aicc configuration tree" >&2; exit 1; }
done

# ---- required: the two databases the Lua handler reads ---------------------
#
# aicc_xml.lua answers mod_lua's directory and configuration bindings out of
# PostgreSQL: which SIP accounts exist and what their passwords are, which
# queues exist and who is in them. Without a DSN it answers nothing, and a
# switch in that state is the failure this project keeps having to design
# against — it starts, it is healthy, fs_cli says UP, and every agent phone is
# told its extension does not exist. Refusing to start says what is wrong once,
# in the place where somebody is looking.
missing_required=()
[ -n "${AICC_LUA_DSN:-}" ] || missing_required+=(AICC_LUA_DSN)
[ -n "${AICC_CC_DSN:-}" ]  || missing_required+=(AICC_CC_DSN)
if [ "${#missing_required[@]}" -gt 0 ]; then
  cat >&2 <<EOF
[entrypoint] refusing to start: ${missing_required[*]} not set.

This image ships no database credentials — the configuration in it is the
repository's, with nothing site-specific baked in. The switch reads its
directory (accounts, passwords, contexts) and its queues from PostgreSQL
through aicc_xml.lua, so without these DSNs it would come up looking perfectly
healthy and tell every agent phone that its extension does not exist.

  AICC_LUA_DSN  pgsql://host=db dbname=aicc user=aicc_lua password='...'
  AICC_CC_DSN   pgsql://host=db dbname=aicc_fs user=aicc password='...'

The full variable list is the image overview on Docker Hub, and
freeswitch/DOCKERHUB.md in the repository.
EOF
  exit 1
fi
set_var aicc_lua_dsn "$AICC_LUA_DSN"; note AICC_LUA_DSN
set_var aicc_cc_dsn  "$AICC_CC_DSN";  note AICC_CC_DSN

# ---- addresses -------------------------------------------------------------
#
# local_ip_v4 is a core variable FreeSWITCH computes from the primary
# interface, and inside a container that answer is the right one. The shipped
# tree does not set it; if some tree does, the setting is removed so the core's
# own value wins rather than a value written on somebody else's network.
if [ -n "${FS_LOCAL_IP:-}" ]; then
  set_var local_ip_v4 "$FS_LOCAL_IP"; note FS_LOCAL_IP
else
  unset_var local_ip_v4
fi

# Vanilla resolves the external addresses over STUN. On a host behind NAT that
# advertises the whole network's internet-facing address, and the media for a
# call that never leaves the compose network would be sent there. The
# container's own address is right for a self-contained stack; a phone or a
# trunk outside this host needs the host's, which is what FS_EXTERNAL_IP is
# for.
external_ip='$${local_ip_v4}'
if [ -n "${FS_EXTERNAL_IP:-}" ]; then external_ip="$FS_EXTERNAL_IP"; note FS_EXTERNAL_IP; fi
set_var external_rtp_ip "$external_ip"
set_var external_sip_ip "$external_ip"

# The SIP realm agent phones register to. Defaulted to the container's own
# address, which is vanilla's answer and always resolvable from inside the
# stack; a deployment whose phones register to a name sets FS_DOMAIN to it.
if [ -n "${FS_DOMAIN:-}" ]; then
  set_var domain "$FS_DOMAIN"; note FS_DOMAIN
else
  set_var domain '$${local_ip_v4}'
fi

# Optional. aicc accounts come from PostgreSQL with their own passwords, so
# this only affects directory entries read from files — of which the shipped
# tree has none. Unset leaves vanilla's value, which is a stock default and
# grants access to nothing.
if [ -n "${FS_DEFAULT_PASSWORD:-}" ]; then set_var default_password "$FS_DEFAULT_PASSWORD"; note FS_DEFAULT_PASSWORD; fi

# ---- the voice bot ---------------------------------------------------------
#
# The aicc_bot gateway dials the application's SIP server. Defaulted to this
# container, which is wrong for every real deployment and fails visibly on the
# first AI call rather than quietly: the gateway has nothing listening on 6060
# and the leg is rejected. Set AICC_BOT_HOST to where the application runs.
bot_host='$${local_ip_v4}'
if [ -n "${AICC_BOT_HOST:-}" ]; then bot_host="$AICC_BOT_HOST"; note AICC_BOT_HOST; fi
set_var aicc_bot_host "$bot_host"
if [ -n "${AICC_BOT_PORT:-}" ]; then note AICC_BOT_PORT; fi
set_var aicc_bot_port "${AICC_BOT_PORT:-6060}"

# ---- recordings ------------------------------------------------------------
#
# record_session takes a URL as readily as a directory: with mod_http_cache
# loaded and file formats enabled, http://filer:8888/buckets/aicc-recordings
# makes the switch upload each finished recording itself, with no volume shared
# between it and the application. Defaulted to the local directory that is
# already a declared volume.
AICC_RECORDINGS_DIR="${AICC_RECORDINGS_DIR:-$FS_HOME/recordings}"
set_var aicc_recordings_dir "$AICC_RECORDINGS_DIR"
case "$AICC_RECORDINGS_DIR" in
  /*) mkdir -p "$AICC_RECORDINGS_DIR" ;;
  *)  note "recordings=$AICC_RECORDINGS_DIR" ;;
esac

# ---- PSTN trunk ------------------------------------------------------------
#
# There is no trunk in the image. The development one that used to be baked in
# is exactly the sort of thing this entrypoint exists to keep out.
#
# Unset, the gateway host is set to 192.0.2.1 — TEST-NET-1, reserved by
# RFC 5737 for documentation and routable from nowhere. An outbound PSTN call
# then fails to reach a gateway, which is what should happen when no trunk was
# configured, and the address in the log says plainly that none was.
if [ -n "${PSTN_GATEWAY_HOST:-}" ]; then
  set_var pstn_gateway_host "$PSTN_GATEWAY_HOST"; note PSTN_GATEWAY_HOST
else
  set_var pstn_gateway_host "192.0.2.1"
  note "pstn=none"
fi
set_var pstn_gateway_port "${PSTN_GATEWAY_PORT:-5080}"
# Empty by default so the dialplan passes the call's own
# effective_caller_id_number through, rather than stamping every outbound call
# with one number somebody else's deployment was issued.
set_var pstn_gateway_caller_id "${PSTN_GATEWAY_CALLER_ID:-}"
if [ -n "${PSTN_GATEWAY_PORT:-}" ]; then note PSTN_GATEWAY_PORT; fi
if [ -n "${PSTN_GATEWAY_CALLER_ID:-}" ]; then note PSTN_GATEWAY_CALLER_ID; fi

# ---- event socket ----------------------------------------------------------
#
# 0.0.0.0 rather than loopback: the application is in another container. What
# keeps that safe is the ACL, not the bind address — aicc_esl in acl.conf.xml
# admits loopback and the private ranges a compose network draws from, and the
# port is not published unless a deployment publishes it.
put_param "$ESL" listen-ip   "${FS_ESL_LISTEN_IP:-0.0.0.0}"
put_param "$ESL" listen-port "${FS_ESL_PORT:-18021}"
put_param "$ESL" password    "${FS_ESL_PASSWORD:-ClueCon}"
put_param "$ESL" apply-inbound-acl "${FS_ESL_ACL:-aicc_esl}"
for v in FS_ESL_LISTEN_IP FS_ESL_PORT FS_ESL_ACL; do
  if [ -n "${!v:-}" ]; then note "$v"; fi
done
if [ -n "${FS_ESL_PASSWORD:-}" ] && [ "$FS_ESL_PASSWORD" != "ClueCon" ]; then
  note FS_ESL_PASSWORD
else
  echo "[entrypoint] warning: the event socket password is the stock ClueCon; set FS_ESL_PASSWORD" >&2
fi

# ---- switch.conf -----------------------------------------------------------
#
# Optional, all three. Unset, the core database is the shipped SQLite one and
# the RTP range is whatever the tree ships — both working answers.
#
# A deployment that widens the RTP range has to publish the same range on the
# container, or media from outside the host arrives at a port nothing is
# listening on.
if [ -n "${FS_CORE_DB_DSN:-}" ];    then put_param "$SWITCH" core-db-dsn "$FS_CORE_DB_DSN"; note FS_CORE_DB_DSN; fi
if [ -n "${FS_RTP_START_PORT:-}" ]; then put_param "$SWITCH" rtp-start-port "$FS_RTP_START_PORT"; note FS_RTP_START_PORT; fi
if [ -n "${FS_RTP_END_PORT:-}" ];   then put_param "$SWITCH" rtp-end-port "$FS_RTP_END_PORT"; note FS_RTP_END_PORT; fi

# ---- callcenter ------------------------------------------------------------
#
# Optional. mod_callcenter keeps its own tables and defaults to SQLite inside
# the container, which works and does not survive the container. A deployment
# points it at the aicc_fs database — the module's tables are unqualified in
# public and would collide with the application's.
if [ -n "${FS_CALLCENTER_DSN:-}" ]; then put_param "$CC" odbc-dsn "$FS_CALLCENTER_DSN"; note FS_CALLCENTER_DSN; fi

# ---- http_cache S3 profile -------------------------------------------------
#
# Written only when the whole credential is present; there is no half of an S3
# profile that is useful. mod_http_cache matches a profile by exact URL
# hostname, so the host formed from FS_S3_BUCKET and FS_S3_BASE_DOMAIN has to
# equal the host in AICC_RECORDINGS_DIR.
if [ -n "${FS_S3_ACCESS_KEY_ID:-}" ] && [ -n "${FS_S3_SECRET_ACCESS_KEY:-}" ] && [ -n "${FS_S3_BASE_DOMAIN:-}" ]; then
  note FS_S3_ACCESS_KEY_ID; note FS_S3_SECRET_ACCESS_KEY; note FS_S3_BASE_DOMAIN
  if [ -n "${FS_S3_REGION:-}" ]; then note FS_S3_REGION; fi
  if [ -n "${FS_S3_BUCKET:-}" ]; then note FS_S3_BUCKET; fi
  if ! grep -q '<profiles>' "$HC"; then
    block="  <profiles>\n    <profile name=\"s3\">\n      <aws-s3>\n        <access-key-id>$(esc "$(xml "$FS_S3_ACCESS_KEY_ID")")</access-key-id>\n        <secret-access-key>$(esc "$(xml "$FS_S3_SECRET_ACCESS_KEY")")</secret-access-key>\n        <base-domain>$(esc "$(xml "$FS_S3_BASE_DOMAIN")")</base-domain>\n        <region>$(esc "$(xml "${FS_S3_REGION:-us-east-1}")")</region>\n      </aws-s3>\n      <domains>\n        <domain name=\"$(esc "$(xml "${FS_S3_BUCKET:+$FS_S3_BUCKET.}$FS_S3_BASE_DOMAIN")")\"/>\n      </domains>\n    </profile>\n  </profiles>\n</configuration>"
    sed -i "s|</configuration>|$block|" "$HC"
  fi
fi

# ---- live transcription needs mod_audio_stream -----------------------------
#
# This image builds the module, so the check below looks redundant. It is kept
# because the guarantee that matters is not "the image has the module" — that
# is a property of whatever built the image, and a future one may drop it
# without anybody noticing — but "a stack told to transcribe cannot start
# without it".
#
# The failure it prevents is the worst kind available here. Without the module
# the switch cannot tap an agent's leg, and nothing anywhere reports a fault:
# the application is healthy, the switch is healthy, calls connect, agents
# answer, and the transcript panel says "Connecting..." until the call ends.
# From each component's own point of view there is nothing wrong.
#
# Design note: docs/design/08-transcription.md §B.5.
MOD_DIRS=${AICC_FS_MOD_DIR:-$FS_HOME/mod /usr/lib/freeswitch/mod}
AUDIO_STREAM_SO=""
for dir in $MOD_DIRS; do
  if [ -f "$dir/mod_audio_stream.so" ]; then AUDIO_STREAM_SO="$dir/mod_audio_stream.so"; break; fi
done

case "${AICC_TRANSCRIPTION_ENABLED:-false}" in
  [Tt]rue|1|[Yy]es)
    if [ -z "$AUDIO_STREAM_SO" ]; then
      cat >&2 <<'MISSING'
[entrypoint] transcription is enabled but mod_audio_stream is not in this image.

Refusing to start. Without it the switch cannot tap an agent's leg, so the
stack would come up entirely healthy and transcribe nothing at all: the
application would be fine, the switch would be fine, calls would connect, and
the agent's transcript panel would say "Connecting..." until the call ended.

Either build the module into the image (freeswitch/Dockerfile does, and runs
freeswitch/assert-audio-stream.sh against the result — it must declare
libspeexdsp or it will tap at the wrong rate), or set
AICC_TRANSCRIPTION_ENABLED=false for a stack that honestly does not transcribe.
MISSING
      exit 1
    fi
    if ! grep -q '<load module="mod_audio_stream"/>' "$MODULES"; then
      sed -i -e 's|<load module="mod_callcenter"/>|<load module="mod_callcenter"/>\n    <load module="mod_audio_stream"/>|' "$MODULES"
    fi
    grep -q '<load module="mod_audio_stream"/>' "$MODULES" || {
      echo "[entrypoint] could not enable mod_audio_stream" >&2; exit 1
    }
    echo "[entrypoint] live transcription enabled, mod_audio_stream at $AUDIO_STREAM_SO"
    note "transcription=on"
    ;;
  *)
    # Said once, so that a stack which is not transcribing is never a surprise
    # to whoever reads the log.
    echo "[entrypoint] live transcription disabled (AICC_TRANSCRIPTION_ENABLED is not true)"
    ;;
esac

# ---- TLS ------------------------------------------------------------------
#
# Self-signed when absent, so WSS and DTLS-SRTP work out of the box. Mount
# /usr/local/freeswitch/certs to supply real ones.
CERTS="$FS_HOME/certs"
gen_cert() {
  local out="$1" tmp; tmp="$(mktemp -d)"
  openssl req -x509 -nodes -newkey rsa:2048 -days 3650 -subj "/CN=${FS_DOMAIN:-freeswitch}" \
    -keyout "$tmp/key.pem" -out "$tmp/crt.pem" >/dev/null 2>&1
  cat "$tmp/crt.pem" "$tmp/key.pem" > "$out"; rm -rf "$tmp"; chmod 600 "$out"
}
mkdir -p "$CERTS"
for c in wss.pem dtls-srtp.pem; do
  [ -s "$CERTS/$c" ] || { gen_cert "$CERTS/$c"; note "cert:$c(self-signed)"; }
done

# The switch does not run as root; every directory it writes to has to be its
# own, including the configuration it rewrites on a reload.
chown -R freeswitch:freeswitch "$FS_HOME"/{db,log,recordings,storage,cache,run,certs,conf} 2>/dev/null || true
case "$AICC_RECORDINGS_DIR" in
  /*) chown -R freeswitch:freeswitch "$AICC_RECORDINGS_DIR" 2>/dev/null || true ;;
esac

# Names only. A value printed here is a value in somebody's log aggregator.
echo "[entrypoint] injected: ${INJECTED[*]:-(none)}"
exec "$@"
