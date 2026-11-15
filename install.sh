#!/bin/sh
set -eu
umask 077

fail() { printf 'ohya: %s\n' "$*" >&2; exit 1; }
fetch() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
        --connect-timeout 10 --max-time 180 --retry 2 --max-filesize 268435456 "$@"
}

command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v tar >/dev/null 2>&1 || fail 'tar is required'
release_root=${OHYA_RELEASE_ROOT:-https://github.com/nibzard/ohya/releases}
case "$release_root" in https://*) ;; *) fail 'release root must use HTTPS' ;; esac
version=
next_version=0
for arg do
    case "$arg" in --help|-h) printf '%s\n' 'Usage: sh install.sh [--version VERSION] [--bin-dir DIRECTORY] [setup flags]'; exit 0 ;; esac
    if [ "$next_version" = 1 ]; then version=$arg; next_version=0; continue; fi
    case "$arg" in --version) next_version=1 ;; --version=*) fail 'use --version VALUE' ;; esac
done
[ "$next_version" = 0 ] || fail '--version needs a value'
if [ -z "$version" ]; then
    resolved=$(fetch -o /dev/null -w '%{url_effective}' "$release_root/latest") || fail 'cannot resolve the latest release'
    version=${resolved##*/}
fi
case "$version" in v[0-9]*) ;; *) fail 'release version must start with v and a digit' ;; esac
case "$version" in *[!A-Za-z0-9.+-]*) fail 'invalid release version' ;; esac
case "$(uname -s)" in Darwin) platform=darwin ;; Linux) platform=linux ;; *) fail 'supported systems are macOS and Linux' ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) fail 'unsupported processor architecture' ;; esac
case "$platform-$arch" in darwin-arm64|darwin-amd64|linux-amd64) ;; *) fail 'no verified release target for this system' ;; esac
work=$(mktemp -d "${TMPDIR:-/tmp}/ohya-install.XXXXXXXX") || fail 'cannot create temporary directory'
trap 'rm -rf "$work"' EXIT HUP INT TERM
asset="ohya_${version}_${platform}_${arch}.tar.gz"
base="$release_root/download/$version"
fetch -o "$work/$asset" "$base/$asset" || fail 'cannot download release archive'
fetch -o "$work/SHA256SUMS" "$base/SHA256SUMS" || fail 'cannot download checksums'
expected=$(awk -v name="$asset" '$2 == name {print $1}' "$work/SHA256SUMS")
[ "${#expected}" = 64 ] || fail 'missing or ambiguous archive checksum'
case "$expected" in *[!0-9a-f]*) fail 'invalid archive checksum' ;; esac
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$work/$asset")
elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$work/$asset")
else
    fail 'sha256sum or shasum is required'
fi
actual=${actual%% *}
[ "$actual" = "$expected" ] || fail 'archive checksum mismatch'
tar -tzf "$work/$asset" > "$work/entries" || fail 'invalid release archive'
[ "$(LC_ALL=C sort "$work/entries")" = "$(printf 'INSTALL.md\nLICENSE\nohya')" ] || fail 'unexpected release archive entries'
tar -tvzf "$work/$asset" > "$work/types" || fail 'cannot inspect release archive'
awk 'substr($0,1,1) != "-" {exit 1}' "$work/types" || fail 'release archive contains links or special files'
tar -xzf "$work/$asset" -C "$work" || fail 'cannot extract release archive'
[ -f "$work/ohya" ] && [ ! -L "$work/ohya" ] || fail 'archive has no regular executable'
chmod 700 "$work/ohya"
identity=$("$work/ohya" version) || fail 'downloaded executable cannot run'
case " $identity " in *" version=$version "*) ;; *) fail 'downloaded executable has the wrong release identity' ;; esac
"$work/ohya" install --version "$version" "$@"
