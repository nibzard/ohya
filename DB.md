# ohya — Database plan

[001_initial.sql](internal/store/migrations/001_initial.sql) is the authoritative initial DDL. It defines
the ingestion, annotation and FTS5 tables; [002_session_token_usage.sql](internal/store/migrations/002_session_token_usage.sql)
adds the per-response token totals; [003_event_ordinal.sql](internal/store/migrations/003_event_ordinal.sql)
adds `events.ordinal` and `events.duplicate_of` for replay deduplication across source files;
[004_parser_output_version.sql](internal/store/migrations/004_parser_output_version.sql)
adds `events.parser_version`, the parser output version marker (see "Re-derivation");
[005_ordinal_dedup_by_content.sql](internal/store/migrations/005_ordinal_dedup_by_content.sql)
rekeys that deduplication by content (see the invariant below);
[006_events_settled_view.sql](internal/store/migrations/006_events_settled_view.sql)
creates `events_settled`, the view over sessions idle at least 10 minutes
(see "Settled events");
[007_workspaces.sql](internal/store/migrations/007_workspaces.sql)
adds `workspaces`, the device-observed repository of each session working
directory, and the `sessions (device_id, cwd)` index (see "Repository
derivation");
[008_purged_sources.sql](internal/store/migrations/008_purged_sources.sql)
adds `purged_sources`, the tombstones of owner-purged sources (see
"Source lifecycle" and [ADR 0013](docs/adr/0013-owner-scoped-session-purge.md));
[009_source_workspace.sql](internal/store/migrations/009_source_workspace.sql)
adds `workspace_path` to `sources` and `purged_sources`, the device-observed
workspace of a source (see "Repository derivation" and
[ADR 0018](docs/adr/0018-device-observed-source-workspace.md)). It also drops
`sources.file_identity`, which no write path filled, and adds the index
`events_source_parser_version` on `events(source_id, parser_version)`. The
`parser_versions` check of `ohya doctor` reads that index alone and no row of
`events`.
Do not duplicate the DDL here.
Databases ship with releases: v0.1.0 and v0.1.1 ship schema 6, v0.1.2 and
v0.2.0 ship schema 7, and v0.3.0, v0.4.0, v0.5.0, v0.5.1, v0.6.0, v0.6.1, v0.7.0, and v0.7.1 ship schema 8
([release-operations](docs/release-operations.md)), so databases at released schemas exist outside
this repository and can arrive for upgrade at any time. Migrations 001 and 002 were the pre-release
baseline; every schema change after a release arrives as a new migration file.

## Migrations

Migrations are versioned SQL files in `internal/store/migrations/NNN_name.sql`, embedded in the binary.
`store.Open` applies pending migrations in version order on the single writer before the database is used.
Each applied migration is recorded in `schema_migrations (version, name, checksum, applied_at_ms)`;
the runner owns that table, not a migration file. Migration files hold schema and data changes only.
Connection settings live in `internal/store` code, because SQLite cannot switch journal mode inside a
transaction and `PRAGMA foreign_keys` is a no-op there.

Upgrade policy:

1. Add a new `NNN_name.sql` with the next version number. Never edit, rename, renumber or delete a
   file that any database may already have applied.
2. Each pending migration and its `schema_migrations` row commit in one transaction. A failed migration
   rolls back completely; the next run resumes at the first pending version. The transaction takes the
   write lock first and reads `schema_migrations` again. Two processes that open the same database at
   the same time, such as the memory service and the hub service, then apply each migration once: the
   second process waits for the first and finds the migration recorded. `ohya setup` applies the
   pending migrations itself after the recovery point and before it starts the memory service
   ([ADR 0008](docs/adr/0008-integrated-setup.md), "Recovery points"), so that no migration runs
   inside a starting service. It does so only for a database whose recorded schema it can
   read; any other file is left to the service.
3. Every open verifies applied history. A checksum mismatch, a migration missing from the binary, or a
   new file numbered below the newest applied version is an error, never a silent upgrade.
4. Migration file names match `NNN_name.sql` with a unique three-digit version. `store.SchemaVersion`
   reports the newest embedded version without opening a database.
5. Databases ship with every release, so released schemas exist in the wild: v0.1.0 and v0.1.1
   ship schema 6; v0.1.2 and v0.2.0 ship schema 7; v0.3.0, v0.4.0, v0.5.0, v0.5.1, v0.6.0, v0.6.1, v0.7.0, and v0.7.1 ship schema 8. Test every new migration by upgrading a database from
   each released schema, not only by fresh creation. `internal/store/migrate_released_test.go`
   owns that test: it builds a database at each schema listed in `releasedSchemas` from the
   embedded migration bytes, seeds rows a released write path could have written, and upgrades
   through `store.Open` to the current version, so a migration numbered above every released
   schema still runs as an upgrade, never only as fresh creation. The same change that publishes
   a release adds it to `releasedSchemas` and to this item's list;
   `TestReleasedSchemasMatchPublishedTags` fails `make check` wherever the published tags are
   visible when the list falls behind or a shipped migration file no longer matches its
   released bytes.
6. Keep this file in step with the migration list in the same change, so a review can diff policy
   against DDL.

## Connections and durability

All writes — ingestion, annotations, migrations — go through the single writer connection that
`store.Open` configures with WAL journaling, `synchronous = FULL`, foreign keys and a busy timeout.
Readers get separate read-only connections (`mode=ro`) with foreign keys enabled. The settings apply
to every pooled connection, and `store.Open` verifies them before use. Open the writer first and keep
it open while readers run: a read-only connection cannot create the database or recover the
write-ahead log.

The busy timeout is 5 s. Each transaction of the store waits up to that time for the write lock
that another process holds, such as a backup tool, another service, or a migration. The writer
begins each transaction immediate (`_txlock=immediate`), so it takes the write lock before it
reads. A single write statement and a migration wait in the same way. A lock held for longer fails
the transaction with `database is locked`, and the caller retries: the hub makes three attempts in
about 15 s and sends no acknowledgment, and the ingest service reads the same bytes again on its
next pass. A context that ends does not shorten the wait. The plan of a purge (`PlanPurge`) reads
through a reader pool, so it takes no write lock. Driver parameters
with a leading underscore apply to every pooled connection; `mode=ro` reaches SQLite when it opens
the file. `store.OpenReadOnly` serves `ohya doctor`: it opens an existing hub database in WAL mode
with one read-only pool, applies no migrations, and has no writer.

An up-to-date database needs no write lock to open. The open waits only when a migration is
pending, because each migration begins immediate, or when SQLite answers a conflict between two
processes that open the same database at once without the busy timeout. `store.Open` repeats its
attempt on a busy error until one busy timeout has passed since the attempt that first failed,
and returns the error with a hint. `store.WaitForLock(report)` removes that limit: the open then
repeats its attempt until its context ends and calls `report` after a busy attempt, at most once
per busy timeout. `ohya mcp`, `ohya serve`, and the ingest service open with it. A one-shot
command and `ohya setup` open without it. A context that ends does not shorten the wait of one
attempt, so an open that is stopped during the wait returns up to one busy timeout later.

| Table | Meaning |
|---|---|
| `sessions` | Device/agent/native session identity and observed aggregates |
| `sources` | Watched file path, authoritative committed byte cursor, and device-observed workspace |
| `events` | One immutable original JSONL record and normalized searchable fields |
| `session_token_usage` | Token counts of one API response, keyed by session and message ID |
| `event_annotations` | Agent-written JSON values keyed by event, namespace, key and writer |
| `events_fts` | Search index over event text, maintained transactionally |
| `events_settled` | View over `events` holding only records of sessions idle at least 10 minutes |
| `workspaces` | Write-once repository observation for one session working directory on one device |
| `purged_sources` | Tombstone of one owner-purged source; refuses the write path until the owner re-admits it |

## Source lifecycle

A source moves through three states ([ADR 0013](docs/adr/0013-owner-scoped-session-purge.md)):

```text
registered --ohya purge --apply--> purged --ohya sources reallow--> registered (at the purged cursor)
```

- **registered**: the `sources` row exists and the write path accepts batches.
- **purged**: an owner purge deleted the session's rows — events,
  annotations, token totals, the sessions row, orphaned `workspaces` rows
  whose `(device_id, path)` no remaining session references, and the source
  row — and wrote a `purged_sources` tombstone carrying the source identity,
  device, agent, path, the authoritative committed cursor read inside the
  purge transaction, the purged session's native ID, the source's offset-0
  `first_native_id`, the source's `workspace_path` when it has one, the
  purge scope, and the timestamp. `purged_sources` is
  denied to the MCP `sql` tool by default (`EngineOptions.DenyTables`).
  Registration and batches for the tombstoned source ID are refused
  deterministically with the purge date and the `ohya sources reallow` remedy
  (`store.ErrSourcePurged`), for any device: the tombstone keeps the freed
  source ID from being re-registered, exactly as ownership did while the row
  lived.
- **registered again**: reallow deletes the tombstone and restores the
  source row with `cursor_bytes`, `first_native_id`, and `workspace_path`
  from the tombstone (so header-first parsers such as Codex and Pi can resume and re-derive past
  offset 0 after their offset-0 event was evicted). Because a running
  `ohya join` caches refused sources for its process lifetime, restart
  `ohya join` on the device after reallow; its probe finds the hub cursor at
  (or past) its checkpoint and resumes from there, while a below-cursor
  replay is refused by the existing write-path rule because no stored records
  sit below that cursor. Purged bytes never return automatically.

A purge plan selects real sessions (never the `ohya-synthetic:` companions,
which follow their source) and evicts every source holding the selected
sessions' events — each source holds at most one real session plus its own
source-scoped synthetic session (verified on the whole deployed hub, guarded
in both `PlanPurge` and `purgeOneSource`, and pinned by tests; see ADR 0013).
Selectors are `--session`, `--repo`, `--agent`, `--before`, and `--marked
<namespace.key>`: the marked selector takes the sessions whose events carry
an agent-written annotation mark under that namespace and key, every writer
included (see "Garbage audit" and
[ADR 0016](docs/adr/0016-agent-marked-purge-scope.md); the value splits at
the first dot, so give the namespace no dot). One transaction per source
(detaching cross-source `duplicate_of` references
before deleting the source's events) keeps the WAL bounded and reruns
resumable. Freed pages stay in the database file; there is no VACUUM.

Use Unix milliseconds; absent observations remain NULL. Ingestion JSON metadata contains
only deterministic observations, not inferred outcomes. Raw JSON preserves the
source record text; line terminators are transport framing. Offsets are byte
offsets in the original file, including framing when calculating the next cursor.

### Repository derivation

`sessions.repo` is the project key shared across devices. One project has a
different `cwd` on each device, so cross-device recall filters on `repo`.

`repo` comes from two stored inputs, in this order
([ADR 0009](docs/adr/0009-workspace-repository-observations.md)):

1. **The session's own records.** Codex derives it from
   `session_meta.git.repository_url` (embedded parent metas lend nothing).
   The first non-empty value in deterministic order wins, exactly like `cwd`
   and `branch`. Claude Code and Pi records carry no repository URL.
2. **The workspace observation for `(device_id, cwd)`.** It applies when the
   records carry no repository. The device that ran the session resolves its
   `cwd` against `.git` and sends the result. A `workspaces` row with a NULL
   `repo` records that the directory had no usable repository.

Ingestion, a new observation, and re-derivation apply the same rule, and all
of them read only stored rows. An observation fills `repo` only where it is
NULL. A record-carried URL arrives with the `cwd` in the same Codex
`session_meta`, so the two orders cannot disagree. Observations are
write-once: the first one for a `(device_id, path)` wins, and the hub stores
one only for a path that is the `cwd` of a session on that device.
`store.PendingWorkspaces` lists, per device and agent, the `cwd` values of
sessions with a NULL `repo` and no observation yet.

Every repository value, from records or from a device, is normalized to
`host/path` by `workspace.NormalizeRemote`: the `https://`, `http://`,
`ssh://` or `git://` scheme, all user information up to the last `@`, the
port of a URL with a scheme, and trailing `.git` and `/` are stripped, an
scp-style `git@host:path` separator becomes `/`, and the host is lowercased
while the path keeps its case. A host other than a DNS-style name or a
bracketed IPv6 address, an empty path, or a path with whitespace or control
characters yields no repository. The result normalizes to itself, and the
hub rejects an observation whose value does not.

**The workspace of a source** sets `cwd` when the records of a session carry
none ([ADR 0018](docs/adr/0018-device-observed-source-workspace.md)). The
rule: the `cwd` of a session is the first non-empty record `cwd`, else the
workspace of a source that holds the events of the session. Antigravity
records carry no directory, so its sessions depend on this input.

`sources.workspace_path` is write-once, and only the device that owns the
source sets it: with `store.SetSourceWorkspace` or with the
`sourceWorkspace` frame ([sync-protocol](docs/sync-protocol.md)). The first
path stays. The same path again is a no-op, and a different path is ignored.
Setting it fills `cwd`, and the repository of a known `workspaces` row, for
the sessions of the source that have no `cwd`. Ingestion and re-derivation
apply the rule again for later events, so a session that a later batch or a
rebuilt native ID creates also gets the workspace. A purge keeps the value
in the tombstone, and reallow restores it. `store.SourcesWithoutWorkspace`
lists, per device and agent, the sources that have no workspace.

Never decode the Claude Code project directory name, because it is lossy.
Parsers never read the filesystem: git discovery happens on the device, as
an observation, never inside derivation.

### Settled events

`events_settled` mirrors the `events` columns but hides records whose session
was active in the last 10 minutes (`last_event_at_ms` newer than that;
sessions without timestamps count as settled). It is the default surface the
`sql` tool's description steers agents to: a querying agent's own session is
the fresh one, and its transcript is untrusted text — excluding it keeps a
running agent from reading, and acting on, its own live context. The cutoff
is fixed at 10 minutes so the view needs no parameters and stays
deterministic for a given clock. `events` itself stays fully readable for
queries that deliberately want the live tail.

## Invariants

- Session IDs use an unambiguous encoding of `(device_id, agent, native_id)`:
  `agent + ":" + device_id + ":" + native_id`. Agent names and device IDs
  cannot contain a colon, so the first two separators always split the
  identity; a native ID may contain colons (synthetic IDs do).
- A source holds events of at most one real session, plus at most the
  synthetic session scoped to that source (`ohya-synthetic:<source-id>`).
  Verified against the whole deployed hub and pinned by tests, this is what
  lets a session purge always evict whole sources without touching another
  session's events ([ADR 0013](docs/adr/0013-owner-scoped-session-purge.md)).
- Source IDs survive restarts; replacement/truncation creates a new incarnation.
- `(source_id, source_offset)` deduplicates retransmission, not copied history.
- `(session_id, ordinal, raw content)` deduplicates records that another file
  of the same session replays byte-identically (Codex pages; see
  testdata/codex/README.md). The first stored copy has `duplicate_of` NULL; a
  byte-identical later copy is still inserted, so retry validation and
  original text stay intact, but it names the first copy in `duplicate_of`,
  has NULL `text` (so FTS indexes the record once), and adds nothing to the
  session aggregates or token totals. A copy with different bytes at the same
  `(session_id, ordinal)` re-enumerates the ordinal — a Codex page after a
  rollback holds a different record there — and is stored as another
  original: searchable, counted once, its per-response usage included, so one
  ordinal can name several distinct records. The partial index
  `events_session_ordinal` (migration 005) serves the lookup; the
  single-writer transaction and the re-derivation walk, which compare
  `raw_json`, keep one original per `(session_id, ordinal, content)`.
  Migration 003 enforced one original per `(session_id, ordinal)` alone and
  its header assumed pages byte-replay the original; verified 2026-11-14
  (T044), that assumption is wrong — at every colliding ordinal the two files
  hold different records, and the 003 rule suppressed real records (11
  per-response usage records were lost from token totals across the 7 corpus
  thread pairs), which is why 005 rekeys the rule by content. Duplicate marks
  written under 003 stay as stored until the hub is re-ingested; only a
  database written between 003 and 005 can hold them, and every release
  ships 005, so a database created by a released binary never does. Records
  without an ordinal (Claude Code, Codex before 0.147.0) are never
  deduplicated this way.
- A batch names the device that owns its source (`store.Batch.DeviceID`); the
  write path rejects a batch claiming another device, because ownership is
  fixed at registration. The sync endpoint passes the authenticated identity,
  never a caller-supplied one.
- Validate byte boundaries and conflicting retries before inserting; uniqueness
  alone does not prove payload equality or contiguous delivery.
- Insert events, update aggregates and advance the source cursor in one transaction.
- Count only newly inserted records. Timestamp bounds use observed min/max;
  missing timestamps do not overwrite known bounds. First prompt is selected by
  a deterministic source/order tie-break, not arrival order: the candidate with
  the smallest `(source_id, source_offset)` wins, and `sessions.metadata`
  records the winner under `firstPromptRef` so a later smaller candidate can
  replace an earlier arrival. The `cwd`, `branch` and `model` columns keep the
  first non-empty observation and are never overwritten.
- Token totals live in `session_token_usage`, one row per
  `(session_id, message_id)`. The Claude Code fixtures (see
  testdata/claude-code/README.md, "Usage fields") define the semantics:
  `message.usage` reports one API request and repeats identically on every
  record sharing `message.id`, so the write path counts each usage object once
  and sums the four unambiguous counts. Codex `token_usage_record.usage` is per
  `response_id` (the message ID here); `input_tokens` includes `cached_input_tokens`,
  so the adapter stores the difference as input and the cached part as cache reads.
  A usage whose cached count exceeds its input, or that holds a negative count,
  is contradictory and counts nothing.
  `cache_write_input_tokens` stays out until its relation to `input_tokens` is
  verified: it is 0 in every record of the full real corpus (checked
  2026-11-14 over all 22,209 usage records, versions 0.153.4 through
  0.169.0), so no sample confirms whether it is part of `input_tokens`; map
  it only after a non-zero sample appears. Pi assistant messages carry one
  usage object per response; the message ID is the `responseId`, or the
  entry id when the provider emits none (verified 2026-11-14 over 74 real
  sessions: `openai-completions` records never carry a responseId, and 4 of
  7 `anthropic-messages` records lack one). `input` excludes `cacheRead` and
  `cacheWrite` — in every verified record `input + cacheRead + cacheWrite +
  output = totalTokens`, with `cacheWrite` non-zero and strictly additive in
  7 anthropic-messages records — so the counts map directly, cache writes to
  the cache-creation count, and a usage whose parts do not sum to
  `totalTokens` counts nothing; `reasoning` (no non-zero sample) and
  `cacheWrite1h` (0 in the 3 records carrying it) stay out until their
  subset relations are verified. The first usage observed for a
  message ID wins; a later differing usage for the same message ID is ignored,
  never summed. Usage without a message ID is ambiguous and contributes
  nothing. Tool aggregates stay deferred until a query need justifies them.
- Events and normalized text change only through the explicit re-derivation
  operation below; no implicit updates hide behind the insert/delete triggers,
  and ingestion itself never updates a stored row.
- Foreign keys are enabled on every relevant connection. WAL and FULL synchronous
  durability apply to the writer. Verify these settings and FTS consistency in tests.

Authorization credentials live in a separate protected hub registry before remote
access is enabled. That registry (`internal/registry`, T018) is its own SQLite
database — `registry.db` in the owner-only state directory, WAL with
synchronous FULL, schema version pinned by `PRAGMA user_version`, secrets kept
only as SHA-256 digests — so this database's schema and its `sql` MCP tool
never see credentials. This schema covers transcript data and annotations, not
credentials or device-local checkpoints. See ARCHITECTURE.md for ownership and
sync semantics.

## Write path

`store.Ingest` commits one `Batch` in one transaction. It checks, before any insert:

- Each record spans exactly its JSON bytes plus one terminator byte, and the records are contiguous
  and in source order. Each record names a native session ID and a parser output version of at least 1.
- The batch starts at or behind the stored cursor. A batch that starts ahead of the cursor would skip
  bytes and is rejected.
- Records below the cursor replay the stored records byte for byte. Uniqueness alone does not prove
  payload equality, so a conflicting retry stops the source instead of hiding changed history.
- A stored record at or beyond the cursor contradicts the hub's own acknowledgments, so the batch
  stops instead of adding to it.

A rejected batch changes nothing, and its error carries offsets and lengths, never transcript
content. Other sources are unaffected. `IngestResult.Inserted` counts new rows only; a replay
duplicate of another file's record is a new row and counts. `IngestResult.Cursor` is the offset the
caller may acknowledge.

Error classes let the sync endpoint tell permanent rejections from transient failures:

- `ErrBatchRejected`: a deterministic refusal; the same bytes can never succeed.
- `ErrSourceOwnership`: the batch names another device, or a re-registration names another device or
  agent. Ownership is fixed at registration.
- `ErrSourceNotRegistered`: call `RegisterSource` before the first batch. Re-registering the same
  source is a no-op that only follows a path move. Agent names follow the watcher rule
  (`^[a-z0-9][a-z0-9._-]{0,63}$`), which keeps them free of colons for the session ID encoding.
- `ErrSourcePurged`: the source was purged by the owner and is tombstoned. Registration, batches,
  and state reads for its source ID are refused with the purge date and the `ohya sources reallow`
  remedy, for every device, until the owner re-admits it (see "Source lifecycle" and
  [ADR 0013](docs/adr/0013-owner-scoped-session-purge.md)).

`store.SourceState` returns the source's agent, cursor, and `FirstNativeID`, the native ID of the
record at offset 0, which the Codex and Pi parsers need past offset 0.

`store.Annotate` validates the whole batch (writer, size, operation shape, names, values, duplicate
identities) and then checks that every referenced event exists, before the first write statement.
Content refusals wrap `ErrAnnotationRejected` and can never succeed on retry. Errors of the SQL
execution path, such as lock contention or a failed commit, do not wrap it, and a retry may clear
them. A value nested deeper than SQLite's JSON depth limit, which is lower than Go's, fails the
table check and rolls back the whole batch.

## Re-derivation

The parser output version (`events.parser_version`) is independent of the schema
version. It versions the parser behavior that derived a row's normalized fields —
`text`, `kind`, `role`, `tool_name`, `time_ms`, `metadata`, `ordinal` and the
session assignment — not the shape of the tables. Each parser owns an
`OutputVersion` constant (`internal/claudecode`, `internal/codex`,
`internal/pi`, `internal/antigravity`); ingestion
stamps it per row and refuses records that name no version. Bump the constant
whenever a parser derives different fields from the same bytes — before the
change lands, in the same commit.

Output version history:

| Parser | Version | Change |
|---|---|---|
| `claudecode` | 1 | Output when the marker was introduced |
| `claudecode` | 2 | Ignores `"usage": null`, avoids HTML escaping in compact JSON tool payloads, and excludes sidechain subagent prompts from `first_prompt` |
| `claudecode` | 3 | Stores the request-log and bookkeeping types (`api-request`, `api-request-blob`, `api-request-shape`, `session-owner`, `tools-used`, `pr-link`, `file-history-snapshot`, `file-history-delta`, `fork-context-ref`) under their own kind, not `unknown`, and reads `message` only for `user` and `assistant` records, so an `api-request-blob` copy contributes no `text` |
| `codex` | 1 | Output when the marker was introduced |
| `codex` | 2 | Adds `Record.Repo`, derived from `session_meta.git.repository_url` |
| `codex` | 3 | `Record.Repo` uses `workspace.NormalizeRemote`: drops a port from a URL with a scheme, drops all user information up to the last `@`, and rejects an invalid host |
| `codex` | 4 | Hardens `workspace.NormalizeRemote` (case-insensitive schemes, strict scheme/host/numeric-port/path checks, query/fragment stripping, SCP userinfo stripping before `:`, and `MaxRepoBytes` bound) and ignores `"usage": null` |
| `pi` | 1 | Output when the marker was introduced |
| `pi` | 2 | Adds `Record.EntryID`, the usage-row key when a provider emits no `responseId` (verified 2026-11-14: `openai-completions` records never carry one) |
| `pi` | 3 | Ignores `"usage": null`, guards against signed `int64` token-sum overflow, and avoids HTML escaping in compact JSON tool payloads |
| `antigravity` | 1 | Initial append-only `transcript_full.jsonl` parser (`USER_INPUT`, `PLANNER_RESPONSE`, `GENERIC`, `SYSTEM_MESSAGE`; additive token usage keyed by `step-<step_index>`) |

Add a row in the same commit that bumps a constant.

Because `raw_json` preserves every original record, a bumped parser re-derives
stored rows inside the hub with no source-file access and no resync from
devices: `store.ReDerive` (the `ohya rederive` command) re-parses each row of
the stale agent from `raw_json` in one transaction. It rewrites normalized
fields in place, so event IDs, annotations, raw records and `sources.cursor_bytes`
are untouched; it recomputes session aggregates and token totals, reassigns
byte-identical replay duplicates in deterministic `(source_id, source_offset)`
order instead of arrival order, keeps distinct records that share an ordinal as
separate originals, drops sessions that lost all events, and rebuilds `events_fts`
by clearing the index (`delete-all`) and re-inserting only rows with non-empty
`events.text` (matching the insert/delete triggers so text-less rows never leave
orphaned `docsize` entries across future purges). Re-derived
text is stored once in `events.text`; the FTS index references it as external
content, duplicates keep NULL text, and the aggregates recomputed in the same
transaction are the only derived copies. A failed pass rolls back completely,
and a hub with no stale rows commits nothing — running the command is always
safe. Rows carrying a version above this build's parser are refused: upgrade
first, then re-derive.

The walk reads one agent's sources in source ID order and each source's records in offset order, in
chunks of 128 records with keyset pagination on the offset. Each chunk closes before any update, so
the scan never observes the rows it changes, and memory stays bounded even for gigabyte rollouts.
Each source seeds its native session from `sources.first_native_id` (so a reallowed Codex or Pi
source whose offset-0 event was purged still re-derives past offset 0), the record at offset 0
establishes or updates it when present, and each later record receives the native ID
of the record before it, exactly like a fresh parse. A deriver must return an event that frames
exactly the stored record (offset, end, and raw JSON), names a session, and carries the version it
was bound with. The pass clears replay-original marks and token totals before the walk, so the
first usage of a message ID is the one a fresh ingest in walk order keeps. Sessions belong to one
agent, because the session ID embeds the agent. The walk recomputes `repo` like `cwd` and `branch`.
Unknown metadata keys are preserved; only `firstPromptRef` is managed, as in the write path. The result counts rewritten
events and rebuilt sessions, including removed empty sessions.

## Inspection

`ohya doctor` runs read-only checks through `store.OpenReadOnly`: the applied schema version
against the embedded migrations (`ErrNoSchemaMigrations` marks a file that is not a hub database),
the highest stored parser version per agent against this build, and FTS consistency.

The FTS check runs the FTS5 `integrity-check` command with the `rank` argument, which compares the
index with the external content. SQLite refuses that command on a `mode=ro` connection, because it
is a write statement. The check therefore copies the database to a private temporary file under
TMPDIR with the backup API, which writes nothing to the hub. The copy holds one read transaction on
the source from the first step to the last, so commits from the hub do not restart it, and the hub
keeps its writers. The check needs free disk a little more than the size of the database, and its
time grows with the database size. The `rank` comparison expects an index entry for every content
row, and this schema leaves text-less events unindexed, so the check first drops the delete trigger
on the copy and removes the text-less rows.

The check has three outcomes. `ok`: the index agrees with `events`. An index that disagrees
returns `store.ErrFTSIndexInconsistent`, and the finding says to restore a backup. Free space
below the need, or any other error, gives a finding that says the deep check did not run, with
the cause.

`count(*)` on the external-content `events_fts` table reads the content table, not the index. To
enumerate indexed documents, query through `MATCH`.

## Search example

In a `MATCH` string, wrap every punctuation-bearing term — a hyphenated
branch or directory name, an apostrophe — in double quotes, as in
`'"auth-refresh"'`. The tokenizer splits it into its phrase tokens either
way, but bare `auth-refresh` parses as the term `auth` plus a `-refresh`
column filter, and `events_fts` has no such column, so the query fails
with `no such column: refresh` (other bare punctuation, for example `/`,
is an FTS5 syntax error).

```sql
SELECT e.id, e.session_id, s.repo, s.agent, s.device_id,
       datetime(e.time_ms/1000, 'unixepoch') AS utc_time,
       snippet(events_fts, 0, '[', ']', '...', 24) AS excerpt
FROM events_fts
JOIN events e ON e.id = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH 'authentication AND refresh'
ORDER BY events_fts.rank
LIMIT 10;
```

One best hit per session (dedupes noisy multi-mention sessions; keep `ORDER BY`
and `LIMIT` inside the CTE so FTS5 `snippet()` stays valid):

```sql
WITH hits AS (
  SELECT e.id, e.session_id, s.agent, s.cwd,
         datetime(e.time_ms/1000, 'unixepoch') AS utc_time,
         events_fts.rank AS rk,
         snippet(events_fts, 0, '[', ']', '...', 24) AS excerpt
  FROM events_fts
  JOIN events_settled e ON e.id = events_fts.rowid
  JOIN sessions s ON s.id = e.session_id
  WHERE events_fts MATCH 'authentication AND refresh'
  ORDER BY events_fts.rank LIMIT 100
)
SELECT id, session_id, agent, cwd, utc_time, excerpt, min(rk) AS best_rank
FROM hits GROUP BY session_id ORDER BY best_rank LIMIT 10;
```

Read surrounding records around a hit (always filter `session_id` with `=`,
never `LIKE '%...'`, so SQLite uses `events_session_time`):

```sql
SELECT e.id, datetime(e.time_ms/1000, 'unixepoch') AS utc_time,
       e.kind, e.role, e.tool_name, substr(e.text, 1, 400) AS excerpt
FROM events e
WHERE e.session_id = 'claude-code:dev1:s1'
  AND e.duplicate_of IS NULL
  AND e.id BETWEEN 2-5 AND 2+5
ORDER BY e.id;
```

Count matching sessions and events in one pass:

```sql
SELECT count(DISTINCT e.session_id) AS sessions, count(*) AS events
FROM events_fts
JOIN events_settled e ON e.id = events_fts.rowid
WHERE events_fts MATCH 'authentication AND refresh';
```

## Project recall example

A query for "the newest events of this project" joins `events` to `sessions`
filtered on `cwd` or `repo` and orders by `time_ms`. The planner drives that
join through `events_session_time` and feeds every matching `events` row into
the sort, so the query reads one table row per project event before it can
return the first one; a project with tens of thousands of events exhausts the
`sql` tool's 5 s deadline (observed 2026-11-15 on a 2.7 M-event hub: one call
timed out at 5,027 ms and a second took about 4 s). Select the newest IDs in a
subquery first: it sorts on the covering `events_session_time` index without
reading `events` rows, and the outer query reads only the rows it returns.

```sql
SELECT e.id, e.session_id, datetime(e.time_ms/1000, 'unixepoch') AS utc_time, e.kind, e.role, substr(e.text, 1, 300) AS excerpt
FROM events e WHERE e.id IN (
  SELECT ev.id FROM sessions s JOIN events_settled ev ON ev.session_id = s.id
  WHERE s.cwd = '/home/agent/project' OR s.repo = 'github.com/owner/name'
  ORDER BY ev.time_ms DESC LIMIT 5)
ORDER BY e.time_ms DESC;
```

`EXPLAIN QUERY PLAN` shows the difference: the plain join reports
`SEARCH e USING INDEX events_session_time` plus `USE TEMP B-TREE FOR ORDER BY`
with a row fetch per match, while the subquery form reports
`SEARCH ev USING COVERING INDEX events_session_time` and
`SEARCH e USING INTEGER PRIMARY KEY (rowid=?)`.
The same shape serves an `events_fts MATCH` result when the filter is wide:
filter, order, and limit in a covering subquery, then read the chosen rows.
Measured effect and the rejected alternatives (an `events(time_ms)` index, a
wider covering index) are recorded in docs/performance.md
("Recall query deadline"). When a query exceeds the deadline, the error
names the cause that the SQLite authorizer reported to the engine: `LIKE`
on `events`, a read of `events.raw_json`, `count()` on `events`, or a wide
read. The first cause that applies wins, in that order. A `raw_json` read
comes before `count()` because a grouped count over `json_extract(raw_json, …)`
spends its time in reading full records, and `sessions.event_count` cannot
answer it.

Agents persist their interpretations in `event_annotations`; there is no built-in
classifier or dedicated classification table. Add secondary indexes or normalized file references only after real
query patterns justify them.

## Recall recipes

Agents run these recipes through the MCP `sql` tool. `ohya sql --json`
runs the same engine for scripts and for people at a shell.

The `sql` tool description publishes the core recall recipes at connect
time, one paragraph per recipe, and serves lower-frequency analytics and
review recipes on demand through the `ohya://guides/analytics-and-review`
MCP resource. They are single statements an agent runs
unmodified after substituting one discovered literal (a repo, a model, a
device, or a session id). Each recipe keeps the subquery shape of the
project recall example, because a plain `ORDER BY e.time_ms` over a wide
filter reads one `events` row per match and can exceed the 5 s deadline on
a large hub. The error of a query that exceeds the deadline names the
cause.

**Sessions by agent and time window.** Scope `sessions` by `agent` and
`last_event_at_ms` first, then inspect the chosen `session_id` through
`events`. Replace `'claude'` with `'codex'`, `'pi'`, or `'antigravity'`,
or bound one UTC day with `CAST(strftime('%s','2026-11-20') AS INTEGER) * 1000`:

```sql
SELECT s.id, s.device_id, s.repo, substr(s.first_prompt, 1, 160) AS first_prompt, s.last_event_at_ms
FROM sessions s
WHERE s.agent = 'claude'
  AND s.last_event_at_ms >= (CAST(strftime('%s','now') AS INTEGER) - 7*86400) * 1000
ORDER BY s.last_event_at_ms DESC LIMIT 20;
```

**Date-bounded recall.** `time_ms` holds Unix milliseconds. The cutoff uses
`strftime` — the same expression the `events_settled` view itself uses — so
the query needs no clock knowledge. The `sessions` bound drives the
covering `events_session_time` index through sessions active in the window;
the `events` bound keeps older events of those sessions out of the result.
Both bounds are required: the events filter alone has no index to drive
and degrades into a full scan.

```sql
SELECT e.id, e.session_id, e.time_ms, e.kind, e.role, substr(e.text, 1, 300) AS excerpt
FROM events e WHERE e.id IN (
  SELECT ev.id FROM sessions s JOIN events_settled ev ON ev.session_id = s.id
  WHERE s.last_event_at_ms >= (CAST(strftime('%s','now') AS INTEGER) - 7*86400) * 1000
    AND ev.time_ms >= (CAST(strftime('%s','now') AS INTEGER) - 7*86400) * 1000
  ORDER BY ev.time_ms DESC LIMIT 20)
ORDER BY e.time_ms DESC;
```

**Resume: rebuild context from this repository's last settled session.**
This is ohya's answer to "resume where the last agent stopped". Step 1
picks the newest session of the repository that is settled — idle for at
least 10 minutes, so the pick is never a session still running, including
the querying agent's own. `EXISTS` over `events_settled` inherits the
view's exact settled semantics, including the NULL-timestamp case.

```sql
SELECT s.id, s.agent, s.device_id, substr(s.first_prompt, 1, 160) AS first_prompt, s.last_event_at_ms
FROM sessions s WHERE s.repo = 'github.com/owner/name'
  AND EXISTS (SELECT 1 FROM events_settled e WHERE e.session_id = s.id)
ORDER BY s.last_event_at_ms DESC LIMIT 1;
```

Step 2 reads that session's newest records oldest-first. The inner query
walks the per-session `events_session_time` index backwards and stops at
the limit; the outer query reverses those few rows into reading order.
`duplicate_of IS NULL` hides byte-identical replay copies, which carry no
search text of their own.

```sql
SELECT e.id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, 400) AS excerpt
FROM events e WHERE e.id IN (
  SELECT ev.id FROM events ev WHERE ev.session_id = 'claude:dev1:s1'
    AND ev.duplicate_of IS NULL
  ORDER BY ev.time_ms DESC, ev.id DESC LIMIT 50)
ORDER BY e.time_ms, e.id;
```

**Filter by model.** `sessions.model` keeps the first non-empty model the
session's own records carried, so it is NULL for sessions whose records
named none. Discover the values present on the hub first, then filter (one
statement per call):

```sql
SELECT DISTINCT model FROM sessions;
```

```sql
SELECT s.id, s.agent, s.device_id, substr(s.first_prompt, 1, 160) AS first_prompt, s.last_event_at_ms
FROM sessions s WHERE s.model = 'claude-sonnet-4-5'
ORDER BY s.last_event_at_ms DESC LIMIT 10;
```

**Per-device scoping.** One hub holds every paired device's sessions.
Scoping by `device_id` answers "what happened on the laptop" and keeps a
citation attributed to the machine that produced it. List the devices
first, then scope; combine the filter with the repo or model recipe in one
`WHERE` clause when a question needs both:

```sql
SELECT DISTINCT device_id FROM sessions;
```

```sql
SELECT s.id, s.agent, s.repo, substr(s.first_prompt, 1, 160) AS first_prompt, s.last_event_at_ms
FROM sessions s WHERE s.device_id = 'laptop'
ORDER BY s.last_event_at_ms DESC LIMIT 10;
```

## Garbage audit

When a user asks an agent to "clean up the data" or "find the garbage",
the agent answers end to end without ever holding a destructive
capability ([ADR 0016](docs/adr/0016-agent-marked-purge-scope.md)). The
flow has three steps, and the MCP server exposes the same queries on
demand through the `ohya://guides/garbage-audit` resource (linked from the
`sql` tool description):

1. **FIND** — read-only SQL (the recipes below) locates stale weight.
2. **MARK** — the agent annotates the deciding event of each garbage
   session through the bounded `annotate` tool under the reserved
   namespace `tags`, key `garbage`, with a value stating the reason and
   the evidence event id. Marks are persistent and writer-attributed, so
   the owner has an audit trail of who called what garbage and why.
3. **EXECUTE** — the agent presents the exact `ohya purge --marked
   tags.garbage` dry-run and `--apply` commands, and the owner runs them.
   The `sql` tool is read-only and `annotate` writes only annotations, so
   a prompt-injected agent can never destroy stored records — including
   evidence of itself.

Each recipe is a single statement an agent runs unmodified after
substituting one discovered literal.

**Sources that stopped syncing.** `sources.updated_at_ms` moves whenever
a batch advances the source cursor; NULL means registration never
committed one. A source idle for a month is a device that stopped
appending to that file — deleted locally, excluded, or gone:

```sql
SELECT src.id, src.agent, src.device_id, src.path, src.updated_at_ms
FROM sources src
WHERE src.updated_at_ms IS NULL
   OR src.updated_at_ms < (CAST(strftime('%s','now') AS INTEGER) - 30*86400) * 1000
ORDER BY src.updated_at_ms, src.id;
```

**Sessions whose working directory resolved to no repository.** A
`workspaces` row with a NULL `repo` records that the device looked at
that `cwd` and found no usable repository — the directory was deleted or
never was a checkout. Observations are write-once, so re-check the path
before calling the session garbage:

```sql
SELECT s.id, s.agent, s.device_id, s.cwd, s.last_event_at_ms
FROM sessions s JOIN workspaces w ON w.device_id = s.device_id AND w.path = s.cwd
WHERE w.repo IS NULL
ORDER BY s.last_event_at_ms DESC LIMIT 50;
```

**Oversized records.** The largest sessions first (a source's
`cursor_bytes` is the mirrored size of its file), then the records over
64 KiB inside one of them — on the deployed hub 0.5% of rows hold 27% of
the bytes, and they are tool results and Codex compaction snapshots. The
drill-down reads only that source's rows, because no index carries
record length:

```sql
SELECT src.id, src.agent, src.device_id, src.path, src.cursor_bytes
FROM sources src ORDER BY src.cursor_bytes DESC LIMIT 20;
```

```sql
SELECT e.id, e.session_id, e.kind, e.role, e.tool_name, length(e.raw_json) AS bytes
FROM events e WHERE e.source_id = 'src-3' AND length(e.raw_json) > 65536
ORDER BY bytes DESC;
```

**Empty or single-record sessions.** Aborted starts and failed first
calls. `sessions.event_count` is the maintained total (replay copies add
nothing to it), so the recipe reads only the sessions table — a per-event
count over a large hub walks every session's index range and can exceed
the `sql` deadline. Synthetic `ohya-synthetic:` companions follow their
source and are never selectable:

```sql
SELECT s.id, s.agent, s.device_id, s.last_event_at_ms, s.event_count
FROM sessions s
WHERE s.native_id NOT LIKE 'ohya-synthetic:%' AND s.event_count <= 1
ORDER BY s.event_count, s.last_event_at_ms DESC, s.id LIMIT 50;
```

**Replay copies inside one session.** Byte-identical re-pages (Codex
resumed pages) hold no text of their own and add nothing to totals; 0
means none. The count is per session on purpose: `duplicate_of` carries
no index, so a hub-wide scan would read every stored record and can
exceed the `sql` deadline on a large hub:

```sql
SELECT count(*) AS replay_copies FROM events e
WHERE e.session_id = 'claude:dev1:s2' AND e.duplicate_of IS NOT NULL;
```

**Model distribution.** A stale model shows as its own row with old
activity. List the values, then confirm a model is really dead before
marking its sessions:

```sql
SELECT s.model, count(*) AS sessions, min(s.last_event_at_ms) AS oldest, max(s.last_event_at_ms) AS newest
FROM sessions s WHERE s.model IS NOT NULL GROUP BY s.model
ORDER BY oldest, s.model;
```

**Marking.** The agent annotates the deciding event of each session it
calls garbage; the value states the reason and the evidence, and the hub
attributes the mark to the writer:

```json
{"operations":[{"op":"upsert","event_id":42,"namespace":"tags","key":"garbage","value":{"reason":"aborted start, single record","evidence":42}}]}
```

Review every mark — with its writer — before the owner runs anything:

```sql
SELECT DISTINCT s.id, s.agent, s.device_id, a.writer_id, s.last_event_at_ms
FROM event_annotations a
JOIN events e ON e.id = a.event_id
JOIN sessions s ON s.id = e.session_id
WHERE a.namespace = 'tags' AND a.key = 'garbage'
ORDER BY s.last_event_at_ms DESC;
```

**Execute.** The owner purges the marked sessions; the selector takes
every writer's marks and combines with the other purge selectors (see
"Source lifecycle"). Marks die with their events, so a rerun finds
nothing left:

