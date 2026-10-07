#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
. scripts/build-env.sh
case "$(go env GOOS)" in
 darwin) exec sh scripts/build-macos.sh ;;
 windows) npm --prefix frontend run build; mkdir -p bin; go build -trimpath -ldflags "$BUILD_LDFLAGS -H windowsgui" -tags production -o bin/tamias.exe . ;;
 linux) npm --prefix frontend run build; mkdir -p bin/tamias; go build -trimpath -ldflags "$BUILD_LDFLAGS" -tags production -o bin/tamias/tamias .; cp build/assets/app-icon.png bin/tamias/io.tamiops.tami.png; cp build/assets/io.tamiops.tami.desktop bin/tamias/; cp LICENSE THIRD_PARTY_NOTICES.txt bin/tamias/ ;;
 *) echo 'Unsupported desktop platform' >&2; exit 1 ;;
esac
