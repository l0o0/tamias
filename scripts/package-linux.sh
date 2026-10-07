#!/bin/sh
# Keep `sh scripts/package-linux.sh` callers working while using Bash arrays
# for the linuxdeploy argument vectors below.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

set -Eeuo pipefail

# Build the supported Ubuntu 24.04 x86_64 AppImage. Large upstream tools are
# downloaded only on the Linux build host and are verified before execution.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
ROOT_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd -P)"
VERSION="${VERSION:-0.2.0-beta.1}"
ARCH="${ARCH:-amd64}"

die() {
    printf 'package-linux: %s\n' "$*" >&2
    exit 1
}

[[ "$VERSION" =~ ^[0-9A-Za-z][0-9A-Za-z.+-]*$ ]] || die "invalid VERSION: $VERSION"
[[ "$ARCH" == "amd64" ]] || die "only ARCH=amd64 is currently supported (received: $ARCH)"
[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux; package this target on Ubuntu 24.04 x86_64"
[[ "$(uname -m)" == "x86_64" ]] || die "must run on x86_64 (received: $(uname -m))"

if [[ -r /etc/os-release ]]; then
    # Read OS identification in a subshell so /etc/os-release's VERSION cannot
    # overwrite the release VERSION supplied by the CI job.
    DISTRO_ID="$(. /etc/os-release; printf '%s' "${ID:-}")"
    DISTRO_VERSION_ID="$(. /etc/os-release; printf '%s' "${VERSION_ID:-}")"
    DISTRO_PRETTY_NAME="$(. /etc/os-release; printf '%s' "${PRETTY_NAME:-unknown}")"
else
    die "cannot identify Linux distribution (/etc/os-release missing)"
fi
[[ "$DISTRO_ID" == "ubuntu" && "$DISTRO_VERSION_ID" == "24.04" ]] || \
    die "packaging is pinned to Ubuntu 24.04; found $DISTRO_PRETTY_NAME"

for tool in curl sha256sum ldd readelf file pkg-config dpkg-query dpkg-architecture desktop-file-validate find realpath patchelf cc; do
    command -v "$tool" >/dev/null 2>&1 || die "required tool is missing: $tool"
done

APP_BINARY="$ROOT_DIR/bin/tamias/tamias"
ICON="$ROOT_DIR/build/assets/app-icon.png"
DESKTOP="$ROOT_DIR/build/assets/io.tamiops.tami.desktop"
APP_LICENSE="$ROOT_DIR/LICENSE"
APP_NOTICES="$ROOT_DIR/THIRD_PARTY_NOTICES.txt"
GTK_PLUGIN="$ROOT_DIR/build/linux/linuxdeploy-plugin-gtk.sh"
APP_RUN="$ROOT_DIR/build/linux/AppRun"
WEBKIT_PATH_SHIM_SOURCE="$ROOT_DIR/build/linux/webkit-path-shim.c"
[[ -x "$APP_BINARY" ]] || die "built Linux binary is missing or not executable: $APP_BINARY"
[[ -s "$ICON" ]] || die "application icon is missing or empty: $ICON"
[[ -s "$DESKTOP" ]] || die "desktop entry is missing or empty: $DESKTOP"
[[ -s "$APP_LICENSE" ]] || die "root LICENSE is missing or empty"
[[ -s "$APP_NOTICES" ]] || die "root THIRD_PARTY_NOTICES.txt is missing or empty"
[[ -s "$GTK_PLUGIN" ]] || die "vendored GTK plugin is missing"
[[ -s "$APP_RUN" ]] || die "project AppRun launcher is missing"
[[ -s "$WEBKIT_PATH_SHIM_SOURCE" ]] || die "WebKit sandbox path shim source is missing"

readelf -h "$APP_BINARY" | grep -Eq 'Class:[[:space:]]+ELF64' || die "binary is not ELF64: $APP_BINARY"
readelf -h "$APP_BINARY" | grep -Eq 'Machine:[[:space:]]+Advanced Micro Devices X86-64' || die "binary is not x86_64: $APP_BINARY"
desktop-file-validate "$DESKTOP"
grep -Eq '^Exec=tamias([[:space:]]|$)' "$DESKTOP" || die "desktop entry must launch Exec=tamias"
grep -Fxq 'Icon=io.tamiops.tami' "$DESKTOP" || die "desktop entry must use Icon=io.tamiops.tami"
grep -Fxq 'StartupWMClass=Tamias' "$DESKTOP" || die "desktop entry must match the Tamias window class"

pkg-config --exists gtk4 webkitgtk-6.0 || die "GTK4 and WebKitGTK 6.0 development metadata is required (install libgtk-4-dev and libwebkitgtk-6.0-dev)"
printf 'GTK %s; WebKitGTK %s\n' "$(pkg-config --modversion gtk4)" "$(pkg-config --modversion webkitgtk-6.0)"

GTK4_LIBDIR="$(pkg-config --variable=libdir gtk4)"
GTK4_BINARY_VERSION="$(pkg-config --variable=gtk_binary_version gtk4)"
FCITX_GTK4_MODULE="$GTK4_LIBDIR/gtk-4.0/$GTK4_BINARY_VERSION/immodules/libim-fcitx5.so"
[[ -s "$FCITX_GTK4_MODULE" ]] || die "Fcitx5 GTK4 input module is missing: install fcitx5-frontend-gtk4"
fcitx_ldd="$(ldd "$FCITX_GTK4_MODULE" 2>&1 || true)"
if grep -q 'not found' <<<"$fcitx_ldd"; then
    printf '%s\n' "$fcitx_ldd" >&2
    die "Fcitx5 GTK4 input module has unresolved system libraries"
fi

binary_ldd="$({ ldd "$APP_BINARY" 2>&1 || true; })"
if grep -q 'not found' <<<"$binary_ldd"; then
    printf '%s\n' "$binary_ldd" >&2
    die "the application binary has unresolved system libraries"
fi
grep -q 'libgtk-4\.so' <<<"$binary_ldd" || die "binary is not linked against GTK4"
grep -q 'libwebkitgtk-6\.0\.so' <<<"$binary_ldd" || die "binary is not linked against WebKitGTK 6"

OUTPUT_DIR="$ROOT_DIR/dist"
OUTPUT_NAME="tamias-${VERSION}-linux-${ARCH}.AppImage"
OUTPUT="$OUTPUT_DIR/$OUTPUT_NAME"
TMP_ROOT="${TMPDIR:-/tmp}"
mkdir -p -- "$TMP_ROOT"
TMP_ROOT="$(cd -- "$TMP_ROOT" && pwd -P)"
mkdir -p -- "$OUTPUT_DIR"
if [[ -e "$OUTPUT" && ( -d "$OUTPUT" || -L "$OUTPUT" ) ]]; then
    die "refusing to replace a directory or symlink at output path: $OUTPUT"
fi

WORK_DIR="$(mktemp -d "$TMP_ROOT/tamias-appimage.XXXXXXXX")"
STAGED_OUTPUT=""
cleanup() {
    local status=$?
    if [[ -n "$STAGED_OUTPUT" && -f "$STAGED_OUTPUT" ]]; then
        rm -f -- "$STAGED_OUTPUT"
    fi
    if [[ -n "$WORK_DIR" && -d "$WORK_DIR" && "$WORK_DIR" == "$TMP_ROOT"/tamias-appimage.* ]]; then
        rm -rf -- "$WORK_DIR"
    fi
    exit "$status"
}
trap cleanup EXIT

TOOLS_DIR="$WORK_DIR/tools"
APP_DIR="$WORK_DIR/tamias-x86_64.AppDir"
mkdir -p -- "$TOOLS_DIR" "$APP_DIR/usr/bin" "$APP_DIR/usr/share/doc/tamias" \
    "$APP_DIR/usr/share/applications" "$APP_DIR/usr/share/icons"

fetch_pinned() {
    local url="$1" expected="$2" destination="$3" actual
    printf 'Downloading %s\n' "${url##*/}"
    curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 20 --max-time 900 --retry 3 --retry-all-errors \
        --output "$destination" "$url"
    actual="$(sha256sum "$destination" | awk '{print $1}')"
    [[ "$actual" == "$expected" ]] || die "SHA-256 mismatch for ${url##*/}: expected $expected, got $actual"
    chmod 0755 "$destination"
}

LINUXDEPLOY="$TOOLS_DIR/linuxdeploy-x86_64.AppImage"
APPIMAGETOOL="$TOOLS_DIR/appimagetool-x86_64.AppImage"
TYPE2_RUNTIME="$TOOLS_DIR/runtime-x86_64"
fetch_pinned \
    'https://github.com/linuxdeploy/linuxdeploy/releases/download/1-alpha-20251107-1/linuxdeploy-x86_64.AppImage' \
    'c20cd71e3a4e3b80c3483cef793cda3f4e990aca14014d23c544ca3ce1270b4d' "$LINUXDEPLOY"
fetch_pinned \
    'https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-x86_64.AppImage' \
    'ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0' "$APPIMAGETOOL"
fetch_pinned \
    'https://github.com/AppImage/type2-runtime/releases/download/20251108/runtime-x86_64' \
    '2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d' "$TYPE2_RUNTIME"

printf '%s\n' 'Preparing AppDir'
install -m 0755 "$APP_BINARY" "$APP_DIR/usr/bin/tamias"
install -m 0644 "$ICON" "$APP_DIR/io.tamiops.tami.png"
ln -s io.tamiops.tami.png "$APP_DIR/.DirIcon"
install -m 0644 "$DESKTOP" "$APP_DIR/io.tamiops.tami.desktop"
# AppRun adds usr/share to XDG_DATA_DIRS. GTK's unthemed icon fallback searches
# each data root's icons directory, so no host icon installation is required.
ln -s ../../../io.tamiops.tami.png "$APP_DIR/usr/share/icons/io.tamiops.tami.png"
ln -s ../../../io.tamiops.tami.desktop "$APP_DIR/usr/share/applications/io.tamiops.tami.desktop"
install -m 0755 "$APP_RUN" "$APP_DIR/AppRun"
install -m 0644 "$APP_LICENSE" "$APP_DIR/usr/share/doc/tamias/LICENSE"
install -m 0644 "$APP_NOTICES" "$APP_DIR/usr/share/doc/tamias/THIRD_PARTY_NOTICES.txt"
cat >> "$APP_DIR/usr/share/doc/tamias/THIRD_PARTY_NOTICES.txt" <<'EOF'

Additional license and copyright notices for AppImage packaging tools and the
bundled Ubuntu GTK/WebKitGTK runtime are installed beside this file. See
APPIMAGE_PACKAGING.txt and system-dependencies/ for their licenses, versions,
and source package information.
EOF
install -m 0644 "$ROOT_DIR/build/linux/packaging-provenance.txt" "$APP_DIR/usr/share/doc/tamias/APPIMAGE_PACKAGING.txt"
install -m 0644 "$ROOT_DIR"/build/linux/licenses/* "$APP_DIR/usr/share/doc/tamias/"

[[ "$(sha256sum "$GTK_PLUGIN" | awk '{print $1}')" == \
   'b0f4cbc684a0103a9651f0955b635eaea0096b3a66c0f5a2c2aa337960375171' ]] || \
    die "vendored linuxdeploy GTK plugin differs from the pinned upstream source"
chmod 0755 "$GTK_PLUGIN"
export DEPLOY_GTK_VERSION=4
export NO_STRIP=1
export PATH="$ROOT_DIR/build/linux:$TOOLS_DIR:$PATH"

declare -a WEBKIT_EXECUTABLES=()
declare -a WEBKIT_LIBRARIES=()
find_webkit_file() {
    local name="$1" required="$2" found=0 path target
    while IFS= read -r -d '' path; do
        [[ -f "$path" ]] || continue
        found=1
        if [[ "$name" == WebKitWebProcess && -z "${WEBKIT_SOURCE_EXEC_DIR:-}" ]]; then
            WEBKIT_SOURCE_EXEC_DIR="${path%/*}"
        fi
        target="$APP_DIR/${path#/}"
        mkdir -p -- "$(dirname -- "$target")"
        install -m "$(stat -c '%a' "$path")" "$path" "$target"
        case "$name" in
            WebKitWebProcess|WebKitNetworkProcess|WebKitGPUProcess)
                WEBKIT_EXECUTABLES+=("$target")
                ;;
            libwebkitgtkinjectedbundle.so)
                WEBKIT_LIBRARIES+=("$target")
                ;;
        esac
    done < <(find /usr/lib /usr/libexec -type f -name "$name" -print0 2>/dev/null | sort -z)
    if [[ "$required" == yes && $found -eq 0 ]]; then
        die "required WebKitGTK 6 runtime component was not found: $name"
    fi
}

find_webkit_file WebKitWebProcess yes
find_webkit_file WebKitNetworkProcess yes
find_webkit_file WebKitGPUProcess no
find_webkit_file libwebkitgtkinjectedbundle.so yes

WEBKIT_EXEC_PATH_REL="${WEBKIT_EXECUTABLES[0]#"$APP_DIR"/}"
WEBKIT_EXEC_PATH_REL="${WEBKIT_EXEC_PATH_REL%/*}"
WEBKIT_SOURCE_EXEC_PATH="$WEBKIT_SOURCE_EXEC_DIR"
for helper in "${WEBKIT_EXECUTABLES[@]}"; do
    helper_dir="${helper%/*}"
    [[ "$helper_dir" == "$APP_DIR/$WEBKIT_EXEC_PATH_REL" ]] || \
        die "WebKit helper binaries are split across directories; cannot set one relocatable WEBKIT_EXEC_PATH"
done
WEBKIT_BUNDLE_PATH_REL="${WEBKIT_LIBRARIES[0]#"$APP_DIR"/}"
WEBKIT_BUNDLE_PATH_REL="${WEBKIT_BUNDLE_PATH_REL%/*}"
printf '%s\n%s\n%s\n' "$WEBKIT_EXEC_PATH_REL" "$WEBKIT_BUNDLE_PATH_REL" "$WEBKIT_SOURCE_EXEC_PATH" > "$APP_DIR/.tamiops-webkit-paths"

# WebKitGTK ships translated strings outside the shared object. Include them
# along with any optional versioned data tree present in Ubuntu's runtime pkg.
while IFS= read -r -d '' path; do
    target="$APP_DIR/${path#/}"
    mkdir -p -- "$(dirname -- "$target")"
    install -m 0644 "$path" "$target"
done < <(find /usr/share/locale -type f -name 'WebKitGTK-6.0.mo' -print0 2>/dev/null | sort -z)
if [[ -d /usr/share/webkitgtk-6.0 ]]; then
    mkdir -p -- "$APP_DIR/usr/share"
    cp -a -- /usr/share/webkitgtk-6.0 "$APP_DIR/usr/share/"
fi

for helper in "${WEBKIT_EXECUTABLES[@]}"; do
    [[ -x "$helper" ]] || die "copied WebKit helper is not executable: $helper"
    helper_ldd="$({ ldd "$helper" 2>&1 || true; })"
    if grep -q 'not found' <<<"$helper_ldd"; then
        printf '%s\n' "$helper_ldd" >&2
        die "WebKit helper has unresolved system libraries: ${helper##*/}"
    fi
