# Release and recovery operations

The newest release is v0.6.1, published on 2026-11-23 from commit `bf50937`
after the full candidate matrix passed; its macOS archives are unsigned. Apple signing and
notarization must pass before a signed public release. Generated candidates
are not qualified releases merely because packaging succeeds.

## Prepare a candidate

1. Select the release version and exact source commit. Start with a clean tree.
   Run `make check` and `make tla`. Do not create a public tag during preparation.
   The release change adds the schema the release ships (`store.SchemaVersion()`) to
   `releasedSchemas` in `internal/store/migrate_released_test.go` and to the released-schema
   list in DB.md's upgrade policy item 5, so the released-schema upgrade tests cover the new
   release before the tag is published (see DB.md, "Migrations").
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

The menu bar app follows the same rule. `macos/OhyaMenuBar/scripts/package-app.sh`
takes the same `--sign-identity` and `--notary-profile`, signs the bundled
universal `ohya` and then `Ohya.app` with hardened runtime, signs the DMG,
submits it with `notarytool`, requires an Accepted result, and staples the
ticket to the DMG. Run it on a Mac with the two signed native `ohya`
executables, so the bundled copy has the same bytes as the signed archives.
Without signing options it signs ad-hoc and records `signing=adhoc`.

Standalone executables cannot have notarization tickets stapled to them;
Gatekeeper retrieves their tickets online. Test the actual download and first
launch on a clean Mac. Do not strip quarantine attributes or disable Gatekeeper
as a workaround. Both supported architectures require native evidence.
Unsigned candidates remain marked `signing=unverified`.

