#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
. scripts/build-env.sh
case "$(go env GOOS)" in
 darwin) exec sh scripts/build-macos.sh ;;
 windows) npm --prefix frontend run build; mkdir -p bin; go build -trimpath -ldflags "$BUILD_LDFLAGS -H windowsgui" -tags production -o bin/tamiops.exe . ;;
 linux) npm --prefix frontend run build; mkdir -p bin/tamiops; go build -trimpath -ldflags "$BUILD_LDFLAGS" -tags production -o bin/tamiops/tamiops .; cp build/assets/app-icon.png bin/tamiops/tamiops.png; cp build/assets/tamiops.desktop bin/tamiops/; cp LICENSE THIRD_PARTY_NOTICES.txt bin/tamiops/ ;;
 *) echo 'Unsupported desktop platform' >&2; exit 1 ;;
esac
