# ohya reference

This document holds the full command, tool, and setup reference. For an
overview and a quick start, see the [README](../README.md).

## Commands

```sh
make build
./bin/ohya setup --hub          # local setup plus the serve service, a relay check, and a join code
./bin/ohya serve               # run the hub: transport, sync endpoint, remote tools
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
./bin/ohya status              # runtime state from published local snapshots
./bin/ohya metrics             # counter rates and durations over a time window
./bin/ohya usage               # report how agents use the sql and annotate tools
./bin/ohya version             # print the build, parser, and schema versions
```

Help is a request, not an error: `ohya`, `ohya --help`, `ohya -h`, and
`ohya <command> --help` (down to the devices subcommands) print the same
usage template — usage line, description, arguments, flags, examples — on
stdout and exit 0. A usage error — unknown subcommand, bad flag, missing
argument — prints one line on stderr that names the matching `--help` and
exits 2, the flag package convention; every other failure exits 1. Three
commands report degradation through the exit code: `ohya ingest --once`
exits 3 when the pass completed but at least one source failed or stopped,
so a script or agent that checks the exit code does not read missing data
as success; the stdout summary carries the failed and stopped counts and
stderr keeps the per-source diagnostics. `ohya status` and `ohya metrics`
also exit 3 when the result is valid but partial, stale, or degraded —
a missing publisher, a stale snapshot, a broken sample, or a window the
history does not fully cover — while the JSON stays parseable with data
present and no top-level command error. Flags
are spelled with two dashes (`--db`); the parser also accepts one dash.
Flags may appear before or after positional arguments:
`ohya join --state DIR CODE` and `ohya join CODE --state DIR` both parse.
A bare `--` ends flag parsing; every argument after it is positional. No
command prompts for input: ohya is non-interactive by design.

The commands that report facts also print machine-readable output:
`ingest --once`, `devices list`, `devices invite`, `devices revoke`,
`rederive`, `doctor`, `usage`, and `version` take `--json` (text stays the
default)
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
integrity of `events_fts` with `--deep`, stopped sources in the local watch state with
their [ADR 0004](adr/0004-stopped-source-recovery.md) remedy, the
device identity in the state directory, the saved hub address when that
directory holds a pairing credential, whether a paired hub answers and
over which transport path, and the hub identity when it holds
an owned hub service.
Every finding prints a severity (`ok`, `warn`, `error`) and a
one-line remedy; `--json` prints the same findings in the
[ADR 0005](adr/0005-json-output.md) envelope. Doctor is read-only: it
opens the database read-only — never migrates, repairs, or re-derives —
creates no device identity, and prints no transcript content or secrets.
The `--deep` FTS5 check copies the database into memory, so its time and
memory use grow with the database; without `--deep`, doctor reports the
check as skipped.
Exit code 0 means no finding is an error; warnings alone do not fail. It
takes the same `--db` and `--state` flags as `ingest`, and
`--max-record-bytes` for the stop check — pass the bound ingestion last
ran with. Check a joined device's own state with `--state ~/.ohya/join`.

The stop check parses the first pending record at each source's
acknowledged cursor with the given bound and reports a stop only when the
parser stops exactly there. A source whose pending bytes parse is not
stopped; it only waits for the next pass. The join-state check never
prints the hub address — it is a secret because it holds the pre-shared
key — and rejects an address that is not a valid tailcat address.

On a paired device — a credential and a valid saved hub address — doctor
also dials the hub and adds two checks. `hub_transport` sends disco pings
(`tailcat.Client.DiscoPing`) and reports whether the hub answers, the path
type (`direct` or `relay`), the relay region on a relay path, and the
round-trip time; a hub that does not answer is a warning naming whether
`ohya serve` runs and outbound HTTPS. `hub_protocol` runs one `SELECT 1`
through the sync protocol and reports whether this build's sync protocol
version is compatible with the hub; a version mismatch is an error naming
both versions and the upgrade command, and a refused credential names
revocation or a different hub. In `--json`, a `transport` object carries
the same facts (`hub_answers`, `path`, `relay_region`, `round_trip_ms`,
`protocol_version`, `protocol_compatible`); the object appears only on a
paired device, and a value the library did not report is `unknown` or
`null`, never invented. An unreachable hub makes the whole probe take up
to 45 seconds.

