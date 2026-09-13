#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the release notes for one tag on stdout; the release workflow redirects
# that into a file and hands it to the GitHub release as `body_path`.
#
#   scripts/release-notes.sh v0.1.0
#
# Where the body comes from, in order:
#
#   1. CHANGELOG.md, if it exists and holds a section for this tag. A heading is
#      recognised as `## [v0.1.0]`, `## v0.1.0`, `## [0.1.0]` or `## 0.1.0`, with
#      an optional trailing ` - 2026-09-11`. The section runs to the next `## `.
#   2. Otherwise the commit subjects since the previous `v*` tag, or the whole
#      log when this is the first tag.
#
# Either way the image is named at the end, because the image is the release and
# a reader needs the line that pulls it.
set -euo pipefail

tag="${1:-}"
if [ -z "$tag" ]; then
	echo "usage: $(basename "$0") <tag>" >&2
	exit 2
fi

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# The tag with any leading `v` removed, so a CHANGELOG written either way matches.
bare="${tag#v}"

changelog_section() {
	[ -f CHANGELOG.md ] || return 1
	awk -v tag="$tag" -v bare="$bare" '
		function is_heading(line,    body) {
			if (line !~ /^## /) return 0
			body = substr(line, 4)
			sub(/^[ \t]+/, "", body)
			sub(/[ \t]+$/, "", body)
			# Drop an optional trailing " - <date>" or " — <date>". The dash has
			# to be set off by whitespace: the hyphen in v0.1.0-rc.1 is the tag.
			sub(/[ \t]+[-–—].*$/, "", body)
			sub(/[ \t]+$/, "", body)
			# Drop the brackets of a Keep-a-Changelog style heading.
			if (body ~ /^\[.*\]$/) body = substr(body, 2, length(body) - 2)
			heading = body
			return 1
		}
		{
			if (is_heading($0)) {
				if (inside) exit
				if (heading == tag || heading == bare) { inside = 1; found = 1; next }
				next
			}
			if (inside) print
		}
		END { exit(found ? 0 : 1) }
	' CHANGELOG.md
}

previous_tag() {
	git describe --tags --abbrev=0 --match 'v*' "${tag}^" 2>/dev/null || true
}

commit_log() {
	local prev
	prev="$(previous_tag)"
	if [ -n "$prev" ]; then
		echo "## Changes since ${prev}"
		echo
		git log --no-merges --format='- %s (%h)' "${prev}..${tag}"
	else
		echo "## Changes in ${tag}"
		echo
		git log --no-merges --format='- %s (%h)' "${tag}"
	fi
}

body="$(changelog_section || true)"
if [ -n "${body//[[:space:]]/}" ]; then
	# Trim the blank lines a section picks up at either end.
	printf '%s\n' "$body" | awk '
		{ lines[NR] = $0; if ($0 ~ /[^ \t]/) { if (!first) first = NR; last = NR } }
		END { for (i = first; i <= last; i++) print lines[i] }'
else
	commit_log
fi

cat <<EOF

## The image

\`\`\`sh
docker pull rasonyang/ai-native-callcenter:${tag}
\`\`\`

\`linux/amd64\` and \`linux/arm64\`, one executable with the SPA inside it and
nothing else. Only exact tags are published — there is no \`latest\`, so a
deployment names the build it runs.

Point \`AICC_IMAGE\` at it and the stack pulls instead of building; the switch
that goes with it is \`rasonyang/freeswitch-aicc\`, and
[deploy/README.md](deploy/README.md) is the whole procedure.
EOF