```sh
ohya purge --marked tags.garbage
ohya purge --marked tags.garbage --apply
```

## Token totals example

Session token totals are sums over `session_token_usage`; each API response
contributes one row, so replaying ingestion never changes the result:

```sql
SELECT s.id, s.first_prompt,
       sum(u.input_tokens)  AS input_tokens,
       sum(u.output_tokens) AS output_tokens
FROM sessions s
LEFT JOIN session_token_usage u ON u.session_id = s.id
GROUP BY s.id;
```

## Annotation examples

An agent can submit this MCP argument to record a completed security review:

```json
{"operations":[{"op":"upsert","event_id":42,"namespace":"security","key":"review","value":{"analysis_version":"v1","status":"reviewed","findings":[]}}]}
```

The writer is assigned by the hub. A tag can use namespace `tags`, key `needs-review`
and JSON value `true`; a classification can use namespace `classification`, key
`work-type` and a value containing its label and analysis version. The
namespace `tags` with key `garbage` is reserved for the owner's purge audit
(see "Garbage audit"): those marks feed `ohya purge --marked tags.garbage`,
which only the owner runs.

Select the next page needing review (bind the current writer and last seen ID):

```sql
SELECT e.id, e.raw_json
FROM events e
WHERE e.id > :after_id
  AND NOT EXISTS (
    SELECT 1 FROM event_annotations a
    WHERE a.event_id = e.id AND a.writer_id = :writer_id
      AND a.namespace = 'security' AND a.key = 'review'
      AND json_extract(a.value, '$.analysis_version') = 'v1'
      AND json_extract(a.value, '$.status') = 'reviewed'
  )
ORDER BY e.id LIMIT 100;
```

These are illustrative SQL parameters; `sql(query)` callers substitute SQL literals.
Rows are written only through the `annotate` MCP tool (`internal/store.Annotate`):
the hub assigns `writer_id` from the device identity, values are stored as
compact JSON text, an identical upsert rewrites nothing (timestamps included),
a changed value keeps `created_at_ms`, and the result reports `applied` and
`unchanged` counts after the atomic commit. Expose the current writer ID in the
MCP tool description. The primary key supports
per-event annotation lookup. Add broader annotation indexes only with query evidence.
The initial migration includes annotations; later schema changes follow the migration
policy above.
