# ohya reference

This document holds the full command, tool, and setup reference. For an
overview and a quick start, see the [README](../README.md).

## Commands

```sh
make build
./bin/ohya serve               # run the hub: coordination, transport, sync endpoint
./bin/ohya devices invite      # issue a one-time join code (run on the hub)
./bin/ohya join <code>         # pair this device and upload its sessions
./bin/ohya ingest              # import history, then poll until stopped
./bin/ohya ingest --once       # one pass, then exit
./bin/ohya mcp                 # serve the sql and annotate MCP tools on stdio
./bin/ohya mcp --remote        # same tools, through the joined device
./bin/ohya devices list        # show paired devices and invitations
./bin/ohya devices revoke ID   # revoke a device, final for its identity
./bin/ohya rederive            # rebuild normalized rows after a parser upgrade
./bin/ohya doctor              # run read-only health checks and report findings
./bin/ohya version             # print the build, parser, and schema versions
```

Help is a request, not an error: `ohya`, `ohya --help`, `ohya -h`, and
`ohya <command> --help` (down to the devices subcommands) print the same
usage template — usage line, description, arguments, flags, examples — on
stdout and exit 0. A usage error — unknown subcommand, bad flag, missing
argument — prints one line on stderr that names the matching `--help` and
exits 2, the flag package convention; every other failure exits 1. One
command reports degradation through the exit code: `ohya ingest --once`
exits 3 when the pass completed but at least one source failed or stopped,
so a script or agent that checks the exit code does not read missing data
as success; the stdout summary carries the failed and stopped counts and
stderr keeps the per-source diagnostics. Flags
are spelled with two dashes (`--db`); the parser also accepts one dash. Pass
flags before positional arguments — `ohya join --state DIR CODE`, not
`ohya join CODE --state DIR` — because parsing stops at the first
positional. No
command prompts for input: ohya is non-interactive by design.

The commands that report facts also print machine-readable output:
`ingest --once`, `devices list`, `devices invite`, `devices revoke`,
`rederive`, `doctor`, and `version` take `--json` (text stays the default)
and print exactly one JSON object on stdout, for example

```json
{"schema_version":1,"command":"ohya devices list","data":{"devices":[],"invitations":[]},"error":null}
```

`data` carries the command's report and is `null` on failure; `error` is
`null` on success and otherwise holds a stable snake_case `code`, a
`message`, and a `hint` naming the remedy. Diagnostics stay on stderr, the
JSON never carries transcript content, and a join code appears in `data`
only. A degraded `ingest --once --json` pass still exits 3 and reports
through `data` (counts plus every stop's offset and remedy), not through
`error`. Usage errors keep the one-line text contract above — they print no
envelope. The envelope fields, the per-command `data` fields, and the error
codes are the contract of [ADR 0005](adr/0005-json-output.md);
`schema_version` changes only on breaking changes.

`ohya version` (alias: `ohya --version`) prints one line of space-separated
key=value pairs: `version`, `revision`, and `dirty` from the build metadata
`go build` embeds, the `go` version, `schema` — the newest embedded
migration — and each parser's output version (`parser.claude`,
`parser.codex`, `parser.pi`). A binary built outside a version-controlled
checkout reports `version=devel` and unknown revision and dirty state. The
command opens no database and needs no state. Compare a hub's line with the
local one before `ohya rederive`: a build refuses rows derived by a newer
parser, so the line tells operators and agents which binary, parser output,
and schema a hub runs.

`ohya doctor` runs read-only health checks for state the `sql` tool cannot
answer, because it sits outside the database or is tied to this binary: the
database schema against the binary's migrations, stored parser output
versions above the binary's (`ohya rederive` refuses those), the FTS5
integrity of `events_fts`, stopped sources in the local watch state with
their [ADR 0004](adr/0004-stopped-source-recovery.md) remedy, the
device identity in the state directory, and — when that directory holds a
pairing credential — whether the coordination URL answers within a short
timeout. Every finding prints a severity (`ok`, `warn`, `error`) and a
one-line remedy; `--json` prints the same findings in the
[ADR 0005](adr/0005-json-output.md) envelope. Doctor is read-only: it
opens the database read-only — never migrates, repairs, or re-derives —
creates no device identity, and prints no transcript content or secrets.
Exit code 0 means no finding is an error; warnings alone do not fail. It
takes the same `--db` and `--state` flags as `ingest`, `--control` for the
coordination URL to probe when paired, and `--max-record-bytes` for the stop
check — pass the bound ingestion last ran with. Check a joined device's own
state with `--state ~/.ohya/join` and its hub's coordination URL through
`--control`.

