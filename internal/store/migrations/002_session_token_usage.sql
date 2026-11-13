-- Session token totals, one row per API response.
--
-- Claude Code's message.usage reports one API request and repeats identically
-- on every record sharing message.id (verified against 2.1.280; see
-- testdata/claude-code/README.md, "Usage fields"). The write path inserts one
-- row per (session, message_id), so a usage object split across ingestion
-- batches is counted once. The first usage observed for a message_id wins; a
-- later differing usage for the same message_id is ignored, never summed.
-- Only the four unambiguous token counts are stored. Usage without a
-- message_id cannot be deduplicated and contributes nothing.
CREATE TABLE session_token_usage (
  session_id                  TEXT    NOT NULL REFERENCES sessions(id),
  message_id                  TEXT    NOT NULL CHECK (length(message_id) > 0),
  input_tokens                INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_creation_input_tokens >= 0),
  cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_input_tokens >= 0),
  output_tokens               INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  PRIMARY KEY (session_id, message_id)
) STRICT;
