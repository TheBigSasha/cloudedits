#!/usr/bin/env bash
set -euo pipefail

# Cross-build the Windows x64 (.msi) installer using msitools (wixl) on macOS/Linux.
# Note: msitools/wixl supports x86 and x64 only (not arm64); use packaging/windows/build-msi.ps1
# with WiX v5 on Windows to build both amd64 and arm64 MSIs.

if ! command -v wixl >/dev/null 2>&1; then
  echo "ERROR: wixl (msitools) is required (brew install msitools / apt install wixl)." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${VERSION:-dev}"
OUT_DIR="${OUT_DIR:-$ROOT_DIR/dist}"
SMOKE_TEST=0

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
    --smoke-test)
      SMOKE_TEST=1
      shift
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

NUMERIC_VERSION="${VERSION#v}"
NUMERIC_VERSION="${NUMERIC_VERSION%%-*}"
if [[ ! "$NUMERIC_VERSION" =~ ^[0-9]+(\.[0-9]+){1,3}$ ]]; then
  NUMERIC_VERSION="0.0.0"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cd "$ROOT_DIR"

BIN="$WORK/cloudedits.exe"
echo "==> Cross-compiling windows/amd64 (${VERSION})"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
  -o "$BIN" ./cmd/cloudedits

"$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$BIN"

OUT_MSI="$OUT_DIR/cloudedits-windows-amd64.msi"
rm -f "$OUT_MSI"

echo "==> Building cloudedits-windows-amd64.msi with wixl (Version=${NUMERIC_VERSION})"
wixl -v -a x64 \
  -D "Version=${NUMERIC_VERSION}" \
  -D "BinaryPath=${BIN}" \
  -D "IconPath=${ROOT_DIR}/packaging/assets/cloudedits.ico" \
  -D "LicensePath=${ROOT_DIR}/LICENSE" \
  -D "ReadmePath=${ROOT_DIR}/README.md" \
  -o "$OUT_MSI" \
  "$ROOT_DIR/packaging/windows/cloudedits-wixl.wxs"

ls -lh "$OUT_MSI"

if [[ "$SMOKE_TEST" -eq 1 ]]; then
  echo "==> Smoke-testing $OUT_MSI with msiinfo + msiextract"
  SUMINFO="$(msiinfo suminfo "$OUT_MSI")"
  echo "$SUMINFO"
  if [[ "$SUMINFO" != *"Template: x64;1033"* ]]; then
    echo "ERROR: expected Template: x64;1033 in MSI suminfo" >&2
    exit 1
  fi

  EXTRACT_DIR="$WORK/extract"
  mkdir -p "$EXTRACT_DIR"
  msiextract -C "$EXTRACT_DIR" "$OUT_MSI" >/dev/null
  EXTRACTED_EXE="$(find "$EXTRACT_DIR" -name 'cloudedits.exe' -print -quit)"
  if [[ -z "$EXTRACTED_EXE" || ! -f "$EXTRACTED_EXE" ]]; then
    echo "ERROR: cloudedits.exe missing after msiextract" >&2
    exit 1
  fi

  python3 -c '
import struct, sys
data = open(sys.argv[1], "rb").read()
pe_off = struct.unpack_from("<I", data, 0x3C)[0]
sig, machine = struct.unpack_from("<IH", data, pe_off)
assert sig == 0x00004550, f"bad PE sig: {sig:#x}"
assert machine == 0x8664, f"expected 0x8664 (AMD64), got {machine:#06x}"
print(f"OK: extracted MSI PE machine 0x{machine:04X} ({sys.argv[1]})")
' "$EXTRACTED_EXE"

  "$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$EXTRACTED_EXE"
  echo "OK: Windows amd64 MSI smoke test passed."
fi
