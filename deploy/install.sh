#!/usr/bin/env bash
# NetTact Server one-click installer (Linux; requires Docker Engine 24+ with Compose v2).
#
# Installs the SERVER, and only the server. Agents are installed per monitored
# machine — including this one, if you want it monitored — by their own
# installer: https://d.nettact.org/agent/install.sh, driven by an enrollment
# token minted on the console's Agent page. Bundling one in here made a local
# agent look like part of the server rather than a deliberate choice about which
# machines are watched, and it enrolled that agent by logging in as the admin.
#
# Everything lands in ~/nettact (override with NETTACT_INSTALL_DIR), NOT in the
# directory the script was run from: the deployment outlives the shell that
# created it, so its compose file and .env need one predictable home that is
# neither a git working tree nor whichever directory someone happened to be in.
#
#   curl -fsSL https://d.nettact.org/install.sh | bash
#
# From a NetTact checkout — the server repo's deploy/ holds the compose file
# and .env.example next to this script, so they are copied into the install
# directory instead of downloaded:
#
#   ./deploy/install.sh          (standalone server checkout)
#   ./server/deploy/install.sh   (superproject checkout, server submodule)
#
# The script is idempotent: an existing .env and the data volume are kept;
# re-running after a partial failure is safe.
set -euo pipefail

# ---------- defaults ----------------------------------------------------------
PORT=""                 # host port; empty = keep .env / default 12450
SERVER_VERSION=""       # NETTACT_SERVER_VERSION override
AUTO_UPDATE=""          # empty = follow .env / default true; true/false = explicit flag
# Where the deployment lives. Resolved after argument parsing so the error for a
# missing HOME can name the escape hatch.
INSTALL_DIR="${NETTACT_INSTALL_DIR:-}"
# Where to fetch docker-compose.yml / .env.example when not present locally
# (also serves this script itself: curl -fsSL https://d.nettact.org/install.sh | bash).
DIST_BASE_URL="${NETTACT_DIST_BASE_URL:-https://d.nettact.org}"

usage() {
  cat <<'EOF'
NetTact Server installer — deploys the server via docker compose.

Usage: install.sh [options]

  --port <n>              host port for the web console (writes NETTACT_HTTP_PORT).
                          Accepts an address to publish on, too, e.g.
                          10.0.0.5:12450 to expose it on one interface only
  --server-version <tag>  server image tag  (writes NETTACT_SERVER_VERSION)
  --auto-update           enable the auto-update sidecar (default on a fresh
                          install; pinned --server-version turns it off)
  --no-auto-update        manage updates by hand (docker compose pull && up -d)
  -h, --help              this help

Environment:
  NETTACT_INSTALL_DIR   where to install (default: ~/nettact)
  NETTACT_DIST_BASE_URL where to download the compose assets from

No agent is installed here. Install one on each machine you want monitored,
with a token minted on the console's Agent page:
  https://d.nettact.org/agent/install.sh (Linux, macOS, and Docker)
EOF
}

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- argument parsing ---------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --port)            PORT="${2:?--port needs a value}"; shift 2 ;;
    --server-version)  SERVER_VERSION="${2:?}"; shift 2 ;;
    --auto-update)     AUTO_UPDATE=true; shift ;;
    --no-auto-update)  AUTO_UPDATE=false; shift ;;
    -h|--help)         usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

# ---------- preflight ----------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "docker not found — install Docker Engine 24+ first (https://docs.docker.com/engine/install/)"
docker info >/dev/null 2>&1      || die "cannot talk to the Docker daemon (is it running? do you need sudo / the docker group?)"

ENGINE_VER="$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo 0)"
case "${ENGINE_VER%%.*}" in
  ''|*[!0-9]*) warn "cannot parse Docker Engine version '$ENGINE_VER'" ;;
  *) [ "${ENGINE_VER%%.*}" -lt 24 ] && warn "Docker Engine $ENGINE_VER < 24 is untested" ;;
esac

fetch() { # fetch <url> <dest>
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else die "need curl or wget to download $1"; fi
}

# probe <url> — GET the URL, discarding the body. curl or wget, whichever is
# installed: nothing here needs curl specifically any more.
probe() {
  if command -v curl >/dev/null 2>&1; then curl -fsS "$1" >/dev/null
  else wget -qO- "$1" >/dev/null; fi
}

