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
  --auto-update           enable the host systemd update timer (default on a fresh
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
DOCKER_BIN="$(command -v docker 2>/dev/null || true)"
[ -n "$DOCKER_BIN" ] || die "docker not found — install Docker Engine 24+ first (https://docs.docker.com/engine/install/)"
case "$DOCKER_BIN" in /*) ;; *) die "cannot resolve docker to an absolute executable path: $DOCKER_BIN" ;; esac
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

as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    die "installing the systemd update timer needs root; install sudo or run this installer as root with NETTACT_INSTALL_DIR set"
  fi
}

# ---------- install directory ---------------------------------------------------
# Candidate sources for the compose assets, captured BEFORE the cd below and in
# preference order: the current directory first (someone standing in a checkout
# means that checkout), then this script's own directory, then two levels above
# it — the superproject root for server/deploy/install.sh — for the case where
# the script is invoked by path from elsewhere.
#
# Every candidate is content-checked, and that check is what makes $PWD safe to
# consult at all. $PWD is a source because the same file ships both at
# d.nettact.org and in the repo's deploy/, so running it out of a checkout should
# use THAT checkout's compose file rather than pairing a local script with a
# downloaded one. But `curl | bash` runs in whatever directory the operator
# happened to be standing in, and an unrelated docker-compose.yml there is not
# this project's: copying it in and `up -d`-ing it would deploy a stranger's
# stack under NetTact's name. Deploy assets travel as a pair, so requiring
# .env.example alongside also keeps the two files from coming from different
# places (local compose + downloaded example, or the reverse).
#
# Both halves are matched on STRUCTURE, not on the string "nettact" appearing
# somewhere: a comment, an image name or a password in someone else's compose
# file would satisfy a substring search, and what follows this check is
# `docker compose up -d` on whatever got copied in, with the daemon's
# privileges. Rejecting too strictly costs nothing — the download below is the
# canonical file anyway.
#
# is_nettact_compose <file> — does this compose file deploy THIS project? The
# marker is the server's fixed container_name: a NetTact deployment cannot lack
# it (the installer looks the container up by that name), and an unrelated stack
# has no reason to claim it. Tolerates YAML quoting and a trailing comment.
is_nettact_compose() {
  grep -Eq "^[[:space:]]*container_name:[[:space:]]*['\"]?nettact-server['\"]?([[:space:]].*)?\$" "$1" 2>/dev/null
}
# is_nettact_env_example <file> — the compose file interpolates NETTACT_* and
# nothing else, so an example that assigns none of them cannot be its partner.
# Commented-out lines don't count: every real one carries live defaults.
is_nettact_env_example() {
  [ -f "$1" ] || return 1
  grep -Eq '^[[:space:]]*NETTACT_[A-Z0-9_]+=' "$1" 2>/dev/null
}
SRC_DIRS=()
add_src() {
  local d
  d="$(cd "$1" 2>/dev/null && pwd)" || return 0
  case " ${SRC_DIRS[*]:-} " in *" $d "*) return 0 ;; esac
  # No compose file at all is the ordinary `curl | bash` case, not a surprise —
  # say nothing. The two rejections below are worth a word, because something
  # that looked like a source was deliberately passed over.
  [ -f "$d/docker-compose.yml" ] || return 0
  if ! is_nettact_compose "$d/docker-compose.yml"; then
    warn "ignoring $d/docker-compose.yml — it does not deploy nettact-server, so it is not this project's"
    return 0
  fi
  if ! is_nettact_env_example "$d/.env.example"; then
    warn "ignoring $d — its docker-compose.yml is this project's but there is no matching .env.example beside it"
    return 0
  fi
  SRC_DIRS+=("$d")
}
add_src "$PWD"
# Only when there IS a script path. Piped into bash (curl | bash) BASH_SOURCE is
# "bash" or unset and dirname of it is ".", so these would silently re-add $PWD
# and the two levels above it under the guise of the script's own location.
if [ -f "${BASH_SOURCE[0]:-}" ]; then
  SELF_DIR="$(dirname "${BASH_SOURCE[0]}")"
  add_src "$SELF_DIR"
  add_src "$SELF_DIR/../.."
fi

if [ -z "$INSTALL_DIR" ]; then
  [ -n "${HOME:-}" ] || die "HOME is not set — pass NETTACT_INSTALL_DIR=<dir> to choose where to install"
  INSTALL_DIR="$HOME/nettact"
fi
mkdir -p "$INSTALL_DIR" || die "cannot create the install directory $INSTALL_DIR"
INSTALL_DIR="$(cd "$INSTALL_DIR" && pwd -P)"

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
  # SRC_DIRS can be empty (curl | bash outside a checkout), and bash before 4.4
  # treats "${arr[@]}" on an empty array as unset under `set -u`.
  if [ "${#SRC_DIRS[@]}" -gt 0 ]; then
    for d in "${SRC_DIRS[@]}"; do
      [ "$d" = "$INSTALL_DIR" ] && continue
      [ -f "$d/$name" ] || continue
      cp "$d/$name" "$INSTALL_DIR/$name" || return 1
      log "copied $name from $d"
      return 0
    done
  fi
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

# The update timer would pull `latest` over a pinned tag (or nothing at all for a tag
# the catalog never published). Explicitly combining the two is a mistake worth
# stopping on — even when the pin comes from an earlier run's .env rather than
# this command line. This must run before the writes below.
if [ "$AUTO_UPDATE" = true ] && [ "$EFFECTIVE_SERVER_VERSION" != latest ]; then
  die "--auto-update cannot be combined with a pinned --server-version"
fi

[ -n "$PORT" ]           && set_env NETTACT_HTTP_PORT "$PORT"
[ -n "$SERVER_VERSION" ] && set_env NETTACT_SERVER_VERSION "$SERVER_VERSION"

# ---------- auto-update resolution ---------------------------------------------
# A host systemd timer performs Docker updates. Automatic updates are the DEFAULT
# on a fresh install: the operator opted in by choosing this installer at all.
# Pinning a version means the operator wants a specific release, so auto-update
# must not fight it (same rule as the agent installer).
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
# No current compose service uses profiles. Remove the switch left by the old
# Watchtower deployment on every run, including an auto-update-to-auto-update
# migration, so a stale profile cannot change future compose behavior.
if grep -q '^COMPOSE_PROFILES=' .env; then
  sed -i '/^COMPOSE_PROFILES=/d' .env
fi
if [ "$AUTO_UPDATE" = true ]; then
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
  set_env NETTACT_AUTO_UPDATE false
  log "auto-update: disabled"
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

# Validate and normalize the persisted schedule before stopping an existing
# timer or changing the running server. A typo in .env must not turn a healthy
# auto-updating install into a half-migrated one with no timer.
if [ "$AUTO_UPDATE" = true ]; then
  UPDATE_CRON="$(grep '^NETTACT_UPDATE_CRON=' .env | tail -1 | cut -d= -f2- || true)"
  read -r CRON_SECOND UPDATE_MINUTE UPDATE_HOUR CRON_DAY CRON_MONTH CRON_WEEKDAY CRON_EXTRA <<< "$UPDATE_CRON"
  if [ -n "${CRON_EXTRA:-}" ] || [ "$CRON_SECOND" != 0 ] || [ "$CRON_DAY" != '*' ] || \
     [ "$CRON_MONTH" != '*' ] || [ "$CRON_WEEKDAY" != '*' ]; then
    die "NETTACT_UPDATE_CRON=$UPDATE_CRON must have the form '0 <minute> <hour> * * *'"
  fi
  case "$UPDATE_MINUTE:$UPDATE_HOUR" in
    *[!0-9:]*|:*|*:) die "NETTACT_UPDATE_CRON=$UPDATE_CRON must use a fixed numeric minute and hour" ;;
  esac
  [ "$((10#$UPDATE_MINUTE))" -le 59 ] || die "NETTACT_UPDATE_CRON minute must be between 0 and 59"
  [ "$((10#$UPDATE_HOUR))" -le 23 ] || die "NETTACT_UPDATE_CRON hour must be between 0 and 23"
  printf -v UPDATE_MINUTE '%02d' "$((10#$UPDATE_MINUTE))"
  printf -v UPDATE_HOUR '%02d' "$((10#$UPDATE_HOUR))"
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

# ---------- stop previous updater before changing the server -------------------
UPDATE_SERVICE=/etc/systemd/system/nettact-server-update.service
UPDATE_TIMER=/etc/systemd/system/nettact-server-update.timer
UPDATE_SCRIPT=/usr/local/lib/nettact-server/update.sh
HAD_UPDATE_UNITS=false
if [ -e "$UPDATE_SERVICE" ] || [ -e "$UPDATE_TIMER" ] || [ -e "$UPDATE_SCRIPT" ]; then
  HAD_UPDATE_UNITS=true
fi
if [ "$AUTO_UPDATE" = true ] || [ "$HAD_UPDATE_UNITS" = true ]; then
  command -v systemctl >/dev/null 2>&1 || die "systemd is required to install or remove the automatic Docker update timer"
  as_root systemctl disable --now nettact-server-update.timer >/dev/null 2>&1 || true
  as_root systemctl stop nettact-server-update.service >/dev/null 2>&1 || true
  as_root rm -f "$UPDATE_SERVICE" "$UPDATE_TIMER" "$UPDATE_SCRIPT"
  as_root rmdir /usr/local/lib/nettact-server >/dev/null 2>&1 || true
  as_root systemctl daemon-reload
fi
# A previous release used a Watchtower container. Stop it before compose changes
# the server so it cannot recreate the container during this installation.
docker rm -f nettact-server-updater >/dev/null 2>&1 || true

# ---------- start server & wait for health --------------------------------------
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

# ---------- host auto-update timer ----------------------------------------------
if [ "$AUTO_UPDATE" = true ]; then
  INSTALL_USER="$(id -un)"
  INSTALL_HOME="${HOME:-}"
  if [ -z "$INSTALL_HOME" ] && command -v getent >/dev/null 2>&1; then
    INSTALL_HOME="$(getent passwd "$(id -u)" | cut -d: -f6 || true)"
  fi
  [ -n "$INSTALL_HOME" ] && [ -d "$INSTALL_HOME" ] || die "cannot resolve the installing user's home directory for Docker credentials"
  INSTALL_HOME="$(cd "$INSTALL_HOME" && pwd -P)"
  printf -v INSTALL_DIR_Q '%q' "$INSTALL_DIR"
  printf -v DOCKER_BIN_Q '%q' "$DOCKER_BIN"
  SYSTEMD_HOME="$(printf '%s' "$INSTALL_HOME" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')"

  unit_dir="$(mktemp -d)"
  trap 'rm -rf "$unit_dir"' EXIT
  cat > "$unit_dir/update.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
INSTALL_DIR=$INSTALL_DIR_Q
DOCKER_BIN=$DOCKER_BIN_Q
old_image="\$("\$DOCKER_BIN" inspect --format '{{.Image}}' nettact-server 2>/dev/null || true)"
"\$DOCKER_BIN" compose --project-directory "\$INSTALL_DIR" --env-file "\$INSTALL_DIR/.env" -f "\$INSTALL_DIR/docker-compose.yml" pull server
"\$DOCKER_BIN" compose --project-directory "\$INSTALL_DIR" --env-file "\$INSTALL_DIR/.env" -f "\$INSTALL_DIR/docker-compose.yml" up -d --no-deps server
new_image="\$("\$DOCKER_BIN" inspect --format '{{.Image}}' nettact-server)"
if [ -n "\$old_image" ] && [ "\$old_image" != "\$new_image" ]; then
  "\$DOCKER_BIN" image rm "\$old_image" >/dev/null 2>&1 || true
fi
EOF
  cat > "$unit_dir/nettact-server-update.service" <<EOF
[Unit]
Description=Update the NetTact Server container
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=$INSTALL_USER
Environment="HOME=$SYSTEMD_HOME"
ExecStart=$UPDATE_SCRIPT
EOF
  cat > "$unit_dir/nettact-server-update.timer" <<EOF
[Unit]
Description=Check nightly for NetTact Server container updates

[Timer]
OnCalendar=*-*-* ${UPDATE_HOUR}:${UPDATE_MINUTE}:00
Persistent=true

[Install]
WantedBy=timers.target
EOF
  as_root install -d -m 0755 /usr/local/lib/nettact-server
  as_root install -m 0755 "$unit_dir/update.sh" "$UPDATE_SCRIPT"
  as_root install -m 0644 "$unit_dir/nettact-server-update.service" "$UPDATE_SERVICE"
  as_root install -m 0644 "$unit_dir/nettact-server-update.timer" "$UPDATE_TIMER"
  as_root systemctl daemon-reload
  as_root systemctl enable --now nettact-server-update.timer
  trap - EXIT
  rm -rf "$unit_dir"
fi

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
  echo "  Updates:   auto (systemd timer, nightly at ${UPDATE_CRON_SHOWN:-03:00} host time)"
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