`ohya ingest` watches `~/.claude/projects` (override with `--claude-root`),
two Codex directories, `$CODEX_HOME/sessions` and
`$CODEX_HOME/archived_sessions`, or `~/.codex/sessions` and
`~/.codex/archived_sessions` (`--codex-root` overrides the sessions
directory; the archived root is watched in addition), and the Pi agent
sessions root, `$PI_CODING_AGENT_SESSION_DIR`, `$PI_CODING_AGENT_DIR/sessions`
or `~/.pi/agent/sessions` (override with `--pi-root`). Codex moves ended
rollouts into `archived_sessions`, so both directories are default roots;
`ohya join` watches the same roots. It stores everything in
`~/.ohya/hub.db` (override with `--db`; the state directory defaults to the
database directory and can be set with `--state`). A default directory that
does not exist holds no sessions, so an agent you do not use costs nothing.
A directory you pass must exist, and an empty value disables that agent.
Every regular `*.jsonl` file under a root is a session file; spilled tool
output and memory files use other extensions. A replaced or truncated file
starts a new source that re-imports from byte zero, and ingest prints one
line about it; new files stay silent. The watcher follows moves by file
identity, so a rollout moved between the two Codex roots keeps its
checkpoint and continues where it left off. A watched file that stops being
statable — deleted, or moved to a place no root watches — prints one
diagnostic line per disappearance and keeps its checkpoint; it resumes if
the file returns, and appends written to the new place stay uncaptured
until that place is a root.
`--max-record-bytes` (default 32 MiB) bounds one record's JSON text — also a
memory bound, since one record is held in memory up to it.
The state directory holds `device.json`, the stable device identity,
`credential.json`, the owner-only hub pairing credential, and one
checkpoint file per agent (`sources/claude-code.json`,
`sources/codex.json`, `sources/pi.json`) with the acknowledged byte cursor
of every watched file.

`ohya serve` runs the hub side of device sync until it receives SIGINT or
SIGTERM. On its first start it creates the Tailcat hub identity — node key,
pre-shared key, relay region — and saves it owner-only under `tailcat-hub/`
in the state directory, so the hub's address stays the same across
restarts. The sync endpoint listens on TCP port 9027 inside the Tailcat
network stack and opens no port on the host: devices meet the hub through a
DERP relay (outbound HTTPS, TCP 443, and for a direct path outbound UDP)
and negotiate a direct encrypted path when NAT traversal succeeds
([ADR 0011](adr/0011-tailcat-address-transport.md)). The address is a
secret — it carries the pre-shared key — so serve prints the relay region
and the hub's public node key, never the address. Serve records each
device's last transport path (`direct` or `relay`) and last sync time in
the device registry when a device connects and at least every 30 seconds
while it stays connected; `ohya devices list` reports both. A value the
transport library does not report appears as `unknown`. Flags: `--db` for
the hub database, `--state` for the state directory (default: the database
directory).

```sh
ohya serve --db /srv/ohya/hub.db --state /srv/ohya
```

