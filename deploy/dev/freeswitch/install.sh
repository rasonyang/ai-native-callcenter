#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Install this machine's FreeSWITCH overlay into a native FreeSWITCH tree, then
# tell the running switch to read it.
#
# It copies two files and reloads. It does not touch vars.xml: that file holds
# database passwords, and the operator edits it by hand from
# vars.d.example.xml. If a variable this overlay needs is missing there, the
# reload below will succeed and the calls will not — see the README.
#
# Usage:  ./install.sh
#         FS_ROOT=/opt/freeswitch ESL_PASSWORD=secret ./install.sh
set -eu

SRC_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

FS_ROOT=${FS_ROOT:-/usr/local/freeswitch}
FS_CONF=${FS_CONF:-$FS_ROOT/conf}
FS_CLI=${FS_CLI:-$FS_ROOT/bin/fs_cli}
ESL_HOST=${ESL_HOST:-127.0.0.1}
ESL_PORT=${ESL_PORT:-18021}
ESL_PASSWORD=${ESL_PASSWORD:-ClueCon}

STAMP=$(date +%Y%m%d%H%M%S)

die() {
	echo "install.sh: $1" >&2
	exit 1
}

# Refuse rather than create. A missing destination tree means FS_ROOT is wrong,
# and inventing the directories would leave a plausible-looking overlay under a
# path no switch reads.
[ -d "$FS_CONF" ] || die "no FreeSWITCH configuration at $FS_CONF (set FS_ROOT)"
[ -f "$FS_CONF/vars.xml" ] || die "$FS_CONF has no vars.xml; that is not a FreeSWITCH conf tree"
[ -d "$FS_CONF/dialplan" ] || die "$FS_CONF has no dialplan directory"
[ -d "$FS_CONF/sip_profiles/external" ] || die "$FS_CONF has no sip_profiles/external directory"

# dialplan/aicc/ is the include point aicc.xml opens with
# <X-PRE-PROCESS cmd="include" data="aicc/*.xml"/>. It exists only once
# something has been dropped into it, so create it if it is absent.
[ -d "$FS_CONF/dialplan/aicc" ] || mkdir -p "$FS_CONF/dialplan/aicc"

install_file() {
	src=$1
	dst=$2
	if [ -f "$dst" ]; then
		if cmp -s "$src" "$dst"; then
			echo "unchanged  $dst"
			return 0
		fi
		cp -p "$dst" "$dst.bak-$STAMP"
		echo "backed up  $dst.bak-$STAMP"
	fi
	cp "$src" "$dst"
	echo "installed  $dst"
}

install_file "$SRC_DIR/dialplan/aicc/00_pstn_gateway.xml" \
	"$FS_CONF/dialplan/aicc/00_pstn_gateway.xml"
install_file "$SRC_DIR/sip_profiles/external/pstn_gateway.xml" \
	"$FS_CONF/sip_profiles/external/pstn_gateway.xml"

if [ ! -x "$FS_CLI" ]; then
	echo "install.sh: no fs_cli at $FS_CLI; files are in place, reload by hand" >&2
	exit 0
fi

fs() {
	"$FS_CLI" -H "$ESL_HOST" -P "$ESL_PORT" -p "$ESL_PASSWORD" -x "$1"
}

# reloadxml picks up the dialplan include. A sofia rescan is what picks up the
# gateway: reloadxml alone re-reads the XML without rebuilding the profile's
# gateways, so a changed proxy address would sit in the file and not in the
# switch.
echo "--- reloadxml"
fs "reloadxml"
echo "--- sofia profile external rescan"
fs "sofia profile external rescan"
echo "--- sofia status gateway pstn_gateway"
fs "sofia status gateway pstn_gateway"
