#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
. scripts/build-env.sh
mkdir -p bin/cli
go build -trimpath -ldflags "$BUILD_LDFLAGS" -o "bin/cli/tamiops$(go env GOEXE)" ./cmd/tami