`ohya join CODE` pairs a device and uploads its sessions. The code comes
from `ohya devices invite` on the hub and has the form
`ohya1.<invitation>.<tailcat-address>`: the invitation admits exactly one
device before it expires (`--ttl`, default 15 minutes), and the address
holds the hub's pre-shared key, so the whole code is a secret — do not put
it in shell history or tickets. The previous
`<invitation>:<preauth-key>` format is refused with a message that names
the `ohya1.` format. Join dials the hub on its own (a fresh transport key
per process; no shared node), pairs the device identity, checks the
returned device ID, saves the hub address next to `credential.json` in the
join state, and then watches the same agent roots as `ohya ingest` (same
`--claude-root`, `--codex-root`, `--pi-root` flags; `--state` defaults to
`~/.ohya/join`). A device that already holds a credential sends its hello
to the address in the new code first: when the hub accepts, the device
keeps its identity, credential, and checkpoints, and the invitation stays
unused; when the hub rejects the credential — revoked, or a different hub —
join fails with the reason and never pairs as a new device
([ADR 0012](adr/0012-two-step-multi-machine-setup.md)). Give it a state
directory dedicated to the hub: its checkpoints hold that hub's
acknowledgments, not local ingestion's. After pairing, join reports the
measured path to the hub — path type, relay region, round-trip time in
milliseconds, both read from the transport library's disco ping, and this
build's sync protocol version — plus the upload backlog in bytes across
the watched roots; a value the library does not report is printed as
unknown. While a backlog of at least 1 MiB exists, the uploader reports
bytes acknowledged, bytes left, the current rate, and an estimated finish
time on stderr every 10 seconds, and publishes the same four values as
metrics (`upload_backlog_*`, labeled by agent) for `ohya status`. Revoke a
device with `ohya devices revoke` on the hub; its connections close and
its sources stay bound to the revoked identity. Hub-side commands (`serve`,
`devices invite`, `devices list`, `devices revoke`) take `--state` and not
`--db`: the device registry and the hub identity live in the state
directory, beside the hub database.

`ohya mcp --remote` serves the same `sql` and `annotate` tools over stdio,
but executes them on the hub through the joined device: `--remote` selects
the joined hub and `--state` names the join state directory, which holds
the device identity, pairing credential, and the saved hub address. The hub
derives the annotation writer from the authenticated device, so a caller
cannot choose it. A transport failure drops the connection; the next call
reconnects and retries once.

`join` and every remote MCP process dial the hub independently with their
own transport key — there is no shared node and no owner lock, so any
number of uploaders and remote MCP processes with the same state directory
run side by side, and one stopping never disturbs the others
([ADR 0011](adr/0011-tailcat-address-transport.md)). The state directory
must be on a local filesystem and must not be shared between machines.

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

### Usage records

Every `sql` and `annotate` call writes one usage record to
`<state>/observe/<instance>/usage.jsonl` — one record on the machine that
runs the call, and a second, client-side record on the machine that asked
for it. A joined device's remote calls are recorded by the hub, with the
calling device named as `device_id` and the client as `unknown` because
the sync frame carries no client name; `ohya mcp --remote` also records
its own client-side outcome under the join state directory's `observe/`
tree, keeping the MCP client name and version bucket, `path` `remote`, the
device identity, the client-observed total time in `phase_ms.total_ms`,
the returned row count, and the outcome as the client saw it (a hub
refusal maps to `busy` or `rejected`, a lost transport to `sql_error`,
its own deadline to `timeout`). Hub-only facts — phase breakdown, result
bytes, `sqlite_code`, column reads, functions, the FTS flag, and annotate
operation counts — stay zero in the device-side record; `ohya usage
--state <join-state>` reads device-side records the same way it reads
server records, and joining the two across machines needs the remote
aggregates of [plan 001](../plans/001-runtime-observability.md) milestone
5. Each record holds the tool, the MCP client name and version bucket,
the call path (`local` or `remote`), an outcome code (`ok`, `timeout`,
`busy`, `sql_error`, `rejected`, `truncated_rows`, `truncated_bytes`,
`cancelled`), the primary SQLite result code, phase durations, returned
rows and bytes, the `table.column` reads and SQL functions the authorizer
saw — normalized against the fixed schema catalogue, everything else
folded into `other` — an FTS flag, and for `annotate` the operation
counts. Records carry per-field export classes
([ADR 0010](adr/0010-usage-export-boundary.md)); they never hold SQL text,
literals, paths, transcript content, annotation values, or error messages,
on either side of a remote call. The writer is a bounded asynchronous
queue: full queues drop and count records rather than block a call, files
rotate at 8 MiB with two rotated files kept, directories are `0700` and
files `0600`, and nothing is written on the query path itself.

### The usage report

