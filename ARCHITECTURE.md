# ohya — Architecture

## Shape and scope

One Go executable, one hub-owned SQLite WAL transcript database, one read-only
SQL MCP tool. Keep parsing, normalization, persistence, synchronization, transport
and MCP at clear boundaries; introduce interfaces only where consumed.

Start locally on macOS with Claude Code and polling. Add Codex next. Pi agent
and OpenCode are planned phase 2 sources, pending parser spikes for their
session-log formats. Networking
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

## Deferred

Classification, filesystem notifications, extra platforms and indexed file-reference
tables require demonstrated need. No third-party AI processing is part of MVP.
