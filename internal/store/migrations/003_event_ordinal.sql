-- Per-thread record ordinals and replay deduplication across source files.
--
-- A Codex thread spans several rollout files: a resumed or paginated page
-- replays some tail records of the earlier file (verified against codex-cli
-- 0.166.0; see testdata/codex/README.md, "Native session identity"). Byte
-- offsets cannot see this, because each file is its own source. The agent's
-- per-thread ordinal names one record of the thread, so the write path
-- deduplicates on (session_id, ordinal).
--
-- A replayed record is still stored: its original text stays retained, and
-- the (source_id, source_offset) retry validation keeps working. It is marked
-- with duplicate_of, the id of the first stored copy, and its text stays NULL,
-- so search returns the record once. Duplicates add nothing to the session
-- aggregates or token totals. Records without an ordinal (Claude Code, Codex
-- before 0.147.0) keep ordinal NULL and are never deduplicated this way.
ALTER TABLE events ADD COLUMN ordinal INTEGER CHECK (ordinal >= 0);
ALTER TABLE events ADD COLUMN duplicate_of INTEGER REFERENCES events(id);

CREATE UNIQUE INDEX events_session_ordinal ON events(session_id, ordinal)
  WHERE ordinal IS NOT NULL AND duplicate_of IS NULL;
