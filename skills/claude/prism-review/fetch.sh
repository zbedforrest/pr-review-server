#!/usr/bin/env bash
# Backward-compatible entry point for existing prism-review installations.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$SCRIPT_DIR/scripts/prism.sh" fetch "$@"
