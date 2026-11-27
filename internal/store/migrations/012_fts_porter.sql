DROP TABLE events_fts;

CREATE VIRTUAL TABLE events_fts USING fts5(
  text, content='events', content_rowid='id', tokenize='porter unicode61'
);

INSERT INTO events_fts(events_fts) VALUES ('rebuild');
