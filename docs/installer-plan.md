# Installer plan

Status: implemented locally; native release qualification pending, 2026-11-15.
No installer URL or release is published. See `docs/release-operations.md` for
the candidate workflow, evidence, and remaining release gates.

## Recommendation

Ship prebuilt ohya binaries through GitHub Releases. Provide a small POSIX
shell installer as the primary entry point. Add a Homebrew tap after the
archive contract passes native tests. Use the same release archives for both.

The default command delivers working local memory, not just an executable.
It installs ohya, starts ingestion, connects installed agents, and checks the
result. It needs no development toolchain or elevated privileges. Existing
agents retain their own runtime requirements.

A small shell bootstrap downloads and verifies the executable. It then invokes
`ohya setup` by absolute path. Go owns service setup, client adapters, recovery,
and verification. Users and coding agents get the same non-interactive flow.
The commands and options in this document define the implemented interface.

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
  installed absolute path. Add one owned PATH block to the detected shell's
  supported startup file when needed. Preserve other content, avoid duplicate
  blocks, and support `--no-modify-path`. Service and agent configuration use
  absolute paths and work without opening a new shell. A child installer cannot
  change the parent shell environment. Support spaces in all paths.
- Keep all output and errors suitable for unattended use: no prompts, stable
  exit codes, and an explicit diagnostic for each failed prerequisite.

## One-command experience

The published installer defaults to full local setup. Users paste its download
command once; there are no configuration files to copy or service commands to
run. Until an installer URL exists, illustrate the interface with a local file:

```sh
sh install.sh
sh install.sh --agents claude,codex,pi --json
sh install.sh --join-code-env OHYA_JOIN_CODE
```

The first command discovers installed supported agents. It configures all of
those agents. Missing agents are listed as absent. An installed agent with an
unsupported version or failed configuration produces a partial result and a
nonzero exit code. Do not quietly skip it and report complete success.

The second command selects clients explicitly and prints one versioned JSON
result. Missing requested clients are failures. The third joins an existing hub
instead of creating local memory. Read the code from the named environment
variable; keep it out of logs, process arguments, receipts, and service files.
Subsequent starts use the stored credential.

Offer `--version`, `--bin-dir`, `--state`, `--dry-run`, `--no-service`,
`--no-agents`, and `--no-modify-path` for automation and custom deployments.
Normal users need none of these. `ohya setup` repeats setup after installing a
new agent or repairing a partial installation, without another download.
A Homebrew user runs `ohya setup` after package installation; it uses the same
setup engine without taking ownership of the Homebrew executable.

## Default setup flow

1. Detect supported agent executables, versions, home overrides, session roots,
   existing ohya state, and the user's service manager. Print a short plan.
   Do not create configurations for clients that are absent.
2. Create or reuse local ohya identity and storage. Keep credentials private.
   Preserve prior choices on reruns. On a new local install, use `ohya ingest`
   as the background process; no network listener or pairing is required.
3. Install and start a user service: LaunchAgent on macOS, systemd user service
   on supported Linux desktops. Use absolute paths, explicit session roots,
   captured home overrides, bounded logs, restart backoff, and start at login.
   Do not require root or enable system-wide services. If the user service
   manager is unavailable, return a precise partial result; support an explicit
   foreground deployment rather than claiming persistence was installed.
4. Register the `ohya` stdio server in detected clients at user scope. Point each
   client at the installed absolute path and explicit database/state paths.
   Joined devices instead use `ohya mcp --remote` with the correct join state
   and coordination URL. Their managed process runs the uploader.
5. Provide memory guidance through MCP server instructions and tool descriptions.
   Where a client needs a separate skill, install an owned `ohya-memory` skill
   through its supported mechanism. Teach selective recall, event citations,
   bounded queries, and treating transcript content as data. Current-context
   questions must not trigger unnecessary memory queries. Preserve user
   instruction files and all existing permission settings.
6. Verify the service, database, effective client entry, MCP initialization,
   tool discovery, and a read-only `SELECT 1` through that entry. Do not invoke
   a paid model or read transcript bodies for the setup check. Detect project
   overrides and organizational policy that prevent the user entry taking
   effect in the current directory. Do not bypass client approvals.
7. Print per-agent results and import progress, plus one repair command for each
   failure. A healthy installation can report that history import continues;
   it must distinguish usable memory from complete history. Show whether an
   existing client needs a reload or a new session.

Keep initial import asynchronous so a large history does not block setup for
minutes. Report source counts, durable cursors, failures, and stopped sources.
A running process alone does not prove ingestion works. Empty histories are
valid; unreadable or stopped sources produce a visible degraded status.

## Agent adapters

Use structured client files with receipts to preserve unrelated settings and
resume interrupted writes. The adapters use the documented client formats.
Read the effective existing
entry before changing it. Apply changes only to the owned ohya entry. Use
bounded subprocess timeouts, argument arrays, and structured output when
available. Do not run configuration commands in a repository that can load
untrusted project settings; use a controlled working directory.

| Agent | Registration | Verification |
| --- | --- | --- |
| Claude Code | Owned `mcpServers.ohya` entry in the user JSON file | Inspect the user entry and connect through its exact command |
| Codex | Owned `[mcp_servers.ohya]` table in the user TOML file | Read the owned table and connect through its exact command |
| Pi with native MCP | Owned `mcpServers.ohya` entry after capability detection | Inspect its user entry and verify the connection |
| Supported older Pi | Install a pinned, ohya-maintained user extension that exposes the same MCP tools | Load the extension in an isolated test session and call the same tools |

