#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# Creates the confined role FreeSWITCH reads the luacc views with, once the
# application has created them.
#
# Role creation deliberately lives outside the migrations (deploy/sql/
# lua_role.sql explains why), which leaves a gap in an automated stack: the
# grants need the views, the views arrive when the application migrates, and
# the switch needs the role before it answers its first registration. This
# waits for the first, does the second, and lets compose hold back the third.
set -euo pipefail

: "${LUA_PASSWORD:?set the database password the switch reads with}"

echo "lua-role: waiting for the application to create the luacc views"
for attempt in $(seq 1 120); do
    if psql -h postgres -U aicc -d aicc -tAc \
        "SELECT 1 FROM information_schema.schemata WHERE schema_name = 'luacc'" \
        2>/dev/null | grep -q 1; then
        break
    fi
    if [ "$attempt" -eq 120 ]; then
        echo "lua-role: the luacc views never appeared; is the application healthy?" >&2
        exit 1
    fi
    sleep 2
done

psql -h postgres -U aicc -d aicc -v ON_ERROR_STOP=1 \
    -v lua_password="'${LUA_PASSWORD}'" -f /lua_role.sql

echo "lua-role: aicc_lua may read the luacc views"
