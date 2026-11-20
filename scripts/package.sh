#!/bin/sh
set -eu
umask 077
version=
out=dist
allow_dirty=0
signing_identity=
notary_profile=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--out|--sign-identity|--notary-profile)
            key=$1; shift
            [ "$#" -gt 0 ] || { printf 'missing value for %s\n' "$key" >&2; exit 2; }
            case "$key" in --version) version=$1 ;; --out) out=$1 ;; --sign-identity) signing_identity=$1 ;; --notary-profile) notary_profile=$1 ;; esac ;;
        --allow-dirty) allow_dirty=1 ;;
        *) printf 'unknown packaging argument: %s\n' "$1" >&2; exit 2 ;;
    esac
    shift
done
printf '%s\n' "$version" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+-]+)?$/ {ok=1} END {exit !ok}' || { printf 'use --version vMAJOR.MINOR.PATCH\n' >&2; exit 2; }
[ -f go.mod ] && [ -f install.sh ] || { printf 'run from the repository root\n' >&2; exit 1; }
revision=$(git rev-parse HEAD)
dirty=false
if [ -n "$(git status --porcelain)" ]; then
    dirty=true
    [ "$allow_dirty" = 1 ] || { printf 'release candidate needs a clean working tree\n' >&2; exit 1; }
fi
case "$(uname -s)" in Darwin) platform=darwin ;; Linux) platform=linux ;; *) exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) exit 1 ;; esac
case "$platform-$arch" in darwin-arm64|darwin-amd64|linux-amd64) ;; *) printf 'unsupported native release target\n' >&2; exit 1 ;; esac
[ "$(go env GOOS)" = "$platform" ] && [ "$(go env GOARCH)" = "$arch" ] || { printf 'cross-compilation does not satisfy native packaging\n' >&2; exit 1; }
if [ -n "$signing_identity$notary_profile" ]; then
    [ "$platform" = darwin ] && [ -n "$signing_identity" ] && [ -n "$notary_profile" ] || { printf 'signing requires macOS, --sign-identity, and --notary-profile\n' >&2; exit 1; }
fi
mkdir -p "$out"
asset="ohya_${version}_${platform}_${arch}.tar.gz"
[ ! -e "$out/candidate.$platform-$arch.txt" ] || { printf 'candidate metadata already exists; use a new output directory\n' >&2; exit 1; }
[ ! -e "$out/$asset" ] && [ ! -L "$out/$asset" ] || { printf 'archive already exists; use a new output directory\n' >&2; exit 1; }
work=$(mktemp -d "${TMPDIR:-/tmp}/ohya-package.XXXXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -buildvcs=true -ldflags "-X github.com/nibzard/ohya/internal/buildinfo.ReleaseVersion=$version" -o "$work/ohya" ./cmd/ohya
identity=$("$work/ohya" version)
case " $identity " in *" version=$version "*) ;; *) printf 'binary release identity mismatch\n' >&2; exit 1 ;; esac
short_revision=$(printf '%s' "$revision" | cut -c 1-7)
case " $identity " in *" revision=$short_revision "*) ;; *) printf 'binary source revision mismatch\n' >&2; exit 1 ;; esac
case " $identity " in *" dirty=$dirty "*) ;; *) printf 'binary source cleanliness mismatch\n' >&2; exit 1 ;; esac
cp LICENSE "$work/LICENSE"
cp docs/INSTALL.md "$work/INSTALL.md"
chmod 755 "$work/ohya"
chmod 644 "$work/LICENSE" "$work/INSTALL.md"
signing=unverified
if [ -n "$signing_identity" ]; then
    codesign --force --options runtime --timestamp --sign "$signing_identity" "$work/ohya"
    codesign --verify --strict --verbose=2 "$work/ohya"
    ditto -c -k --keepParent "$work/ohya" "$work/notary.zip"
    xcrun notarytool submit "$work/notary.zip" --keychain-profile "$notary_profile" --wait --output-format json > "$out/notarization.$platform-$arch.json"
    python3 - "$out/notarization.$platform-$arch.json" <<'VERIFY_NOTARY'
import json
import sys
with open(sys.argv[1]) as result_file:
    result = json.load(result_file)
if result.get("status") != "Accepted":
    raise SystemExit("Apple did not accept the notarization; review the submission log")
VERIFY_NOTARY
    signing=developer-id-notarized
fi
tar -czf "$work/$asset" -C "$work" ohya LICENSE INSTALL.md
if command -v sha256sum >/dev/null 2>&1; then checksum=$(sha256sum "$work/$asset"); else checksum=$(shasum -a 256 "$work/$asset"); fi
checksum=${checksum%% *}
mv "$work/$asset" "$out/$asset"
printf '%s  %s\n' "$checksum" "$asset" > "$out/SHA256SUMS.$platform-$arch"
sed "s/^version=$/version=$version/" install.sh > "$out/install.sh"
chmod 755 "$out/install.sh"
printf 'version=%s\nrevision=%s\ndirty=%s\nplatform=%s\narchitecture=%s\nsigning=%s\nqualification=candidate-only\nbinary_identity=%s\n' "$version" "$revision" "$dirty" "$platform" "$arch" "$signing" "$identity" > "$out/candidate.$platform-$arch.txt"
printf 'Prepared candidate: %s/%s\nNo release was published.\n' "$out" "$asset"
