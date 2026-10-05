#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
. scripts/build-env.sh
export CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=12.0"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -mmacosx-version-min=12.0"
npm --prefix frontend run build
app=bin/tamias.app
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
go build -trimpath -ldflags "$BUILD_LDFLAGS" -tags production -o "$app/Contents/MacOS/tamias" .
cp LICENSE "$app/Contents/Resources/LICENSE"
cp THIRD_PARTY_NOTICES.txt "$app/Contents/Resources/THIRD_PARTY_NOTICES.txt"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleDevelopmentRegion</key><string>en</string>
<key>CFBundleName</key><string>Tamias</string>
<key>CFBundleDisplayName</key><string>Tamias</string>
<key>CFBundleIdentifier</key><string>io.tamiops.tami</string>
<key>CFBundleExecutable</key><string>tamias</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$PACKAGE_VERSION</string>
<key>CFBundleVersion</key><string>$BUILD_NUMBER</string>
<key>CFBundleGetInfoString</key><string>Tamias $VERSION</string>
<key>CFBundleIconFile</key><string>app.icns</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSAppTransportSecurity</key><dict><key>NSAllowsArbitraryLoads</key><true/></dict>
<key>LSMinimumSystemVersion</key><string>12.0</string>
</dict></plist>
PLIST
mkdir -p "$app/Contents/Resources/zh_CN.lproj"
cat > "$app/Contents/Resources/zh_CN.lproj/InfoPlist.strings" <<'LOCALIZATION'
"CFBundleName" = "小花鼠";
"CFBundleDisplayName" = "小花鼠";
LOCALIZATION
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