The stop check parses the first pending record at each source's
acknowledged cursor with the given bound and reports a stop only when the
parser stops exactly there. A source whose pending bytes parse is not
stopped; it only waits for the next pass. The coordination check passes
when the URL returns any HTTP response within 3 s, because the plane can
answer 404 on its bare URL.

`ohya ingest` watches `~/.claude/projects` (override with `--claude-root`),
the Codex sessions directory, `$CODEX_HOME/sessions` or
`~/.codex/sessions` (override with `--codex-root`), and the Pi agent
sessions root, `$PI_CODING_AGENT_SESSION_DIR`, `$PI_CODING_AGENT_DIR/sessions`
or `~/.pi/agent/sessions` (override with `--pi-root`). It stores everything in
`~/.ohya/hub.db` (override with `--db`; the state directory defaults to the
database directory and can be set with `--state`). A default directory that
does not exist holds no sessions, so an agent you do not use costs nothing.
A directory you pass must exist, and an empty value disables that agent.
Every regular `*.jsonl` file under a root is a session file; spilled tool
output and memory files use other extensions. A replaced or truncated file
starts a new source that re-imports from byte zero, and ingest prints one
line about it; new files stay silent.
`--max-record-bytes` (default 32 MiB) bounds one record's JSON text — also a
memory bound, since one record is held in memory up to it.
The state directory holds `device.json`, the stable device identity,
`credential.json`, the owner-only hub pairing credential, and one
checkpoint file per agent (`sources/claude-code.json`,
`sources/codex.json`, `sources/pi.json`) with the acknowledged byte cursor
of every watched file.

`ohya serve` runs the hub side of device sync until it receives SIGINT or
SIGTERM. It provisions the self-hosted coordination plane in-process —
own SQLite database under `control/`, a generated configuration, and an
embedded DERP relay — with no external service and no second admin surface
([ADR 0002](adr/0002-tailcat-transport.md)). It also enrolls the hub's
own durable transport node (`tailcat-hub/`) and serves the sync endpoint on
the encrypted tailnet. Flags override the addresses: `--control-listen`
(TCP), `--stun-listen` (UDP), `--tailnet-port`, and `--control-url`, the
URL devices reach the coordination plane at. The defaults bind loopback for
the single-machine deployment; binding every interface requires
`--control-url`, because serve cannot guess the address devices should use.
`--tls-cert` and `--tls-key` serve the coordination plane and its embedded
DERP over TLS; the default `--control-url` becomes https, and every device
must trust the certificate's CA. Cross-machine hubs need this for the
relay path: the tailscale client never falls back to a plain-HTTP DERP
relay ([ADR 0002](adr/0002-tailcat-transport.md)). A hub already on a
Tailscale tailnet can bind its Tailscale address and use a `tailscale cert`
certificate:

```sh
ohya serve --control-listen 100.x.y.z:9417 --stun-listen 100.x.y.z:3478 \
  --tls-cert cert.pem --tls-key key.pem \
  --control-url https://hub.example.ts.net:9417
```

`ohya join CODE` pairs a device and uploads its sessions. The code comes
from `ohya devices invite` on the hub and carries two halves: the one-time
registry invitation and a single-use coordinator preauth key. Each half is
consumable exactly once within the invitation's lifetime. Join enrolls the
transport node (`tailcat/` in the state directory), pairs the device
identity over the tailnet, checks the returned device ID, and then watches
the same agent roots as
`ohya ingest` (same `--claude-root`, `--codex-root`, `--pi-root` flags;
`--control` names the hub's coordination URL; `--state` defaults to
`~/.ohya/join`). Give it a state directory dedicated to the hub: its
checkpoints hold that hub's acknowledgments, not local ingestion's. Revoke
a device with `ohya devices revoke` on the hub; its connections close and
its sources stay bound to the revoked identity. Hub-side commands (`serve`,
`devices invite`, `devices list`, `devices revoke`) take `--state` and not
`--db`: the device registry and the coordination plane live in the state
directory, beside the hub database. `serve` defaults its state directory to
the `--db` directory, so the single-machine deployment needs one path.

`ohya mcp --remote` serves the same `sql` and `annotate` tools over stdio,
but executes them on the hub through the joined device: `--remote` selects
the joined hub, `--hub-addr` its address inside the tailnet (default
`ohya-hub.tail.ohya:9027`), `--control` the coordination URL, `--state`
the join state directory. The hub derives the annotation writer from the
authenticated device, so a caller cannot choose it. A transport failure
drops the connection; the next call reconnects and retries once.

`join` and all remote MCP processes with the same state directory share one
transport node. The first process to dial owns it; other processes use its
authenticated loopback proxy. When the owner stops, another process takes
over on its next dial. Remote MCP works without a running uploader. All
processes must use the same `--control` URL. The state directory must be on
a local filesystem and must not be shared between machines. When upgrading
from a version without transport sharing, stop all old `join` and remote
MCP processes first. See [ADR 0006](adr/0006-shared-device-transport.md).

