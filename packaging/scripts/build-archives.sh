#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${VERSION:-dev}"
OUT_DIR="${OUT_DIR:-$ROOT_DIR/dist}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="$2"
      shift 2
      ;;
    --out-dir)
      OUT_DIR="$2"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cd "$ROOT_DIR"

TARGETS=(
  "darwin arm64"
  "darwin amd64"
  "linux amd64"
  "linux arm64"
  "windows amd64"
  "windows arm64"
)

for target in "${TARGETS[@]}"; do
  read -r goos goarch <<< "$target"
  ext=""
  if [[ "$goos" == "windows" ]]; then
    ext=".exe"
  fi

  stage="$WORK/${goos}_${goarch}"
  mkdir -p "$stage"
  bin="$stage/cloudedits${ext}"

  echo "==> Building ${goos}/${goarch} (${VERSION})"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$bin" ./cmd/cloudedits

  "$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$bin"

  cp "$ROOT_DIR/LICENSE" "$stage/LICENSE"
  cp "$ROOT_DIR/README.md" "$stage/README.md"

  if [[ "$goos" == "windows" ]]; then
    setup_bin="$stage/cloudedits-setup.exe"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags="-H=windowsgui -s -w -X main.version=${VERSION}" \
      -o "$setup_bin" ./cmd/cloudedits-setup
    "$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$setup_bin"

    archive="$OUT_DIR/cloudedits_${goos}_${goarch}.zip"
    rm -f "$archive"
    (
      cd "$stage"
      if command -v zip >/dev/null 2>&1; then
        zip -q -9 "$archive" "cloudedits.exe" "cloudedits-setup.exe" LICENSE README.md
      else
        python3 -c '
import sys, zipfile
out = sys.argv[1]
files = sys.argv[2:]
with zipfile.ZipFile(out, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
    for f in files:
        zf.write(f)
' "$archive" "cloudedits.exe" "cloudedits-setup.exe" LICENSE README.md
      fi
    )
  else
    archive="$OUT_DIR/cloudedits_${goos}_${goarch}.tar.gz"
    rm -f "$archive"
    tar -C "$stage" -czf "$archive" "cloudedits" LICENSE README.md
  fi

  ls -lh "$archive"
done

echo "==> Generating checksums.txt in $OUT_DIR"
(
  cd "$OUT_DIR"
  rm -f checksums.txt
  shopt -s nullglob
  files=(cloudedits_*.tar.gz cloudedits_*.zip cloudedits-*.dmg cloudedits-*.msi cloudedits-*.AppImage)
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "ERROR: no release artifacts found in $OUT_DIR" >&2
    exit 1
  fi
  IFS=$'\n' sorted=($(printf '%s\n' "${files[@]}" | LC_ALL=C sort))
  unset IFS
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${sorted[@]}" > checksums.txt
  else
    shasum -a 256 "${sorted[@]}" > checksums.txt
  fi
  cat checksums.txt
)
