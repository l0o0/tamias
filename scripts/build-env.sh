#!/bin/sh
# Source from a build script after changing to the repository root.
export GOTOOLCHAIN=local
export CGO_ENABLED=1
VERSION=${VERSION:-0.2.0-dev}
if ! printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "Invalid VERSION: expected a semantic version without a leading v" >&2
  exit 1
fi
PACKAGE_VERSION=${VERSION%%-*}
BUILD_NUMBER=${BUILD_NUMBER:-2}
case "$BUILD_NUMBER" in ''|*[!0-9]*) echo 'BUILD_NUMBER must be numeric' >&2; exit 1 ;; esac
export VERSION PACKAGE_VERSION BUILD_NUMBER
BUILD_LDFLAGS="-s -w -X tamiops/internal/core.Version=$VERSION"
export BUILD_LDFLAGS
