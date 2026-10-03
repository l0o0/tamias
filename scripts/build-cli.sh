#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
mkdir -p bin/cli
go build -trimpath -ldflags '-s -w' -o "bin/cli/tamiops$(go env GOEXE)" ./cmd/tami
