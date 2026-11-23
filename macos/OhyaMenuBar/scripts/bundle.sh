#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/../.." && pwd)"
TEMPLATE_SRC="$REPO_ROOT/docs/assets/brand/menubar-template.svg"
TEMPLATE_DST="$ROOT/Sources/OhyaMenuBar/Resources/menubar-template.svg"
OUT_APP="${OHYA_APP_OUT:-$ROOT/.build/Ohya.app}"

cp "$TEMPLATE_SRC" "$TEMPLATE_DST"

cd "$ROOT"
BUILD_ARGS=(-c release)
for arch in ${OHYA_APP_ARCHS:-}; do
    BUILD_ARGS+=(--arch "$arch")
done
xcrun swift build "${BUILD_ARGS[@]}"

BIN_DIR="$(xcrun swift build "${BUILD_ARGS[@]}" --show-bin-path)"
rm -rf "$OUT_APP"
mkdir -p "$OUT_APP/Contents/MacOS" "$OUT_APP/Contents/Resources"

cp "$BIN_DIR/OhyaMenuBar" "$OUT_APP/Contents/MacOS/OhyaMenuBar"
cp "$ROOT/Info.plist" "$OUT_APP/Contents/Info.plist"
cp "$TEMPLATE_DST" "$OUT_APP/Contents/Resources/menubar-template.svg"
cp "$ROOT/Resources/AppIcon.icns" "$OUT_APP/Contents/Resources/AppIcon.icns"

if [ -d "$BIN_DIR/OhyaMenuBar_OhyaMenuBar.bundle" ]; then
    cp -R "$BIN_DIR/OhyaMenuBar_OhyaMenuBar.bundle" "$OUT_APP/Contents/Resources/"
fi

if [ -n "${OHYA_APP_VERSION:-}" ]; then
    short="${OHYA_APP_VERSION#v}"
    short="${short%%[-+]*}"
    plutil -replace CFBundleShortVersionString -string "$short" "$OUT_APP/Contents/Info.plist"
    plutil -replace CFBundleVersion -string "$short" "$OUT_APP/Contents/Info.plist"
fi

if [ -n "${OHYA_BINARY:-}" ]; then
    cp "$OHYA_BINARY" "$OUT_APP/Contents/MacOS/ohya"
    chmod 755 "$OUT_APP/Contents/MacOS/ohya"
fi

echo "Built $OUT_APP"