Sources checked 2026-11-15:
[Apple notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow)
and [Apple signing requirements](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

## macOS menu bar app

The **Prepare release candidates** workflow runs a `macos-app` job after the
native jobs. It takes the darwin arm64 and amd64 candidate executables,
runs `package-app.sh`, and verifies the DMG: the code signature, universal
app and `ohya` executables, the bundled `ohya` release identity, the
`Info.plist` version, and a macOS 15 minimum. The assemble job adds
`SHA256SUMS.macos-app` to `SHA256SUMS` and prepares an unpublished cask with
`python3 scripts/prepare-cask.py --version VERSION --artifacts DIRECTORY
--output ohya-cask.rb`.

Before publication, open the DMG on a clean Mac, drag the app to
Applications, and check first-run **Use this Mac** and **Join a hub** with a
test hub. A join pairs a real device, so never use a production hub for this
test. An ad-hoc signed app needs **Open Anyway** in Privacy & Security on its
first launch; state that in the release notes until the app is notarized.

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

Setup automatically saves a consistent SQLite recovery point before a build
with a different schema version reopens an existing local database, and keeps
the two newest points. It contains committed WAL pages, and remains private
and separate from agent transcript directories. This is a database recovery
point, not a coordinated backup of every device and its checkpoints.

For a full writable restore, stop ingestion, serving, uploading, and MCP writer
processes before backing up the complete ohya state directory. Include custom
database locations, matching checkpoint files, device identities, registry,
the hub's saved transport identity, and credentials. Keep that archive
private. Agent source
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

## Recovery point cost

Before a recovery copy, setup checks free space on the filesystem holding
`<state>/setup/backups` and refuses without writing anything when free space is
below the database size plus its WAL, a 10 percent margin, and 64 MiB. The
failure names the free and required bytes and the remedy: remove old recovery
points under that directory (each kept point holds one full copy), free space,
or run setup with `--state` on a disk with more room.

Budget the copy itself from the database size, because the copy writes about
one database in pages:

- Fast SSD: the stepped copy loop moves at most about 100 MiB/s, so expect
  about 10 s per GB at the ceiling.
- Busy emulated or spinning disks: minutes per GB.
- Measured worst case: a 13.1 GB recovery point on a saturated single-disk VM
  took 8.5 h, about 39 min per GB, and starved every other process on the
  machine (2026-11-16, NOTES.md). Schedule migrations on such disks for idle
  time; while the copy runs, setup holds the setup lock and owned ingestion is
  stopped.

Setup reports the estimated copy size before starting and a progress line on
stderr at every further 10 percent of the copy. A copy whose average rate
stays below 256 KiB/s is aborted; the deadline scales with the database size,
so the measured worst case above still completes. An interrupted or refused
copy leaves no partial recovery point behind: the copy is staged under a
hidden directory and renamed into place only after it succeeds, and staging
left by a killed run is removed on the next run.

## Purge and recovery points

Migration 008 (`purged_sources`, [ADR 0013](adr/0013-owner-scoped-session-purge.md))
is schema-only, but the next `ohya setup` run after the upgrade takes one
more full recovery copy, so the cost above applies once per hub. Beyond that:

- `ohya purge` frees pages but does not shrink the database file; the freed
  pages are reused by later writes. Shrinking needs an operator-run VACUUM,
  which is deliberately not part of the command.
- A recovery copy taken before a purge still contains the purged data, and
  restoring it undoes the purge completely — the deleted rows return and the
  tombstones vanish, so devices keep syncing normally afterwards. Treat every
  pre-purge recovery point as still holding everything the purge removed, and
  prune recovery points after a deliberate purge if the removed data must not
  survive in copies.

## Current verification limits

- v0.1.0 (commit `8fb3d2e`, dirty=false) was published on 2026-11-15 for
  darwin-arm64, darwin-amd64, and linux-amd64 after the hosted matrix passed:
  packaging, HTTPS bootstrap, user-service ingestion smoke, doctor,
  native-dependency inspection, and Homebrew install, test, and removal on
  both macOS architectures.
- v0.1.1 (commit `dab456e`) followed the same day: it fixes the launchctl
  readiness parse for logged-in GUI Macs and was verified end to end on
  macOS 27.2.1 arm64 through the released installer.
- v0.1.2 (commit `6ed9fc3`, dirty=false) was published on 2026-11-16 after
  the full hosted matrix passed (run 43844322695; the darwin-arm64 build ran
  on attempt 2 because no hosted runner picked up attempt 1). It adds schema 7
  (workspaces) and cuts idle ingest checkpoint writes. The released installer
  upgraded the linux-amd64 hub host end to end; `ohya doctor` exited 0.
- v0.2.0 (commit `3e6adba`, dirty=false) was published on 2026-11-17 after
  the full hosted matrix passed (run 44087678215). The first candidate
  (run 44085094843, commit `49170ee`) failed all three native builds:
  perf/recallduration used a linux-only syscall, and three tests assumed
  fast clocks, fine timestamp granularity, or unnormalized paths. The fixes
  at `3e6adba` split evictCache by filename constraint and hardened the
  tests. v0.2.0 adds usage instrumentation with the `ohya usage`, `status`,
  `metrics`, `logs`, and `diagnose` commands, and replaces the embedded
  Headscale stack with the Tailcat transport plus `ohya setup --hub` and a
  one-line device join. The released installer upgraded the linux-amd64 hub
  host end to end; `ohya doctor` exited 0 with the three long-standing
  stopped sources (ADR 0004) as the only warnings.
- v0.3.0 (commit `3fe9f4e`, dirty=false) was published on 2026-11-20 after
  the full hosted matrix passed (run 44814572741) and local `make check` +
  `make tla` passed with zero errors. The first candidate (run 44813684376,
  commit `69b2112`) passed Ubuntu 22.04 and failed on macOS in
  `TestDiscoverSessionFilesExcludeMatchesSymlinkedPathForm` due to `/var ->
  /private/var` temp-dir symlinks; fixed at `3fe9f4e`. v0.3.0 adds schema 8
  (ADR 0013 owner-scoped purge & `ohya sources reallow`), `--exclude`
  discovery scope controls (ADR 0016), negotiated zstd batch upload
  (ADR 0014), `ohya search`/`ohya show`, secret-shaped value redaction,
  parser upgrades (`claudecode` v2, `codex` v4, `pi` v3), and end-to-end
  reliability/security/observability hardening. The released installer
  upgraded the linux-amd64 hub host end to end; `ohya.service` and
  `ohya-serve.service` are active.
- v0.4.0 (commit `ae217b7`, dirty=false) was published on 2026-11-22 after
  the full hosted matrix passed (run 45070974033) and local `make check` +
  `make tla` passed with zero errors. v0.4.0 stays on schema 8 and adds
  clean-shutdown snapshot markers (`stopped: true`), periodic stale
  instance cleanup, retained-instance folding (`--all`) and live-only
  daemon duplicate detection in `ohya status` and `ohya metrics`, unified
  TTY/NO_COLOR styled CLI formatting, `ohya status --remote` hub
  aggregates (Plan 001 M5, `Auth.tla` verified), `ohya service
  status|start|stop|restart`, `ohya sessions` plus `--json` for
  `search`/`show`, `64 MiB` `journal_size_limit` with WAL truncation after
  `rederive`, and the native SwiftUI `macos/OhyaMenuBar` app source (ADR
  0017). The released installer upgraded the linux-amd64 hub host end to
  end; after restarting `ohya-serve.service`, both `ohya doctor` and
  `ohya status` exited 0.
- v0.5.0 (commit `8d9c99e`, dirty=false) was published on 2026-11-22 after
  the full hosted matrix passed (run 45076216169) and local `make check` +
  `make tla` passed with zero errors. v0.5.0 stays on schema 8 and adds
  first-class Antigravity (`agy`) support (`internal/antigravity` parser v1,
  `--antigravity-root` discovery of append-only `transcript_full.jsonl`,
  `agy` MCP wiring in `ohya setup`/`uninstall`/`doctor`, `parser.antigravity`
  version/status reporting) plus `cmd/recall-bench`. End-to-end upgrade on
  the linux-amd64 host via `install.sh` caught that `agy` initializes a
  0-byte `~/.gemini/config/mcp_config.json`, which `setup.ApplyJSONEntry`
  rejected with `unexpected end of JSON input`.
- v0.5.1 (commit `ac46079`, dirty=false) followed the same day after the full
  hosted matrix passed (run 45078362626) and local `make check` passed with
  zero errors. It treats empty or whitespace-only JSON MCP configs as `{}` in
  `setup.ApplyJSONEntry` and project preflight while preserving original bytes
  in backups and still rejecting malformed JSON. The released installer
  upgraded the linux-amd64 hub host end to end with exit 0, configured all
  four clients (`claude`, `codex`, `pi`, `agy`), ingested all 40 local
  Antigravity conversations (`antigravity 1`), and after restarting
  `ohya-serve.service`, both `ohya doctor` and `ohya status` exited 0.
- v0.6.0 (commit `66ccefc`, dirty=false) was published on 2026-11-23 after
  the full hosted matrix passed (run 45160837836) and local `make check` +
  `make tla` passed with zero errors. v0.6.0 stays on schema 8 and adds
  on-demand MCP guide resources (`ohya://guides/garbage-audit`,
  `ohya://guides/analytics-and-review`) trimming upfront MCP schemas from
  15.4 KB to <8 KB, FTS5 `snippet()` / UTC timestamp / `session_id` /
  context-around-hit / one-hit-per-session / agent-window / FTS-count
  recipes, the `ohya sql` CLI subcommand, owned cross-harness
  `skills/ohya/SKILL.md` installation (`claude`, `codex`, `pi`, `agy`), and
  `cmd/recall-bench` repeatability and grading hygiene (`local-echo`
  rejection on `remote-only` questions, max-usage timeout parsing, unbilled
  timeout cost markers). The released installer upgraded the linux-amd64 hub
  host end to end with exit 0, installed all four skills, restarted
  `ohya-serve.service`, and both `ohya doctor` (20/20 `ok`) and `ohya status`
  exited 0.
- v0.6.1 (commit `bf50937`, dirty=false) was published on 2026-11-23 after
  the full hosted matrix passed (run 45180792640) and local `make check` +
  `make tla` passed with zero errors. v0.6.1 stays on schema 8 and changes
  only CLI text output: terminal-width fitting, shorter `sessions` and
  `search` records, `doctor` remedies only on failing checks, `metrics`
  counter tables, aligned `setup` rows, and tidier help. It also records
  v0.6.0 and v0.6.1 in `releasedSchemas`; v0.6.0 had shipped without that
  record, which failed `TestReleasedSchemasMatchPublishedTags`. The
  released installer upgraded the linux-amd64 hub host with exit 0; after
  restarting `ohya-serve.service`, `ohya doctor` reported no errors. The
  upgrade showed that v0.6.1 marks the healthy setup statuses `running`,
  `ready`, and `not needed` with the warning mark; the fix is on `main` for
  the next release.
- The installer refuses to replace an executable whose digest matches neither
  its receipt entry nor the previous one ("installed executable has changed;
  preserve it"). A copy installed by hand outside `install.sh` triggers this
  refusal. Restore the receipted executable, then rerun the installer.
- The published macOS archives are unsigned and not notarized
  (`signing=unverified`); Developer ID credentials are still required.
- No Homebrew tap is published; the qualified formula from the candidate run
  is ready for a future tap.
- Pi 0.84.2 automatically discovered the compatibility extension and completed
  SQL and Unicode annotation round trips without model access.

The candidate workflow also tests the generated formula on both native macOS
architectures. `scripts/homebrew-install-smoke.sh dist` creates a temporary local
tap and rewrites download URLs to the verified local archives. It runs
`brew install`, `brew test`, and removal. It refuses an existing ohya formula
or test tap. This test does not publish a tap or run client setup.
