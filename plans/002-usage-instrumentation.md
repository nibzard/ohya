# Usage and friction instrumentation

Status: in delivery. The usage record (T092), the `ohya usage` report
(T094), ADR 0010 (T095), and the device-side client-outcome record for
`ohya mcp --remote` (T108) are implemented, delivered 2026-11-16 to
2026-11-17. The usage baseline (T099), the acceptance gate for plan 004,
is open. Priority P1. Planned against `d237c54`, 2026-11-16.

## Outcome

The maintainer can answer, from local evidence and without reading
transcripts by hand:

1. How often agents call `sql()` and `annotate()`, per agent, device, and
   week, and in how many sessions the tools were available but unused.
2. Which tables and columns each call reads. In particular, how often
   agents read `events.raw_json` (this decides the storage design in plan
   004).
3. Where agents meet friction: deadline hits, admission rejections, row or
   byte truncation, empty results, SQL errors by class, and large results.
4. How long each call takes on the hub, phase by phase, and as the agent
   saw it.

The design also lets a later release add anonymized, opt-in usage and
friction telemetry from other users without a schema break. This plan does
not implement any export, network code, or consent flow.

## Evidence that motivates this plan

Measured 2026-11-16 on the VM hub (see `NOTES.md`):

- The MCP server records no query telemetry. Plan 001 is still a proposal.
- The hub holds 8 `sql()` calls since 2026-11-13, all from Claude Code, in
  4 sessions on both devices. The tools were available in 26 sessions. No
  Codex call was found.
- 1 of 8 calls hit the 5 s deadline; 1 took 4 s; the rest took 19–942 ms.
  Results were up to 48 KB. None read `raw_json`.
- Diagnosing the idle ingest write rate needed `strace` and
  `/proc/<pid>/io`, because ingest publishes no I/O counters.
- `brainstorm.md` lists the open storage questions that need this data.

## Relation to plan 001

Plan 001 (runtime observability) is a prerequisite and stays the source of
truth for the recorder, snapshots, metrics, logs, retention, and overhead
budgets. This plan adds usage-specific records on top of plan 001's
recorder. Keep every plan 001 rule, including:

- No SQL text, literal values, paths, or transcript content in metrics or
  logs.
- No regex parsing of SQL to infer semantics.
- Instrumentation never changes a Batch, Acknowledgment, Cursor, parser
  outcome, authorization decision, or query result.

Order of delivery:

1. Plan 001 milestone 1 (recorder and schemas), extended with the usage
   record schema below.
2. Plan 001 milestone 2 (ingest, storage, upload, hub, SQL measurements),
   extended with usage records for `sql()` and `annotate()`.
3. Plan 001 milestone 3 (`ohya status`, `ohya metrics`).
4. This plan's `ohya usage` report.
5. Plan 001 milestone 4 (logs, `ohya diagnose`).
6. Plan 001 milestone 6 (overhead baseline).
7. Plan 001 milestone 5 (remote aggregates) last; usage questions do not
   need it, because remote queries run on the hub and the hub records them.

## Design

### Usage record

One record for each `sql()` or `annotate()` call, written by the engine
that runs the call. On a joined device the hub runs remote queries, so the
hub records them; the device records only its client-side outcome. The
hubsync query frame does not carry the MCP client name today, so the hub
records `client` as `unknown` for remote calls. The device-side record
keeps the client name, and `ohya usage` joins the two. Carrying the client
name over the wire needs a protocol version bump; defer it to plan 001
milestone 5.

Fields, each with a fixed classification (see "Field classes"):

| Field | Class | Notes |
|---|---|---|
| `schema_version` | export-safe | starts at 1 |
| `time_bucket` | export-safe | UTC, truncated to the hour |
| `time_ms` | local-only | exact UTC milliseconds |
| `instance_id` | local-only | plan 001 random instance ID |
| `tool` | export-safe | `sql` or `annotate` |
| `client` | export-safe | MCP `initialize` client name mapped to a fixed set: `claude-code`, `codex`, `pi`, `other`, `unknown` |
| `client_version` | export-safe | bucketed to major.minor, or `unknown` |
| `path` | export-safe | `local` or `remote` |
| `device_id` | local-only | the device that asked; never exported |
| `session_hint` | local-only | MCP session ID when present; never exported |
| `outcome` | export-safe | `ok`, `timeout`, `busy`, `sql_error`, `rejected`, `truncated_rows`, `truncated_bytes`, `cancelled` |
| `sqlite_code` | export-safe | primary SQLite result code number, never the message |
| `phase_ms` | export-safe | admission wait, reader acquisition, execute-to-first-row, iteration, total |
| `rows`, `result_bytes` | export-safe | exact locally; bucketed on export |
| `reads` | export-safe | set of `table.column` reads from the SQLite authorizer, limited to the fixed schema catalogue; anything else maps to `other` |
| `functions` | export-safe | set of SQL functions from the authorizer (`SQLITE_FUNCTION`), limited to a fixed catalogue, for example `bm25`, `snippet`, `json_extract`; others map to `other` |
| `fts` | export-safe | true when the statement reads an FTS5 table |
| `annotate_ops` | export-safe | count of operations by type, for `annotate` |

