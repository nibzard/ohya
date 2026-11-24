ALTER TABLE sources ADD COLUMN workspace_path TEXT;

ALTER TABLE purged_sources ADD COLUMN workspace_path TEXT;

ALTER TABLE sources DROP COLUMN file_identity;

CREATE INDEX events_source_parser_version ON events(source_id, parser_version);
