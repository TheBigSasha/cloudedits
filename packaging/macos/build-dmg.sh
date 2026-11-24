#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: build-dmg.sh must run on macOS (needs lipo, iconutil, and hdiutil)." >&2
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

# Strip leading v and prerelease suffix for macOS numeric CFBundleVersion.
NUMERIC_VERSION="${VERSION#v}"
NUMERIC_VERSION="${NUMERIC_VERSION%%-*}"
if [[ ! "$NUMERIC_VERSION" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; then
  NUMERIC_VERSION="0.0.0"
fi

WORK="$(mktemp -d)"
MOUNT_POINT=""
cleanup() {
  if [[ -n "$MOUNT_POINT" && -d "$MOUNT_POINT" ]]; then
    hdiutil detach "$MOUNT_POINT" -force >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

cd "$ROOT_DIR"

echo "==> Cross-compiling darwin/arm64 and darwin/amd64 (${VERSION})"
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
  -o "$WORK/cloudedits-arm64" ./cmd/cloudedits

CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
  -o "$WORK/cloudedits-amd64" ./cmd/cloudedits

"$ROOT_DIR/packaging/scripts/check-release-symbols.sh" \
  "$WORK/cloudedits-arm64" \
  "$WORK/cloudedits-amd64"

echo "==> Combining universal binary with lipo"
lipo -create -output "$WORK/cloudedits-universal" \
  "$WORK/cloudedits-arm64" \
  "$WORK/cloudedits-amd64"
lipo -info "$WORK/cloudedits-universal"

echo "==> Generating AppIcon.icns"
ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"
SRC_PNG="$ROOT_DIR/packaging/assets/cloudedits-1024.png"
for sz in 16 32 128 256 512; do
  sips -z "$sz" "$sz" "$SRC_PNG" --out "$ICONSET/icon_${sz}x${sz}.png" >/dev/null
  sz2=$((sz * 2))
  sips -z "$sz2" "$sz2" "$SRC_PNG" --out "$ICONSET/icon_${sz}x${sz}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$WORK/AppIcon.icns"

echo "==> Assembling cloudedits.app bundle"
STAGING="$WORK/dmg-root"
APP="$STAGING/cloudedits.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

cp "$WORK/cloudedits-universal" "$APP/Contents/MacOS/cloudedits"
chmod 755 "$APP/Contents/MacOS/cloudedits"

cp "$ROOT_DIR/packaging/macos/cloudedits-launcher.sh" "$APP/Contents/MacOS/cloudedits-launcher"
chmod 755 "$APP/Contents/MacOS/cloudedits-launcher"

cp "$WORK/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"

sed \
  -e "s|@SHORT_VERSION@|${NUMERIC_VERSION}|g" \
  -e "s|@BUNDLE_VERSION@|${NUMERIC_VERSION}|g" \
  "$ROOT_DIR/packaging/macos/Info.plist" > "$APP/Contents/Info.plist"

if command -v codesign >/dev/null 2>&1; then
  codesign --force --deep --sign - "$APP"
fi

ln -s /Applications "$STAGING/Applications"

cat > "$STAGING/README.txt" <<EOF
cloudedits ${VERSION}

1. Drag cloudedits.app into Applications.
2. Double-click cloudedits.app to launch the browser setup wizard.
3. For headless CLI usage, run:
   /Applications/cloudedits.app/Contents/MacOS/cloudedits --help
EOF

OUT_DMG="$OUT_DIR/cloudedits-macos-universal.dmg"
rm -f "$OUT_DMG"

echo "==> Creating compressed UDZO DMG: $OUT_DMG"
hdiutil create \
  -volname "cloudedits ${VERSION}" \
  -srcfolder "$STAGING" \
  -ov \
  -format UDZO \
  -imagekey zlib-level=9 \
  "$OUT_DMG"

ls -lh "$OUT_DMG"

if [[ "$SMOKE_TEST" -eq 1 ]]; then
  echo "==> Smoke-testing $OUT_DMG"
  MOUNT_POINT="$WORK/mnt"
  mkdir -p "$MOUNT_POINT"
  hdiutil attach -nobrowse -readonly -mountpoint "$MOUNT_POINT" "$OUT_DMG" >/dev/null

  APP_BIN="$MOUNT_POINT/cloudedits.app/Contents/MacOS/cloudedits"
  APP_LAUNCHER="$MOUNT_POINT/cloudedits.app/Contents/MacOS/cloudedits-launcher"

  LIPO_OUT="$(lipo -info "$APP_BIN")"
  echo "lipo: $LIPO_OUT"
  if [[ "$LIPO_OUT" != *"arm64"* || "$LIPO_OUT" != *"x86_64"* ]]; then
    echo "ERROR: expected universal arm64 + x86_64 binary, got: $LIPO_OUT" >&2
    exit 1
  fi

  VER_JSON="$("$APP_BIN" version --json)"
  echo "version --json: $VER_JSON"
  if [[ "$VER_JSON" != *"\"version\": \"${VERSION}\""* && "$VER_JSON" != *"\"version\":\"${VERSION}\""* ]]; then
    echo "ERROR: version mismatch in DMG binary: $VER_JSON" >&2
    exit 1
  fi

  LAUNCHER_JSON="$("$APP_LAUNCHER" version --json)"
  echo "launcher passthrough version --json: $LAUNCHER_JSON"
  if [[ "$LAUNCHER_JSON" != *"\"version\": \"${VERSION}\""* && "$LAUNCHER_JSON" != *"\"version\":\"${VERSION}\""* ]]; then
    echo "ERROR: version mismatch via launcher passthrough: $LAUNCHER_JSON" >&2
    exit 1
  fi

  if [[ ! -L "$MOUNT_POINT/Applications" ]]; then
    echo "ERROR: /Applications symlink missing in DMG" >&2
    exit 1
  fi

  hdiutil detach "$MOUNT_POINT" >/dev/null
  MOUNT_POINT=""
  echo "OK: macOS DMG smoke test passed."
fi
