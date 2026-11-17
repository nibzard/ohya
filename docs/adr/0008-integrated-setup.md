# 0008: Integrated installation and setup

Status: implemented; native release qualification pending, 2026-11-15.

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
Remote join, legacy Pi, private backups, PATH setup, and the bootstrap use
the same setup engine. Native release qualification remains pending.

Managed ingestion uses `--allow-empty` so agents with no session files can
start writing later. Missing roots remain absent and are polled without being
created. Normal ingestion keeps its existing missing-root error behavior.
Uninstall preserves edited service files before sending any stop command.

## Recovery points

Before setup starts a memory process against an existing local database, it
reads the applied schema version read-only. When that version matches the
build's newest migration, the new process cannot migrate the database, so
setup reports the backup as `not needed` and makes no copy. Otherwise, or when
the version cannot be read, setup creates an owner-only recovery directory and
copies the database through SQLite's online backup API. A copy of a large
database takes minutes and stops ingestion for that time, so a copy on every
rerun has a high cost and no benefit. After a successful copy, setup keeps the
two newest recovery directories and removes older ones. The copy includes committed WAL pages and does not
migrate the source. Each backup uses an exclusive destination and a bounded,
context-aware copy loop whose deadline scales with the database size. The backup is flushed before setup proceeds.

The copy is size-aware (2026-11-16, plan 004 S1): setup checks free space on
the recovery directory's filesystem first and refuses, changing nothing, when
it is below the database size plus its WAL, a ten percent margin, and 64 MiB;
the refusal names both numbers and a remedy. Setup reports the estimated size
before the copy and a progress line at every further ten percent. The copy is
staged under a hidden directory and renamed into the kept set only after it
succeeds, so an interrupted copy never leaves a partial recovery point that
pruning would keep, and staging abandoned by a killed run is removed on the
next run.

Owned ingestion is stopped before the recovery point, including on a setup
rerun with unchanged service settings. External writers do not invalidate the
online snapshot. Setup does not proceed to database opening when backup or
service preparation fails. Backups are retained during removal. Setup prints
a progress line on stderr before and during a copy and prints each component
result when it completes. Restore with
a compatible binary and matching stopped device state and checkpoints. A
database-only recovery point cannot safely replace a coordinated state backup.
Never combine older hub data with newer acknowledgments. Preserve the failed
database and its WAL files together. See `docs/release-operations.md`.

## Download bootstrap and executable ownership

`install.sh` resolves one release and downloads its archive and checksum list
from that exact version over HTTPS. It verifies SHA-256, the allowed archive
entries, regular file types, and the executable's release identity before
calling the staged binary's install command. When the latest-release lookup
lands on the releases list — GitHub answers `/releases/latest` with a
redirect to `/releases` for a repository without a stable release — the
installer says so, points to the release list, and asks for `--version`.
A release root override remains HTTPS-only. Checksums share the release
host's trust boundary.

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

## Memory guidance

The MCP initialization response delivers memory guidance to every compatible
client. It covers selective recall, bounded queries, event citations, and the
untrusted status of stored transcripts. It avoids unnecessary recall when the
current conversation already supplies the answer. This keeps guidance current
with the executable without replacing user instruction files or changing client
permissions. The initialization test verifies that clients receive it.

## Remote setup

Setup stores its connection choice under the setup state directory. Remote
transport, credential, and upload checkpoints live in its `join` subdirectory;
local memory remains at the original location. Service and client ownership
receipts remain at the setup root so switching modes updates owned entries.
A remote join state is bound to one hub and cannot be rebound through setup.

The invitation comes from a named environment variable, is validated without
printing its value, and is removed from the process environment before children
start. Setup uses the existing pairing path with a bounded context and closes
the transport before starting the uploader. Service files carry only saved
connection addresses and state paths. Reruns reuse the credential and saved
mode. Local setup is an explicit mode switch. The existing pairing and sync
protocol transitions are unchanged; all TLA+ models were checked successfully.

An integration test starts a real hub, pairs through setup, checks the remote
MCP tools, and reruns setup without the invitation. It disables user services,
client configuration, and PATH changes in the test process.

## Pi compatibility

Pi 0.84.2 has no native MCP command but supports user extensions. Setup installs
an owned extension and a private connection file for this verified version.
Versions with native MCP keep the native path. Other versions without native
MCP fail capability detection rather than receiving an unverified integration.

