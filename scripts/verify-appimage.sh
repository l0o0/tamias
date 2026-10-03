#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
: "${VERSION:?}"
: "${ARCH:?}"
image="$PWD/dist/tamiops-${VERSION}-linux-${ARCH}.AppImage"
test -x "$image"
test "$("$image" --appimage-extract-and-run -version)" = "$VERSION"
workspace=$(mktemp -d)
trap 'rm -rf "$workspace"' EXIT
trap 'exit 1' HUP INT TERM
export TAMIOPS_SMOKE_IMAGE="$image" TAMIOPS_SMOKE_DIR="$workspace"
xvfb-run -a dbus-run-session -- sh -eu <<'SMOKE'
  "$TAMIOPS_SMOKE_IMAGE" --appimage-extract-and-run -data-dir "$TAMIOPS_SMOKE_DIR/data" > "$TAMIOPS_SMOKE_DIR/app.log" 2>&1 &
  pid=$!
  trap 'kill "$pid" 2>/dev/null || true' EXIT
  sleep 10
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$TAMIOPS_SMOKE_DIR/app.log"
    echo 'AppImage exited during desktop startup' >&2
    exit 1
  fi
  if ! pgrep -f '/WebKitWebProcess' >/dev/null; then
    cat "$TAMIOPS_SMOKE_DIR/app.log"
    echo 'AppImage did not start a WebKit web process' >&2
    exit 1
  fi
  echo 'AppImage desktop and WebKit process started successfully.'
SMOKE
