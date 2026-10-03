#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
export CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=11.0"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -mmacosx-version-min=11.0"
npm --prefix frontend run build
app=bin/tamiops.app
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
go build -trimpath -ldflags '-s -w' -tags production -o "$app/Contents/MacOS/tamiops" .
cat > "$app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>tamiops</string>
<key>CFBundleDisplayName</key><string>tamiops</string>
<key>CFBundleIdentifier</key><string>io.tamiops.tami</string>
<key>CFBundleExecutable</key><string>tamiops</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.2.0</string>
<key>CFBundleVersion</key><string>2</string>
<key>CFBundleIconFile</key><string>app.icns</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSAppTransportSecurity</key><dict><key>NSAllowsArbitraryLoads</key><true/></dict>
<key>LSMinimumSystemVersion</key><string>11.0</string>
</dict></plist>
PLIST
iconset="$app/Contents/Resources/app.iconset"
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
 sips -z "$size" "$size" build/assets/app-icon.png --out "$iconset/icon_${size}x${size}.png" >/dev/null
 double=$((size*2))
 sips -z "$double" "$double" build/assets/app-icon.png --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/app.icns"
rm -r "$iconset"
codesign --force --deep --sign - "$app"
du -sh "$app"