`ohya usage` reports how agents use the `sql` and `annotate` tools. It
combines two evidence sources: the server usage records above, when the
`--state` directory holds them, and transcript evidence already committed in
the hub. Transcript evidence covers calls the server records cannot see —
calls made before instrumentation, and the latency and outcome the agent
itself observed. For Claude Code it pairs `mcp__ohya__sql` and
`mcp__ohya__annotate` `tool_use` blocks with their `tool_result` blocks,
found through `events_fts`. For Codex it reads
`item_completed` records that carry an `McpToolCall` item for the `ohya`
server and the older `mcp_tool_call_end` records. A call counts as a
timeout when the result is an error that names the execution deadline.

The report lists calls per agent, tool, device, and ISO week; outcomes;
agent-observed latency and result sizes; how many `sql` queries read
`raw_json` columns; and, for Claude Code, sessions where the tools were
available but never called (prompt snapshots and deferred tool deltas list
the tools; Codex transcripts record no availability).

The report is deterministic and bounded. It runs under a deadline
(`--deadline`, default 120 s) with caps on sessions and calls per agent; a
report that hits a bound or the deadline is marked partial and lists what
was skipped, and the command exits 1 only when it collected nothing. It
prints no SQL text, query text, or transcript excerpts: the JSON envelope
carries counts and aggregates only. Pass `--local-detail` to add one row
per call with the local query text; result text is never printed. The
window defaults to the last 30 days and ends now; `--since` and `--until`
move it (`--since` takes `30d` or a Go duration, `--until` an RFC 3339
time). The command takes the same `--db` and `--state` flags as `ingest`
and opens the database read-only.

## Local observability

Every long-running ohya process — `serve`, `join`, continuous `ingest`,
and `mcp` — publishes process-local observations under
`<state>/observe/<instance-id>/`, where the instance id is random per
process start. The directory holds `snapshot.json` (the current sample,
replaced atomically every ten seconds, at startup, and at clean shutdown),
`history.jsonl` (one aggregate sample per minute, retained 24 hours and at
most 16 MiB), `events.jsonl` with two rotated files (the structured
application logs below), and, for processes that serve the tools, the
usage records above. Directories are `0700`, files `0600`, and no
publication fsyncs: this data is disposable. Counters are cumulative
since process start and reset on restart; a failed publication is counted and retried on the next
tick and never blocks ingestion, syncing, or queries. On startup a
publisher removes only provably terminated instances of its own kind,
keeping at most eight per role and seven days of files, and never removes
a live instance. Pass `--observe off` to any long-running command to run
without publication for baseline measurements.

A snapshot older than thirty seconds is stale. A valid recent snapshot is
evidence of recent activity, not proof that the process is alive, so
liveness is probed separately; a pid that no process answers reads as
`dead`, and a pid held by some process with no fresh publication reads as
`unknown` — pid reuse and a stalled process are indistinguishable without
kernel start-time data.

### `ohya status`

`ohya status` reads the cached snapshots and the saved setup connection;
it never contacts the hub, starts a service, migrates a database, or
repairs files. It resolves the state directory the way doctor does —
`--state` wins, otherwise the database directory, switching to
`<state>/join` when `setup/connection.json` selects remote memory — and
`--role` and `--instance` narrow the report when several processes
publish. Output carries the CLI identity, topology (mode, state
directory, agent-root counts without paths, owned service loaded/running
facts), storage sizes with their observation time (a joined client never
reports its old local database as the hub), one section per instance with
freshness, liveness, source-state gauges, pending and upload backlog in
bytes, resource readings, and the raw counters, gauges, duration
histograms, and resources from the snapshot. Findings carry a stable
code, severity, evidence, and hint: `instance_terminated`,
`instance_stale`, `instance_unreadable`, `instance_unsupported_version`,
`no_instances`, `multiple_instances`, `pending_backlog`,
`upload_backlog`, `sources_stopped`, `discovery_truncated`,
`remote_status_unsupported`. Instances are never merged; a null value and
a zero value stay distinct. `--remote` reports the capability gap
honestly: this build cannot fetch hub-side aggregates yet (plan 001
milestone 5). Exit 0 means every selected instance is fresh with no
degraded component; exit 3 means the result is valid but partial, stale,
or degraded.

