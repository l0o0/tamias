#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

: "${VERSION:?Set VERSION to the release version, for example 0.2.0-beta.1}"
: "${ARCH:?Set ARCH to arm64 or amd64}"
case "$VERSION" in
	[0-9]* ) ;;
	* ) printf '%s\n' "VERSION must start with a digit: $VERSION" >&2; exit 2 ;;
esac
case "$VERSION" in
	*[!A-Za-z0-9.+_-]* ) printf '%s\n' "VERSION contains characters that cannot be used in a package name: $VERSION" >&2; exit 2 ;;
esac
case "$ARCH" in
	arm64) pkg_arch=arm64 ;;
	amd64) pkg_arch=x86_64 ;;
	*) printf '%s\n' "ARCH must be arm64 or amd64: $ARCH" >&2; exit 2 ;;
esac

app="$root/bin/tamias.app"
executable="$app/Contents/MacOS/tamias"
license="$root/LICENSE"
resources="$root/build/macos/installer"
for path in "$app/Contents/Info.plist" "$executable" "$license" "$resources/Welcome.html" "$resources/Conclusion.html"; do
	if [ ! -f "$path" ]; then
		printf '%s\n' "Required packaging input is missing: $path" >&2
		exit 1
	fi
done

for tool in pkgbuild productbuild pkgutil plutil lipo ditto textutil; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf '%s\n' "Required macOS packaging tool is unavailable: $tool" >&2
		exit 1
	fi
done

bundle_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")
if [ "$bundle_id" != "io.tamiops.tami" ]; then
	printf '%s\n' "Unexpected application bundle identifier: $bundle_id" >&2
	exit 1
fi
actual_arch=$(/usr/bin/lipo -archs "$executable")
if [ "$actual_arch" != "$pkg_arch" ]; then
	printf '%s\n' "ARCH=$ARCH requires a $pkg_arch application binary; found: $actual_arch" >&2
	exit 1
fi

mkdir -p "$root/dist/.local"
stage=$(mktemp -d "$root/dist/.local/macos-${VERSION}-${ARCH}.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM
mkdir -p "$root/dist"

mkdir -p "$stage/root" "$stage/resources"
ditto "$app" "$stage/root/tamias.app"
ditto "$resources/Welcome.html" "$stage/resources/Welcome.html"
ditto "$resources/Conclusion.html" "$stage/resources/Conclusion.html"
textutil -convert rtf -font Menlo -fontsize 10 -output "$stage/resources/License.rtf" "$license"
if ! textutil -convert txt -stdout "$stage/resources/License.rtf" | cmp - "$license"; then
	printf '%s\n' "Installer license does not match the project LICENSE file" >&2
	exit 1
fi

cat > "$stage/components.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
  <dict>
    <key>RootRelativeBundlePath</key>
    <string>tamias.app</string>
    <key>BundleIsRelocatable</key>
    <false/>
    <key>BundleIsVersionChecked</key>
    <false/>
    <key>BundleHasStrictIdentifier</key>
    <true/>
    <key>BundleOverwriteAction</key>
    <string>upgrade</string>
  </dict>
</array>
</plist>
PLIST

cat > "$stage/requirements.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>os</key>
  <array><string>12.0</string></array>
  <key>arch</key>
  <array><string>$pkg_arch</string></array>
</dict>
</plist>
PLIST
plutil -lint "$stage/components.plist" >/dev/null
plutil -lint "$stage/requirements.plist" >/dev/null

pkgbuild \
	--root "$stage/root" \
	--component-plist "$stage/components.plist" \
	--identifier io.tamiops.tami.pkg \
	--version "$VERSION" \
	--install-location /Applications \
	--ownership recommended \
	"$stage/tamias-component.pkg"

productbuild \
	--synthesize \
	--product "$stage/requirements.plist" \
	--package "$stage/tamias-component.pkg" \
	"$stage/Distribution.base.xml"

awk '
/<choices-outline>/ && !inserted {
	print "  <title>小花鼠 Tamias</title>"
	print "  <welcome file=\"Welcome.html\"/>"
	print "  <license file=\"License.rtf\"/>"
	print "  <conclusion file=\"Conclusion.html\"/>"
	inserted = 1
}
{ print }
END { if (!inserted) exit 1 }
' "$stage/Distribution.base.xml" > "$stage/Distribution.xml"

output="$root/dist/tamias-${VERSION}-macos-${ARCH}.pkg"
productbuild \
	--distribution "$stage/Distribution.xml" \
	--package-path "$stage" \
	--resources "$stage/resources" \
	--identifier io.tamiops.tami \
	--version "$VERSION" \
	"$stage/product.pkg"

rm -rf "$stage/expanded"
pkgutil --expand-full "$stage/product.pkg" "$stage/expanded"
expanded_distribution="$stage/expanded/Distribution"
if [ ! -f "$expanded_distribution" ]; then
	printf '%s\n' "Expanded product is missing its Distribution file" >&2
	exit 1
fi
for resource in 'Welcome.html' 'License.rtf' 'Conclusion.html'; do
	if ! grep -Fq "$resource" "$expanded_distribution"; then
		printf '%s\n' "Distribution does not reference installer resource: $resource" >&2
		exit 1
	fi
done
if ! grep -Fq "hostArchitectures=\"$pkg_arch\"" "$expanded_distribution"; then
	printf '%s\n' "Distribution does not restrict installation to $pkg_arch" >&2
	exit 1
fi
if ! find "$stage/expanded" -path '*/Payload/tamias.app/Contents/MacOS/tamias' -type f -print -quit | grep -q .; then
	printf '%s\n' "Expanded product is missing the Tamias application payload" >&2
	exit 1
fi
if ! find "$stage/expanded" -name 'License.rtf' -type f -print -quit | grep -q .; then
	printf '%s\n' "Expanded product is missing its Installer license resource" >&2
	exit 1
fi

mv "$stage/product.pkg" "$output"
printf 'Created %s\n' "$output"
