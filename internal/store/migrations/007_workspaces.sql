CREATE TABLE workspaces (
  device_id       TEXT NOT NULL,
  path            TEXT NOT NULL,
  repo            TEXT,
  observed_at_ms  INTEGER NOT NULL,
  PRIMARY KEY (device_id, path)
) STRICT;

CREATE INDEX sessions_device_cwd ON sessions(device_id, cwd);
