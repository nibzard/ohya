# ohya

**One memory for all your coding agents. Everywhere.**

ohya is a shared, searchable memory layer for AI coding agents. It captures
agent sessions automatically, synchronizes them across devices, and exposes
the entire history to any connected agent through read-only SQL and bounded annotation MCP tools.
One Go binary, one SQLite database, no cloud account.

Status: pre-MVP. Local Claude Code, Codex, and Pi agent ingestion runs end to
end: `ohya ingest` imports existing session history once, then polls
incrementally into one SQLite WAL database through the transactional write
path. `ohya mcp` serves
the read-only `sql` tool and the bounded atomic `annotate` tool over stdio.
Device sync runs end to end: the hub-side batch endpoint (`internal/hubsync`)
implements the model-checked sync contract — registration, bounded batches,
hub-side normalization with the local parsers, device ownership binding and
idempotent replay — as a library over any transport listener, and pairing is
implemented (`internal/registry`, T018): a hub-owned credential registry in
its own protected database, one-time expiring invitations, final revocation
with per-request checks and connection teardown, and `ohya devices
invite|list|revoke` for operators. The device uploader (`internal/uploader`,
T019) persists hub acknowledgments in the local checkpoints, retries after
timeouts and reconnects, catches up from retained files, and detects hub
cursor regression. Paired devices also reach the shared memory remotely
(T036, [ADR 0003](docs/adr/0003-remote-query-annotate.md)): query and
annotate frames on the same sync connection run through the same bounded
engine as the stdio tools, with the annotation writer derived from the
authenticated device and revocation checked on every frame. The commands are
wired (T034): `ohya serve` runs the hub with the self-hosted coordination
plane embedded in-process ([ADR 0002](docs/adr/0002-tailcat-transport.md));
`ohya join` pairs a device with one one-time code and uploads its sessions;
`ohya mcp --remote` bridges the shared tools through the joined device.
Forced-relay (DERP-only) validation is complete (T032); the macOS work
stays open (T046 runtime verification, T040/T045 distribution). See
[MANIFESTO.md](MANIFESTO.md) for
engineering principles, [PRODUCT.md](PRODUCT.md) for the product vision,
[ARCHITECTURE.md](ARCHITECTURE.md) for system design, and [DB.md](DB.md)
for the database plan.

