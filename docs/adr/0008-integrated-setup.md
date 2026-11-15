# 0008: Integrated installation and setup

Status: accepted design, implementation in progress, 2026-11-15.

## Context

Users need one pasted command that installs memory and connects their agents.
A downloaded executable alone does not meet that requirement. Setup must also
support coding agents without prompts or provider credentials.

## Decision

A shell bootstrap downloads and verifies a release executable. The executable
owns setup orchestration, user services, client adapters, health checks, repair,
and removal. The detailed contract is in `docs/installer-plan.md`.

Default setup uses local ingestion and user-scoped agent integrations. Remote
join is an explicit alternative. Existing agent policies remain authoritative.
Setup does not change model selection or disable permission checks.

Client configuration edits require ownership receipts. For JSON clients, setup
preserves unrelated values, rejects malformed objects and file symlinks, and
writes through the existing durable file helper. A receipt is prepared before
the client entry is changed. It records both the intended entry and the prior
owned value so an interrupted update can resume. An existing unowned entry is
a conflict. Removal preserves entries edited after setup.

Setup must serialize its own operations. Optimistic rereads detect external
edits before replacement; they cannot provide a transaction with an unrelated
client process. Configuration adapters must report conflicts and avoid whole
file rollback over later user edits. Private backups and receipts can contain
client configuration and must remain owner-only.

Success requires protocol verification, not only a successful file write.
Report partial completion explicitly and keep completed components repairable.
The setup receipt is not an ingestion checkpoint and never advances a Source
Cursor or supplies a hub Acknowledgment.

## Consequences

The application gains operational setup commands, but retains one binary.
Client-specific integration stays outside ingestion, synchronization, and SQL.
The bootstrap stays small. Native release testing and signing remain necessary
before a public installer can claim support for a platform.

No release is published by this decision.

## Client formats and verification

The Codex adapter appends an owned TOML table and preserves the rest of the
file byte for byte. It uses the existing pinned TOML parser to reject malformed
configuration, detect foreign entries, and detect fields added to an owned
table. A prepared receipt retains the previous block for interrupted updates.
The parser moves from an indirect dependency to a direct dependency; no module
or version is added.

Claude Code and native Pi use the JSON adapter. Client paths honor their home
overrides. Claude Code's `CLAUDE_CONFIG_DIR` stores `.claude.json` directly in
that directory, verified with the installed CLI in an isolated directory.
Missing clients and missing explicitly requested clients have distinct results.
Capability checks for older Pi remain required before invoking its native
configuration adapter.

Setup verification uses the existing MCP SDK as a client. It requires both
memory tools and checks the exact result of `SELECT 1 AS ohya_setup_check`.
It has a bounded lifetime and launches from a neutral working directory.
The integration test launches the real application through its existing
subprocess test entry point. It requires no model access.

## Service files

User services use argument arrays on macOS and escaped systemd arguments on
Linux. The Linux unit uses the user journal with a rate limit. Neither template
embeds credentials. Service paths and arguments reject line breaks and null
bytes. Native service-manager tests remain a release requirement.

Service and other complete files have separate ownership receipts. Prepared
updates retain the prior owned bytes. Removal refuses a file edited after
setup. A nonblocking file lock serializes setup processes using one state
directory. The lock file remains on disk after release; process exit releases
the operating system lock.

## CLI orchestration

`ohya setup` exposes dry runs, agent selection, JSON results, and service and
agent opt-outs. The local flow serializes updates, applies service files,
checks the memory protocol, and registers supported native clients. A failed
component makes the command fail while preserving its completed result report.
The CLI integration is incremental: remote join, legacy Pi, backups, PATH
setup, and the bootstrap remain to implement before release.

Managed ingestion uses `--allow-empty` so agents with no session files can
start writing later. Missing roots remain absent and are polled without being
created. Normal ingestion keeps its existing missing-root error behavior.
Uninstall preserves edited service files before sending any stop command.

## Recovery points

Before setup starts a memory process against an existing local database, it
creates an owner-only recovery directory and copies the database through
SQLite's online backup API. The copy includes committed WAL pages and does not
migrate the source. Each backup uses an exclusive destination and a bounded,
context-aware copy loop. The backup is flushed before setup proceeds.

Owned ingestion is stopped before the recovery point, including on a setup
rerun with unchanged service settings. External writers do not invalidate the
online snapshot. Setup does not proceed to database opening when backup or
service preparation fails. Backups are retained during removal. Restore with
a compatible binary after stopping writers and preserving the old database
and WAL files together; copying a live database file is not a backup procedure.

## Download bootstrap and executable ownership

`install.sh` resolves one release and downloads its archive and checksum list
from that exact version over HTTPS. It verifies SHA-256, the allowed archive
entries, regular file types, and the executable's release identity before
calling the staged binary's install command. A release root override remains
HTTPS-only. Checksums share the release host's trust boundary.

The staged executable streams its own bytes into a private staging directory
on the destination filesystem. A destination lock serializes installations.
A hash receipt rejects foreign files and edited executables. Upgrades save
the prior executable in a private recovery directory before replacing the
installed executable with a rename. The receipt retains the prior hash so an
interrupted replacement is recoverable. Failed integrated setup leaves the
installed binary available for repair; it does not attempt a database downgrade.

Release builds set `main.releaseVersion` to the exact tag and retain Go build
revision metadata. After installation, setup registers the stable destination
path, not the temporary download path. Installer diagnostics go to stderr so
the setup JSON result remains parseable. Native platform validation and release
publication are still required before users can run a hosted install command.

## Shell PATH configuration

Setup appends an owned PATH block for Bash, Zsh, and POSIX shells. Bash login
setup chooses the first existing login file rather than creating a file that
would hide an existing lower-priority profile. Fish gets a separate owned
configuration file. Existing shell content stays byte-identical outside the
owned block. Prepared receipts allow interrupted updates to resume. Repeated
sourcing does not add duplicate PATH entries.

Removal preserves edited blocks and later user content. New empty profiles
are removed only when their receipt records that setup created them. Shell
syntax uses literal quoting and does not interpolate executable paths into
commands. Colons in executable directories are rejected because PATH uses
colons as separators. Unsupported shells require `--no-modify-path` and an
explicit PATH configuration. Existing processes cannot inherit changes made
by a child installer; service and client entries use absolute paths.
