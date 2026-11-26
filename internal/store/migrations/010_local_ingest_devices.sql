CREATE TABLE local_ingest_devices (
  device_id TEXT PRIMARY KEY CHECK (length(device_id) = 32 AND device_id NOT GLOB '*[^0-9a-f]*')
) STRICT;
