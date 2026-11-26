#!/bin/sh
# Build the static aarch64 station daemon.
#
#   ./build.sh    # -> dist/athena
set -eu
root=$(CDPATH='' cd "$(dirname "$0")" && pwd)
cd "$root"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/athena ./cmd/athena
echo "built dist/athena"