The engine already registers an authorizer
(`internal/mcpserver/query.go`, `RegisterAuthorizer(e.authorize)`). The
recorder collects `SQLITE_READ` and `SQLITE_FUNCTION` actions during
statement preparation. This gives exact column-access sets with no SQL
parsing and no SQL text.

### Storage of usage records

- Keep usage records in the plan 001 per-instance directory under
  `<state>/observe/`, as a bounded, rotated `usage.jsonl` (8 MiB per file,
  two rotated files). Never write them to the hub database: the hub
  database holds transcripts, and its schema changes need migrations.
- Write asynchronously through the plan 001 bounded queue. Count dropped
  records. No fsync.
- Keep 30 days locally, or less if the size limit is reached first.

### Field classes and the future export boundary

Every field carries one class, declared once in the schema definition:

- **export-safe:** operational facts with bounded cardinality. A future
  exporter may send these after bucketing.
- **local-only:** identifiers and exact values that stay on the machine.
- **never-collected:** SQL text, literal values, paths, repository names,
  transcript content, annotation values, raw error messages, credentials.
  The schema has no field for these.

A unit test fails if a field has no class, or if a field named in the
never-collected list appears. A later exporter is then a pure function
over export-safe fields only. It would:

- replace `instance_id` with a per-install random pseudonym that rotates;
- bucket numbers (rows, bytes, durations) into the plan 001 histogram
  bounds;
- drop values seen fewer than k times in the export window;
- send nothing unless the user has opted in.

Record this boundary in an ADR now. Do not implement the exporter, network
code, consent storage, or any user interface in this plan.

### `ohya usage` report

A read-only command that answers the outcome questions for a time window:

```sh
ohya usage --since 30d --json
```

It combines two sources:

1. **Server usage records** from `<state>/observe/`: authoritative for
   phases, outcomes, column reads, and sizes.
2. **Transcript evidence** already in the hub: agent tool calls to
   `mcp__ohya__sql` and `mcp__ohya__annotate` (Claude Code `tool_use` and
   `tool_result` pairs), and the Codex equivalent. It gives
   agent-observed latency, sessions where the tools were available but
   unused, and calls made before instrumentation existed.

Rules:

- Deterministic. No LLM, no network.
- Bounded: a default deadline and row limits; report partial results.
- Never print SQL text or transcript excerpts in the default output.
  `--local-detail` may print query text from transcripts, which are
  already local data, for the maintainer's own analysis.
- Verify the Codex transcript format for MCP tool calls with real
  fixtures before claiming "no Codex use". The 2026-11-16 scan found none,
  but a format mismatch is possible.

## Verification

- Golden fixtures for the usage record; unknown fields tolerated.
- Authorizer collection tests: statements that read `raw_json`, use
  `bm25`, join `sessions`, and fail during preparation.
- Secret-seeding test: seed secrets into SQL literals, annotation values,
  and transcript text; assert none appear in usage records or default
  `ohya usage` output.
- Classification test: every field has a class; no never-collected field
  exists.
- Overhead within plan 001 budgets (median ≤3% on query workloads).
- `ohya usage` on the live hub reproduces the 2026-11-16 manual numbers
  (8 calls, 1 timeout) from transcript evidence.

## Acceptance gate for plan 004 decisions

Collect at least 200 `sql()` calls from both Claude Code and Codex, over
at least 2 weeks, before deciding the storage design in plan 004. If
Codex use stays at zero, fix that first (plan 003).

## Non-goals

- No telemetry export, network endpoint, consent flow, or opt-in prompt.
- No new MCP tool and no change to `sql()` or `annotate()` responses.
- No embeddings or AI classification of queries.
