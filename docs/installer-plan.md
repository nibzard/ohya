# Installer plan

Status: proposed, 2026-11-15. No installer or release is published by this plan.

## Recommendation

Ship prebuilt ohya binaries through GitHub Releases. Provide a small POSIX
shell installer as the primary entry point. Add a Homebrew tap after the
archive contract passes native tests. Use the same release archives for both.

The installer needs no Go, C compiler, Node.js, Python, or running ohya process.
It installs the executable and prints the next commands. Installation needs
no elevated privileges and reads no agent sessions.

Start with macOS arm64 and amd64, plus Linux amd64. Each target must pass
native tests before publication. Linux arm64 is a later addition unless a
native runner is available. Windows and Linux musl are outside the first
release. Do not imply support merely because an archive cross-compiles.

## Research basis

The sources below were checked on 2026-11-15.

| Tool | Verified approach | Lesson for ohya |
| --- | --- | --- |
| [Claude Code](https://code.claude.com/docs/en/setup) | Native installer, version selection, automatic updates, package managers, signed release manifests | Make installation easy and keep executable files separate from user state |
| [Codex](https://github.com/openai/codex/blob/main/README.md) | Standalone installers, npm, Homebrew, release archives | Reuse one set of binaries across distribution methods |
| [Pi](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/quickstart.md) | Managed installers with pinned dependencies and `pi update`; npm and Nix alternatives | Installation method must determine update ownership |
| [uv](https://docs.astral.sh/uv/getting-started/installation/) | Standalone and version-specific installers; package manager installations disable self-update | Support exact versions and predictable automation |
| [GitHub CLI](https://github.com/cli/cli/blob/trunk/docs/install_linux.md) | Signed package repositories and prebuilt binaries | Package managers should own their installations |

Claude Code is a comparison for user experience. This plan does not classify
its CLI as open source. Pi's current managed installer is distinct from its
legacy npm installation path.

## Installation contract

- Default destination: `~/.local/bin/ohya`. Support `--bin-dir` and `--version`.
  Install the latest stable release when no version is supplied. Exclude
  prereleases unless the user supplies their exact version.
- Resolve the release once. Download the archive and checksum list from that
  exact release. Do not mix a mutable latest archive with a separate latest
  checksum request.
- Require HTTPS, bounded downloads, timeouts, and SHA-256 verification. Use
  `sha256sum` or macOS `shasum -a 256`. Verify before extracting or executing.
- Stage files privately. Accept only the expected archive entries. Reject
  path traversal, unexpected links, unsupported platforms, and wrong binaries.
- Run the staged binary's `version` command and compare its release identity.
  This command opens no database. Publish clean builds with an exact version
  and revision; verify the current Go build metadata supplies both.
- Copy to a temporary file in the destination directory and replace the
  executable with one rename. Do not truncate an installed executable. A
  failed download or verification must leave the old executable usable.
- Serialize installations for the same destination. Store a small ownership
  receipt beside the executable. Define crash recovery so an interrupted
  receipt write cannot authorize replacement of an unrelated executable.
  Refuse a foreign file or symlink; never follow it into a Homebrew prefix.
- Detect another `ohya` earlier on PATH and report the conflict. Print the
  installed absolute path and the required PATH command. Do not edit shell
  profiles automatically. Support spaces in paths and an existing destination.
- Keep all output and errors suitable for unattended use: no prompts, stable
  exit codes, and an explicit diagnostic for each failed prerequisite.

The following is the proposed interface, not a command that works today:

```sh
sh install.sh --version v0.1.0 --bin-dir "$HOME/.local/bin"
```

For the first release, document download-then-run and a shorter pipe form
using the repository's actual published installer URL. Do not invent a domain.
A version-pinned installation must also use the release's versioned installer.

## Updates, removal, and setup

Use the installer again to update a standalone installation. Add an
`ohya update` command only if repeated use establishes a need. Homebrew owns
`brew upgrade` and `brew uninstall`; a shell installer must not replace its
files. Do not add an npm wrapper for a Go binary in the first release.

Running processes continue using the old executable after replacement.
Upgrading the file does not restart a hub, uploader, or Model Context Protocol
(MCP) client. Document a coordinated stop, backup, update, and restart procedure.
All processes sharing a device transport must restart together. A database
migration or newer parser output can prevent use of an older binary; binary
replacement alone is not a database rollback procedure.

Removal deletes only installer-owned executable files and the receipt.
Preserve `~/.ohya`, custom state directories, databases, credentials, and agent
session directories. Provide a tested removal path through the installer and
manual removal instructions. Do not include a data purge option initially.

Keep executable installation separate from service startup and MCP setup.
Print `ohya version`, `ohya doctor`, and links to local and remote setup.
Do not automatically start ingestion, create launch agents, join a hub, or
change another tool's configuration. Service installation can follow as a
separate command with explicit options.

## Release artifacts and trust

Use archives such as `ohya_v0.1.0_darwin_arm64.tar.gz`, each containing `ohya`,
`LICENSE`, and a short installation document. Publish `SHA256SUMS`, build
provenance, the installer, and release notes for the exact tag.

Build with CGO and `sqlite_fts5` on target-appropriate runners. Inspect dynamic
libraries and test the oldest supported operating system baseline. End users
must not need a compiler or an external SQLite installation. Do not claim
static Linux binaries without verifying them.

Sign and notarize macOS release artifacts before computing archive checksums.
Verify them on a clean Mac through the real download path. Do not remove
quarantine attributes as an installation workaround.

For Linux, the initial bootstrap trusts HTTPS and the GitHub release account.
Checksums detect corruption, not an attacker who can replace both files.
Publish provenance with documented verification. Decide whether automatic
signature verification is a first-release requirement before implementing
bootstrap dependencies. Never silently skip a required verification step.

Use protected release jobs with minimal permissions. Assemble a draft release
and verify every artifact before publication. Enable immutable releases where
available and verify that setting. Never replace an asset under an existing
version. A failed release needs a new version.

## Implementation sequence

1. Define target platforms, minimum operating system versions, first release
   version, ownership rules, and the trust policy. Record the accepted design
   in an architecture decision record. Confirm signing credentials.
2. Add a release build workflow and archive generation. Test binaries natively,
   including SQLite FTS5, `version`, local ingestion, and transport. Verify the
   archive identity, library dependencies, signing, and checksums.
3. Implement `install.sh` with version selection, platform detection, staging,
   verification, ownership checks, atomic replacement, and removal. Keep it
   independent of the Go application and its database.
4. Add installer tests with a local fixture server and isolated home directory.
   Cover fresh install, repeat install, upgrade, wrong checksum, missing asset,
   interrupted transfer, invalid archive, foreign destination, concurrent
   install, custom paths, PATH conflicts, removal, and unchanged user data.
   Production URLs must still require HTTPS.
5. Test downloaded release candidates on clean macOS and Linux environments.
   Test both macOS architectures, the minimum Linux baseline, and shell
   compatibility. Exercise a live-process upgrade and the documented restart
   procedure. Cross-build success does not satisfy these gates.
6. Add a Homebrew tap using the verified archives and checksums. Test install,
   upgrade, removal, and conflict handling with the standalone installer.
   Document install, update, uninstall, source builds, and recovery in README
   and the operations guide.
7. Prepare a draft release with test evidence and known limits. Review it before
   publication. Publishing is a separate action from implementing this plan.

## Acceptance criteria

A user installs and runs ohya without a development toolchain or elevated
privileges. An exact version selects an exact release. Failed installation
preserves the previous executable. Successful updates require an explicit
process restart. Uninstallation preserves all memory and credentials.

Native tests prove every advertised platform. macOS verification proves signing
and notarization. The installed binary reports its release identity. Homebrew
and standalone installation never overwrite one another's files.

## Open decisions

- Confirm the first release version and the macOS minimum version.
- Confirm native amd64 macOS runner access and Apple signing credentials.
- Select and test the Linux libc baseline.
- Decide whether Linux bootstrap signatures are mandatory initially.
- Confirm repository release settings and the Homebrew tap owner.
