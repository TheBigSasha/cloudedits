#!/usr/bin/env bash
set -euo pipefail

# Public repository boundary check.
# Fails CI if any private internal identifier or dev-log path is present in tracked files.
# Patterns use single-character classes so this script never embeds forbidden literals.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# 1. Forbid dev_logs directories or dev-log markdown files in this public repository.
DEV_LOG_HITS="$(
  find . -path './.git' -prune -o \
    \( -type d -name 'dev_logs' -o -type f -name '*dev-log*.md' -o -type f -name '*dev_log*.md' \) \
    -print
)"
if [[ -n "$DEV_LOG_HITS" ]]; then
  echo "ERROR: dev-log paths are forbidden in this public repository:" >&2
  echo "$DEV_LOG_HITS" >&2
  exit 1
fi

# 2. Scan all repository files (excluding .git) for forbidden private identifiers.
PATTERNS=(
  '[u]ltrahdrutil'
  '[g]ainmappaint'
  '[b]ursthunter'
  '[i]ppd9000'
  '[l]ibrary-destroyer'
  '[l]ibrary destroyer 9000'
  '[r]edteam-9000'
  '[s]rc/store/'
  '[s]rc/workers/'
  '[w]ebapp/'
)

REGEX="$(IFS='|'; echo "${PATTERNS[*]}")"

MATCHES=""
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  # Scan both tracked and untracked non-ignored files so local pre-commit checks catch everything.
  FILES="$(git ls-files --cached --others --exclude-standard)"
  if [[ -n "$FILES" ]]; then
    while IFS= read -r file; do
      [[ -f "$file" ]] || continue
      if grep -nEi "$REGEX" "$file" >/dev/null 2>&1; then
        HIT="$(grep -nEi "$REGEX" "$file" | sed "s|^|$file:|")"
        MATCHES+="$HIT"$'\n'
      fi
    done <<< "$FILES"
  fi
else
  MATCHES="$(grep -rnEi --exclude-dir=.git "$REGEX" . || true)"
fi

if [[ -n "$MATCHES" ]]; then
  echo "ERROR: Forbidden private identifier(s) detected in public repository:" >&2
  printf '%s' "$MATCHES" >&2
  exit 1
fi

echo "OK: public boundary check passed."
