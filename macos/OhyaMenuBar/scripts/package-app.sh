#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
    echo "usage: package-app.sh --version vX.Y.Z --ohya-arm64 PATH --ohya-amd64 PATH --out DIR [--sign-identity ID --notary-profile PROFILE]" >&2
    exit 2
}

version= ohya_arm64= ohya_amd64= out= signing_identity= notary_profile=
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || usage
    case "$1" in
        --version) version=$2 ;;
        --ohya-arm64) ohya_arm64=$2 ;;
        --ohya-amd64) ohya_amd64=$2 ;;
        --out) out=$2 ;;
        --sign-identity) signing_identity=$2 ;;
        --notary-profile) notary_profile=$2 ;;
        *) usage ;;
    esac
    shift 2
done
[ -n "$version" ] && [ -n "$ohya_arm64" ] && [ -n "$ohya_amd64" ] && [ -n "$out" ] || usage
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+-]+)?$' || { echo "use --version vMAJOR.MINOR.PATCH" >&2; exit 2; }
if [ -n "$signing_identity$notary_profile" ] && { [ -z "$signing_identity" ] || [ -z "$notary_profile" ]; }; then
    echo "Developer ID signing needs both --sign-identity and --notary-profile" >&2
    exit 2
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/../.." && pwd)"
revision="$(git -C "$REPO_ROOT" rev-parse HEAD)"
dirty=false
[ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] || dirty=true

check_slice() {
    local binary=$1 arch=$2
    [ -x "$binary" ] || { echo "$binary is not an executable" >&2; exit 1; }
    [ "$(lipo -archs "$binary")" = "$arch" ] || { echo "$binary must hold only $arch" >&2; exit 1; }
    case " $("$binary" version 2>/dev/null || true) " in
        *" version=$version "*) ;;
        *) [ "$arch" != "$(uname -m)" ] || { echo "$binary does not report version=$version" >&2; exit 1; } ;;
    esac
}
check_slice "$ohya_arm64" arm64
check_slice "$ohya_amd64" x86_64

mkdir -p "$out"
dmg="$out/Ohya_${version}_macos.dmg"
[ ! -e "$dmg" ] || { echo "$dmg already exists; use a new output directory" >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/ohya-app.XXXXXXXX")"
trap 'rm -rf "$work"' EXIT HUP INT TERM
lipo -create "$ohya_arm64" "$ohya_amd64" -output "$work/ohya"
identity="$("$work/ohya" version)"
case " $identity " in *" version=$version "*) ;; *) echo "universal ohya does not report version=$version" >&2; exit 1 ;; esac

app="$work/stage/Ohya.app"
mkdir -p "$work/stage"
OHYA_APP_ARCHS="arm64 x86_64" OHYA_APP_VERSION="$version" OHYA_BINARY="$work/ohya" OHYA_APP_OUT="$app" "$ROOT/scripts/bundle.sh"
[ "$(lipo -archs "$app/Contents/MacOS/OhyaMenuBar")" = "x86_64 arm64" ] || { echo "the app executable is not universal" >&2; exit 1; }

if [ -n "$signing_identity" ]; then
    sign=(codesign --force --options runtime --timestamp --sign "$signing_identity")
    signing=developer-id-notarized
else
    sign=(codesign --force --sign -)
    signing=adhoc
fi
"${sign[@]}" "$app/Contents/MacOS/ohya"
"${sign[@]}" "$app"
codesign --verify --strict --deep --verbose=2 "$app"

ln -s /Applications "$work/stage/Applications"
hdiutil create -quiet -volname ohya -srcfolder "$work/stage" -format UDZO -ov "$work/ohya.dmg"

if [ -n "$signing_identity" ]; then
    codesign --force --timestamp --sign "$signing_identity" "$work/ohya.dmg"
    xcrun notarytool submit "$work/ohya.dmg" --keychain-profile "$notary_profile" --wait --output-format json > "$out/notarization.macos-app.json"
    python3 - "$out/notarization.macos-app.json" <<'VERIFY_NOTARY'
import json
import sys
with open(sys.argv[1]) as result_file:
    result = json.load(result_file)
if result.get("status") != "Accepted":
    raise SystemExit("Apple did not accept the notarization; review the submission log")
VERIFY_NOTARY
    xcrun stapler staple "$work/ohya.dmg"
    xcrun stapler validate "$work/ohya.dmg"
fi

mv "$work/ohya.dmg" "$dmg"
checksum="$(shasum -a 256 "$dmg")"
printf '%s  %s\n' "${checksum%% *}" "$(basename "$dmg")" > "$out/SHA256SUMS.macos-app"
printf 'version=%s\nrevision=%s\ndirty=%s\nplatform=darwin\narchitecture=universal\nsigning=%s\nqualification=candidate-only\nbinary_identity=%s\n' \
    "$version" "$revision" "$dirty" "$signing" "$identity" > "$out/candidate.macos-app.txt"
echo "Prepared app candidate: $dmg ($signing)"
echo "No release was published."
