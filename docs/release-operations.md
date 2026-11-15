# Release and recovery operations

The installer is implemented, but no release assets are published yet. Local
Linux candidate tests pass. The macOS matrix, Apple signing, and Homebrew tests
must pass before a public release. Generated candidates are not qualified
releases merely because packaging succeeds.

## Prepare a candidate

1. Select the release version and exact source commit. Start with a clean tree.
   Run `make check` and `make tla`. Do not create a public tag during preparation.
2. Run `scripts/package.sh --version vMAJOR.MINOR.PATCH --out dist/candidate`
   on each target architecture. The script refuses cross-compilation and
   existing output archives. `--allow-dirty` is only for local experiments;
   dirty candidates cannot become release artifacts.
3. Run the **Prepare release candidates** workflow for that source and version.
   It resolves one commit for all jobs. Its proposed baseline matrix is Ubuntu
   22.04 amd64 and macOS 15 arm64/amd64. These are candidate test baselines,
   not support claims until the corresponding native jobs pass.
4. Verify the HTTPS bootstrap and real user service on each native target.
   `scripts/native-install-smoke.sh ARCHIVE` creates synthetic input, starts
   a user service, waits for 16 events and the complete Source cursor, runs
   doctor, and removes owned integrations. It changes no agent configuration
   or shell profile. Run it only on a test account with no existing ohya service.
5. Inspect the native library report. Confirm that the executable runs without
   a compiler or external SQLite installation on the minimum target system.
   Review every `candidate.PLATFORM-ARCH.txt` file for matching source identity
   and `dirty=false`. Keep the exact tested archive bytes.
6. Verify all archive checksums and prepare the draft formula with
   `python3 scripts/prepare-formula.py --version VERSION --artifacts DIRECTORY
   --output ohya.rb`. It needs all three actual archives and their checksum
   manifests. It rejects invalid layouts, checksum mismatches, and an existing
   output file. Never invent a checksum for an unavailable target.
7. Review the resulting artifacts and native evidence. Sign and notarize macOS
   artifacts as described below, rerun the download tests, and regenerate
   checksums and the formula from final bytes. Publication requires a separate
   explicit release action. The preparation workflow has read-only repository
   permissions and creates no tags or GitHub releases.

Local bootstrap verification uses:

```sh
OHYA_TEST_RELEASE_DIR=/absolute/candidate/directory \
  go test -tags sqlite_fts5 ./internal/install -run TestBootstrapRealReleaseArchive -count=1
```

This checks the real packaged executable over a local HTTPS server. It installs
into a private temporary directory with services, agent configuration, and PATH
changes disabled. The separate native smoke checks the real service manager.

## Sign macOS artifacts

Use a Developer ID Application certificate and a private keychain profile
prepared outside the installer. On a native Mac, pass `--sign-identity` and
`--notary-profile` to `scripts/package.sh`. The script signs the executable
with hardened runtime and a secure timestamp, verifies the code signature,
submits a ZIP payload with `notarytool`, and requires an Accepted result before
creating the final tar archive and checksums. It does not receive raw account
passwords. Review Apple's submission log, including warnings.

Standalone executables cannot have notarization tickets stapled to them;
Gatekeeper retrieves their tickets online. Test the actual download and first
launch on a clean Mac. Do not strip quarantine attributes or disable Gatekeeper
as a workaround. Both supported architectures require native evidence.
Unsigned candidates remain marked `signing=unverified`.

Sources checked 2026-11-15:
[Apple notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow)
and [Apple signing requirements](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

## Homebrew ownership

The formula installs only the executable. Run `ohya setup` after installing or
upgrading it. The setup engine preserves a stable Homebrew link in service and
agent entries. It never replaces package manager files through the standalone
installer. A future one-command Homebrew entry point can combine package
installation and setup, once the tap and release URLs exist.

Before publication, test formula installation, `brew test`, upgrade, and removal
on native Macs. `ohya uninstall` disconnects integrations but keeps the Homebrew
executable; `brew uninstall` owns its removal. The formula generator and local
checksum tests do not replace native Homebrew execution.

## Backup and restore

Setup automatically saves a consistent SQLite recovery point before reopening
an existing local database. It contains committed WAL pages, and remains private
and separate from agent transcript directories. This is a database recovery
point, not a coordinated backup of every device and its checkpoints.

For a full writable restore, stop ingestion, serving, uploading, and MCP writer
processes before backing up the complete ohya state directory. Include custom
database locations, matching checkpoint files, device identities, registry,
coordination state, and credentials. Keep that archive private. Agent source
logs remain on their original devices and must remain available for replay.
Never copy only a live SQLite database file and omit its WAL.

Restore a stopped backup as a matching set with a compatible binary. Do not
combine a rolled-back database with newer acknowledged checkpoints. A hub
restored behind a device's acknowledged cursor needs explicit replay recovery;
the uploader reports regression instead of silently discarding events. Do not
delete identities or checkpoints to suppress that diagnostic.

Use automatic database-only recovery points for evidence recovery in an
isolated directory unless you also have matching state and a verified restore
procedure. Keep the failed database and its WAL files together for diagnosis.
A binary downgrade cannot undo a schema migration or newer parser output.

## Current verification limits

- Linux amd64 packaging, HTTPS bootstrap, real user-service ingestion, doctor,
  and service removal passed on the current Linux host.
- Pi 0.84.2 automatically discovered the compatibility extension and completed
  SQL and Unicode annotation round trips without model access.
- The hosted native matrix has not run for these unpublished commits.
- Apple signing and notarization have not run; credentials and a native Mac
  are required. Homebrew native execution is also pending.
- No GitHub release, tag, Homebrew tap, or installer URL has been published.