### `ohya metrics`

`ohya metrics --window 1m|5m|1h|24h` reads the bounded history samples,
not transcripts. Per instance it reports the sample count, observed and
requested durations, missing-minute gaps, counter deltas between the
first and last sample in the window, rates per second whose denominator
is the monotonic time the process actually ran, and duration histogram
deltas with p50/p95 bucket estimates — null below twenty observations.
Percentiles are never averaged between instances, and a window the
history does not cover is reported as partial rather than manufactured
from less data. Exit 3 marks a sample-less, gapped, or partial window.

### `ohya logs`

`ohya logs` reads the structured application logs every long-running
process writes to `<state>/observe/<instance-id>/events.jsonl` next to
its snapshots, plus the two rotated files beside it. Entries carry a
curated event code, an optional outcome, numeric counts, and a
correlation id at most; they never carry transcript text, SQL, source
paths, or raw error strings, because parsers, queries, and transports
can embed untrusted input in those. The writer coalesces repeated
warnings by event code while an identical entry is still queued and
reports the merge count, drops rather than blocks when its 256-entry
queue is full, bounds each line to 8 KiB, keeps 8 MiB per file with two
rotated files, and emits one activity summary per minute plus an idle
heartbeat at most every five minutes and a final summary on clean
shutdown. When writes fail (disk full, read-only path), the writer falls
back to one sanitized stderr notice per minute and keeps counting.

The command filters with `--since` (default 10m), `--level` (that
severity and above), `--component`, and `--limit` (default 100, maximum
1000, newest entries win). A finite report stops at 1 MiB of entries and
says which bound applied; oversize or unreadable lines are skipped and
counted, never printed. Logs are local only: the command never contacts
the hub. `--follow` requires exactly one selected instance (`--instance`
or `--role` disambiguates), replays the lookback window once, then
streams new entries, follows rotation without re-emitting the rotated
file, and exits 0 when cancelled. With `--follow --json` each entry is
one independently versioned envelope per line. Exit 3 marks a partial
report: no instances, skipped lines, or an unreadable instance.

### `ohya diagnose`

`ohya diagnose --output FILE` writes one JSON artifact with the local
status report, the 1m/5m/1h metric windows, up to 100 recent warning and
error log entries, the CLI build identity, the configured runtime
limits, and what this build can and cannot export. The artifact is
private (`0600`), never uploaded, capped at 2 MiB, and lists every
section dropped for that cap. It refuses to overwrite an existing file
without `--force` and rejects a symlink destination. The content is
assembled from already-sanitized pieces — snapshots, history samples,
and curated log entries — so transcripts, SQL text, annotation values,
invitations, credentials, environment dumps, private keys, full source
paths, and raw third-party errors never enter it; the boundary is pinned
by tests that seed such values around the state directory and assert
their absence. With `--remote` the artifact records the unsupported
hub-side fetch as a finding and still exports every local section, so a
partial artifact with the failure identified replaces an abort. Exit 0
means the artifact is complete; exit 3 means partial (for example the
remote gap or an unreadable local section).

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
entries and service files use ownership receipts. On Linux the service keeps
its stderr in the journal with rate limiting; on macOS the launchd template
owns a private stderr startup file under `<state>/logs/` that setup creates
and rotates at 8 MiB, so startup failures stay discoverable without an
unbounded stdout file. Application events go to the `observe/` log files read
by `ohya logs`; the service stderr files only carry what the process prints
before its own logging starts. Setup also prunes stale `observe/` instances
under the retention rules above and never touches a live instance. Uninstall
preserves memory, credentials, sessions, entries edited after setup, and the
diagnostic files: `observe/` and the stderr startup logs survive an uninstall.