The extension uses Pi's supplied TypeBox module and Node's process API. It
registers memory tools and sends bounded JSON requests through stdin to the
installed binary's bridge command. The bridge uses the existing official Go
MCP client SDK to invoke the same MCP server. It permits only sql and annotate,
requires its own executable's MCP command, and bounds request lifetime. It
contains no SQL or annotation write implementation. Response decoding preserves
UTF-8 across pipe chunks.

The real installed Pi loader test registers both tools, queries memory, writes
an annotation, and reads Unicode back. It uses isolated state and no provider
credentials. Setup and removal preserve user-edited extension files. Switching
to native MCP removes the unchanged owned compatibility extension first so
agents do not get two copies of the tools.

## Readiness and read-only inspection

Loaded service state is distinct from running process state. Setup uses loaded
state when deciding whether an owned job needs to be stopped and reloaded.
Readiness requires a positive process ID and the running state. Setup checks
readiness twice within a bounded wait to reject immediate startup failures.

Doctor reads ownership receipts and current client, profile, extension, and
service files. It reports changes without rewriting them. Service inspection
uses read-only status commands. Missing state stays missing. A remote setup
selects its saved join state and saved hub address (the coordination URL
ended with the embedded coordination plane; ADR 0011); doctor does not
require a local hub database. Upload checkpoints can be checked for
readability, while
hub-side parser stops remain the uploader's diagnostic responsibility. Doctor
never starts an MCP server merely to test a connection, because that could
create or migrate state.

## Client preflight

Before changing configuration or consuming a remote invitation, setup checks
project MCP entries in the current directory and its ancestors. A project
entry named ohya requires review because it can shadow the user registration.
Setup does not edit that entry. User configuration itself is excluded from
project override checks.

Claude preflight checks its documented local settings files and system managed
MCP files. Exclusive managed MCP configuration blocks user registration.
Allowlist command matching takes precedence over name matching, and denylist
matches always block. No policy is loosened. Invalid or unreadable settings
produce a diagnostic without printing their contents. Additional cloud or MDM
policy remains under the client's control when it starts; setup does not claim
to enumerate policy sources that are unavailable locally.

Sources checked 2026-11-15:
https://code.claude.com/docs/en/managed-mcp and
https://learn.chatgpt.com/docs/extend/mcp?surface=cli.
Setup reads regular configuration files with an 8 MiB bound and refuses final
symlinks. It never executes project configuration to inspect it.

Local trial (2026-11-15): Claude settings managed as dotfiles are often
symlinks, and Claude Code follows them. Refusing them blocked Claude setup,
and the blocked client stopped the service and the other agents too. The
read-only preflight inspections (Claude policy settings and project MCP
entries) now follow symlinks to a regular file, with the same 8 MiB bound.
A symlink to a directory or another non-regular file is still refused.
Files that setup writes still refuse final symlinks.

Native Linux service validation (2026-11-15) found that systemd treats quotes
in WorkingDirectory as literal path characters. That field now uses the raw
absolute path with specifiers escaped; ExecStart keeps argument quoting. A
systemd-analyze regression test parses the generated unit with spaces and
expansion characters. The corrected native service imported 16 synthetic events,
acknowledged the full Source cursor, passed doctor, and uninstalled cleanly.
This verifies the current Linux host, not the unrun release baseline matrix.

## Executable paths and removal

Setup prefers the invoked executable path when it resolves to the running
executable. This preserves a package manager's stable link rather than storing
a version-specific Cellar path. An unrelated path cannot be adopted this way.

Uninstall removes a standalone executable only after integration removal
succeeds and its installer ownership hash still matches. Manual and package
manager executables remain under their existing owner. `--keep-binary` retains
an installer-owned executable for repair. The install lock file and recovery
copies are retained; deleting an advisory lock file could split lock ownership.
An edited executable is preserved and reported as a failure.

## Saved choices and failed attempts

Setup saves only an allowlisted set of flags and client home overrides. It
reuses agent selection, absolute session roots, disabled roots, service choices,
and PATH choices on reruns. Explicit flags take precedence, including false
booleans. An automatic agent selection stays automatic for new clients.
Provider credentials and one-time invitations are excluded from preferences.

Client adapters preserve exact original file bytes in private backups before
changes. Backups stay beside the client receipts and remain after uninstall.
They support selective recovery without replacing later user settings.

Service ownership is checked before stopping a loaded service. Setup saves the
SQLite recovery point before changing its service file. A failed attempt
restores the prior owned service configuration and restarts it under a bounded
cleanup context. It reports both the original error and any recovery failure.
This does not restore an older database or executable.

Setup reports when another ohya executable is selected by the current PATH.
It keeps all service and client paths absolute and does not adopt that file.
