-- Tombstones of owner-purged sources (ADR 0013).
--
-- A purge deletes a session's whole rows — events, aggregates, usage,
-- annotations, the sessions row, and every source that held the session's
-- events — and replaces each deleted sources row with one tombstone here.
-- A tombstoned source cannot come back through the write path: registration
-- and batches for its source ID are refused deterministically with the
-- purge date and the reallow remedy, so purged bytes never return
-- automatically. Re-admission is the owner's explicit act (ohya sources
-- reallow), which removes the tombstone and restores the sources row at the
-- purged cursor, so history resumes from the purge point and a replay of
-- the purged bytes below that cursor is rejected.
--
-- cursor_bytes is the source's committed cursor at eviction; the refusal
-- and reallow paths read it. native_id names the session the purge
-- selected (the source's real session; the source-scoped synthetic
-- session's native ID when the source held no real one). first_native_id
-- preserves the source's offset-0 native ID across purge and reallow so
-- header-first parsers (Codex, Pi) can resume and re-derive past offset 0
-- after the offset-0 event was deleted. scope records the selector text of
-- the purge that evicted the source.
ALTER TABLE sources ADD COLUMN first_native_id TEXT;

UPDATE sources
SET first_native_id = (
  SELECT s.native_id
  FROM events e JOIN sessions s ON s.id = e.session_id
  WHERE e.source_id = sources.id AND e.source_offset = 0
)
WHERE first_native_id IS NULL;

CREATE TABLE purged_sources (
  source_id       TEXT PRIMARY KEY,
  device_id       TEXT NOT NULL,
  agent           TEXT NOT NULL,
  path            TEXT NOT NULL,
  cursor_bytes    INTEGER NOT NULL CHECK (cursor_bytes >= 0),
  native_id       TEXT,
  first_native_id TEXT,
  scope           TEXT NOT NULL,
  purged_at_ms    INTEGER NOT NULL
) STRICT;

CREATE INDEX purged_sources_native ON purged_sources(native_id);
CREATE INDEX purged_sources_device_agent ON purged_sources(device_id, agent);
