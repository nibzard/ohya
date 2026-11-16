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
  (default 33,554,432, the parsers' `DefaultMaxRecordBytes`; 1 MiB before the
  2026-11-15 amendment below). A source stopped
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
- A degraded pass is visible in the exit code, not only on stderr
  (T060): `ohya ingest --once` exits 3 when at least one source failed or
  stopped in the pass, and its stdout summary carries the failed and
  stopped counts. A script or agent that checks the exit code no longer
  reads missing data as success. The long-running poll loop keeps running
  and keeps its stderr diagnostics instead; a `--strict` flag was rejected
  as redundant (YAGNI) — the exit code is the contract.
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
stop. A degraded `--once` pass is no longer silent either: exit code 3 and
the summary counts make the missing events observable to every caller. The
parsers keep `MaxRecordBytes <= 0` meaning "default", so library
callers (hub-side batch normalization, re-derivation with its exact
`len(raw_json)` bound) are unchanged; the sync endpoint passes the wire bound
explicitly so normalization cannot drift from the protocol. Users who raise
the limit accept the memory cost; the diagnostic and the flag help text say
so. Records above any limit a user will set remain uningestible by design —
that boundary is now documented here instead of silently deferring a
recovery operation nobody owns.

## Amendment (2026-11-15): 32 MiB record bound

Real data demanded the larger wire bound. A full local history on one macOS
machine (3,292 Claude Code, Codex, and Pi files, 4.2 GB) held 303 records
above 1 MiB, mostly Codex tool outputs; the largest was 16.8 MiB. The 1 MiB
default stopped 104 sources (3%), and a joined device could not sync those
records at all, because the uploader and the hub enforce the wire bound with
no flag.

- `DefaultMaxRecordBytes` is 32 MiB in all three parsers. The
  `--max-record-bytes` default and the wire bound `hubsync.MaxRecordBytes`
  follow it. One record can hold 32 MiB of memory.
- `hubsync.MaxBatchBytes` is 64 MiB. The uploader reads at most one batch
  window past the cursor, so the window must hold the largest record and its
  newline; otherwise a record between the two bounds never completes and its
  source waits without a diagnostic. A test enforces
  `MaxBatchBytes > MaxRecordBytes + 1`.
- Hub and devices must run builds with the same bounds. A device with the
  larger bound can send a record that an older hub refuses. Upgrade the hub
  first. No release has shipped with the 1 MiB wire bound.

## Amendment (2026-11-16): repair detection while a source stays stopped

A stopped source that never grows (no later newline, no appended bytes)
otherwise re-read and re-parsed its unconsumed range on every pass for no
reason. `ohya ingest` and the uploader now cache, in process memory only,
that a source made no progress and skip the read until something that could
unblock it changes: the file grows past the previous end, the cursor moves,
or replacement/truncation mints a new source ID. Because a user-applied
repair at the stop offset can keep the file's size and identity unchanged
(an in-place, same-length fix, for example `dd conv=notrunc` or an editor
writing in place), the cache also keys on the file's modification time, so
a repair is detected as soon as the write updates it. A repair that also
restores the original modification time is detected only on the next
process restart, which always re-reads once. This does not change the
recovery contract above: resuming from the checkpointed offset and the
named remedies are unchanged, only how promptly a long-running `ohya ingest`
or device uploader notices a repair without a restart.
