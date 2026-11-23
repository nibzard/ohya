# ohya Menu Bar (macOS)

Native, zero-dependency SwiftUI `MenuBarExtra` companion for `ohya`.
Designed for macOS 26 (Tahoe) Liquid Glass with graceful fallback to macOS 15 (Sequoia).
See [ADR 0017](../../docs/adr/0017-macos-menubar-app.md) and [Plan 005](../../plans/005-macos-menubar.md).

## Requirements

- macOS 15.0 or later (macOS 26 SDK recommended for Liquid Glass styling)
- Swift 6.0+ / Xcode Command Line Tools
- An `ohya` binary: an installed one (`~/.local/bin/ohya`, `/opt/homebrew/bin/ohya`, or `/usr/local/bin/ohya`), or the copy inside `Ohya.app/Contents/MacOS/ohya`

## Build & test on macOS

```sh
cd macos/OhyaMenuBar
xcrun swift test
xcrun swift run OhyaMenuBar
```

Use `xcrun swift`, not a bare `swift`. A toolchain from swiftly can come first on `PATH`, and it cannot compile against the Xcode SDK (`cannot find 'URL' in scope`, `unknown argument: '-target-arch-variant'`).

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

`bundle.sh` also reads `OHYA_APP_ARCHS` (for example `"arm64 x86_64"`),
`OHYA_APP_VERSION` (stamped into `Info.plist`), and `OHYA_APP_OUT`.

## Binary resolution and first run

The app uses the first executable in this order: the path set in Settings,
`~/.local/bin/ohya`, `/opt/homebrew/bin/ohya`, `/usr/local/bin/ohya`, and
the bundled copy. An installed `ohya` always wins, so the app and the
launchd service run the same binary.

- When `ohya service status --json` reports no installed service, the app
  shows the first-run view. **Use this Mac** runs `ohya setup --json`.
  **Join a hub** passes the join code to `ohya setup --join-code-env` in the
  child environment only. When the resolved binary is the bundled copy, the
  app runs `ohya install` with the same flags, which copies it to
  `~/.local/bin` first.
- When the bundled copy is a newer release than the installed one, the
  Status tab offers **Update**, which runs the bundled `ohya install`.

## Release disk image

`scripts/package-app.sh` builds the release DMG from the two native
`ohya` release binaries:

```sh
./scripts/package-app.sh --version vX.Y.Z \
    --ohya-arm64 PATH/TO/arm64/ohya --ohya-amd64 PATH/TO/amd64/ohya \
    --out ../../dist/app
```

It builds a universal app, merges the two binaries into one universal
`ohya`, stamps the version, signs the app, and writes
`Ohya_vX.Y.Z_macos.dmg`, `SHA256SUMS.macos-app`, and
`candidate.macos-app.txt`. Without signing options it signs ad-hoc. With
`--sign-identity` (a Developer ID Application certificate) and
`--notary-profile` (a `notarytool` keychain profile), it signs with the
hardened runtime, notarizes and staples the DMG, and requires an Accepted
result. The **Prepare release candidates** workflow runs it on every
candidate; see [release operations](../../docs/release-operations.md).

`scripts/make-icon.sh` regenerates `Resources/AppIcon.icns` from
`docs/assets/brand/icon.svg`. It needs `rsvg-convert` and ImageMagick.

## Boundaries

- Never opens `hub.db`, `registry.db`, or `credential.json` directly; all reads and controls run through `ohya ... --json`.
- `launchd` (`com.nibzard.ohya` / `com.nibzard.ohya.serve`) owns background capture and serving; quitting `Ohya.app` never interrupts ingestion or sync.
- Copied join codes carry `org.nspasteboard.TransientType` and `org.nspasteboard.ConcealedType` and clear automatically after 60 seconds when untouched.
