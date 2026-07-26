#!/usr/bin/env bash
# NetTact one-click installer (Linux; requires Docker Engine 24+ with Compose v2).
#
# One-click (downloads compose assets from https://d.nettact.org into the
# current directory, then deploys server + local agent):
#
#   curl -fsSL https://d.nettact.org/install.sh | bash
#
# From a NetTact superproject checkout:
#
#   ./server-lite/deploy/install.sh
#
# From a standalone server-lite checkout:
#
#   ./deploy/install.sh
#
# The script is idempotent: existing .env, secrets and data volumes are kept;
# re-running after a partial failure is safe.
set -euo pipefail

# ---------- defaults ----------------------------------------------------------
PORT=""                 # host port; empty = keep .env / default 12450
LITE_VERSION=""         # NETTACT_LITE_VERSION override
AGENT_VERSION=""        # NETTACT_AGENT_VERSION override
SERVER_ONLY=false
HOST_NETWORK=false
ASSUME_YES=false
AUTO_UPDATE=false
# Where to fetch docker-compose.yml / .env.example when not present locally
# (also serves this script itself: curl -fsSL https://d.nettact.org/install.sh | bash).
DIST_BASE_URL="${NETTACT_DIST_BASE_URL:-https://d.nettact.org}"

usage() {
  cat <<'EOF'
NetTact installer

Usage: install.sh [options]

Full deploy (default: server + local agent via docker compose):
  --port <n>            host port for the web console (writes NETTACT_HTTP_PORT)
  --lite-version <tag>  server image tag  (writes NETTACT_LITE_VERSION)
  --agent-version <tag> agent image tag   (writes NETTACT_AGENT_VERSION)
  --server-only         deploy only the server
  --host-network        agent monitors the DOCKER HOST network (Linux only;
                        leaves the compose network — see docs/deploy.md §9)
  --auto-update         check daily for a new Agent image and restart it
  -y, --yes             no confirmation prompts

Remote Agent install:
  https://d.nettact.org/agent/install.sh (Linux, macOS, and Docker)

  -h, --help            this help
EOF
}

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- argument parsing ---------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --port)          PORT="${2:?--port needs a value}"; shift 2 ;;
    --lite-version)  LITE_VERSION="${2:?}"; shift 2 ;;
    --agent-version) AGENT_VERSION="${2:?}"; shift 2 ;;
    --server-only)   SERVER_ONLY=true; shift ;;
    --host-network)  HOST_NETWORK=true; shift ;;
    --auto-update)   AUTO_UPDATE=true; shift ;;
    -y|--yes)        ASSUME_YES=true; shift ;;
    -h|--help)       usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done
$SERVER_ONLY && $AUTO_UPDATE && die "--auto-update only applies when an agent is installed"

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

# http <curl-args...> — thin wrapper; the whole script standardizes on curl for
# API calls (wget stays a fallback for plain file downloads only).
http() { curl -fsS "$@"; }

# json_str <s> — escape backslashes and double quotes for embedding in JSON.
json_str() { local s="${1//\\/\\\\}"; printf '%s' "${s//\"/\\\"}"; }

enable_agent_auto_update() {
  log "enabling daily automatic Agent image updates"
  docker rm -f nettact-agent-updater >/dev/null 2>&1 || true
  docker run -d --name nettact-agent-updater --restart unless-stopped \
    -v /var/run/docker.sock:/var/run/docker.sock \
    containrrr/watchtower:latest \
    --cleanup --interval 86400 nettact-agent >/dev/null
}

# ---------- full deploy: locate / fetch compose assets --------------------------
# Prefer files already present (git checkout or previous install) — never overwrite.
if [ ! -f docker-compose.yml ]; then
  log "docker-compose.yml not found — downloading from $DIST_BASE_URL"
  fetch "$DIST_BASE_URL/docker-compose.yml" docker-compose.yml \
    || die "cannot fetch docker-compose.yml; run this script from a NetTact checkout or set NETTACT_DIST_BASE_URL"
fi
if [ ! -f .env.example ] && [ ! -f .env ]; then
  fetch "$DIST_BASE_URL/.env.example" .env.example \
    || die "cannot fetch .env.example; run from a NetTact checkout or set NETTACT_DIST_BASE_URL"
fi

docker compose version >/dev/null 2>&1 || die "Docker Compose v2 ('docker compose') not available — the legacy v1 'docker-compose' is not supported"

# ---------- .env (idempotent: create once, then only apply explicit overrides) --
if [ ! -f .env ]; then
  cp .env.example .env
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
[ -n "$PORT" ]          && set_env NETTACT_HTTP_PORT "$PORT"
[ -n "$LITE_VERSION" ]  && set_env NETTACT_LITE_VERSION "$LITE_VERSION"
[ -n "$AGENT_VERSION" ] && set_env NETTACT_AGENT_VERSION "$AGENT_VERSION"

HTTP_PORT="$(grep '^NETTACT_HTTP_PORT=' .env | tail -1 | cut -d= -f2 || true)"
HTTP_PORT="${HTTP_PORT:-12450}"
BASE_URL="http://127.0.0.1:$HTTP_PORT"

# ---------- host-network override (explicit opt-in; see docs/deploy.md §9) ------
if $HOST_NETWORK; then
  if [ ! -f docker-compose.override.yml ]; then
    cat > docker-compose.override.yml <<EOF
# Written by install.sh --host-network: the agent monitors the DOCKER HOST's
# real interfaces. It leaves the compose network, so it reaches the server via
# the host-published port instead of the service name. Delete this file to
# return to the default (container-scoped) agent.
services:
  agent:
    network_mode: host
    environment:
      NETTACT_AGENT_SERVER_URL: http://127.0.0.1:$HTTP_PORT