The first pass imports existing session files; later polls read only the
bytes past each checkpoint. All writes commit through one transactional
write path, so a crash between commit and checkpoint replays safely on
restart and inserts nothing twice. A malformed record stops its source at
that offset with a diagnostic and never skips bytes; other sources continue,
and the `--once` pass exits 3 (completed, degraded) with the stopped count
in its summary.
The diagnostic states the remedy: repair the file at that offset, or — when
the record exceeds `--max-record-bytes` — rerun with a larger limit, which
resumes at the stop offset ([ADR 0004](adr/0004-stopped-source-recovery.md)).
A replaced or truncated file starts a new source and re-imports history —
the run reports it instead of staying silent. Hardlinked copies inside the
watched tree share one file identity and are watched once; the checkpoint
follows the surviving link when others disappear.

A Codex thread can span several rollout files. Each file is its own source,
and all of them join the thread's one session. A page does not copy the
earlier file's records: after a rollback it re-enumerates the same ordinals
with new records, so the files disagree at every shared ordinal. The write
path deduplicates by `(session, ordinal)` plus content — a byte-identical
copy keeps its original text in `raw_json`, `duplicate_of` names the first
stored copy, its `text` stays empty, and it adds nothing to the session
counts — while a differing record at a known ordinal is a distinct record
that stays searchable and counts on its own. Search therefore returns each
distinct record once. Use `WHERE duplicate_of IS NULL` to read a thread
without byte-identical replays.

A Pi session is one file named by its header: the session id never spans
files, and the directory name (an encoding of the working directory) is not
identity. Sessions from failed model calls are partial by design — the
agent flushes every entry as it goes — and their all-zero usage adds
nothing to the session totals. Each assistant response carries its own
usage, so session token totals sum straight over responses.

Every stored row also names the parser output version that derived its
normalized fields (`events.parser_version`), separate from the schema
version. When an ohya upgrade changes what a parser derives, run
`ohya rederive` (same `--db` flag). It has no `--state` flag because it
uses only the database, without device identity, pairing credentials, or
source checkpoints. Run it on the hub; use `--db` to select its database.
It re-derives stale rows from the stored
raw records — inside the hub, with no access to the original files — in one
transaction, recomputing sessions and token totals and rebuilding the search
index. Event IDs and annotations survive; an up-to-date hub is a no-op, so
the command is safe to run any time.

`ohya mcp` serves the read-only `sql` tool to any MCP client over stdio
(same `--db` and `--state` flags). The tool runs exactly one statement per
call — `SELECT` and read-only `PRAGMA` only — on a restricted read-only
connection guarded by a SQLite authorizer. Writes, schema changes,
`ATTACH`, extension loading, and state-changing pragmas are denied; queries
run under a 5 second deadline, return at most 500 rows and 256 KiB of row
JSON, and report truncation explicitly instead of silently. The tool
description publishes the schema, SQL examples, the limits, and this
device's writer ID, and steers agents to the `events_settled` view —
`events` without sessions active in the last 10 minutes — so a querying
agent does not read its own live session. To connect an agent, register
the command, for example
for Claude Code:

```json
{
  "mcpServers": {
    "ohya": {
      "command": "/absolute/path/to/bin/ohya",
      "args": ["mcp"]
    }
  }
}
```

On a joined device, register the same command with
`"args": ["mcp", "--remote"]` to query the shared history instead of a
local database.

The `annotate` tool lets an agent persist its own interpretations — tags,
classifications, security-review findings — as JSON values on existing
events, without modifying any original transcript record. Each operation
upserts or deletes one annotation, identified by `(event_id, namespace, key,
writer_id)`. The hub assigns the writer from the device identity; a caller
cannot choose it. A batch of at most 100 operations commits atomically or
not at all, values are valid JSON of at most 64 KiB stored compacted, and
the result reports `applied` and `unchanged` counts. Identical retries
change nothing, not even timestamps. Agents query annotations through the
`sql` tool like any other table.

## Setup and installer

The integrated installer is under development; see
[the installer plan](installer-plan.md). The local setup engine is
available from source builds:

```sh
./bin/ohya setup --dry-run --json
./bin/ohya setup
./bin/ohya uninstall
```

Setup configures a user ingestion service and native MCP entries for installed
agents. Use `--agents claude,codex` to select clients, `--no-service` for an
existing process manager, and `--no-agents` to skip client changes. Agent
entries and service files use ownership receipts. Uninstall preserves memory,
credentials, sessions, and entries edited after setup.

