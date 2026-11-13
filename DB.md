# ohya — Database plan

[ohya_schema.sql](ohya_schema.sql) is the authoritative initial DDL. It defines
three core tables and an external-content FTS5 index; do not duplicate the DDL here.
There is no released database yet. Add versioned migrations with implementation.

| Table | Meaning |
|---|---|
| `sessions` | Device/agent/native session identity and observed aggregates |
| `sources` | File incarnation and authoritative committed byte cursor |
| `events` | One immutable original JSONL record and normalized searchable fields |
| `events_fts` | Search index over event text, maintained transactionally |

Use Unix milliseconds; absent observations remain NULL. JSON metadata contains
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
access is enabled. The three-table claim concerns transcript data, not credentials
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

Agents interpret evidence to identify unfinished work. There is no classification
table in MVP. Add secondary indexes or normalized file references only after real
query patterns justify them.
