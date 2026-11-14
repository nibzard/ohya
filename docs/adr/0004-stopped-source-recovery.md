# 0004: Stopped sources, the record size limit, and recovery

Status: accepted, 2026-11-14 (T042).

## Context

Malformed complete lines and records above the size limit stop a source at the
offending offset and expose a diagnostic without transcript content; bytes are
never skipped (ARCHITECTURE.md, "Record handling"). A real-data run of
`ohya ingest --once` over `~/.claude/projects` (2,665 files, 2.0 GB) printed
25 stop diagnostics; 13 were size-limit stops that reappeared identically on
every pass — for example a 12.5 MB file stopped at offset 4,614,163, leaving
7.9 MB permanently unsearched.

The rule was sound, but the remedy was missing: the limit was configurable
only in code (`claudecode.Options.MaxRecordBytes` and its siblings), the
diagnostics named no way out, and the "explicit future recovery operation"
the architecture deferred had no owner in the backlog. This ADR replaces that
deferral with a decision.

## Decision

The stop-and-diagnose rule stands. Recovery is user-owned, explicit and
documented; no automatic skipping.

- `ohya ingest` exposes the per-record bound as `--max-record-bytes`
  (default 1,048,576, the parsers' `DefaultMaxRecordBytes`). A source stopped
  by the limit recovers by rerunning with a larger value: ingestion resumes
  from the checkpointed stop offset, so nothing re-imports and nothing skips.
  The bound is also a memory bound — one record is held in memory up to it —
  which is why it is a user choice, not an unbounded default.
- The local read path honors the bound above the ingestion batch too: a batch
  never splits a record, so one read extends past the 8 MiB batch bound to
  the end of a single larger record, and the parser stops the source as soon
  as the record exceeds the configured limit. Without this, a limit above the
  batch bound could never take effect.
- Every stop diagnostic states its remedy. A size stop names
  `--max-record-bytes`; every other stop (malformed JSON, invalid UTF-8, a
  non-object line) names repairing the source file at the stop offset. Both
  name this ADR. `events.StopError.RecordTooLarge` carries the
  classification from each parser.
- Automatic skipping (skip-with-report) is rejected. A gap in a session file
  silently corrupts everything derived from it — first prompt, token totals,
  replay deduplication, cursor contiguity — and violates "no silent data
  loss". Dropping observed bytes is a lossy edit of history and stays a user
  decision made on the source file, never an ingestion behavior.
- An explicit hub-side resync or skip operation is rejected as speculation
  (YAGNI). The two remedies above cover both stop causes observed in real
  data. Revisit this decision with evidence: a source that cannot be
  repaired and holds a record no practical limit covers.
- Malformed-line stops follow the same ownership. Wild files with malformed
  complete lines are verified input reality (testdata/claude-code/README.md,
  testdata/codex/README.md), the stop-and-diagnose rule already handles them,
  and the remedy is repairing the file; no recovery operation is owed.
- The sync wire bound (`hubsync.MaxRecordBytes`, 1 MiB) stays a protocol
  constant, independent of the local flag. Frames are length-checked against
  it before allocation, and the device uploader stops a source locally at
  the same bound, so no device can send a record the hub must refuse.
  Raising the wire bound is a protocol change that rides the serving and
  joining commands (T033, T034) if real device sources demand it.

## Consequences

A size stop is no longer a dead end: the diagnostic names the flag, the
checkpoint holds the offset, and one rerun with a larger limit clears the
stop. The parsers keep `MaxRecordBytes <= 0` meaning "default", so library
callers (hub-side batch normalization, re-derivation with its exact
`len(raw_json)` bound) are unchanged; the sync endpoint passes the wire bound
explicitly so normalization cannot drift from the protocol. Users who raise
the limit accept the memory cost; the diagnostic and the flag help text say
so. Records above any limit a user will set remain uningestible by design —
that boundary is now documented here instead of silently deferring a
recovery operation nobody owns.
