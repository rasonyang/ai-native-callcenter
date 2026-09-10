# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0
#
# The switch, built. FreeSWITCH v1.11.3 compiled from source, plus
# mod_audio_stream, plus *this repository's* configuration and Lua scripts.
#
#   docker buildx build -f freeswitch/Dockerfile freeswitch/     # or: freeswitch/build.sh
#
# The build context is freeswitch/, so every path below is relative to that
# directory. Nothing is read from the machine doing the build: the FreeSWITCH
# and mod_audio_stream sources are cloned at pinned refs, the sound files are
# downloaded at pinned versions and checked by sha256, and the configuration
# comes from conf/ and scripts/ in this repository.
#
# That last part is the reason this file exists rather than the recipe it was
# ported from. The published v1.11.3 image staged its build context by rsyncing
# the *build host's* /usr/local/freeswitch/{conf,scripts,sounds} into it, which
# put three real credentials, one office's ACL subnets, a developer's LAN
# address and a development PSTN trunk inside a public image. Everything
# environment-specific now arrives at container start through the environment
# variables docker-entrypoint.sh reads, and the image carries only defaults
# that are unmistakably defaults.
#
# What is kept from the published recipe is kept because it was verified on the
# live switch and in the published image: the v1.11.3 tag, the sofia-sip and
# spandsp commit pins (the packages branch of spandsp is 3.0.0 and fails
# FreeSWITCH's >= 3.1.1 check), the module list in modules.conf, the two-stage
# build onto debian:bookworm-slim, the OCI labels, the fs_cli healthcheck, the
# multi-arch build and the entrypoint's environment-variable contract.

# --- pins ---------------------------------------------------------------------
#
# FROM-scope. A global ARG is not visible inside a stage; each stage that wants
# one redeclares it by name, which is why the ARG lines repeat below.
ARG SOFIASIP_REF=ad36ac8f755308e8b87f98a505e83d4e408e5cc3
ARG SPANDSP_REF=8f1e1646bdec99eac5fd2cd92c35563f736b9b89
ARG FS_VERSION=v1.11.3

# mod_audio_stream comes from public upstream at a pinned commit.
#
# The published image was built from a private fork whose two extra commits —
# one of them the SpeexDSP fix below — are unpushed, so nothing outside the
# author's laptop can reproduce it. Upstream main is ec2a781 as of 2026-09-09
# and is what the fork was branched from; the fork's own public main is the
# same commit.
ARG MOD_AUDIO_STREAM_REPO=https://github.com/amigniter/mod_audio_stream.git
ARG MOD_AUDIO_STREAM_COMMIT=ec2a781026a1c09b0c3a6161b8b896f0b9305922
# libs/libwsc, as pinned by that commit. Asserted during the build rather than
# trusted, so a wrong submodule state is a build failure and not a label lie.
ARG LIBWSC_COMMIT=09d18e5db9243a996692261a7a0839c9c346bc47

# Sound files. 8 kHz only by default — the 32k and 48k sets are another ~450MB
# and nothing in this product plays a prompt above narrowband.
#   SOUNDS=8000  fetch the two tarballs below (default)
#   SOUNDS=none  ship no sounds at all
# Without them the switch still runs and calls still connect; what stops
# working is queue hold music (music/8000, which mod_local_stream serves as
# moh_stream) and the stock English prompts. An AI call plays neither.
ARG SOUNDS=8000
ARG SOUNDS_CALLIE_VERSION=1.0.53
ARG SOUNDS_CALLIE_SHA256=24a2baad88696169950c84cafc236124b2bfa63114c7c8ac7d330fd980c8db05
ARG SOUNDS_MUSIC_VERSION=1.0.52
ARG SOUNDS_MUSIC_SHA256=2491dcb92a69c629b03ea070d2483908a52e2c530dd77791f49a45a4d70aaa07

# Stamped into a label by build.sh so a pushed image names the commit it was
# built from. Left "unknown" when someone runs docker build by hand.
ARG GIT_REVISION=unknown

ARG MAKE_JOBS=

