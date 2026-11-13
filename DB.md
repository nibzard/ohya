# ohya — Database plan

[001_initial.sql](internal/store/migrations/001_initial.sql) is the authoritative initial DDL. It defines
three ingestion tables, one annotation table and an external-content FTS5 index; do not duplicate the DDL here.
There is no released database yet. This versioned SQL is the fresh-database baseline; a migration runner and upgrade policy remain to be implemented.

| Table | Meaning |
|---|---|
| `sessions` | Device/agent/native session identity and observed aggregates |
| `sources` | File incarnation and authoritative committed byte cursor |
| `events` | One immutable original JSONL record and normalized searchable fields |
| `event_annotations` | Agent-written JSON values keyed by event, namespace, key and writer |
| `events_fts` | Search index over event text, maintained transactionally |

Use Unix milliseconds; absent observations remain NULL. Ingestion JSON metadata contains
only deterministic observations, not inferred outcomes. Raw JSON preserves the
source record text; line terminators are transport framing. Offsets are byte
offsets in the original file, including framing when calculating the next cursor.

## Invariants

- Session IDs use an unambiguous encoding of `(device_id, agent, native_id)`.
- Source IDs survive restarts; replacement/truncation creates a new incarnation.
- `(source_id, source_offset)` deduplicates retransmission, not copied history.
- Validate byte boundaries and conflicting retries before inserting; uniqueness
  alone does not prove payload equality or contiguous delivery.
- Insert events, update aggregates and advance the source cursor in one transaction.
- Count only newly inserted records. Timestamp bounds use observed min/max;
  missing timestamps do not overwrite known bounds. First prompt is selected by
  a deterministic source/order tie-break, not arrival order.
- Defer token/tool aggregates until parser fixtures define cumulative versus
  incremental values. Never sum ambiguous usage fields.
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
The initial migration includes annotations. Future schema changes require new migrations; do not edit an applied migration.
