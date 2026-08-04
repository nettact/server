# syntax=docker/dockerfile:1
#
# nettact-server — the self-hosted server (API; web console downloaded at runtime).
#
# The web console is NOT built or embedded here: the exact web-console release
# tag is stamped in via the WEBUI_VERSION build-arg, and the server downloads it
# on first start into /data/webui (verified against SHA256SUMS). Air-gapped
# deployments can set NETTACT_WEBUI_BASE_URL to a mirror or pre-seed
# /data/webui/<version>/ with a built dist.
#
# Build context: an assembled workspace directory with protocol/, server-core/ and
# server/ as siblings (the release workflow checks the siblings out from their
# default branches and records the resolved commits in nettact-server-dependencies.json):
#   docker build -f server/ci/Dockerfile --build-arg VERSION=v1.0.0 \
#     --build-arg WEBUI_VERSION=v0.1.0 <workspace-dir>
# An isolated single-repo build fails on the v0.0.0 placeholder pseudo-versions.

# ---- Stage 1: cross-compile the static server binary -----------------------
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
# The server compiles protocol + server-core into itself; nothing else is needed.
COPY protocol/    protocol/
COPY server-core/ server-core/
COPY server/      server/
# Trimmed workspace: mirror the local go.work's *versioned* replace directives so the
# placeholder require versions resolve to these local dirs (`use` alone does not
# override the invalid v0.0.0 version). The three go.sum files cover the build.
RUN go work init ./protocol ./server-core ./server \
 && go work edit -replace github.com/nettact/protocol@v0.0.0-00010101000000-000000000000=./protocol \
 && go work edit -replace github.com/nettact/server-core@v0.0.0-00010101000000-000000000000=./server-core
ARG TARGETOS
ARG TARGETARCH
ARG WEBUI_VERSION=dev
# VERSION is this server's own release tag (the image tag). It is stamped into the
# binary so the console's update check can tell whether the server is out of date;
# stage 2 re-declares it for the OCI label.
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X github.com/nettact/server/internal/webui.Version=${WEBUI_VERSION} -X github.com/nettact/server/internal/version.Version=${VERSION}" \
      -o /out/nettact-server ./server/cmd/nettact-server

# ---- Stage 2: minimal runtime ----------------------------------------------
FROM alpine:3.22
# ca-certificates for outbound TLS; busybox already provides wget for the healthcheck.
RUN apk add --no-cache ca-certificates \
 && addgroup -S nettact && adduser -S -G nettact nettact \
 && mkdir -p /data && chown nettact:nettact /data
COPY --from=build /out/nettact-server /usr/local/bin/nettact-server
USER nettact
WORKDIR /data
# A fresh named volume inherits /data's ownership (nettact), so the non-root user can
# write nettact.db* and the runtime-downloaded web console under /data/webui.
# Bind mounts must be made writable by this UID on the host.
VOLUME ["/data"]
# Container-internal port is always 12450; the published port is chosen in compose.
EXPOSE 12450
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:12450/api/v1/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["nettact-server"]
# Sensible defaults; compose overrides `command:` to set -addr/-secure-cookie etc. The
# admin account is auto-created with a printed one-time password on first run.
CMD ["-addr", ":12450", "-db", "/data/nettact.db"]

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="nettact-server" \
      org.opencontainers.image.description="NetTact self-hosted server (API; web console downloaded at runtime)" \
      org.opencontainers.image.source="https://github.com/nettact/server" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0"