done
for library in "${WEBKIT_LIBRARIES[@]}"; do
    library_ldd="$({ ldd "$library" 2>&1 || true; })"
    if grep -q 'not found' <<<"$library_ldd"; then
        printf '%s\n' "$library_ldd" >&2
        die "WebKit injected bundle has unresolved system libraries: ${library##*/}"
    fi
done

printf 'Deploying GTK4, WebKitGTK6, and their process helpers into AppDir\n'
declare -a LINUXDEPLOY_ARGS=(--appimage-extract-and-run --appdir "$APP_DIR" --executable "$APP_DIR/usr/bin/tamias")
for helper in "${WEBKIT_EXECUTABLES[@]}"; do
    LINUXDEPLOY_ARGS+=(--executable "$helper")
done
for library in "${WEBKIT_LIBRARIES[@]}"; do
    LINUXDEPLOY_ARGS+=(--library "$library")
done
# GTK loads input methods dynamically, so linuxdeploy cannot discover this
# dependency from the application ELF. Deploy the Fcitx module explicitly;
# the GTK plugin also copies the GTK4 module directory into its canonical path.
LINUXDEPLOY_ARGS+=(--library "$FCITX_GTK4_MODULE")
LINUXDEPLOY_ARGS+=(--plugin gtk)
APPIMAGE_EXTRACT_AND_RUN=1 "$LINUXDEPLOY" "${LINUXDEPLOY_ARGS[@]}"

