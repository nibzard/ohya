# ohya — Architecture

## Shape and scope

One Go executable, one hub-owned SQLite WAL transcript database, two MCP tools: read-only
`sql(query)` and bounded `annotate(operations)`. Keep parsing, normalization, persistence, synchronization, transport
and MCP at clear boundaries; introduce interfaces only where consumed.

Start locally on macOS with Claude Code and polling. Add Codex and the Pi agent
next; both write append-only JSONL session logs that fit the cursor model.
OpenCode is a planned phase 2 source, pending a parser spike for its
per-message JSON storage. Networking
uses an isolated Tailcat adapter after a feasibility spike; deployment and relay
requirements are not yet verified. See README.md for implementation order.

The hub also ingests its own files through the same transactional write path.
All writes use one writer. Readers use separate restricted connections.

## Domain and local state

- Device: stable random ID with separate authentication credentials.
- Source: one watched file incarnation, identified by a persisted random ID.
- Session: identity derived unambiguously from device, agent and native session ID.
- Event: one original complete JSONL record plus deterministic normalized fields.
- Cursor: byte position immediately after the last contiguous committed record.
- Batch: one source's contiguous records and their byte boundaries.
- Acknowledgment: the hub's durable committed cursor for that source.

Persist device identity, source IDs, file identity and acknowledged offsets in
small local state files using atomic replacement and durable writes. Persist a
new source ID before sending its first batch. The hub cursor is authoritative;
a lost acknowledgment or older local checkpoint causes safe replay.

Use platform file identity plus size and a saved content fingerprint at the
checkpoint to detect replacement or truncation. A detected change creates a new
source ID; a rename retaining identity does not. Arbitrary in-place rewrites are
outside the append-only source assumption. Copied files and new incarnations may
reimport history: deduplication guarantees retry safety, not semantic deduplication.
If local identity state is lost, report recovery needs instead of silently
claiming exactly-once ingestion.

The local state directory holds `device.json` — the stable random Device ID,
created once and never regenerated in place; an unreadable identity file is an
error, because a new ID would fork the device — and one checkpoint file per
agent, `sources/<agent>.json`, mapping each watched path to its source ID,
platform file identity, acknowledged cursor, and the SHA-256 fingerprint of the
bytes ending at that cursor (4 KiB window by default). Polling is caller-driven
(`internal/watch`): every poll takes the caller's discovered paths, rebinds a
source whose incarnation moved, and registers new sources durably before their
first ingestion. Two live paths can share one platform file identity through
hardlinks, for example from a hardlinking backup tool inside the watched tree:
the incarnation is watched once through the first path in path order, and a
rebind onto a path that another source already claims is refused — the refused
source stays missing with its checkpoint retained, so a move never overwrites
another source's acknowledged checkpoint. `ReadPending` re-verifies the incarnation before handing bytes
to ingestion, and `Acknowledge` advances the checkpoint only after a durable
commit. An existing but unusable checkpoint file stops the watcher with a
recovery error rather than a silent restart.

## Record handling

Parse complete newline-terminated records; retain an incomplete tail for the
next poll. Preserve the original JSON text without reserialization. A record can
contain several semantic activities: concatenate searchable text in source order,
keep compound details in metadata, and populate singular role/tool fields only
when unambiguous. Unknown valid records remain stored with kind `unknown`.

If native session identity is missing, use a source-scoped synthetic native ID
in a reserved namespace; do not guess or later merge sessions automatically.
Malformed complete lines and records above the configured size limit stop that
source at the offending offset and expose a diagnostic without transcript content.
Do not silently skip bytes. Other sources continue. Recovery requires repairing
the source or an explicit future recovery operation; automatic skipping is not MVP.

## Sync contract

A batch contains protocol version, device ID, source ID, start/end byte offsets,
and original records with start/end offsets. Bound record and batch bytes before
allocation; normalize at the hub using the same parser as local ingestion.

1. Authenticate the device and bind the source to that identity. Reject attempts
   to upload under another device or change source ownership.
2. Validate contiguous record boundaries and the batch end. Reject gaps ahead of
   the stored cursor. For overlapping retries, compare stored original records
   and boundaries; reject conflicting content at an existing offset.
3. In one transaction, insert only new records, update aggregates only for those
   inserts, maintain FTS, and advance `sources.cursor_bytes` over contiguous bytes.
4. Commit durably before acknowledging the current cursor. A fully repeated
   batch returns the existing cursor without changing aggregates.
5. Persist the acknowledgment locally. Retry after timeout or reconnect; never
   infer success from having sent a batch.

Allow one in-flight batch per source initially. No acknowledgment can move beyond
validated committed bytes. Incomplete tails, validation failures and rolled-back
transactions never advance the cursor. Bound queues and retry with capped backoff.

Offline replay relies on retained source files. Replacement or deletion before
upload can lose uncommitted history; surface this limitation. There is no outgoing
spool or offline query replica in the MVP. Hub backup restoration may roll back
acknowledged history: detect cursor regression and replay retained files, or report
unrecoverable gaps. Durability assumes intact hub storage, not immunity to loss.