########## sounds ##############################################################
#
# Pinned by $BUILDPLATFORM: sound files are architecture-independent, and
# running the download natively keeps it out of the emulated half of a
# multi-arch build.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS sounds
ARG SOUNDS
ARG SOUNDS_CALLIE_VERSION
ARG SOUNDS_CALLIE_SHA256
ARG SOUNDS_MUSIC_VERSION
ARG SOUNDS_MUSIC_SHA256
ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# The checksums above are the publisher's own .tar.gz.sha256 files, verified on
# 2026-09-09 against a fresh download of each tarball. files.freeswitch.org
# publishes .sha256 only from release 1.0.50 onwards; an older pin has md5 and
# sha1 only and cannot be checked this way.
RUN mkdir -p /sounds && cd /sounds \
    && if [ "$SOUNDS" = "none" ]; then \
         echo "sounds: skipped (SOUNDS=none); queue hold music will not play"; \
       else \
         base=https://files.freeswitch.org/releases/sounds; \
         callie="freeswitch-sounds-en-us-callie-8000-${SOUNDS_CALLIE_VERSION}.tar.gz"; \
         music="freeswitch-sounds-music-8000-${SOUNDS_MUSIC_VERSION}.tar.gz"; \
         curl -fsSL -o "$callie" "$base/$callie" && echo "$SOUNDS_CALLIE_SHA256  $callie" | sha256sum -c - && tar xzf "$callie" && rm "$callie"; \
         curl -fsSL -o "$music"  "$base/$music"  && echo "$SOUNDS_MUSIC_SHA256  $music"   | sha256sum -c - && tar xzf "$music"  && rm "$music"; \
       fi

########## builder #############################################################
FROM debian:bookworm AS builder
ARG SOFIASIP_REF
ARG SPANDSP_REF
ARG FS_VERSION
ARG MOD_AUDIO_STREAM_REPO
ARG MOD_AUDIO_STREAM_COMMIT
ARG LIBWSC_COMMIT
ARG MAKE_JOBS
ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential autoconf automake libtool libtool-bin pkg-config cmake git ca-certificates \
      libpq-dev libssl-dev libcurl4-openssl-dev libpcre2-dev libspeex-dev libspeexdsp-dev \
      libedit-dev libsqlite3-dev libtiff-dev libjpeg62-turbo-dev uuid-dev libldns-dev \
      libopus-dev liblua5.3-dev libsndfile1-dev libevent-dev zlib1g-dev libjbig-dev \
      libdb-dev libncurses-dev libogg-dev libvorbis-dev libflac-dev yasm \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src

# sofia-sip, pinned.
RUN git clone https://github.com/freeswitch/sofia-sip.git && cd sofia-sip \
    && git checkout "$SOFIASIP_REF" \
    && ./autogen.sh && ./configure --prefix=/usr/local --with-pic --with-glib=no --without-doxygen --disable-stun \
    && make -j"${MAKE_JOBS:-$(nproc)}" && make install

# spandsp, pinned on master (3.1.1). The packages branch is 3.0.0, which
# FreeSWITCH's configure rejects as older than the 3.1.1 it requires.
RUN git clone https://github.com/freeswitch/spandsp.git && cd spandsp \
    && git checkout "$SPANDSP_REF" \
    && ./autogen.sh && ./configure --prefix=/usr/local --with-pic \
    && make -j"${MAKE_JOBS:-$(nproc)}" && make install

ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig
RUN echo /usr/local/freeswitch/lib > /etc/ld.so.conf.d/freeswitch.conf && ldconfig

# FreeSWITCH itself, at the pinned tag. modules.conf is the build list and is
# checked against what the shipped configuration actually loads, further down.
RUN git clone --depth 1 --branch "$FS_VERSION" https://github.com/signalwire/freeswitch.git
COPY modules.conf /usr/src/freeswitch/modules.conf
WORKDIR /usr/src/freeswitch
RUN ./bootstrap.sh -j \
    && ./configure --prefix=/usr/local/freeswitch --disable-fhs --with-modinstdir=/usr/local/freeswitch/mod --disable-libvpx --without-python3 --disable-dependency-tracking \
    && make -j"${MAKE_JOBS:-$(nproc)}" && make install

# mod_audio_stream, pinned, with its libs/libwsc submodule.
#
# Upstream's own build script uses `git submodule init/update`, which does not
# recurse and leaves libs/libwsc empty; --recursive is required. The submodule
# commit the pin names is asserted rather than assumed, so the LIBWSC_COMMIT
# label cannot drift away from what was built.
#
# The WORKDIR is not decoration either: the FreeSWITCH build above leaves it at
# /usr/src/freeswitch, and a relative clone from there lands the module inside
# the FreeSWITCH tree. The next WORKDIR then creates an empty
# /usr/src/mod_audio_stream and cmake reports a source directory with no
# CMakeLists.txt, which reads as a bad checkout rather than a wrong path.
WORKDIR /usr/src
RUN git clone "$MOD_AUDIO_STREAM_REPO" mod_audio_stream && cd mod_audio_stream \
    && git checkout "$MOD_AUDIO_STREAM_COMMIT" \
    && git submodule update --init --recursive \
    && actual="$(git -C libs/libwsc rev-parse HEAD)" \
    && if [ "$actual" != "$LIBWSC_COMMIT" ]; then \
         echo "libs/libwsc is $actual, expected $LIBWSC_COMMIT" >&2; exit 1; \
       fi

