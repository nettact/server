#!/usr/bin/env bash
# fetch-webui.sh — put the web-console SPA into server-lite's embed dir
# (internal/webui/dist) from a chosen source, before `go build` embeds it.
#
# Source selection (first match wins):
#   $WEBUI_SOURCE                      explicit override:
#     local:<path>       copy a locally built dist (dev), e.g. local:../web-console/dist
#     release:latest     download the newest web-console GitHub Release dist
#     release:vX.Y.Z     download an exact release
#   ci/deps.env WEB_CONSOLE_VERSION    default (latest | vX.Y.Z), used as release:<value>
#
# Downloads verify SHA256SUMS. Prints the resolved version as the last line
# ("resolved: <version>") so CI can capture it for release notes.
#
# Requirements for release: mode: gh CLI authenticated with read access to
# nettact/web-console (in Actions: GH_TOKEN=${{ secrets.GITHUB_TOKEN }} works for
# same-org repos the token can read; otherwise a PAT / CI_SUBMODULE_SSH_KEY-based
# token). Local dev: `gh auth login` once.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
DEST="$REPO_ROOT/internal/webui/dist"
WEB_REPO="nettact/web-console"

source_spec="${WEBUI_SOURCE:-}"
if [ -z "$source_spec" ]; then
  # shellcheck disable=SC1091
  source "$SCRIPT_DIR/deps.env"
  source_spec="release:${WEB_CONSOLE_VERSION:?WEB_CONSOLE_VERSION missing from ci/deps.env}"
fi

case "$source_spec" in
  local:*)
    src="${source_spec#local:}"
    [ -f "$src/index.html" ] || { echo "error: $src does not look like a built dist (no index.html)" >&2; exit 1; }
    rm -rf "$DEST"
    mkdir -p "$DEST"
    cp -r "$src/." "$DEST/"
    echo "resolved: local ($src)"
    ;;
  release:*)
    version="${source_spec#release:}"
    command -v gh >/dev/null || { echo "error: gh CLI required for release: mode" >&2; exit 1; }
    if [ "$version" = "latest" ]; then
      version=$(gh release view --repo "$WEB_REPO" --json tagName -q .tagName)
      [ -n "$version" ] || { echo "error: could not resolve latest release of $WEB_REPO" >&2; exit 1; }
    fi
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    gh release download "$version" --repo "$WEB_REPO" --dir "$tmp" \
      --pattern "web-console-dist-${version}.tar.gz" --pattern SHA256SUMS
    ( cd "$tmp" && sha256sum -c SHA256SUMS )
    rm -rf "$DEST"
    mkdir -p "$DEST"
    tar xzf "$tmp/web-console-dist-${version}.tar.gz" -C "$DEST"
    [ -f "$DEST/index.html" ] || { echo "error: downloaded dist has no index.html" >&2; exit 1; }
    echo "resolved: $version"
    ;;
  *)
    echo "error: unknown WEBUI_SOURCE '$source_spec' (expect local:<path> | release:latest | release:vX.Y.Z)" >&2
    exit 1
    ;;
esac