docker compose version >/dev/null 2>&1 || die "Docker Compose v2 ('docker compose') not available — the legacy v1 'docker-compose' is not supported"

# ---------- install directory ---------------------------------------------------
# Candidate sources for the compose assets, captured BEFORE the cd below and in
# preference order: the current directory first (someone standing in a checkout
# means that checkout), then the superproject root two levels above this script
# — server-lite/deploy/install.sh — for the case where the script is invoked by
# path from elsewhere.
SRC_DIRS=("$PWD")
add_src() {
  local d
  d="$(cd "$1" 2>/dev/null && pwd)" || return 0
  case " ${SRC_DIRS[*]} " in *" $d "*) return 0 ;; esac
  SRC_DIRS+=("$d")
}
# Only when there IS a script path. Piped into bash (curl | bash) BASH_SOURCE is
# "bash" or unset, dirname of it is ".", and "./../.." would then be two levels
# above whatever directory the operator happened to be standing in — a stray
# docker-compose.yml up there is not this project's and must never be deployed.
if [ -f "${BASH_SOURCE[0]:-}" ]; then
  SELF_DIR="$(dirname "${BASH_SOURCE[0]}")"
  add_src "$SELF_DIR/../.."
  add_src "$SELF_DIR"
fi

if [ -z "$INSTALL_DIR" ]; then
  [ -n "${HOME:-}" ] || die "HOME is not set — pass NETTACT_INSTALL_DIR=<dir> to choose where to install"
  INSTALL_DIR="$HOME/nettact"
fi
mkdir -p "$INSTALL_DIR" || die "cannot create the install directory $INSTALL_DIR"
INSTALL_DIR="$(cd "$INSTALL_DIR" && pwd)"

# An install done from some other directory is not just "somewhere else": compose
# derives the project name from the directory, so that deployment's containers
# and — crucially — its DATA VOLUME belong to a different project, invisible from
# here, while its fixed container_name still occupies the name this one needs.
# Deploying anyway would either collide on the name or quietly start a second
# server on an empty database. Neither is something to discover afterwards.
PREV_DIR="$(docker inspect nettact-server \
  --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' 2>/dev/null || true)"
case "$PREV_DIR" in '<no value>') PREV_DIR="" ;; esac
if [ -n "$PREV_DIR" ] && [ "$PREV_DIR" != "$INSTALL_DIR" ]; then
  die "a NetTact server is already deployed from $PREV_DIR, and this installer now installs into $INSTALL_DIR.
  Its database lives in that deployment's own docker volume, so installing here would start a SECOND, empty server.
  Pick one:
    * keep the existing location:  re-run this script with NETTACT_INSTALL_DIR=$PREV_DIR
    * manage it where it is:       cd $PREV_DIR && docker compose ps
    * start fresh here (the old database is NOT carried over, and the old volume is left behind):
                                   cd $PREV_DIR && docker compose down, then re-run this script"
fi

# provide <name> — ensure $INSTALL_DIR/<name> exists. An existing file is kept
# untouched (an operator may have edited it, and .env is generated from the
# example only once), otherwise the first local copy is copied in, otherwise it
# is downloaded. Copying rather than deploying in place is the point: a checkout
# is a working tree that gets pulled and rewritten under a running deployment.
provide() {
  local name="$1" d
  [ -f "$INSTALL_DIR/$name" ] && return 0
  for d in "${SRC_DIRS[@]}"; do
    [ "$d" = "$INSTALL_DIR" ] && continue
    [ -f "$d/$name" ] || continue
    cp "$d/$name" "$INSTALL_DIR/$name" || return 1
    log "copied $name from $d"
    return 0
  done
  log "downloading $name from $DIST_BASE_URL"
  fetch "$DIST_BASE_URL/$name" "$INSTALL_DIR/$name"
}

log "installing into $INSTALL_DIR"
provide docker-compose.yml \
  || die "cannot obtain docker-compose.yml; run this script from a NetTact checkout or set NETTACT_DIST_BASE_URL"
if [ ! -f "$INSTALL_DIR/.env" ]; then
  provide .env.example \
    || die "cannot obtain .env.example; run this script from a NetTact checkout or set NETTACT_DIST_BASE_URL"
