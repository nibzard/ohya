CREATE INDEX events_duplicate_of ON events(duplicate_of)
  WHERE duplicate_of IS NOT NULL;
