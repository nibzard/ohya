# 0010: Usage record field classes and the export boundary

Status: accepted, 2026-11-16 (T090, plans/001 milestone 1 and plans/002).

## Context

Plan 002 adds one usage record per `sql()` and `annotate()` call so the
maintainer can answer, from local evidence, how agents use the memory tools —
call rates per agent and week, which tables and columns each call reads (this
decides the plan 004 storage design), where agents meet friction, and what
each call costs phase by phase. The records hold operational facts, but some
fields are identifiers that must never leave the machine, and whole
categories of data must never be collected at all.

The design goal is that a later release can add anonymized, opt-in usage
telemetry from other users without a schema break. That is only possible if
the export boundary is a property of the schema itself, decided once, before
any records exist — not a filtering decision each exporter re-derives.

## Decision

Every field of the usage record (`internal/observe`, `UsageRecord`) declares
exactly one class, in one place (`UsageFieldClasses`):

- **export-safe** — operational facts with bounded cardinality: tool, client,
  client version bucket, call path, outcome, SQLite result code, phase
  durations, rows, result bytes, column reads, SQL functions, FTS flag,
  annotate operation counts, hour bucket, schema version. A future exporter
  may send these after bucketing.
- **local-only** — identifiers and exact values that stay on the machine:
  exact timestamp, instance ID, device ID, MCP session hint.
- **never-collected** — SQL text, literal values, paths, repository names,
  transcript content, annotation values, raw error messages, credentials.
  The schema has **no field** for these; `NeverCollectedFields` names them
  as banned field names.

Two tests in `internal/observe/usage_test.go` hold the line:

- `TestUsageFieldClassificationIsComplete` fails if a field lacks a class, if
  a declaration names a field the record does not hold, or if any field is
  classified never-collected.
- `TestUsageSchemaHoldsNoNeverCollectedField` fails if a field name from the
  never-collected list appears on the record.

One naming subtlety is fixed here: the record's `path` field is the bounded
enum `local` or `remote` — which side ran the call — and is export-safe. It
is not a filesystem path; the banned list therefore names `file_path`,
`source_path`, and similar, never a filesystem sense of a bare call-path
enum.

Nothing else is decided or built by this ADR:

- No exporter, network endpoint, consent storage, or opt-in prompt exists,
  and none is authorized by this record. Plan 002's non-goals stand.
- If an exporter is ever proposed, it is a pure function over export-safe
  fields only. Per plan 002 it would replace `instance_id` with a rotating
  per-install pseudonym, bucket numbers (rows, bytes, durations) into the
  plan 001 histogram bounds, drop values seen fewer than k times in the
  export window, and send nothing unless the user has opted in. That
  exporter needs its own ADR and a consent design before a line of it is
  written.

The `reads` and `functions` values are bounded by fixed catalogues
(`ReadCatalogue` from the hub schema, `FunctionCatalogue`); anything outside
a catalogue maps to the single value `other`, so cardinality is bounded by
construction, not by filtering later.

## Consequences

Adding a field to the usage record now forces two conscious acts: giving it
a class and surviving the classification test. An exporter written years
later reads the class map instead of re-litigating each field, and a
reviewer of that exporter checks one property — it touches export-safe
fields only. Local-only identifiers (`time_ms`, `instance_id`, `device_id`,
`session_hint`) are what make `ohya usage` able to join device-side and
hub-side records of the same call (plans/002, "Usage record"); they stay
local because the join is local.

The frozen format versions of this milestone live in `internal/observe`:
snapshot payload 1 (also the `history.jsonl` sample payload), log entry 1,
usage record 1. Readers must tolerate unknown fields (golden fixtures in
`internal/observe/testdata/` pin this) so additive change needs no version
bump; changing a field's meaning or class requires a version bump.
