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
(see "Settled events").
Do not duplicate the DDL here.
There is no released database yet, so migrations 001 and 002 are the pre-release baseline; every later
schema change arrives as a new migration file.

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
   rolls back completely; the next run resumes at the first pending version.
3. Every open verifies applied history. A checksum mismatch, a migration missing from the binary, or a
   new file numbered below the newest applied version is an error, never a silent upgrade.
4. Migration file names match `NNN_name.sql` with a unique three-digit version. `store.SchemaVersion`
   reports the newest embedded version without opening a database.
5. Until a database ships, 001 is the only baseline. After a release exists, test every new migration
   by upgrading a database from each released schema, not only by fresh creation.
6. Keep this file in step with the migration list in the same change, so a review can diff policy
   against DDL.

## Connections and durability

All writes — ingestion, annotations, migrations — go through the single writer connection that
`store.Open` configures with WAL journaling, `synchronous = FULL`, foreign keys and a busy timeout.
Readers get separate read-only connections (`mode=ro`) with foreign keys enabled. The settings apply
to every pooled connection, and `store.Open` verifies them before use. Open the writer first and keep
it open while readers run: a read-only connection cannot create the database or recover the
write-ahead log.

The busy timeout is 5 s, so a lock held by another process, such as a backup tool, fails a statement
only after that wait. Driver parameters with a leading underscore apply to every pooled connection;
`mode=ro` reaches SQLite when it opens the file. `store.OpenReadOnly` serves `ohya doctor`: it opens
an existing hub database in WAL mode with one read-only pool, applies no migrations, and has no
writer.

| Table | Meaning |
|---|---|
| `sessions` | Device/agent/native session identity and observed aggregates |
| `sources` | File incarnation and authoritative committed byte cursor |
| `events` | One immutable original JSONL record and normalized searchable fields |
| `session_token_usage` | Token counts of one API response, keyed by session and message ID |
| `event_annotations` | Agent-written JSON values keyed by event, namespace, key and writer |
| `events_fts` | Search index over event text, maintained transactionally |
| `events_settled` | View over `events` holding only records of sessions idle at least 10 minutes |

Use Unix milliseconds; absent observations remain NULL. Ingestion JSON metadata contains
only deterministic observations, not inferred outcomes. Raw JSON preserves the
source record text; line terminators are transport framing. Offsets are byte
offsets in the original file, including framing when calculating the next cursor.

### Repository derivation

`sessions.repo` holds the repository URL a session's own records carry,
normalized to `host/path`: the `https://`, `http://`, `ssh://` or `git://`
scheme, any `user@`, and a trailing `.git` or `/` are stripped, an scp-style
`git@host:path` separator becomes `/`, and the host is lowercased while the
path keeps its case. Codex derives it from `session_meta.git.repository_url`
(embedded parent metas lend nothing); Claude Code and Pi records carry no
repository URL, so their sessions keep NULL. The first non-empty observation
in deterministic order wins, exactly like `cwd` and `branch`, and re-derivation
recomputes it. Never decode the Claude Code project directory name — it is
lossy — and never discover git roots from the filesystem at ingestion time.

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
  written under 003 stay as stored until the hub is re-ingested; no released
  database exists. Records without an ordinal (Claude Code, Codex before
  0.147.0) are never deduplicated this way.
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
`internal/pi`); ingestion
stamps it per row and refuses records that name no version. Bump the constant
whenever a parser derives different fields from the same bytes — before the
change lands, in the same commit.

Output version history:

| Parser | Version | Change |
|---|---|---|
| `claudecode` | 1 | Output when the marker was introduced |
| `codex` | 1 | Output when the marker was introduced |
| `codex` | 2 | Adds `Record.Repo`, derived from `session_meta.git.repository_url` |
| `pi` | 1 | Output when the marker was introduced |
| `pi` | 2 | Adds `Record.EntryID`, the usage-row key when a provider emits no `responseId` (verified 2026-11-14: `openai-completions` records never carry one) |

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
from the content table with the external-content `rebuild` command. Re-derived
text is stored once in `events.text`; the FTS index references it as external
content, duplicates keep NULL text, and the aggregates recomputed in the same
transaction are the only derived copies. A failed pass rolls back completely,
and a hub with no stale rows commits nothing — running the command is always
safe. Rows carrying a version above this build's parser are refused: upgrade
first, then re-derive.

The walk reads one agent's sources in source ID order and each source's records in offset order, in
chunks of 128 records with keyset pagination on the offset. Each chunk closes before any update, so
the scan never observes the rows it changes, and memory stays bounded even for gigabyte rollouts.
The record at offset 0 establishes the native session, and each later record receives the native ID
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
is a write statement. The check therefore copies the database into a private in-memory database with
the backup API, which writes nothing to the hub. The `rank` comparison expects an index entry for
every content row, and this schema leaves text-less events unindexed, so the check first drops the
delete trigger on the copy and removes the text-less rows. It runs on one connection, because each
pooled connection would get its own in-memory database.

`count(*)` on the external-content `events_fts` table reads the content table, not the index. To
enumerate indexed documents, query through `MATCH`.

## Search example

```sql
SELECT s.repo, s.agent, s.device_id,
       e.time_ms, substr(e.text, 1, 500) AS excerpt
FROM events_fts
JOIN events e ON e.id = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH 'authentication AND refresh'
ORDER BY events_fts.rank
LIMIT 10;
```

Agents persist their interpretations in `event_annotations`; there is no built-in
classifier or dedicated classification table. Add secondary indexes or normalized file references only after real
query patterns justify them.

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
`work-type` and a value containing its label and analysis version.

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
