# ohya — Database plan

[001_initial.sql](internal/store/migrations/001_initial.sql) is the authoritative initial DDL. It defines
the ingestion, annotation and FTS5 tables; [002_session_token_usage.sql](internal/store/migrations/002_session_token_usage.sql)
adds the per-response token totals. Do not duplicate the DDL here.
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
  and sums the four unambiguous counts. The first usage observed for a
  message ID wins; a later differing usage for the same message ID is ignored,
  never summed. Usage without a message ID is ambiguous and contributes
  nothing. Tool aggregates stay deferred until a query need justifies them.
- Events and normalized text are immutable in MVP. Future reprocessing needs an
  explicit migration and FTS rebuild; no implicit updates behind insert/delete triggers.
- Foreign keys are enabled on every relevant connection. WAL and FULL synchronous
  durability apply to the writer. Verify these settings and FTS consistency in tests.

Authorization credentials live in a separate protected hub registry before remote
access is enabled. The schema covers transcript data and annotations, not credentials
or device-local checkpoints. See ARCHITECTURE.md for ownership and sync semantics.

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
Expose the current writer ID in the MCP tool description. The primary key supports
per-event annotation lookup. Add broader annotation indexes only with query evidence.
The initial migration includes annotations; later schema changes follow the migration
policy above.