Model-check cursor advancement, retries, crashes and lost acknowledgments before
network sync implementation. State source-retention and durable-storage assumptions;
liveness assumes eventual reconnection and retries. Test implementation separately.

## Authorization and SQL

For initial local use, stdio operates under the user's OS identity. Before enabling
remote access, persist a hub-owned device credential registry, separate from queryable
transcript tables. Deriving devices from sessions is insufficient authorization.
All paired devices may read all history and upload only their own sources in MVP.

Use one-time expiring pairing invitations and protected credential files. Check
revocation on every request; close existing connections and cancel active requests
on revocation. Already returned data cannot be revoked. Model-check authorization
and invitation consumption before shipping pairing; do not implement cryptography.

SQL runs with a read-only connection, SQLite authorizer, one statement, execution
deadline, row/byte limits and bounded concurrency. Deny attachment, extension
loading, unsafe functions and state-changing pragmas. Verify driver support for
FTS5, authorizer and cancellation before selecting it. Output truncation is explicit.
Publish schema and examples in the MCP tool description; permit safe schema
inspection through SQL. Credentials and internal authorization state are not exposed.

Implemented in `internal/mcpserver` and served by `ohya mcp` on stdio (official
MCP Go SDK, pinned per ADR 0001). One statement per call is enforced by a local
scanner before execution, because the driver executes every statement in the
input and answers with the last one's rows. The authorizer deny-lists
`load_extension` and `fts3_tokenizer`, allow-lists the schema-inspection pragmas
(plus `data_version`, which FTS5 reads internally), and deny-lists named tables
through `EngineOptions.DenyTables`; the device-credential registry required for
remote access must be added there before it ships — note the table deny-list is
data-level, so credential column names should not be sensitive either.
Defaults: 5 s deadline,
500 rows, 256 KiB of row JSON, 4 concurrent queries, 64 KiB query text; a
caller past the concurrency bound fails fast instead of queueing.

## Deferred

Built-in classification, filesystem notifications, extra platforms and indexed file-reference
tables require demonstrated need. No third-party AI processing is part of MVP.

## Agent annotations

Agents may classify records, attach tags or record security-review findings through
`annotate(operations)`. Intelligence remains in the calling agent; ohya runs no
classifier. Annotations are untrusted interpretations, never ingestion facts.

Each operation supplies `op` (`upsert` or `delete`), `event_id`, `namespace`, and
`key`; upsert additionally supplies a JSON `value`. The hub derives `writer_id`
from the authenticated device (local stdio uses the local Device ID), never from
caller input. All authorized devices may annotate any event, but may update or
delete only their own annotations. Agents on one device share that identity;
use distinct namespaces for independent workflows. This is not agent authentication.

The annotation identity is `(event_id, namespace, key, writer_id)`. Upsert replaces
the complete value. Delete of an absent annotation succeeds. Identical upserts
are no-ops, including timestamps; changed values use hub timestamps and preserve
creation time. Concurrent writes to the same identity use last committed value;
there is no revision history or compare-and-swap in MVP. A delayed retry can replace
a newer value, so callers must serialize writes to each identity.

Accept at most 100 operations and 1 MiB per request, with at most 64 KiB of UTF-8
JSON per value. Namespaces and keys are 1–128 ASCII characters from letters,
digits, `_`, `-`, `.`, `/`. Reject unknown fields, duplicate identities in a batch,
invalid JSON, missing events and oversized input before committing anything.
Use parameterized statements through the hub writer. Commit the entire batch
atomically and return success only after durable commit. Report applied and
unchanged operation counts; failure applies nothing. No arbitrary SQL or changes
to events, sessions, sources, cursors or FTS are permitted through this tool.

Check authorization/revocation for annotation requests as for queries. Include
annotation ownership in the authorization model before remote writes ship. Tests
must cover atomic failure, limits, foreign keys, writer spoofing, cross-writer
isolation, retry behavior and unchanged raw records. Annotations are queried via
`sql`; no extra read tool or annotation FTS index is needed initially.

For bulk reviews, page events by ID, inspect them, then persist a versioned review
result for each event, including explicit `reviewed` status and an empty findings
list when appropriate. Resume by querying missing/version-mismatched results.
Store redacted locations/descriptions of suspected secrets rather than copying
secret values into annotations. A finding does not redact the original transcript.

Implemented in `internal/mcpserver` (tool surface, strict decoding, request
limits) and `internal/store` (`Annotate`, the transactional write path on the
hub writer), served by `ohya mcp` next to `sql`. The tool's input schema and
the handler's strict decoder reject unknown fields together, so a caller
cannot supply `writer_id`; the hub always writes the local Device ID. Values
are stored as compact JSON text, so "identical" means equal compacted JSON:
a retry with different whitespace is still a no-op. The result counts
`applied` operations (insert, replacement, delete that removed a row) and
`unchanged` ones (identical upsert, delete of an absent annotation); an empty
batch is accepted and reports zeros. A delete carrying a `value` is rejected,
and `updated_at_ms` never moves below `created_at_ms`, even across a hub
clock step backwards.