## Use

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
```

`ohya ingest` watches `~/.claude/projects` (override with `--claude-root`),
the Codex sessions directory, `$CODEX_HOME/sessions` or
`~/.codex/sessions` (override with `--codex-root`), and the Pi agent
sessions root, `$PI_CODING_AGENT_SESSION_DIR`, `$PI_CODING_AGENT_DIR/sessions`
or `~/.pi/agent/sessions` (override with `--pi-root`). It stores everything in
`~/.ohya/hub.db` (override with `--db`; the state directory defaults to the
database directory and can be set with `--state`). A default directory that
does not exist holds no sessions, so an agent you do not use costs nothing.
A directory you pass must exist, and an empty value disables that agent.
`--max-record-bytes` (default 1 MiB) bounds one record's JSON text — also a
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
([ADR 0002](docs/adr/0002-tailcat-transport.md)). It also enrolls the hub's
own durable transport node (`tailcat-hub/`) and serves the sync endpoint on
the encrypted tailnet. Flags override the addresses: `--control-listen`
(TCP), `--stun-listen` (UDP), `--tailnet-port`, and `--control-url`, the
URL devices reach the coordination plane at. The defaults bind loopback for
the single-machine deployment; binding every interface requires
`--control-url`, because serve cannot guess the address devices should use.

`ohya join CODE` pairs a device and uploads its sessions. The code comes
from `ohya devices invite` on the hub and carries two halves: the one-time
registry invitation and a single-use coordinator preauth key. Each half is
consumable exactly once within the invitation's lifetime. Join enrolls the
transport node (`tailcat/` in the state directory), pairs the device
identity over the tailnet, and then watches the same agent roots as
`ohya ingest` (same `--claude-root`, `--codex-root`, `--pi-root` flags;
`--control` names the hub's coordination URL; `--state` defaults to
`~/.ohya/join`). Give it a state directory dedicated to the hub: its
checkpoints hold that hub's acknowledgments, not local ingestion's. Revoke
a device with `ohya devices revoke` on the hub; its connections close and
its sources stay bound to the revoked identity.

`ohya mcp --remote` serves the same `sql` and `annotate` tools over stdio,
but executes them on the hub through the joined device: `--remote` selects
the joined hub, `--hub-addr` its address inside the tailnet (default
`ohya-hub.tail.ohya:9027`), `--control` the coordination URL, `--state`
the join state directory. The hub derives the annotation writer from the
authenticated device, so a caller cannot choose it. A transport failure
drops the connection; the next call reconnects and retries once.

The first pass imports existing session files; later polls read only the
bytes past each checkpoint. All writes commit through one transactional
write path, so a crash between commit and checkpoint replays safely on
restart and inserts nothing twice. A malformed record stops its source at
that offset with a diagnostic and never skips bytes; other sources continue.
The diagnostic states the remedy: repair the file at that offset, or — when
the record exceeds `--max-record-bytes` — rerun with a larger limit, which
resumes at the stop offset ([ADR 0004](docs/adr/0004-stopped-source-recovery.md)).
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
`ohya rederive` (same `--db` flag): it re-derives stale rows from the stored
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
device's writer ID. To connect an agent, register the command, for example
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

## Scope

### MVP (phase 1)

- Ingest Claude Code, Codex, and Pi agent session logs (macOS, polling) into
  one hub-owned SQLite WAL database with FTS5/BM25 search.
- Read-only `sql(query)` and bounded `annotate(operations)` MCP tools.
  Agents can persist tags, classifications and security-review findings without
  modifying original transcript records.
- Isolated Tailcat transport adapter for encrypted device-to-hub sync. The
  feasibility spike is complete ([ADR 0002](docs/adr/0002-tailcat-transport.md)),
  the hub sync endpoint (`internal/hubsync`) commits device batches
  through the transactional write path, and device pairing with revocation
  is implemented (`internal/registry`), and so is the device uploader
  (`internal/uploader`); `ohya serve`, `ohya join` and `ohya mcp --remote`
  wire them into commands with the coordination plane embedded in serve.

### Phase 2

- OpenCode as an ingestion source, pending a parser spike for its
  per-message JSON storage (enumerated files, not append-only JSONL).

### Explicitly deferred

Built-in classification, embeddings, vector search, filesystem notifications, extra
platforms, and any third-party AI processing. See MANIFESTO.md (YAGNI).

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for build requirements and checks,
[BACKLOG.md](BACKLOG.md) for implementation order, the
[dependency decisions](docs/adr/0001-dependencies.md) for tradeoffs, and
[docs/performance.md](docs/performance.md) for the measured performance
limits (ingestion rate, query latency, database size, memory) and the
benchmarks behind them. The
Tailcat transport spike lives in `cmd/tailcat-spike`; the hub sync endpoint
lives in `internal/hubsync`; the embedded coordination plane lives in
`internal/control`. The spike's measurements and
the selection decision are in
[ADR 0002](docs/adr/0002-tailcat-transport.md). The sync cursor and
acknowledgment protocol and the authorization, pairing, and revocation
rules are model-checked with TLA+/TLC in `tla/`
(`make tla`; see [tla/README.md](tla/README.md)). The memory verification
gate (`make memory-e2e`, `cmd/memory-e2e`) exercises the memory surface end
to end with real Claude Code and Codex clients before release claims: it
seeds an isolated hub with synthetic recorded decisions, and each client must
retrieve the evidence through the `sql` MCP tool, cite event IDs that resolve,
and answer a current-context case without querying memory.

## License

MIT — see [LICENSE](LICENSE).