# -Wl,--no-as-needed is not decoration.
#
# Upstream calls speex_resampler_* and find_package(SpeexDSP REQUIRED)s the
# library, but never puts it in target_link_libraries. On Debian the symbols
# resolve anyway from the copy FreeSWITCH already has mapped, so the module
# builds, loads and registers — and then silently does not resample, handing a
# stream requested at 24000 the channel's own 8000 wearing a 24000 label. The
# fork that fixed this in CMakeLists is unpushed (see the pin above), so the
# fix is made here on the link line instead.
#
# Plain `-lspeexdsp` does not work: gcc on Debian defaults to --as-needed and
# CMake puts CMAKE_SHARED_LINKER_FLAGS *before* the object files, so the
# library has no outstanding undefined symbols when the linker reaches it and
# is dropped without a word. Measured on debian:bookworm/amd64, gcc 12.2.0,
# GNU ld 2.40, with a translation unit that really calls speex_resampler_init:
# `-shared -lspeexdsp -o a.so u.o` produced no DT_NEEDED entry at all, while
# both `-Wl,--no-as-needed -lspeexdsp` and a trailing `-lspeexdsp` produced
# libspeexdsp.so.1.
#
# Two side effects: --no-as-needed persists for every -l after it, so a few
# unused libraries also collect DT_NEEDED entries (harmless, just a longer
# list); and upstream's LINK_FLAGS_RELEASE "-s" strips the module without
# touching the dynamic section, so the assertion below still sees the entry.
WORKDIR /usr/src/mod_audio_stream
RUN mkdir -p build && cd build \
    && PKG_CONFIG_PATH=/usr/local/freeswitch/lib/pkgconfig:/usr/local/lib/pkgconfig \
       cmake -DCMAKE_BUILD_TYPE=Release \
             -DCMAKE_SHARED_LINKER_FLAGS="-Wl,--no-as-needed -lspeexdsp" .. \
    && make -j"${MAKE_JOBS:-$(nproc)}" && make install \
    && test -f /usr/local/freeswitch/mod/mod_audio_stream.so

# The gate. A module that compiles and loads without SpeexDSP linked is
# indistinguishable from a working one until a recogniser starts returning
# confident nonsense; this turns that into a build failure. Read the script —
# it explains the failure in full and checks the DT_NEEDED declaration, not the
# symbols, because undefined speex symbols are normal in a dynamic module.
COPY assert-audio-stream.sh /usr/local/bin/assert-audio-stream.sh
RUN chmod +x /usr/local/bin/assert-audio-stream.sh \
    && /usr/local/bin/assert-audio-stream.sh /usr/local/freeswitch/mod/mod_audio_stream.so

