#!/bin/sh
# Build the YAM v2 package.
#
# Requires python3 + zstandard for the deterministic packer.
#   ./package.sh    # -> dist/athena-assistant-<version>.yampkg
set -eu
root=$(CDPATH='' cd "$(dirname "$0")" && pwd)
cd "$root"
sh build.sh
python3 scripts/pack.py
