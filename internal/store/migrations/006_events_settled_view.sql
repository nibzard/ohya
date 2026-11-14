CREATE VIEW events_settled AS
SELECT e.id, e.session_id, e.source_id, e.source_offset, e.time_ms, e.kind,
       e.role, e.tool_name, e.text, e.raw_json, e.metadata, e.ordinal,
       e.duplicate_of
FROM events e
JOIN sessions s ON s.id = e.session_id
WHERE s.last_event_at_ms IS NULL
   OR s.last_event_at_ms <= (CAST(strftime('%s', 'now') AS INTEGER) - 600) * 1000;
