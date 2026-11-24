#!/usr/bin/env bash
set -euo pipefail

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

HOST_OS="$(uname -s)"
HOST_ARCH="$(uname -m)"

USE_APPIMAGETOOL=0
if [[ "$HOST_OS" == "Linux" ]]; then
  if [[ "$HOST_ARCH" != "x86_64" && "$HOST_ARCH" != "aarch64" ]]; then
    echo "ERROR: unsupported Linux build host arch: $HOST_ARCH" >&2
    exit 1
  fi
  USE_APPIMAGETOOL=1
  APPIMAGETOOL="$WORK/appimagetool.AppImage"
  if command -v appimagetool >/dev/null 2>&1; then
    APPIMAGETOOL_CMD=(appimagetool)
  else
    echo "==> Downloading appimagetool (${HOST_ARCH})"
    curl -fsSL "https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-${HOST_ARCH}.AppImage" -o "$APPIMAGETOOL"
    chmod +x "$APPIMAGETOOL"
    APPIMAGETOOL_CMD=("$APPIMAGETOOL" --appimage-extract-and-run)
  fi
else
  if ! command -v mksquashfs >/dev/null 2>&1; then
    echo "ERROR: mksquashfs is required for cross-building Type 2 AppImages on ${HOST_OS} (install squashfs-tools / brew install squashfs)." >&2
    exit 1
  fi
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
  if [[ "$USE_APPIMAGETOOL" -eq 1 ]]; then
    echo "==> Packaging $out_name with appimagetool (ARCH=${app_arch})"
    ARCH="$app_arch" APPIMAGE_EXTRACT_AND_RUN=1 \
      "${APPIMAGETOOL_CMD[@]}" \
      --no-appstream \
      --runtime-file "$runtime_file" \
      "$appdir" \
      "$out_path"
  else
    echo "==> Packaging $out_name with type2-runtime + mksquashfs (ARCH=${app_arch})"
    local sqfs="$WORK/appdir-${goarch}.squashfs"
    rm -f "$sqfs"
    mksquashfs "$appdir" "$sqfs" -root-owned -noappend -no-xattrs -comp gzip >/dev/null
    cat "$runtime_file" "$sqfs" > "$out_path"
  fi

  chmod 755 "$out_path"
  ls -lh "$out_path"
}

build_one amd64 x86_64
build_one arm64 aarch64

if [[ "$SMOKE_TEST" -eq 1 ]]; then
  echo "==> Smoke-testing Linux AppImages"

  AMD64_IMG="$OUT_DIR/cloudedits-linux-amd64.AppImage"
  ARM64_IMG="$OUT_DIR/cloudedits-linux-arm64.AppImage"

  AMD64_FILE="$(file "$AMD64_IMG")"
  ARM64_FILE="$(file "$ARM64_IMG")"
  echo "file(amd64): $AMD64_FILE"
  echo "file(arm64): $ARM64_FILE"

  if [[ "$AMD64_FILE" != *"x86-64"* && "$AMD64_FILE" != *"x86_64"* ]]; then
    echo "ERROR: expected x86-64 AppImage runtime for $AMD64_IMG" >&2
    exit 1
  fi
  if [[ "$ARM64_FILE" != *"aarch64"* && "$ARM64_FILE" != *"ARM aarch64"* ]]; then
    echo "ERROR: expected aarch64 AppImage runtime for $ARM64_IMG" >&2
    exit 1
  fi

  if [[ "$HOST_OS" == "Linux" && "$HOST_ARCH" == "x86_64" ]]; then
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

  if command -v unsquashfs >/dev/null 2>&1; then
    for pair in "amd64:x86-64" "arm64:aarch64"; do
      arch="${pair%%:*}"
      want="${pair##*:}"
      img="$OUT_DIR/cloudedits-linux-${arch}.AppImage"
      offset="$(python3 -c '
import struct, sys
data = open(sys.argv[1], "rb").read()
# Verify ELF64 header size matches squashfs offset cleanly.
e_shoff = struct.unpack_from("<Q", data, 40)[0]
e_shentsize = struct.unpack_from("<H", data, 58)[0]
e_shnum = struct.unpack_from("<H", data, 60)[0]
elf_end = e_shoff + e_shentsize * e_shnum
if data[elf_end:elf_end+4] != b"hsqs":
    idx = data.find(b"hsqs")
    if idx < 0:
        sys.exit(1)
    print(idx)
else:
    print(elf_end)
' "$img")"
      extract="$WORK/unsquash-${arch}"
      rm -rf "$extract"
      unsquashfs -o "$offset" -d "$extract" "$img" >/dev/null
      bin_file="$(file "$extract/usr/bin/cloudedits")"
      echo "${arch} extracted binary (offset=${offset}): $bin_file"
      if [[ "$bin_file" != *"$want"* ]]; then
        echo "ERROR: expected ${want} binary inside ${img}: $bin_file" >&2
        exit 1
      fi
      if [[ ! -x "$extract/AppRun" ]]; then
        echo "ERROR: AppRun is not executable inside ${img}" >&2
        exit 1
      fi
      if [[ ! -f "$extract/cloudedits.desktop" || ! -f "$extract/cloudedits.png" ]]; then
        echo "ERROR: missing desktop entry or icon inside ${img}" >&2
        exit 1
      fi
      "$ROOT_DIR/packaging/scripts/check-release-symbols.sh" "$extract/usr/bin/cloudedits"
    done
  fi

  echo "OK: Linux AppImage smoke test passed."
fi
