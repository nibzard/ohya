#!/bin/sh
# Run TLC on every sync model. Downloads the pinned TLA+ tools jar on
# first use and verifies its checksum. Requires Java 11 or later.
#
# Usage: make tla   (or ./tla/run-tlc.sh from the repository root)
set -eu

cd "$(dirname "$0")"

TLA_VERSION=1.8.0
TLA_SHA256=e1f11b75c4b5bc4478ba0ac5cb6ae28e40a01b0016bc33e0aa97da22dfdbcd7f
TLA_URL="https://github.com/tlaplus/tlaplus/releases/download/v${TLA_VERSION}/tla2tools.jar"

JAR=".tools/tla2tools-${TLA_VERSION}.jar"

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

if [ ! -f "$JAR" ]; then
    mkdir -p .tools
    echo "Downloading tla2tools ${TLA_VERSION} (pinned, checksum-verified)..."
    curl -fsSL -o "$JAR.tmp" "$TLA_URL"
    GOT=$(sha256 "$JAR.tmp")
    if [ "$GOT" != "$TLA_SHA256" ]; then
        echo "Checksum mismatch for $JAR.tmp: $GOT" >&2
        echo "Expected: $TLA_SHA256" >&2
        rm -f "$JAR.tmp"
        exit 1
    fi
    mv "$JAR.tmp" "$JAR"
fi

# -deadlock: the drained system legitimately reaches a quiescent fixed
# point where no action changes state; that is not an error. See
# tla/README.md.
for cfg in SyncConcurrent SyncFailures SyncLiveness; do
    echo ""
    echo "=== $cfg ==="
    java -XX:+UseParallelGC -XX:MaxRAMPercentage=75 \
        -cp "$JAR" tlc2.TLC -deadlock -config "${cfg}.cfg" Sync.tla
done

echo ""
echo "All sync models checked: no errors."
