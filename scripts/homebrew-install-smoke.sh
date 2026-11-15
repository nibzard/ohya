#!/bin/sh
set -eu
[ "$#" = 1 ] || { printf 'usage: %s ARTIFACT_DIRECTORY\n' "$0" >&2; exit 2; }
artifacts=$(cd "$1" && pwd)
[ -f "$artifacts/ohya.rb" ] || { printf 'missing generated formula\n' >&2; exit 1; }
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1
tap=ohya/installer-check
formula=$tap/ohya
if brew list --formula ohya >/dev/null 2>&1; then
    printf 'an ohya formula is already installed; use a clean test host\n' >&2
    exit 1
fi
if brew tap | awk -v expected="$tap" '$0 == expected {found=1} END {exit !found}'; then
    printf 'the temporary test tap already exists\n' >&2
    exit 1
fi
brew tap-new --no-git "$tap"
tap_dir=$(brew --repository "$tap")
cleanup() {
    result=$?
    trap - EXIT HUP INT TERM
    if brew list --formula "$formula" >/dev/null 2>&1; then
        if ! brew uninstall --formula "$formula"; then
            printf 'keep the test tap for removal recovery: %s\n' "$tap_dir" >&2
            exit 1
        fi
    fi
    if ! brew untap "$tap"; then exit 1; fi
    exit "$result"
}
trap cleanup EXIT HUP INT TERM
python3 - "$artifacts" "$tap_dir/Formula/ohya.rb" <<'LOCAL_FORMULA'
from pathlib import Path
import re
import sys
artifacts = Path(sys.argv[1])
source = (artifacts / "ohya.rb").read_text()
source, count = re.subn(r'https://github.com/nibzard/ohya/releases/download/[^/]+/', artifacts.as_uri() + '/', source)
if count != 3:
    raise SystemExit("unexpected generated formula URLs")
with Path(sys.argv[2]).open("x") as output:
    output.write(source)
LOCAL_FORMULA
brew install --formula "$formula"
brew test "$formula"
brew_bin=$(brew --prefix)/bin/ohya
"$brew_bin" setup --dry-run --no-agents --no-service --no-modify-path
printf 'Homebrew archive installation and dry-run setup passed.\n'