Pi versions with native MCP use their user MCP settings. Pi 0.84.2 uses an
owned compatibility extension; other versions without native MCP are reported
as unsupported until verified. Remote
setup accepts `--join-code-env` and saves the hub connection for reruns. The download bootstrap exists
as `install.sh`, but no release assets are published yet. Existing agent sessions need a reload or a new session after
registration. Native service verification remains a release gate.

Setup keeps private copies of changed client files beside each client receipt
under `STATE/setup/clients/*.json.backups/`. Each copy retains the original
bytes. Uninstall preserves these copies. Restore only the needed settings;
copying an old file over a current client file can remove later user edits.

Setup saves your agent selection, session roots, client home overrides, and
service and PATH choices. Reruns reuse them. Explicit flags override saved
choices. For example, use `--agents auto` to resume automatic discovery, or
`--no-service=false` to enable the service after a previous skipped setup.
An empty root still disables that source. Provider secrets are never saved.

Before local setup restarts ingestion or opens an existing database whose
schema is older or newer than this build, it saves an owner-only SQLite
recovery copy under `STATE/setup/backups/recovery-*/hub.db`. When the schema
already matches, setup reports `backup: not needed` and makes no copy.
The copy includes committed WAL data. Setup stops if it cannot make the copy.
Setup keeps the two newest recovery copies; uninstall preserves them. A database copy is not
a complete backup of device state and checkpoints. Stop all writers and
restore matching state as a set. Never combine an older database with newer
acknowledgments. See [the operations guide](release-operations.md) before
a writable restore. A binary downgrade alone is not recovery.

Setup adds an owned PATH block for Bash, Zsh, and POSIX shells, or an owned Fish
configuration file. Use `--no-modify-path` to leave shell files unchanged.
Service and agent commands use absolute executable paths immediately. A newly
opened shell reads the PATH change; the installer cannot change its parent
shell environment. Uninstall removes only unchanged owned blocks and keeps
later user settings.

The MCP server supplies memory guidance when a client connects. It teaches
selective recall, bounded queries, and event citations. It treats transcript
content as data and leaves your agent instruction files and permissions intact.

For a joined device, set a one-time invitation in an environment variable and
pass its name to setup:

```sh
./bin/ohya setup --join-code-env OHYA_JOIN_CODE --control https://your-hub.example
```

Setup pairs once and configures the uploader and agents for the remote hub.
The environment variable is consumed before child processes start. The code
is not stored in setup receipts or service files. Subsequent `ohya setup` runs
reuse the saved connection and credential. Local memory stays separate from
join checkpoints. Use `--local` to return to local memory. A join state cannot
be moved to a different hub; use a new setup state directory for that hub.

The Pi compatibility extension uses the same `sql` and `annotate` MCP tools
through a bounded subprocess bridge. Requests go through stdin, not process
arguments. It needs no npm package download. Setup removes its owned extension
when switching to native Pi MCP. User edits to either integration are preserved.
To repeat the native Pi loader test without model access, set
`OHYA_PI_PACKAGE` to the installed Pi package directory and `OHYA_TEST_BINARY`
to a built ohya executable, then run
`go test -tags sqlite_fts5 ./internal/setup -run TestPiExtensionThroughInstalledLoader`.

`ohya doctor` also inspects owned services and agent integrations. It reports
changed or missing entries without modifying them. For remote setup, it follows
the saved join state and coordination URL. It checks uploader checkpoints
without claiming to reproduce the hub's parser decisions locally.

Release preparation is documented in [release and recovery operations](release-operations.md).
The manual candidate workflow builds native archives, tests downloaded binaries,
and exercises a real user service. It publishes no releases. Candidate packaging
and formula generation are available under `scripts/`; no Homebrew tap exists yet.

## Code map

See [CONTRIBUTING.md](../CONTRIBUTING.md) for build requirements and checks,
[BACKLOG.md](../BACKLOG.md) for implementation order, the
[dependency decisions](adr/0001-dependencies.md) for tradeoffs, and
[docs/performance.md](performance.md) for the measured performance
limits (ingestion rate, query latency, database size, memory) and the
benchmarks behind them. The
Tailcat transport spike lives in `cmd/tailcat-spike`; the hub sync endpoint
lives in `internal/hubsync`; the embedded coordination plane lives in
`internal/control`. The spike's measurements and
the selection decision are in
[ADR 0002](adr/0002-tailcat-transport.md). The sync cursor and
acknowledgment protocol and the authorization, pairing, and revocation
rules are model-checked with TLA+/TLC in `tla/`
(`make tla`; see [tla/README.md](../tla/README.md)). The memory verification
gate (`make memory-e2e`, `cmd/memory-e2e`) exercises the memory surface end
to end with real Claude Code and Codex clients before release claims: it
seeds an isolated hub with synthetic recorded decisions, and each client must
retrieve the evidence through the `sql` MCP tool, cite event IDs that resolve,
and answer a current-context case without querying memory.
