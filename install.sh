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
join=0
join_env=
join_env_given=0
next_join_env=0
for arg do
    case "$arg" in --help|-h) printf '%s\n' 'Usage: sh install.sh [--version VERSION] [--bin-dir DIRECTORY] [--join] [--join-code-env NAME] [setup flags]'; exit 0 ;; esac
    if [ "$next_version" = 1 ]; then version=$arg; next_version=0; continue; fi
    if [ "$next_join_env" = 1 ]; then join_env=$arg; join_env_given=1; next_join_env=0; continue; fi
    case "$arg" in
        --version) next_version=1 ;;
        --version=*) fail 'use --version VALUE' ;;
        --join-code-env) next_join_env=1 ;;
        --join-code-env=*) fail 'use --join-code-env NAME' ;;
        --join) join=1 ;;
    esac
done
[ "$next_version" = 0 ] || fail '--version needs a value'
[ "$next_join_env" = 0 ] || fail '--join-code-env needs a value'

read_join_code() {
    command -v stty >/dev/null 2>&1 || fail 'stty is required to read the join code without echo; pass --join-code-env NAME'
    (exec 3</dev/tty) 2>/dev/null || fail 'install.sh --join found no terminal for the join code; pass --join-code-env NAME with the code in that variable'
    printf 'ohya join code: ' >/dev/tty
    stty -echo </dev/tty 2>/dev/null || fail 'cannot turn off terminal echo; pass --join-code-env NAME'
    trap 'stty echo </dev/tty 2>/dev/null || true; trap - INT TERM HUP; exit 130' INT TERM HUP
    IFS= read -r join_code </dev/tty || join_code=
    stty echo </dev/tty 2>/dev/null || true
    trap - INT TERM HUP
    [ -n "$join_code" ] || fail 'the join code read from the terminal is empty; pass --join-code-env NAME'
}

join_code=
if [ "$join" = 1 ] && [ -z "$join_env" ]; then
    wants_json=0
    for arg do
        case "$arg" in --json) wants_json=1 ;; esac
    done
    if [ "$wants_json" = 1 ]; then
        fail 'install.sh --json prompts for nothing; pass --join-code-env NAME with the code in that variable'
    fi
    read_join_code
    join_env=OHYA_JOIN_CODE
fi

if [ -z "$version" ]; then
    resolved=$(fetch -o /dev/null -w '%{url_effective}' "$release_root/latest") || fail 'cannot resolve the latest release'
    [ "$resolved" != "$release_root" ] || fail "no stable release: $release_root/latest redirected to $release_root; list the releases there and pass --version VERSION"
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
kept=$#
while [ "$kept" -gt 0 ]; do
    case "$1" in
        --join) ;;
        *) set -- "$@" "$1" ;;
    esac
    shift
    kept=$((kept - 1))
done
if [ "$join" = 1 ] && [ "$join_env_given" = 0 ]; then
    set -- "$@" --join-code-env "$join_env"
fi
if [ "$join" = 1 ] && [ -n "$join_code" ]; then
    export "$join_env=$join_code"
fi
"$work/ohya" install --version "$version" "$@"
