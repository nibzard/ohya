#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/../.." && pwd)"
SOURCE="$REPO_ROOT/docs/assets/brand/icon.svg"
OUT="$ROOT/Resources/AppIcon.icns"

for tool in rsvg-convert magick iconutil sips; do
    command -v "$tool" >/dev/null || { echo "make-icon.sh needs $tool" >&2; exit 1; }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
rsvg-convert -w 824 -h 824 "$SOURCE" -o "$work/body.png"
magick -size 1024x1024 xc:none "$work/body.png" -gravity center -composite "$work/icon-1024.png"

iconset="$work/AppIcon.iconset"
mkdir "$iconset"
for size in 16 32 128 256 512; do
    sips -z "$size" "$size" "$work/icon-1024.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
    double=$((size * 2))
    sips -z "$double" "$double" "$work/icon-1024.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
mkdir -p "$(dirname "$OUT")"
iconutil -c icns "$iconset" -o "$OUT"
echo "Built $OUT"
