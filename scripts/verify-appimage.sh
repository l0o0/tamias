#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
: "${VERSION:?}"
: "${ARCH:?}"
image="$PWD/dist/tamias-${VERSION}-linux-${ARCH}.AppImage"
test -x "$image"
image=$(readlink -f "$image")
test "$("$image" --appimage-extract-and-run -version)" = "$VERSION"
workspace=$(mktemp -d)
apparmor_policy=
apparmor_profile=
apparmor_loaded=0
cleanup() {
  cleanup_status=0
  if [ "$apparmor_loaded" -eq 1 ]; then
    if ! sudo -n apparmor_parser -R "$apparmor_policy"; then
      echo "Could not unload temporary AppArmor profile $apparmor_profile" >&2
      cleanup_status=1
    fi
  fi
  rm -rf "$workspace"
  [ -z "$apparmor_policy" ] || rm -f "$apparmor_policy"
  return "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ "${GITHUB_ACTIONS:-}" = true ]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo 'Ubuntu AppImage smoke requires sudo to load and unload its temporary AppArmor profile' >&2
    exit 1
  fi
  if ! command -v apparmor_parser >/dev/null 2>&1; then
    echo 'Ubuntu AppImage smoke requires apparmor_parser; refusing to skip the user-namespace check' >&2
    exit 1
  fi
  if [ ! -r /sys/module/apparmor/parameters/enabled ] || \
     ! grep -q '^Y' /sys/module/apparmor/parameters/enabled; then
    echo 'Ubuntu AppImage smoke requires the AppArmor kernel module; refusing to skip the user-namespace check' >&2
    exit 1
  fi
  case "$image" in
    *[!A-Za-z0-9_./-]*)
      echo "Cannot safely create an exact-path AppArmor profile for image path: $image" >&2
      exit 1
      ;;
  esac
  apparmor_profile="tamias-appimage-smoke-$$"
  apparmor_policy="$workspace/$apparmor_profile.profile"
  cat > "$apparmor_policy" <<PROFILE
abi <abi/4.0>,
profile $apparmor_profile $image flags=(unconfined) {
  userns,
}
PROFILE
  apparmor_loaded=1
  if ! sudo -n apparmor_parser -r -W "$apparmor_policy"; then
    echo 'Could not load the exact-AppImage AppArmor userns exception; smoke test is not being skipped' >&2
    exit 1
  fi
  echo "Loaded temporary AppArmor userns rule for $image only ($apparmor_profile)."
fi

(
  cd "$workspace"
  "$image" --appimage-extract >/dev/null
)
appdir="$workspace/squashfs-root"
test -s "$appdir/io.tamiops.tami.desktop"
grep -Fxq 'StartupWMClass=Tamias' "$appdir/io.tamiops.tami.desktop"
cmp build/assets/app-icon.png "$appdir/usr/share/icons/io.tamiops.tami.png"
test -d "$appdir/usr/lib/gtk-4.0"
test -s "$appdir/usr/lib/libgtk-4.so.1"
test -s "$appdir/usr/lib/gtk-4.0/$(pkg-config --variable=gtk_binary_version gtk4)/immodules/libim-fcitx5.so"
test -s "$workspace/squashfs-root/usr/lib/libFcitx5GClient.so.2"
gtk4_cflags=$(pkg-config --cflags gtk4)
gtk4_libs=$(pkg-config --libs gtk4)
cc -Wall -Wextra -Werror $gtk4_cflags \
  scripts/tests/gtk4-fcitx-module-smoke.c -o "$workspace/gtk4-fcitx-module-smoke" $gtk4_libs
# Keep bundled libraries out of the host X server and D-Bus launcher. A newer
# host dbus-run-session can require private symbols absent in bundled libdbus.
xvfb-run -a dbus-run-session -- env \
  GTK_PATH="$appdir/usr/lib/gtk-4.0" \
  GTK_EXE_PREFIX="$appdir/usr" \
  LD_LIBRARY_PATH="$appdir/usr/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" \
  GDK_BACKEND=x11 \
  "$workspace/gtk4-fcitx-module-smoke" "$appdir"

