#!/usr/bin/env sh
# POSIX launcher script for TORQ torture test harness
set -e
ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"

if command -v python3 >/dev/null 2>&1; then
    exec python3 "$ROOT_DIR/torture/torture.py" "$@"
elif command -v python >/dev/null 2>&1; then
    exec python "$ROOT_DIR/torture/torture.py" "$@"
else
    echo "Error: python3 or python not found in PATH" >&2
    exit 1
fi
