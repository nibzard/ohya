# ohya Menu Bar (macOS)

Native, zero-dependency SwiftUI `MenuBarExtra` companion for `ohya`.
Designed for macOS 26 (Tahoe) Liquid Glass with graceful fallback to macOS 15 (Sequoia).
See [ADR 0017](../../docs/adr/0017-macos-menubar-app.md) and [Plan 005](../../plans/005-macos-menubar.md).

## Requirements

- macOS 15.0 or later (macOS 26 SDK recommended for Liquid Glass styling)
- Swift 6.0+ / Xcode Command Line Tools
- An installed `ohya` binary (`~/.local/bin/ohya`, `/opt/homebrew/bin/ohya`, or `/usr/local/bin/ohya`)

## Build & test on macOS

```sh
cd macos/OhyaMenuBar
swift test
swift run OhyaMenuBar
```

## Build `Ohya.app` bundle

```sh
cd macos/OhyaMenuBar
./scripts/bundle.sh
open .build/Ohya.app
```

To embed a specific `ohya` binary inside `Ohya.app/Contents/MacOS/ohya`:

```sh
OHYA_BINARY=../../bin/ohya ./scripts/bundle.sh
```

## Boundaries

- Never opens `hub.db`, `registry.db`, or `credential.json` directly; all reads and controls run through `ohya ... --json`.
- `launchd` (`com.nibzard.ohya` / `com.nibzard.ohya.serve`) owns background capture and serving; quitting `Ohya.app` never interrupts ingestion or sync.
- Copied join codes carry `org.nspasteboard.TransientType` and `org.nspasteboard.ConcealedType` and clear automatically after 60 seconds when untouched.
