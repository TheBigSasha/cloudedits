#!/usr/bin/env bash
set -euo pipefail

# Verify that a compiled release binary contains zero dev_apis symbols.
if [[ $# -lt 1 ]]; then
  echo "usage: $0 <binary-path> [<binary-path>...]" >&2
  exit 2
fi

SYMBOL_PATTERN='devapi|registerDevRoutesImpl|handleDev(Reset|Fault|Stats)|devFaultConfig|faultReadSeeker'

for bin in "$@"; do
  if [[ ! -f "$bin" ]]; then
    echo "ERROR: binary not found: $bin" >&2
    exit 1
  fi

  if go tool nm "$bin" 2>/dev/null | grep -E "$SYMBOL_PATTERN"; then
    echo "ERROR: dev_apis symbol leaked into release binary (nm): $bin" >&2
    exit 1
  fi

  if command -v strings >/dev/null 2>&1; then
    if strings "$bin" | grep -E 'registerDevRoutesImpl|handleDev(Reset|Fault|Stats)|/__dev/(fault|reset|stats)'; then
      echo "ERROR: dev_apis marker leaked into release binary (strings): $bin" >&2
      exit 1
    fi
  fi

  echo "OK: zero dev_apis symbols in $bin"
done
