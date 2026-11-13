PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;

CREATE TABLE sessions (
  id                TEXT PRIMARY KEY,
  agent             TEXT NOT NULL,
  device_id         TEXT NOT NULL,
  native_id         TEXT NOT NULL,
  repo              TEXT,
  cwd               TEXT,
  branch            TEXT,
  model             TEXT,
  first_prompt      TEXT,
  started_at_ms     INTEGER,
  last_event_at_ms  INTEGER,
  event_count       INTEGER NOT NULL DEFAULT 0,
  metadata          TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
  UNIQUE (device_id, agent, native_id)
) STRICT;

CREATE TABLE sources (
  id               TEXT PRIMARY KEY,
  device_id        TEXT NOT NULL,
  agent            TEXT NOT NULL,
  path             TEXT NOT NULL,
  file_identity    TEXT,
  cursor_bytes     INTEGER NOT NULL DEFAULT 0 CHECK (cursor_bytes >= 0),
  updated_at_ms    INTEGER
) STRICT;

CREATE TABLE events (
  id               INTEGER PRIMARY KEY,
  session_id       TEXT NOT NULL REFERENCES sessions(id),
  source_id        TEXT NOT NULL REFERENCES sources(id),
  source_offset    INTEGER NOT NULL CHECK (source_offset >= 0),
  time_ms          INTEGER,
  kind             TEXT NOT NULL,
  role             TEXT,
  tool_name        TEXT,
  text             TEXT,
  raw_json         TEXT NOT NULL,
  metadata         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
  UNIQUE (source_id, source_offset)
) STRICT;

CREATE INDEX sessions_recent ON sessions(last_event_at_ms DESC);
CREATE INDEX sessions_repo_recent ON sessions(repo, last_event_at_ms DESC);
CREATE INDEX events_session_time ON events(session_id, time_ms, id);

CREATE VIRTUAL TABLE events_fts USING fts5(
  text, content='events', content_rowid='id'
);

CREATE TRIGGER events_fts_insert AFTER INSERT ON events
WHEN NEW.text IS NOT NULL AND length(NEW.text) > 0
BEGIN
  INSERT INTO events_fts(rowid, text) VALUES (NEW.id, NEW.text);
END;

CREATE TRIGGER events_fts_delete AFTER DELETE ON events
WHEN OLD.text IS NOT NULL AND length(OLD.text) > 0
BEGIN
  INSERT INTO events_fts(events_fts, rowid, text)
  VALUES ('delete', OLD.id, OLD.text);
END;
