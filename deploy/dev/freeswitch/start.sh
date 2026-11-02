#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Start the native macOS FreeSWITCH with libaicc_keep_priority.dylib loaded, so
# its threads are not lowered into the efficiency-core tier (#451). Run
# install.sh first: it builds the library next to the switch.
#
# Arguments go to bin/freeswitch unchanged; with none, it starts with -nonat.
#
# Usage:  ./start.sh
#         ./start.sh -nc -nonat
#         FS_ROOT=/opt/freeswitch ./start.sh
set -eu

FS_ROOT=${FS_ROOT:-/usr/local/freeswitch}
LIB=$FS_ROOT/lib/libaicc_keep_priority.dylib

die() {
	echo "start.sh: $1" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || die "only macOS lowers thread priorities this way; start the switch as usual"
[ -f "$LIB" ] || die "no $LIB; run deploy/dev/freeswitch/install.sh first"
[ -x "$FS_ROOT/bin/freeswitch" ] || die "no executable $FS_ROOT/bin/freeswitch (set FS_ROOT)"

if [ "$#" -eq 0 ]; then
	set -- -nonat
fi

cd "$FS_ROOT"
exec env DYLD_INSERT_LIBRARIES="$LIB" bin/freeswitch "$@"