# The configuration `make install` laid down is vanilla and is replaced
# wholesale by this repository's tree in the runtime stage. Headers, libtool
# archives and the module .la files are build residue.
RUN rm -rf /usr/local/freeswitch/conf/* /usr/local/freeswitch/include /usr/local/freeswitch/lib/*.la \
    && find /usr/local/freeswitch/mod -name '*.la' -delete \
    && ls /usr/local/freeswitch/mod/*.so | wc -l

########## runtime #############################################################
FROM debian:bookworm-slim
ARG FS_VERSION
ARG MOD_AUDIO_STREAM_COMMIT
ARG LIBWSC_COMMIT
ARG SOFIASIP_REF
ARG SPANDSP_REF
ARG GIT_REVISION
ENV DEBIAN_FRONTEND=noninteractive

# image.source is this repository, because the recipe and the configuration are
# what make this image what it is; freeswitch.upstream keeps the provenance of
# the code it compiles. image.revision is the aicc commit build.sh was run at.
LABEL org.opencontainers.image.title="freeswitch-aicc" \
      org.opencontainers.image.description="FreeSWITCH built for the AI-native call center, with mod_audio_stream" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/rasonyang/ai-native-callcenter" \
      org.opencontainers.image.revision="${GIT_REVISION}" \
      org.opencontainers.image.version="${FS_VERSION}" \
      freeswitch.upstream="https://github.com/signalwire/freeswitch" \
      freeswitch.version="${FS_VERSION}" \
      mod_audio_stream.commit="${MOD_AUDIO_STREAM_COMMIT}" \
      libwsc.commit="${LIBWSC_COMMIT}" \
      sofia-sip.commit="${SOFIASIP_REF}" \
      spandsp.commit="${SPANDSP_REF}"

RUN apt-get update && apt-get install -y --no-install-recommends \
      libpq5 libssl3 libcurl4 libpcre2-8-0 libspeex1 libspeexdsp1 libedit2 libsqlite3-0 libtiff6 \
      libjpeg62-turbo libuuid1 libldns3 libopus0 liblua5.3-0 libsndfile1 libevent-2.1-7 libevent-pthreads-2.1-7 zlib1g \
      libjbig0 libdb5.3 libncurses6 libtinfo6 ca-certificates procps openssl gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r freeswitch && useradd -r -g freeswitch -d /usr/local/freeswitch -s /usr/sbin/nologin freeswitch

COPY --from=builder /usr/local/lib/libsofia-sip-ua.so* /usr/local/lib/libspandsp.so* /usr/local/lib/
COPY --from=builder /usr/local/freeswitch /usr/local/freeswitch
RUN echo /usr/local/freeswitch/lib > /etc/ld.so.conf.d/freeswitch.conf && ldconfig

# The configuration and the Lua scripts, from this repository. conf/ is a
# complete FreeSWITCH configuration tree and replaces the vanilla one entirely;
# scripts/ is the four aicc_*.lua files mod_lua's XML handler runs.
COPY conf/ /usr/local/freeswitch/conf/
COPY scripts/ /usr/local/freeswitch/scripts/
COPY --from=sounds /sounds/ /usr/local/freeswitch/sounds/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Every module the shipped configuration loads must be a module this image
# actually built. modules.conf (the build list) and modules.conf.xml (the load
# list) are two files that have to agree, and when they do not FreeSWITCH says
# so once in a log nobody is reading and then behaves as though the feature was
# never asked for — a queue with no mod_callcenter answers nothing. Comments
# are stripped first so a commented-out <load/> does not count.
RUN set -e; \
    strip_comments() { \
      awk '{ line=$0; while (1) { if (c==0) { i=index(line,"<!--"); if (i==0) { print line; break } print substr(line,1,i-1); line=substr(line,i+4); c=1 } else { j=index(line,"-->"); if (j==0) break; line=substr(line,j+3); c=0 } } }' "$1"; \
    }; \
    missing=""; \
    for f in /usr/local/freeswitch/conf/autoload_configs/modules.conf.xml \
             /usr/local/freeswitch/conf/autoload_configs/pre_load_modules.conf.xml; do \
      [ -f "$f" ] || continue; \
      for m in $(strip_comments "$f" | grep -oE 'module="[a-z_0-9]+"' | sed 's/module="\(.*\)"/\1/' | sort -u); do \
        [ -f "/usr/local/freeswitch/mod/$m.so" ] || missing="$missing $m"; \
      done; \
    done; \
    if [ -n "$missing" ]; then \
      echo "the shipped configuration loads modules this image does not build:$missing" >&2; \
      echo "add them to freeswitch/modules.conf, or stop loading them in conf/autoload_configs/" >&2; \
      exit 1; \
    fi; \
    echo "modules: every <load/> in the shipped configuration is present"

RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
    && cd /usr/local/freeswitch && mkdir -p db log recordings storage cache run certs htdocs grammar sounds \
    && chown -R freeswitch:freeswitch db log recordings storage cache run certs conf scripts \
    && ln -sf /usr/local/freeswitch/bin/freeswitch /usr/local/bin/freeswitch \
    && ln -sf /usr/local/freeswitch/bin/fs_cli /usr/local/bin/fs_cli

# Read by the healthcheck as well as by the entrypoint, so they cannot disagree
# about which port and password fs_cli should use.
#
# `docker buildx build --check` flags FS_ESL_PASSWORD as SecretsUsedInArgOrEnv,
# and it is right in general and wrong here: ClueCon is FreeSWITCH's published
# stock password, kept so the healthcheck has something to use when nobody set
# one, and the entrypoint warns on every start that it is still in place. A
# deployment passes -e FS_ESL_PASSWORD and this default is never read.
ENV FS_ESL_PORT=18021 \
    FS_ESL_PASSWORD=ClueCon

WORKDIR /usr/local/freeswitch
VOLUME ["/usr/local/freeswitch/db", "/usr/local/freeswitch/log", "/usr/local/freeswitch/recordings", "/usr/local/freeswitch/storage"]
EXPOSE 5060/udp 5060/tcp 5080/udp 5080/tcp 5066/tcp 7443/tcp 18021/tcp 16384-32768/udp

HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
  CMD fs_cli -H 127.0.0.1 -P "${FS_ESL_PORT}" -p "${FS_ESL_PASSWORD}" -x status | grep -q 'UP' || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
# -nf keeps the switch in the foreground, which is what a container needs.
#
# -nc is deliberately absent. It suppresses the console, and mod_logfile is
# then the only place anything is written — so `docker logs` shows the
# entrypoint's few lines and nothing else, and diagnosing a switch means
# exec-ing in to read a file. That cost real time the first time this image
# was run: a failing database query was invisible from outside the
# container. With the console attached the log reaches stdout, where a
# container's log belongs, and log/freeswitch.log still gets its copy.
CMD ["freeswitch", "-u", "freeswitch", "-g", "freeswitch", "-nonat", "-nf"]