Sources: [Claude Code MCP configuration](https://code.claude.com/docs/en/mcp),
[Codex MCP configuration](https://developers.openai.com/codex/mcp), and
[Pi MCP configuration](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/mcp.md).

Current Pi documentation describes native MCP, but the installed Pi on the
research machine prints general help for `pi mcp --help` and lists no MCP
command. Capability detection and a tested version matrix are required.
A matching version string alone is insufficient when extensions can replace
built-in MCP. The older-Pi adapter must ship with this release or be fetched
as an exact verified artifact. Use the official MCP client library rather
than a second SQL or annotation implementation. Do not silently upgrade Pi
or install an arbitrary third-party bridge.

Respect `CODEX_HOME`, the Claude configuration override supported by the
installed client, and `PI_CODING_AGENT_DIR`. Service processes do not inherit
interactive shell configuration reliably. Persist the resolved paths needed
for ingestion. Never copy provider API keys into service environments.

The receipt records owned entries, service files, profile blocks, skill files,
and previous values. Back up changed configuration privately. Recheck files
before writes and report concurrent changes. A pre-existing foreign `ohya`
entry is a conflict, not permission to overwrite it. Identical owned entries
are no-ops. Do not disable approvals, change models, or replace `AGENTS.md` or
`CLAUDE.md`. Explain any client-enforced approval or reload still required.

## Recovery, updates, and removal

Setup is rerunnable after every step. Record completed steps and provide a
versioned JSON report with changed, unchanged, skipped, failed, and pending
items. Diagnostics go to stderr. Success exits zero only when every requested
component is configured and verified; partial completion is nonzero. A dry run
reports proposed changes without starting services or writing configuration.

On failure, preserve working services and client entries. Revert only changes
made by this attempt when safe; do not restore a whole old configuration over
new user edits. Keep the binary available so `ohya setup` can repair failures.

Use the installer again to update a standalone installation. Reuse setup to
restart owned services and verify connections after the executable changes.
Homebrew owns executable upgrades. Do not add an npm wrapper initially.

Before a migration, stop owned writers and make a consistent SQLite backup.
Record a recovery point before opening the database with the new version.
Never copy only the live database file while its WAL may contain commits.
Restart owned processes in the correct order. Report external running clients
that need a new session; do not terminate the user's coding sessions.
A previous executable alone cannot roll back a newer schema or parser output.

Add `ohya uninstall` to stop owned services and remove owned MCP entries,
skills, profile blocks, executable files, and receipts. Preserve databases,
credentials, agent sessions, and custom state. If an owned entry has since
been edited, preserve it and report the remaining item. Do not include a data
purge option initially. For Homebrew, remove integrations through ohya before
removing the executable through Homebrew.

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

1. Define release targets, trust policy, and tested agent versions. Record the
   integrated setup design in an architecture decision record. Confirm signing
   credentials and user service behavior on each supported platform.
2. Build and package native release candidates. Verify release identity,
   library dependencies, signing, checksums, and SQLite FTS5 behavior.
3. Implement the shell bootstrap and `ohya setup` orchestration. Add user
   services, receipts, private backups, resumable steps, dry run, and JSON
   results. The bootstrap delegates configuration to the verified binary.
4. Add Claude Code, Codex, and both Pi adapters. Include memory guidance and
   real protocol checks. Test environment overrides, spaces in paths,
   conflicting entries, policy restrictions, and existing configuration.
5. Test the complete paste-to-ready flow on clean supported systems. Cover
   fresh install, rerun, new agent discovery, large initial history, failures
   at every step, concurrent edits, unavailable service managers, remote join,
   logout/login, upgrade, and removal. Verify unrelated files and memory stay
   intact. Test actual client tool use in isolated release validation sessions;
   ordinary installation needs no provider credentials.
6. Add Homebrew using the same release archives and setup engine. Verify the
   two installation methods respect ownership. Document one primary command,
   concise result states, and repair commands. Extend `ohya doctor` to inspect
   service and client integration health without modifying them.
7. Prepare a draft release with native test evidence. Publish only after a
   separate release review. No implementation step authorizes publication.

## Acceptance criteria

One pasted command installs usable memory and connects every detected supported
agent, without configuration copying, elevated privileges, or toolchain setup.
A fresh agent session can discover and query ohya. The background service
survives installer exit and restarts at login. Existing history imports in the
background and live additions become searchable.

Rerunning setup adds newly installed agents and repairs owned configuration
without duplicates. JSON reports accurately distinguish ready, importing,
absent, unsupported, blocked, and failed components. Client approval or reload
requirements remain visible. Partial setup never prints unconditional success.

Failed downloads preserve the executable. Failed setup preserves unrelated
settings and working integrations. Updates restart owned services and establish
a consistent recovery point before migrations. Uninstall disconnects owned
integrations while preserving all memory, credentials, and agent sessions.

Native tests prove every advertised platform. macOS verification proves signing
and notarization. The binary reports its exact release identity. Package manager
and standalone installation never overwrite one another's files.

## Open decisions

- Confirm the first release version and the macOS minimum version.
- Confirm native amd64 macOS runner access and Apple signing credentials.
- Select and test the Linux libc baseline and user service prerequisites.
- Decide whether Linux bootstrap signatures are mandatory initially.
- Confirm repository release settings and the Homebrew tap owner.
- Pin the supported client versions and older-Pi extension compatibility.
