-- Content-keyed replay deduplication across source files.
--
-- Migration 003 assumed a resumed Codex page byte-replays records that the
-- earlier file of the same thread already stored, so one original per
-- (session_id, ordinal) was enough. Verified corpus-wide on 2026-11-14
-- (T044; see testdata/codex/README.md, "Native session identity"), that
-- assumption is wrong: a real page re-enumerates after a rollback, and at
-- every colliding ordinal the two files hold different records. The 003
-- rule then suppressed records the original file never had and dropped
-- their per-response usage from the token totals. 003's header still
-- states the disproven assumption; applied migrations are immutable, and
-- this file is the correcting note.
--
-- Deduplication is now keyed by content: a later copy whose raw bytes
-- equal a stored original's is a replay duplicate exactly as before, but
-- a copy with different bytes at the same (session_id, ordinal) is a
-- distinct record — another original, searchable and counted once, usage
-- included. The unique index becomes a lookup index; the single-writer
-- transaction and the re-derivation walk, which compare raw_json, keep
-- one original per (session_id, ordinal, content).
--
-- Duplicate marks written under the 003 rule stay as stored until the
-- hub is re-ingested: the suppressed rows lost their derived text, and
-- only a parser can restore it. No released database exists.
DROP INDEX events_session_ordinal;
CREATE INDEX events_session_ordinal ON events(session_id, ordinal)
  WHERE ordinal IS NOT NULL AND duplicate_of IS NULL;
