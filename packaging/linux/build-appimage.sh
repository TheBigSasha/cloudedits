#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "ERROR: build-appimage.sh must run on Linux." >&2
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

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cd "$ROOT_DIR"

HOST_ARCH="$(uname -m)"
if [[ "$HOST_ARCH" != "x86_64" && "$HOST_ARCH" != "aarch64" ]]; then
  echo "ERROR: unsupported Linux build host arch: $HOST_ARCH" >&2
  exit 1
fi

APPIMAGETOOL="$WORK/appimagetool.AppImage"
if command -v appimagetool >/dev/null 2>&1; then
  APPIMAGETOOL_CMD=(appimagetool)
else
  echo "==> Downloading appimagetool (${HOST_ARCH})"
  curl -fsSL "https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-${HOST_ARCH}.AppImage" -o "$APPIMAGETOOL"
  chmod +x "$APPIMAGETOOL"
  APPIMAGETOOL_CMD=("$APPIMAGETOOL" --appimage-extract-and-run)
fi

build_one() {
  local goarch="$1"
  local app_arch="$2"
  local out_name="cloudedits-linux-${goarch}.AppImage"
  local out_path="$OUT_DIR/$out_name"
  local appdir="$WORK/AppDir-${goarch}"

  mkdir -p "$appdir/usr/bin" "$appdir/usr/share/applications" "$appdir/usr/share/icons/hicolor/256x256/apps"

  echo "==> Cross-compiling linux/${goarch} (${VERSION})"
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$appdir/usr/bin/cloudedits" ./cmd/cloudedits
  chmod 755 "$appdir/usr/bin/cloudedits"

  "$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$appdir/usr/bin/cloudedits"

  cp "$ROOT_DIR/packaging/linux/AppRun" "$appdir/AppRun"
  chmod 755 "$appdir/AppRun"

  cp "$ROOT_DIR/packaging/linux/cloudedits.desktop" "$appdir/cloudedits.desktop"
  cp "$ROOT_DIR/packaging/linux/cloudedits.desktop" "$appdir/usr/share/applications/cloudedits.desktop"

  cp "$ROOT_DIR/packaging/assets/cloudedits-256.png" "$appdir/cloudedits.png"
  cp "$ROOT_DIR/packaging/assets/cloudedits-256.png" "$appdir/usr/share/icons/hicolor/256x256/apps/cloudedits.png"
  ln -sf cloudedits.png "$appdir/.DirIcon"

  local runtime_file="$WORK/runtime-${app_arch}"
  if [[ ! -f "$runtime_file" ]]; then
    echo "==> Downloading AppImage type2 runtime (${app_arch})"
    curl -fsSL "https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-${app_arch}" -o "$runtime_file"
  fi

  rm -f "$out_path"
  echo "==> Packaging $out_name (ARCH=${app_arch})"
  ARCH="$app_arch" APPIMAGE_EXTRACT_AND_RUN=1 \
    "${APPIMAGETOOL_CMD[@]}" \
    --no-appstream \
    --runtime-file "$runtime_file" \
    "$appdir" \
    "$out_path"
  chmod 755 "$out_path"
  ls -lh "$out_path"
}

build_one amd64 x86_64
build_one arm64 aarch64

if [[ "$SMOKE_TEST" -eq 1 ]]; then
  echo "==> Smoke-testing Linux AppImages"

  # 1. Verify amd64 AppImage via --appimage-extract and live execution on x86_64 host.
  AMD64_IMG="$OUT_DIR/cloudedits-linux-amd64.AppImage"
  ARM64_IMG="$OUT_DIR/cloudedits-linux-arm64.AppImage"

  echo "file(amd64): $(file "$AMD64_IMG")"
  echo "file(arm64): $(file "$ARM64_IMG")"

  if [[ "$(file "$ARM64_IMG")" != *"aarch64"* && "$(file "$ARM64_IMG")" != *"ARM aarch64"* ]]; then
    echo "ERROR: expected aarch64 AppImage runtime for $ARM64_IMG" >&2
    exit 1
  fi

  if [[ "$HOST_ARCH" == "x86_64" ]]; then
    EXTRACT_DIR="$WORK/extract-amd64"
    mkdir -p "$EXTRACT_DIR"
    (
      cd "$EXTRACT_DIR"
      "$AMD64_IMG" --appimage-extract >/dev/null
      VER_JSON="$("./squashfs-root/usr/bin/cloudedits" version --json)"
      echo "amd64 usr/bin/cloudedits version --json: $VER_JSON"
      if [[ "$VER_JSON" != *"\"version\": \"${VERSION}\""* && "$VER_JSON" != *"\"version\":\"${VERSION}\""* ]]; then
        echo "ERROR: version mismatch in amd64 AppImage: $VER_JSON" >&2
        exit 1
      fi
      APPRUN_JSON="$("./squashfs-root/AppRun" version --json)"
      echo "amd64 AppRun version --json: $APPRUN_JSON"
      if [[ "$APPRUN_JSON" != *"\"version\": \"${VERSION}\""* && "$APPRUN_JSON" != *"\"version\":\"${VERSION}\""* ]]; then
        echo "ERROR: AppRun passthrough version mismatch: $APPRUN_JSON" >&2
        exit 1
      fi
    )
  fi

  # 2. Verify arm64 AppImage squashfs payload contains aarch64 ELF binary and executable AppRun.
  if command -v unsquashfs >/dev/null 2>&1; then
    OFFSET="$(python3 -c '
import sys
data = open(sys.argv[1], "rb").read()
idx = data.find(b"hsqs")
if idx < 0:
    sys.exit(1)
print(idx)
' "$ARM64_IMG")"
    ARM64_EXTRACT="$WORK/extract-arm64"
    unsquashfs -o "$OFFSET" -d "$ARM64_EXTRACT" "$ARM64_IMG" >/dev/null
    ARM64_BIN_FILE="$(file "$ARM64_EXTRACT/usr/bin/cloudedits")"
    echo "arm64 extracted binary: $ARM64_BIN_FILE"
    if [[ "$ARM64_BIN_FILE" != *"aarch64"* && "$ARM64_BIN_FILE" != *"ARM aarch64"* ]]; then
      echo "ERROR: expected aarch64 binary inside arm64 AppImage: $ARM64_BIN_FILE" >&2
      exit 1
    fi
    if [[ ! -x "$ARM64_EXTRACT/AppRun" ]]; then
      echo "ERROR: AppRun is not executable inside arm64 AppImage" >&2
      exit 1
    fi
  fi

  echo "OK: Linux AppImage smoke test passed."
fi
