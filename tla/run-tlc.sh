#!/bin/sh
set -eu

cd "$(dirname "$0")"

TLA_VERSION=1.8.0
TLA_SHA256=7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f
JAR="vendor/tla2tools-${TLA_VERSION}.jar"

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

if [ ! -f "$JAR" ]; then
    echo "Vendored jar missing: $JAR" >&2
    echo "Restore it with: git checkout -- tla/vendor/tla2tools-${TLA_VERSION}.jar" >&2
    exit 1
fi

GOT=$(sha256 "$JAR")
if [ "$GOT" != "$TLA_SHA256" ]; then
    echo "Checksum mismatch for $JAR: $GOT" >&2
    echo "Expected: $TLA_SHA256" >&2
    echo "Restore it with: git checkout -- tla/vendor/tla2tools-${TLA_VERSION}.jar" >&2
    exit 1
fi

check() {
    module=$1
    shift
    for cfg in "$@"; do
        echo ""
        echo "=== $cfg ==="
        java -XX:+UseParallelGC -XX:MaxRAMPercentage=75 \
            -cp "$JAR" tlc2.TLC -workers auto -deadlock -config "${cfg}.cfg" "$module.tla"
    done
}

check Sync SyncConcurrent SyncFailures SyncLiveness
check Auth Auth AuthWide

echo ""
echo "All models checked: no errors."