# WebKit's release helper lookup is a compiled absolute path, while the AppDir
# lives under a per-run FUSE mount. The launcher shim changes only the three
# exact WebKit helper argv values to their AppDir paths. Bubblewrap's own
# LD_LIBRARY_PATH bind handling then exposes the bundled usr/lib subtree at the
# same absolute path inside the namespace; no sandbox flags are changed.
WEBKIT_PATH_SHIM="$APP_DIR/usr/lib/libtamiops-webkit-paths.so"
mkdir -p -- "${WEBKIT_PATH_SHIM%/*}"
cc -shared -fPIC -O2 -Wall -Wextra -Werror -Wl,-z,relro,-z,now \
    -o "$WEBKIT_PATH_SHIM" "$WEBKIT_PATH_SHIM_SOURCE" -ldl
[[ -s "$WEBKIT_PATH_SHIM" ]] || die "WebKit path shim compilation produced no output"
for elf in "$APP_DIR/usr/bin/tamias" "${WEBKIT_EXECUTABLES[@]}" "${WEBKIT_LIBRARIES[@]}"; do
    if [[ "$elf" != "$APP_DIR/usr/bin/tamias" ]]; then
        # linuxdeploy can also create a flattened usr/bin copy for the
        # --executable argument. WebKit starts its original libexec copy, so
        # give that original helper (and the injected bundle) an AppDir-local
        # search path explicitly as well.
        patchelf --set-rpath '$ORIGIN:$ORIGIN/..:$ORIGIN/../..:$ORIGIN/../../..' "$elf"
    fi
    elf_rpath="$(patchelf --print-rpath "$elf" 2>/dev/null || true)"
    [[ "$elf_rpath" == *'$ORIGIN'* ]] || \
        die "linuxdeploy did not set an AppDir-relative RPATH on ${elf#"$APP_DIR"/}"
