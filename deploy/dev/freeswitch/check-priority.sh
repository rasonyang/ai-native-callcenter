#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Check that no thread of the running native macOS FreeSWITCH sits below
# priority 4, the edge of the throttled tier that runs only on the efficiency
# cores (#451). Run it before a live call round. Read-only.
#
# Exit 0: every thread is at 4 or above. Exit 1: a thread is below 4 (the
# switch was not started through start.sh) or there is no single switch.
set -eu

die() {
	echo "check-priority.sh: $1" >&2
	exit 1
}

pids=$(pgrep -x freeswitch || true)
[ -n "$pids" ] || die "no running freeswitch"
[ "$(printf '%s\n' "$pids" | wc -l)" -eq 1 ] || die "more than one freeswitch is running: $(printf '%s' "$pids" | tr '\n' ' ')"

# ps -M prints one line per thread, with the priority as the last field and a
# letter after it (31T). The first line is the header.
rc=0
counts=$(ps -M -o pri -p "$pids" |
	awk '
		NR == 1 { next }
		{
			p = $NF
			sub(/[^0-9]+$/, "", p)
			count[p]++
			if (p + 0 < 4) low += 1
		}
		END {
			for (p in count) printf "%d %d\n", p, count[p]
			exit (low > 0)
		}') || rc=$?

echo "freeswitch pid $pids: threads per priority"
printf '%s\n' "$counts" | sort -n | awk '{ printf "  priority %3d: %d threads\n", $1, $2 }'

if [ "$rc" -ne 0 ]; then
	low=$(printf '%s\n' "$counts" | awk '$1 < 4 { n += $2 } END { print n + 0 }')
	echo "check-priority.sh: $low thread(s) below priority 4; start the switch through deploy/dev/freeswitch/start.sh (#451)" >&2
	exit 1
fi
