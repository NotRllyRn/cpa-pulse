#!/bin/sh
# Builds the plugin for the CPU architecture of the Docker host.
# Usage: sh scripts/build.sh [version]
set -eu
VERSION=${1:-0.0.0-dev}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$ROOT/dist"
docker run --rm -v "$ROOT:/src" -w /src -v cpa-pulse-gomod:/go/pkg/mod golang:1.26-bookworm \
  sh -c "go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags '-s -w -X main.version=$VERSION' -o dist/cpa-pulse-v$VERSION.so . && rm -f dist/cpa-pulse-v$VERSION.h"
echo "$ROOT/dist/cpa-pulse-v$VERSION.so"