export TAMIOPS_SMOKE_IMAGE="$image" TAMIOPS_SMOKE_DIR="$workspace"
export TAMIOPS_SMOKE_APPARMOR_PROFILE="$apparmor_profile"
xvfb-run -a dbus-run-session -- sh -eu <<'SMOKE'
  data_dir="$TAMIOPS_SMOKE_DIR/data"
  "$TAMIOPS_SMOKE_IMAGE" --appimage-extract-and-run --data-dir "$data_dir" > "$TAMIOPS_SMOKE_DIR/app.log" 2>&1 &
  runtime_pid=$!
  app_pid=
  trap 'kill "${app_pid:-}" "$runtime_pid" 2>/dev/null || true' EXIT
  sleep 10
  # --appimage-extract-and-run may leave the runtime as a wrapper process.
  # Identify the actual Go app by its executable and this smoke's unique data
  # directory instead of assuming $! is the application process.
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    for proc in /proc/[0-9]*; do
      [ -r "$proc/exe" ] || continue
      executable=$(readlink -f "$proc/exe" 2>/dev/null || true)
      case "$executable" in
        */usr/bin/tamias) ;;
        *) continue ;;
      esac
      cmdline=$(tr '\000' '\n' < "$proc/cmdline" 2>/dev/null || true)
      printf '%s\n' "$cmdline" | grep -Fxq -- '--data-dir' || continue
      printf '%s\n' "$cmdline" | grep -Fxq -- "$data_dir" || continue
      app_pid=${proc##*/}
      break
    done
    [ -n "$app_pid" ] && break
    sleep 1
  done
  if [ -z "$app_pid" ]; then
    cat "$TAMIOPS_SMOKE_DIR/app.log"
    echo 'AppImage did not start the application process with the smoke data directory' >&2
    exit 1
  fi
  executable=$(readlink -f "/proc/$app_pid/exe" 2>/dev/null || true)
  case "$executable" in
    */usr/bin/tamias) ;;
    *)
      cat "$TAMIOPS_SMOKE_DIR/app.log"
      echo "Smoke process is not the Tamias executable: ${executable:-unavailable}" >&2
      exit 1
      ;;
  esac
  if [ -n "$TAMIOPS_SMOKE_APPARMOR_PROFILE" ]; then
    profile=$(cat "/proc/$app_pid/attr/current" 2>/dev/null || true)
    case "$profile" in
      "$TAMIOPS_SMOKE_APPARMOR_PROFILE"*) ;;
      *)
        cat "$TAMIOPS_SMOKE_DIR/app.log"
        echo "Application process did not enter its exact-path AppArmor profile (current: ${profile:-unavailable})" >&2
        exit 1
        ;;
    esac
  fi
  appdir=$(tr '\000' '\n' < "/proc/$app_pid/environ" | sed -n 's/^APPDIR=//p')
  if [ -z "$appdir" ]; then
    cat "$TAMIOPS_SMOKE_DIR/app.log"
    echo 'AppImage process did not expose its APPDIR' >&2
    exit 1
  fi
  python3 scripts/tests/linux-window-identity.py --pid "$app_pid" --output "$TAMIOPS_SMOKE_DIR/window-icon.png"
  bundled_webkit=0
  bundled_network=0
  for proc in /proc/[0-9]*; do
    [ -r "$proc/cmdline" ] || continue
    cmdline=$(tr '\000' ' ' < "$proc/cmdline" 2>/dev/null || true)
    case "$cmdline" in
      *WebKitWebProcess*|*WebKitNetworkProcess*) ;;
      *) continue ;;
    esac
    executable=$(readlink -f "$proc/exe" 2>/dev/null || true)
    case "$executable" in
      "$appdir"/*/WebKitWebProcess) bundled_webkit=1 ;;
      "$appdir"/*/WebKitNetworkProcess) bundled_network=1 ;;
    esac
    [ "$bundled_webkit" -eq 1 ] && [ "$bundled_network" -eq 1 ] && break
  done
  if [ "$bundled_webkit" -ne 1 ] || [ "$bundled_network" -ne 1 ]; then
    cat "$TAMIOPS_SMOKE_DIR/app.log"
    echo "AppImage did not start both bundled WebKitWebProcess and WebKitNetworkProcess under $appdir (web=$bundled_webkit network=$bundled_network)" >&2
    pgrep -af '[W]ebKitWebProcess' >&2 || true
    exit 1
  fi
  echo 'AppImage desktop and WebKit process started successfully.'
SMOKE
cp "$workspace/window-icon.png" dist/linux-window-icon.png