fi
cd "$INSTALL_DIR"

# host_timezone — the host's IANA zone name, or empty if it cannot be determined.
# The times PEOPLE read (notification bodies, log lines) are printed in the
# server's local zone, and a container inherits none of the host's, so without
# this the default deployment announces outages hours off the operator's wall
# clock. Stored and transmitted times are UTC either way.
host_timezone() {
  local tz=""
  if command -v timedatectl >/dev/null 2>&1; then
    tz="$(timedatectl show -p Timezone --value 2>/dev/null || true)"
  fi
  if [ -z "$tz" ] && [ -r /etc/timezone ]; then
    tz="$(head -n1 /etc/timezone 2>/dev/null || true)"
  fi
  if [ -z "$tz" ] && [ -L /etc/localtime ]; then
    # e.g. /etc/localtime -> /usr/share/zoneinfo/Asia/Shanghai
    tz="$(readlink -f /etc/localtime 2>/dev/null | sed -n 's|.*/zoneinfo/||p' || true)"
  fi
  # This lands in .env unquoted and becomes the container's $TZ, so pass through
  # only something shaped like a zone name.
  case "$tz" in
    ''|Local|*[!A-Za-z0-9/_+-]*) tz="" ;;
  esac
  printf '%s' "$tz"
}

# ---------- .env (idempotent: create once, then only apply explicit overrides) --
FRESH_ENV=false
if [ ! -f .env ]; then
  cp .env.example .env
  FRESH_ENV=true
  log "created .env from .env.example"
else
  log ".env already exists — keeping it"
fi
chmod 600 .env 2>/dev/null || true

set_env() { # set_env KEY VALUE — replace or append in .env
  if grep -q "^[# ]*$1=" .env; then
    sed -i "s|^[# ]*$1=.*|$1=$2|" .env
  else
    printf '%s=%s\n' "$1" "$2" >> .env
  fi
}

# ---------- effective image tag ------------------------------------------------
# The tag that will actually run: the flag on THIS command line wins, else the
# value already in .env (a previous pin), else latest. Resolved before any
# .env write so the auto-update conflict check below can run against the
# deployment's REAL tag — not just the flag — and so a rejected invocation does
# not leave a half-mutated .env behind.
CURRENT_SERVER_VERSION="$(grep '^NETTACT_SERVER_VERSION=' .env | tail -1 | cut -d= -f2- || true)"
if [ -n "$SERVER_VERSION" ]; then
  EFFECTIVE_SERVER_VERSION="$SERVER_VERSION"
elif [ -n "$CURRENT_SERVER_VERSION" ]; then
  EFFECTIVE_SERVER_VERSION="$CURRENT_SERVER_VERSION"
else
  EFFECTIVE_SERVER_VERSION="latest"
fi

# The updater would pull `latest` over a pinned tag (or nothing at all for a tag
# the catalog never published). Explicitly combining the two is a mistake worth
# stopping on — even when the pin comes from an earlier run's .env rather than
# this command line. This must run before the writes below.
if [ "$AUTO_UPDATE" = true ] && [ "$EFFECTIVE_SERVER_VERSION" != latest ]; then
  die "--auto-update cannot be combined with a pinned --server-version"
fi

[ -n "$PORT" ]           && set_env NETTACT_HTTP_PORT "$PORT"
[ -n "$SERVER_VERSION" ] && set_env NETTACT_SERVER_VERSION "$SERVER_VERSION"

# ---------- auto-update resolution ---------------------------------------------
# The Watchtower sidecar lives in docker-compose.yml behind the `updater` compose
# profile; the on/off switch is COMPOSE_PROFILES=updater in this .env. Automatic
# updates are the DEFAULT on a fresh install: the operator opted in by choosing
# this installer at all. Pinning a version means the operator wants a specific
# release, so auto-update must not fight it (same rule as the agent installer).
#
# Resolution order: explicit --auto-update/--no-auto-update flag, else a pinned
# --server-version turns it off, else the value already in .env (kept from a
# previous run), else true for a fresh install. NETTACT_AUTO_UPDATE is written
# back so the container sees it and the console can say "updates itself" instead
# of offering a manual download.
CURRENT_AUTO_UPDATE="$(grep '^NETTACT_AUTO_UPDATE=' .env | tail -1 | cut -d= -f2- || true)"
if [ -n "$AUTO_UPDATE" ]; then
  : # explicit flag wins
