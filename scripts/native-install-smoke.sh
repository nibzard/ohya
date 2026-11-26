#!/bin/sh
set -eu
[ "$#" = 1 ] || { printf 'usage: native-install-smoke.sh ARCHIVE\n' >&2; exit 2; }
archive=$1
work=$(mktemp -d "${TMPDIR:-/tmp}/ohya-native.XXXXXXXX")
mkdir "$work/package" "$work/sessions"
tar -xzf "$archive" -C "$work/package"
ohya="$work/package/ohya"
state="$work/state"
cleanup() {
    result=$?
    trap - EXIT HUP INT TERM
    if "$ohya" uninstall --state "$state" --json; then
        rm -rf "$work"
    else
        printf 'Cleanup incomplete; retained synthetic state at %s\n' "$work" >&2
        exit 1
    fi
    exit "$result"
}
trap cleanup EXIT HUP INT TERM
"$ohya" setup --no-agents --no-modify-path --state "$state" --claude-root "$work/sessions" --codex-root '' --pi-root '' --antigravity-root '' --json
cp testdata/claude-code/main-session.jsonl "$work/sessions/session.jsonl"
python3 - "$state/hub.db" "$work/sessions/session.jsonl" <<'PY'
import pathlib
import sqlite3
import sys
import time

database = pathlib.Path(sys.argv[1])
expected = pathlib.Path(sys.argv[2]).stat().st_size
deadline = time.monotonic() + 90
while time.monotonic() < deadline:
    try:
        with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=1) as connection:
            count = connection.execute("SELECT count(*) FROM events").fetchone()[0]
            cursor = connection.execute("SELECT max(cursor_bytes) FROM sources").fetchone()[0]
            if count == 16 and cursor == expected:
                print("Native user service imported 16 events and acknowledged the complete source")
                break
    except sqlite3.Error:
        pass
    time.sleep(0.2)
else:
    raise SystemExit("Native user service did not import the synthetic source")
PY
"$ohya" doctor --state "$state" --db "$state/hub.db" --json