Pi versions with native MCP use their user MCP settings. Pi 0.84.2 uses an
owned compatibility extension; other versions without native MCP are reported
as unsupported until verified. Remote
setup accepts `--join-code-env` and saves the hub connection for reruns. The download bootstrap exists
as `install.sh`, but no release assets are published yet. Existing agent sessions need a reload or a new session after
registration. Native service verification remains a release gate.

### Hub setup

`ohya setup --hub` ([ADR 0012](adr/0012-two-step-multi-machine-setup.md))
runs the whole local flow above and then adds the hub side: a second owned
user service running `ohya serve` (`ohya-serve.service` on systemd,
`com.nibzard.ohya.serve.plist` on launchd) with its own receipt and the
same ownership rules as the ingestion service. After the service starts,
setup waits for the Tailcat identity the serve process creates, checks
that the hub reaches its relay and answers a device dial, issues a one-time
join code (`--ttl`, default 15 minutes), and prints the device command:

```sh
curl -fsSL https://github.com/nibzard/ohya/releases/latest/download/install.sh | sh -s -- --join
```

In `--json` the code sits in one field marked secret
(`data.hub.code_secret`) next to its expiry, the relay region, the hub's
public node key, and the device command. `--hub` cannot be combined with
remote setup or `--no-service`, and a setup state bound to a remote hub
cannot become a hub. Reruns keep both services and the identity, and issue
a fresh code each time. Every hub-setup failure names its cause and one
next step:

- No relay reachable — the machine needs outbound HTTPS (TCP 443) to its
  DERP relay; allow it, then rerun `ohya setup --hub`.
- Hub not answering through its relay — check whether the
  `ohya serve` service runs, then rerun `ohya setup --hub`.
- The serve service never created the identity — check its logs, then
  rerun `ohya setup --hub`.

The installer's `--join` reads the join code from the terminal
(`/dev/tty`) without echo, so the code never enters shell history or the
screen. Without a terminal it requires `--join-code-env NAME` with the
code already in that variable. Under `--json` nothing prompts: a missing
code is a usage error. Agents use the same two flags with
`ohya setup` directly.

### Join failures

Each device-side failure names its cause and one next step:

- Expired or used invitation — ask the hub for a fresh code from
  `ohya devices invite`.
- Old `<invitation>:<preauth-key>` code format — refused with a message
  naming the `ohya1.` format and `ohya devices invite`.
- No relay reachable — the device needs outbound HTTPS (TCP 443); also
  check that `ohya serve` runs on the hub.
- Hub not answering through the relay — check that `ohya serve` runs on
  the hub.
- Protocol version mismatch — the message names both versions and the
  upgrade command; upgrade ohya on the older machine and rerun the join.
- Credential rejected on rebind — the device identity was revoked, or the
  code names a different hub; join never pairs such a device as new.

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
pass its name to setup, or let the installer's `--join` read it from the
terminal:

```sh
curl -fsSL <release>/install.sh | sh -s -- --join
./bin/ohya setup --join-code-env OHYA_JOIN_CODE
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
changed or missing entries without modifying them — the ingestion service and,
when present, the hub service. A state directory with an owned hub service
also reports whether the hub identity exists and names its public node key.
For remote setup, it follows
the saved join state (`credential.json` and `hub-address.json`). It checks
uploader checkpoints without claiming to reproduce the hub's parser decisions
locally.

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
benchmarks behind them. The Tailcat address transport lives in
`internal/transport/address`; the hub sync endpoint
lives in `internal/hubsync`. The transport selection decision is
[ADR 0011](adr/0011-tailcat-address-transport.md). The sync cursor and
acknowledgment protocol and the authorization, pairing, and revocation
rules are model-checked with TLA+/TLC in `tla/`
(`make tla`; see [tla/README.md](../tla/README.md)). The memory verification
gate (`make memory-e2e`, `cmd/memory-e2e`) exercises the memory surface end
to end with real Claude Code and Codex clients before release claims: it
seeds an isolated hub with synthetic recorded decisions, and each client must
retrieve the evidence through the `sql` MCP tool, cite event IDs that resolve,
and answer a current-context case without querying memory.