elif [ "$EFFECTIVE_SERVER_VERSION" != latest ]; then
  # Pinned version: no updater. Unless --auto-update was ALSO given, which the
  # effective-tag check above stops as a contradiction.
  AUTO_UPDATE=false
  warn "--server-version pins a specific release, so automatic updates are off (omit --server-version, or run --auto-update, to turn them on)"
elif [ "$FRESH_ENV" = true ]; then
  # The .env was just copied from .env.example, whose NETTACT_AUTO_UPDATE=false
  # is the safe default for manual compose users, not a choice this operator
  # made. Automatic updates are the installer's default — the operator opted in
  # by choosing this installer at all.
  AUTO_UPDATE=true
elif [ -n "$CURRENT_AUTO_UPDATE" ]; then
  AUTO_UPDATE="$CURRENT_AUTO_UPDATE"
else
  AUTO_UPDATE=true
fi

set_env NETTACT_AUTO_UPDATE "$AUTO_UPDATE"
if [ "$AUTO_UPDATE" = true ]; then
  set_env COMPOSE_PROFILES updater
  # Randomly bake a minute/hour in the 02:00–05:00 window on first install, so a
  # fleet doesn't all hit the registry at the same instant. Existing installs
  # keep their baked cron (idempotent re-run).
  if [ "$FRESH_ENV" = true ] || ! grep -q '^NETTACT_UPDATE_CRON=' .env; then
    UPDATE_CRON_H="$((2 + RANDOM % 3))"
    UPDATE_CRON_M="$((RANDOM % 60))"
    set_env NETTACT_UPDATE_CRON "0 $UPDATE_CRON_M $UPDATE_CRON_H * * *"
    log "auto-update: enabled, nightly at 0 $UPDATE_CRON_M $UPDATE_CRON_H * * * (host time; change NETTACT_UPDATE_CRON in .env)"
  else
    log "auto-update: enabled, nightly at $(grep '^NETTACT_UPDATE_CRON=' .env | tail -1 | cut -d= -f2-) (change NETTACT_UPDATE_CRON in .env)"
  fi
else
  # Turning the sidecar off means it must also stop if it is already running:
  # compose leaves a profile's container running until removed.
  set_env NETTACT_AUTO_UPDATE false
  if grep -q '^COMPOSE_PROFILES=' .env; then
    sed -i '/^COMPOSE_PROFILES=/d' .env
    log "auto-update: disabled (docker compose stop updater will stop a running sidecar)"
  fi
fi

# Adopt the host's timezone on FIRST install only, so re-running the installer
# never overwrites a zone the operator has since edited by hand.
if [ "$FRESH_ENV" = true ]; then
  HOST_TZ="$(host_timezone)"
  if [ -n "$HOST_TZ" ]; then
    set_env NETTACT_TZ "$HOST_TZ"
    log "timezone: $HOST_TZ (change NETTACT_TZ in .env)"
  else
    warn "could not detect the host timezone — notification times will be UTC (set NETTACT_TZ in .env)"
  fi
fi

# NETTACT_HTTP_PORT is interpolated into the HOST side of the compose port
# mapping, and compose accepts an address there — `100.64.70.192:12450` publishes
# on that interface only. So the value is not necessarily a port, and pasting it
# after "127.0.0.1:" produced `http://127.0.0.1:100.64.70.192:12450`. Worse than
# the ugly URL: when a bind address IS given, 127.0.0.1 is not listening at all,
# so the health check could only ever time out and call the install a failure.
HTTP_PUBLISH="$(grep '^NETTACT_HTTP_PORT=' .env | tail -1 | cut -d= -f2- || true)"
HTTP_PUBLISH="${HTTP_PUBLISH:-12450}"
case "$HTTP_PUBLISH" in
  # Rightmost colon: the address may be IPv6 in brackets ([::1]:12450), where
  # every other colon belongs to the address.
  *:*) HTTP_PORT="${HTTP_PUBLISH##*:}"; BIND_ADDR="${HTTP_PUBLISH%:*}" ;;
  *)   HTTP_PORT="$HTTP_PUBLISH";       BIND_ADDR="" ;;
