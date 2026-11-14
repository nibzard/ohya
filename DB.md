# ohya — Database plan

[001_initial.sql](internal/store/migrations/001_initial.sql) is the authoritative initial DDL. It defines
the ingestion, annotation and FTS5 tables; [002_session_token_usage.sql](internal/store/migrations/002_session_token_usage.sql)
adds the per-response token totals; [003_event_ordinal.sql](internal/store/migrations/003_event_ordinal.sql)
adds `events.ordinal` and `events.duplicate_of` for replay deduplication across source files;
[004_parser_output_version.sql](internal/store/migrations/004_parser_output_version.sql)
adds `events.parser_version`, the parser output version marker (see "Re-derivation");
[005_ordinal_dedup_by_content.sql](internal/store/migrations/005_ordinal_dedup_by_content.sql)
rekeys that deduplication by content (see the invariant below).
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
4. Until a database ships, 001 is the only baseline. After a release exists, test every new migration
   by upgrading a database from each released schema, not only by fresh creation.
5. Keep this file in step with the migration list in the same change, so a review can diff policy
   against DDL.

## Connections and durability

All writes — ingestion, annotations, migrations — go through the single writer connection that
`store.Open` configures with WAL journaling, `synchronous = FULL`, foreign keys and a busy timeout.
Readers get separate read-only connections (`mode=ro`) with foreign keys enabled. The settings apply
to every pooled connection, and `store.Open` verifies them before use. Open the writer first and keep
it open while readers run: a read-only connection cannot create the database or recover the
write-ahead log.

| Table | Meaning |
|---|---|
| `sessions` | Device/agent/native session identity and observed aggregates |
| `sources` | File incarnation and authoritative committed byte cursor |
| `events` | One immutable original JSONL record and normalized searchable fields |
| `session_token_usage` | Token counts of one API response, keyed by session and message ID |
| `event_annotations` | Agent-written JSON values keyed by event, namespace, key and writer |
| `events_fts` | Search index over event text, maintained transactionally |

Use Unix milliseconds; absent observations remain NULL. Ingestion JSON metadata contains
only deterministic observations, not inferred outcomes. Raw JSON preserves the
source record text; line terminators are transport framing. Offsets are byte
offsets in the original file, including framing when calculating the next cursor.

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
