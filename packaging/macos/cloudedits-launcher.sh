#!/bin/sh
set -eu

DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BIN="$DIR/cloudedits"

# Finder launches pass either 0 arguments or a legacy -psn_* process serial number.
if [ "$#" -eq 0 ]; then
  LOG_DIR="${HOME}/Library/Logs"
  mkdir -p "$LOG_DIR"
  exec "$BIN" gui >>"$LOG_DIR/cloudedits-gui.log" 2>&1
fi

case "$1" in
  -psn_*)
    LOG_DIR="${HOME}/Library/Logs"
    mkdir -p "$LOG_DIR"
    exec "$BIN" gui >>"$LOG_DIR/cloudedits-gui.log" 2>&1
    ;;
  *)
    exec "$BIN" "$@"
    ;;
esac