esac
case "$HTTP_PORT" in
  ''|*[!0-9]*) die "NETTACT_HTTP_PORT=$HTTP_PUBLISH is not a port. Use a port (12450), or an address to publish on (127.0.0.1:12450, 10.0.0.5:12450)." ;;
esac
# A wildcard bind names no reachable address, so it stays the placeholder for the
# reader and loopback for the probe; a specific one is both. The IPv6 wildcard
# probes over IPv6: docker publishes it on `::` only, and reaching that through
# 127.0.0.1 relies on the host's ipv6-mapped-v4 default — where it is off, the
# container is healthy while the probe times out and the install reports failure.
case "$BIND_ADDR" in
  ''|0.0.0.0)     PROBE_HOST="127.0.0.1"; CONSOLE_HOST="<this-host>" ;;
  '::'|'[::]')    PROBE_HOST="[::1]";     CONSOLE_HOST="<this-host>" ;;
  *)              PROBE_HOST="$BIND_ADDR"; CONSOLE_HOST="$BIND_ADDR" ;;
esac
BASE_URL="http://$PROBE_HOST:$HTTP_PORT"

# ---------- start server & wait for health --------------------------------------
# With the updater profile active, `up -d` (no service name) also starts the
# sidecar; a bare `docker compose up -d` later by the operator does the same,
# because COMPOSE_PROFILES lives in .env next to the compose file. When the
# profile was just disabled, `--remove-orphans` stops the sidecar that is no
# longer part of the active config.
log "starting server (docker compose up -d)…"
docker compose up -d --remove-orphans

log "waiting for $BASE_URL/api/v1/healthz …"
HEALTHY=false
for _ in $(seq 1 60); do
  if probe "$BASE_URL/api/v1/healthz" >/dev/null 2>&1; then HEALTHY=true; break; fi
  sleep 2
done
$HEALTHY || { docker compose logs --tail 40 server >&2 || true; die "server did not become healthy in 120s (logs above; check the port and 'docker compose ps')"; }
log "server is healthy"

# ---------- admin credentials ----------------------------------------------------
# AUTH-001: on first run the server prints a one-time generated password.
ADMIN_USER="" ADMIN_PASS=""
FIRSTRUN="$(docker compose logs --no-color server 2>/dev/null | grep -A3 'NetTact first run' | tail -3 || true)"
if [ -n "$FIRSTRUN" ]; then
  ADMIN_USER="$(printf '%s\n' "$FIRSTRUN" | sed -n 's/.*username: //p' | head -1)"
  ADMIN_PASS="$(printf '%s\n' "$FIRSTRUN" | sed -n 's/.*password: //p' | head -1)"
fi

# ---------- summary ---------------------------------------------------------------
echo
echo "──────────────────────────────────────────────────────────"
echo "  NetTact Server is up."
echo "  Console:   http://$CONSOLE_HOST:$HTTP_PORT"
if [ -n "$ADMIN_PASS" ]; then
  echo "  Login:     $ADMIN_USER / $ADMIN_PASS"
  echo "             (generated on first run, printed ONCE — change it in Settings)"
else
  echo "  Login:     your existing admin credentials"
fi
echo "  Directory: $INSTALL_DIR   (run docker compose from here)"
echo "  Status:    docker compose ps"
if [ "$AUTO_UPDATE" = true ]; then
  UPDATE_CRON_SHOWN="$(grep '^NETTACT_UPDATE_CRON=' .env | tail -1 | cut -d= -f2- || true)"
  echo "  Updates:   auto (Watchtower sidecar, nightly at ${UPDATE_CRON_SHOWN:-03:00} host time)"
else
  echo "  Updates:   manual (docker compose pull && docker compose up -d)"
fi
echo "  Docs:      docs/deploy.md · docs/server-config.md · docs/agent-config.md"
echo
echo "  No agent is installed — nothing is being monitored yet. On each"
echo "  machine you want to watch, mint a token on the console's Agent page"
echo "  and run:"
echo "    curl -fsSL $DIST_BASE_URL/agent/install.sh | sudo bash -s -- \\"
echo "      --server-url http://$CONSOLE_HOST:$HTTP_PORT --token <token>"
echo "──────────────────────────────────────────────────────────"