EOF
    log "wrote docker-compose.override.yml (agent uses host networking)"
  else
    warn "docker-compose.override.yml already exists — not touching it"
  fi
fi
# Future note: the current Linux agent build needs NO extra capabilities
# (no NET_RAW, non-root). If a future version adds ICMP probes / active
# discovery, that will be a documented opt-in here — never a silent default.

# ---------- secrets placeholder (compose refuses to start without the file) -----
mkdir -p secrets
[ -f secrets/agent_enroll_token ] || : > secrets/agent_enroll_token
chmod 600 secrets/agent_enroll_token 2>/dev/null || true

# ---------- start server & wait for health --------------------------------------
log "starting server (docker compose up -d server)…"
docker compose up -d server

log "waiting for $BASE_URL/api/v1/healthz …"
HEALTHY=false
for _ in $(seq 1 60); do
  if http "$BASE_URL/api/v1/healthz" >/dev/null 2>&1; then HEALTHY=true; break; fi
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

if $SERVER_ONLY; then
  log "server-only deploy done."
  echo
  echo "  Console:  http://<this-host>:$HTTP_PORT"
  if [ -n "$ADMIN_PASS" ]; then
    echo "  Login:    $ADMIN_USER / $ADMIN_PASS   (printed once — change it in Settings)"
  else
    echo "  Login:    use your existing admin credentials"
  fi
  exit 0
fi

# ---------- enroll the agent automatically ---------------------------------------
AGENT_RUNNING="$(docker compose ps --status running --services 2>/dev/null | grep -cx agent || true)"
if [ "$AGENT_RUNNING" = "1" ]; then
  log "agent service already running — skipping enrollment (idempotent re-run)"
else
  if [ -z "$ADMIN_PASS" ]; then
    # Not a first run (password was changed / logs rotated): ask, unless -y.
    if $ASSUME_YES; then
      warn "cannot determine the admin password automatically (not a first run)."
      warn "mint a token in the console (Agent page), then:"
      warn "  printf '%s' '<token>' > secrets/agent_enroll_token && docker compose up -d agent"
      exit 1
    fi
    printf 'Admin username [admin]: '; read -r ADMIN_USER; ADMIN_USER="${ADMIN_USER:-admin}"
    printf 'Admin password (input hidden): '; read -rs ADMIN_PASS; echo
    [ -n "$ADMIN_PASS" ] || die "empty password"
  fi

  command -v curl >/dev/null 2>&1 \
    || die "curl is required for automatic enrollment. Manual flow: mint a token in the console (Agent page), then:
  printf '%s' '<token>' > secrets/agent_enroll_token && docker compose up -d agent"

  COOKIES="$(mktemp)"; trap 'rm -f "$COOKIES"' EXIT
  log "logging in to mint a one-time enrollment token…"
  # Credentials go through stdin (never argv), so they don't show up in `ps`.
  if ! printf '{"username":"%s","password":"%s"}' "$(json_str "$ADMIN_USER")" "$(json_str "$ADMIN_PASS")" \
      | http -c "$COOKIES" -H 'Content-Type: application/json' -d @- \
        "$BASE_URL/api/v1/auth/login" >/dev/null; then
    die "login failed. Fallback: log in to http://<host>:$HTTP_PORT, mint a token (Agent page), then:
  printf '%s' '<token>' > secrets/agent_enroll_token && docker compose up -d agent"
  fi

  TOKEN_JSON="$(printf '{"note":"install.sh","ttl_minutes":60}' \
    | http -b "$COOKIES" -H 'Content-Type: application/json' -d @- \
      "$BASE_URL/api/v1/enrollment-tokens")" \
    || die "minting the enrollment token failed (see the console's Agent page for the manual flow)"
  ENROLL_TOKEN="$(printf '%s' "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  [ -n "$ENROLL_TOKEN" ] || die "could not parse the token from the API response: $TOKEN_JSON"

  printf '%s' "$ENROLL_TOKEN" > secrets/agent_enroll_token
  chmod 600 secrets/agent_enroll_token 2>/dev/null || true
  log "token written to secrets/agent_enroll_token (valid 60 minutes, single use)"

  log "starting agent (docker compose up -d agent)…"
  docker compose up -d agent

  # Verify enrollment: the agent must show up in the server's agent list.
  ENROLLED=false
  for _ in $(seq 1 30); do
    AGENTS="$(http -b "$COOKIES" "$BASE_URL/api/v1/agents" 2>/dev/null || echo '[]')"
    if [ "$AGENTS" != "[]" ] && [ -n "$AGENTS" ]; then ENROLLED=true; break; fi
    sleep 2
  done
  http -b "$COOKIES" -X POST "$BASE_URL/api/v1/auth/logout" >/dev/null 2>&1 || true
  if $ENROLLED; then
    log "agent enrolled and visible in the console ✔"
  else
    docker compose logs --tail 30 agent >&2 || true
    warn "agent not visible yet — check 'docker compose logs -f agent' (token expired? re-run this script)"
  fi
fi

$AUTO_UPDATE && enable_agent_auto_update

# ---------- summary ---------------------------------------------------------------
echo
echo "──────────────────────────────────────────────────────────"
echo "  NetTact is up."
echo "  Console:   http://<this-host>:$HTTP_PORT"
if [ -n "$ADMIN_PASS" ]; then
  echo "  Login:     $ADMIN_USER / $ADMIN_PASS"
  echo "             (generated on first run, printed ONCE — change it in Settings)"
else
  echo "  Login:     your existing admin credentials"
fi
echo "  Status:    docker compose ps"
echo "  Docs:      docs/deploy.md · docs/server-config.md · docs/agent-config.md"
echo "──────────────────────────────────────────────────────────"
