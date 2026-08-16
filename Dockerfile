# SPDX-License-Identifier: Apache-2.0
#
# The whole product in one image: the SPA is compiled, embedded into the Go
# binary, and the binary is all that ships. No web server, no interpreter, no
# package manager — the runtime layer is a static base with a CA bundle,
# because the provider connection is the only thing that needs one.
#
#   docker build -t aicc:dev .
#   docker build --build-arg VERSION=v0.1.0 -t aicc:v0.1.0 .

# --- the single-page application --------------------------------------------
FROM node:22-alpine AS web

WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

# --- the executable ----------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The build context excludes web/dist (.dockerignore), so this is the SPA the
# stage above just built and nothing a developer left lying around.
COPY --from=web /src/web/dist ./web/dist

ARG VERSION=""
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/aicc ./cmd/aicc

# The log directory has to exist owned by the runtime user: AICC_LOG_DIR cannot
# be blanked (an empty value means "use the default"), and the static base has
# no shell to create it later.
RUN mkdir -p /out/logs /out/recordings && chown -R 65532:65532 /out/logs /out/recordings

# --- what ships ---------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/aicc /usr/local/bin/aicc
COPY --from=build --chown=nonroot:nonroot /out/logs /var/log/aicc
COPY --from=build --chown=nonroot:nonroot /out/recordings /var/lib/aicc/recordings

ENV AICC_LOG_DIR=/var/log/aicc \
    AICC_HTTP_ADDR=:8080

# 8080 the API, the event stream and the SPA; 6060 the SIP UAS the switch
# bridges AI calls to, with its RTP range beside it. 9090 carries metrics and
# health and stays shut until AICC_METRICS_ADDR is opened deliberately — that
# listener has no authentication of its own.
EXPOSE 8080 9090 6060/udp 40000-40999/udp

USER nonroot
ENTRYPOINT ["/usr/local/bin/aicc"]
