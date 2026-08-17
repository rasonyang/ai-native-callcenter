#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Asserts that a built mod_audio_stream is one that can actually resample.
#
# The module compiles and loads perfectly well without SpeexDSP linked. Its
# CMakeLists includes speex/speex_resampler.h but never links the library, and
# on Debian the symbols resolve anyway from FreeSWITCH's own copy already in
# the process. That build is indistinguishable from a good one at load time —
# module_exists says true, uuid_audio_stream registers, an attach returns +OK.
#
# It goes wrong later and quietly. speex_resampler_init fails or is absent, the
# glue leaves its resampler null, and the send path forwards frames at the
# channel's own rate instead of the rate that was asked for. So a stream
# requested at 24000 arrives as 8000 labelled 24000: the recogniser hears
# chipmunk speech and returns confident, wrong words. Nothing errors.
#
# The check is therefore on the *declaration*, not on the symbols. Undefined
# speex symbols are normal for a dynamically linked module; a missing NEEDED
# entry is what says nobody linked it on purpose.
#
# Usage: assert-audio-stream.sh [path-to-mod_audio_stream.so]

set -e

SO=${1:-/usr/local/freeswitch/mod/mod_audio_stream.so}

if [ ! -f "$SO" ]; then
    echo "assert-audio-stream: $SO does not exist" >&2
    exit 1
fi

# Linux carries the declaration in DT_NEEDED; macOS in the load commands.
if command -v readelf >/dev/null 2>&1; then
    LINKED=$(readelf -d "$SO" 2>/dev/null | grep NEEDED | grep -c speexdsp || true)
elif command -v otool >/dev/null 2>&1; then
    LINKED=$(otool -L "$SO" 2>/dev/null | grep -c speexdsp || true)
else
    echo "assert-audio-stream: neither readelf nor otool is available" >&2
    exit 1
fi

if [ "$LINKED" -eq 0 ]; then
    cat >&2 <<'WHY'
assert-audio-stream: mod_audio_stream does not declare libspeexdsp.

It will still load, and it will still accept a rate argument. What it will not
do is resample: the requested rate becomes a label on audio at the channel's
own rate, and the recogniser transcribes chipmunk speech into confident, wrong
words with nothing reporting an error.

Build it with SpeexDSP linked explicitly — its own CMakeLists includes the
header without linking the library, so this succeeds by accident on some
distributions and must not be relied on:

  cmake -DCMAKE_SHARED_LINKER_FLAGS="-lspeexdsp" ...
WHY
    exit 1
fi

echo "assert-audio-stream: OK — $SO declares libspeexdsp"