done

BUNDLED_FCITX_GTK4_MODULE="$APP_DIR/usr/lib/gtk-4.0/$GTK4_BINARY_VERSION/immodules/libim-fcitx5.so"
[[ -s "$BUNDLED_FCITX_GTK4_MODULE" ]] || die "linuxdeploy did not bundle the Fcitx5 GTK4 input module"
bundled_fcitx_ldd="$(LD_LIBRARY_PATH="$APP_DIR/usr/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ldd "$BUNDLED_FCITX_GTK4_MODULE" 2>&1 || true)"
if grep -q 'not found' <<<"$bundled_fcitx_ldd"; then
    printf '%s\n' "$bundled_fcitx_ldd" >&2
    die "bundled Fcitx5 GTK4 input module has unresolved libraries"
fi
fcitx_client_path="$(awk '/libFcitx5GClient\.so/ { print $3; exit }' <<<"$bundled_fcitx_ldd")"
[[ "$fcitx_client_path" == "$APP_DIR"/* ]] || die "Fcitx5 GTK4 module client library was not bundled: ${fcitx_client_path:-not found}"

collect_package_notices() {
    local doc_dir="$APP_DIR/usr/share/doc/tamias/system-dependencies"
    local manifest="$doc_dir/packages.tsv"
    local package deployed rel source_path owner_output owner_line pkg metadata version source_pkg source_version pkg_doc out_dir candidate canonical multiarch rest
    declare -A packages=()
    declare -a source_candidates=()
    multiarch="$(dpkg-architecture -qDEB_HOST_MULTIARCH)"
    mkdir -p -- "$doc_dir"

    # Resolve deployed files using their original system path. linuxdeploy may
    # flatten multiarch libraries into usr/lib, so try only deterministic
    # system-library locations for those entries; never infer ownership from
    # an arbitrary same-basename file elsewhere on the host.
    while IFS= read -r -d '' deployed; do
        case "$deployed" in
            "$APP_DIR"/usr/lib/*|"$APP_DIR"/usr/libexec/*|"$APP_DIR"/usr/share/*) ;;
            *) continue ;;
        esac
        rel="/${deployed#"$APP_DIR"/}"
        source_candidates=("$rel")
        case "$rel" in
            /usr/lib/*)
                rest="${rel#/usr/lib/}"
                if [[ "$rest" != "$multiarch/"* ]]; then
                    source_candidates+=(
                        "/usr/lib/$multiarch/$rest"
                        "/lib/$multiarch/$rest"
                        "/usr/lib64/$rest"
                        "/lib64/$rest"
                    )
                fi
                ;;
        esac
        owner_output=""
        for candidate in "${source_candidates[@]}"; do
            case "$candidate" in
                /usr/lib/*|/usr/libexec/*|/usr/share/*|/usr/lib64/*|/lib/*|/lib64/*) ;;
                *) continue ;;
            esac
            [[ -e "$candidate" || -L "$candidate" ]] || continue
            owner_output="$(dpkg-query -S -- "$candidate" 2>/dev/null || true)"
            if [[ -z "$owner_output" ]]; then
                canonical="$(realpath -e -- "$candidate" 2>/dev/null || true)"
                case "$canonical" in
                    /usr/lib/*|/usr/libexec/*|/usr/share/*|/usr/lib64/*|/lib/*|/lib64/*)
                        if [[ -n "$canonical" ]]; then
                            owner_output="$(dpkg-query -S -- "$canonical" 2>/dev/null || true)"
                        fi
                        ;;
                esac
            fi
            [[ -n "$owner_output" ]] && break
        done
        while IFS= read -r owner_line; do
            [[ -n "$owner_line" ]] || continue
            pkg="${owner_line%%: *}"
            [[ "$pkg" != "$owner_line" ]] || continue
            packages["$pkg"]=1
        done <<<"$owner_output"
    done < <(find "$APP_DIR/usr/lib" "$APP_DIR/usr/libexec" "$APP_DIR/usr/share" \
        \( -type f -o -type l \) -print0 2>/dev/null)

    # The find expression above deliberately keeps symlinked library files too;
    # sort package identifiers so the in-image inventory is stable.
    : > "$manifest"
    ((${#packages[@]} > 0)) || die "could not map deployed GTK/WebKit system files to Ubuntu packages"
    while IFS= read -r package; do
        [[ -n "$package" ]] || continue
        metadata="$(dpkg-query -W -f='${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\n' "$package" 2>/dev/null || true)"
        [[ -n "$metadata" ]] || die "could not read installed package metadata for $package"
        IFS=$'\t' read -r pkg version source_pkg source_version <<<"$metadata"
        source_pkg="${source_pkg:-${pkg%%:*}}"
        source_version="${source_version:-$version}"
        [[ -n "$version" ]] || die "missing installed package version for $package"
        pkg_doc="/usr/share/doc/${pkg%%:*}/copyright"
        [[ -s "$pkg_doc" ]] || die "deployed package has no installed copyright notice: $pkg ($pkg_doc)"
        out_dir="$doc_dir/${pkg%%:*}"
        mkdir -p -- "$out_dir"
        cp -L -- "$pkg_doc" "$out_dir/copyright"
        printf '%s\t%s\t%s\t%s\n' "$pkg" "$version" "$source_pkg" "$source_version" >> "$manifest"
    done < <(printf '%s\n' "${!packages[@]}" | LC_ALL=C sort)

    cat > "$doc_dir/README.txt" <<'EOF'
System dependency notices
=========================

copyright files above correspond to Ubuntu binary packages that own system
libraries, WebKitGTK helper processes, GTK resources, or other system data
copied into this AppImage. packages.tsv records each binary package version
and its source package/version. On Ubuntu 24.04 (Noble), enable deb-src entries
and retrieve corresponding LGPL/GPL and other covered source with:

  apt-get source SOURCE_PACKAGE=SOURCE_VERSION

The application notices and the AppImage tooling notices are in the parent
directory. Ubuntu's base ABI libraries that AppImage intentionally leaves to
the host (for example glibc) are not copied into this image.
EOF
}

collect_package_notices

STAGED_OUTPUT="$OUTPUT_DIR/.${OUTPUT_NAME%.AppImage}.staging-$$-${RANDOM}.AppImage"
[[ ! -e "$STAGED_OUTPUT" ]] || die "staging path already exists: $STAGED_OUTPUT"
printf 'Creating %s\n' "$OUTPUT"
ARCH=x86_64 APPIMAGE_EXTRACT_AND_RUN=1 "$APPIMAGETOOL" --runtime-file "$TYPE2_RUNTIME" "$APP_DIR" "$STAGED_OUTPUT"
[[ -s "$STAGED_OUTPUT" ]] || die "appimagetool returned without producing an image"
file "$STAGED_OUTPUT" | grep -q 'ELF 64-bit.*x86-64' || die "output is not an x86_64 AppImage ELF"
chmod 0755 "$STAGED_OUTPUT"
mv -f -- "$STAGED_OUTPUT" "$OUTPUT"
STAGED_OUTPUT=""
printf 'AppImage ready: %s\n' "$OUTPUT"
