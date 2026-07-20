# syntax=docker/dockerfile:1
#
# nettact-lite — the self-hosted Lite server (API + embedded web console).
#
# The web console is NOT built here: the caller must place a built SPA into
# server-lite/internal/webui/dist BEFORE this build (the release workflow runs
# ci/fetch-webui.sh; developers can use build-lite.sh or fetch-webui.sh local:).
#
# Build context: an assembled workspace directory with protocol/, server-core/ and
# server-lite/ as siblings (the release workflow checks the siblings out at
# ci/deps.env's pinned SHAs):
#   docker build -f server-lite/ci/Dockerfile --build-arg VERSION=v1.0.0 <workspace-dir>
# An isolated single-repo build fails on the v0.0.0 placeholder pseudo-versions.

# ---- Stage 1: cross-compile the static server binary -----------------------
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
# server-lite compiles protocol + server-core into itself; nothing else is needed.
COPY protocol/    protocol/
COPY server-core/ server-core/
COPY server-lite/ server-lite/
# Trimmed workspace: mirror the local go.work's *versioned* replace directives so the
# placeholder require versions resolve to these local dirs (`use` alone does not
# override the invalid v0.0.0 version). The three go.sum files cover the build.
RUN go work init ./protocol ./server-core ./server-lite \
 && go work edit -replace github.com/nettact/protocol@v0.0.0-00010101000000-000000000000=./protocol \
 && go work edit -replace github.com/nettact/server-core@v0.0.0-00010101000000-000000000000=./server-core
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" \
      -o /out/nettact-lite ./server-lite/cmd/nettact-lite

# ---- Stage 2: minimal runtime ----------------------------------------------
FROM alpine:3.22
# ca-certificates for outbound TLS; busybox already provides wget for the healthcheck.
RUN apk add --no-cache ca-certificates \
 && addgroup -S nettact && adduser -S -G nettact nettact \
 && mkdir -p /data && chown nettact:nettact /data
COPY --from=build /out/nettact-lite /usr/local/bin/nettact-lite
USER nettact
WORKDIR /data
# A fresh named volume inherits /data's ownership (nettact), so the non-root user can
# write nettact.db*. Bind mounts must be made writable by this UID on the host.
VOLUME ["/data"]
# Container-internal port is always 8080; the published port is chosen in compose.
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/v1/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["nettact-lite"]
# Sensible defaults; compose overrides `command:` to inject -admin-user/-admin-pass etc.
CMD ["-addr", ":8080", "-db", "/data/nettact.db"]

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="nettact-lite" \
      org.opencontainers.image.description="NetTact self-hosted Lite server (API + embedded web console)" \
      org.opencontainers.image.source="https://github.com/nettact/server-lite" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0"
