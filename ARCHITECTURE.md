# ohya — Architecture

## Shape and scope

One Go executable, one hub-owned SQLite WAL transcript database, two MCP tools: read-only
`sql(query)` and bounded `annotate(operations)`. Keep parsing, normalization, persistence, synchronization, transport
and MCP at clear boundaries; introduce interfaces only where consumed.

Start locally on macOS with Claude Code and polling. Codex and the Pi agent
are next; both write append-only JSONL session logs that fit the cursor model
(verified against installed versions 2026-11-13; see NOTES.md). Pi keeps
sessions globally under `~/.pi/agent/sessions/`, not in project-local `.pi/`.
OpenCode is a planned phase 2 source, pending a parser spike for its
SQLite storage (v2 replaced per-message JSON files with `opencode.db`). Networking
uses the isolated Tailcat address transport
([ADR 0011](docs/adr/0011-tailcat-address-transport.md)): the hub holds a
saved node key, pre-shared key, and relay region, and devices dial the
hub's address — the two peers meet through a DERP relay and try a direct
UDP path. There is no coordination server, no certificate, and no open
port. All transport code stays behind the `internal/transport` interfaces
consumed by the sync layer. See BACKLOG.md for implementation order.

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
commit. The advance happens in memory immediately; it reaches disk when the
caller calls `Flush`. `Acknowledge` and `Rewind` are no-ops when the cursor
does not move, so a stopped source that never advances costs no rewrite on
later passes. An existing but unusable checkpoint file stops the watcher with a
recovery error rather than a silent restart.

Local state files:

- `internal/durablefile.Write` replaces a file atomically. It writes a
  temporary file in the same directory, flushes it, renames it over the
  target, and then flushes the directory, so the completed rename survives
  a crash. A crash leaves either the old file or the complete new one. The
  target directory must exist, and the file is owner-only. Every local
  state file uses it.
- `device.json` (format version 1) holds the Device ID: 16 bytes from
  `crypto/rand`, written as 32 lowercase hex characters. `device.Load` reads
  it without creating anything, for `ohya doctor`. An unusable file is
  `ErrUnusable`. The remedy is to restore the file, or to delete it and
  deliberately start over as a new device. A silent replacement would fork
  the identity and duplicate its sessions.
- `credential.json` (format version 1) holds the hub credential, a secret of
  at most 128 bytes (the wire bound in `internal/hubsync`), and the Device
  ID it belongs to. It authenticates the identity; it does not create one.
  A missing file means the device has not paired. An unusable file is
  `ErrCredentialUnusable`. Never delete it to re-pair: the hub registry
  already holds the identity as paired and refuses a new invitation for
  it. Restore the file, or have the hub revoke the identity and pair a new
  one.
- `sources/<agent>.json` (format version 1) maps each watched path to its
  checkpoint: source ID (32 lowercase hex characters from `crypto/rand`),
  agent, platform file identity, acknowledged cursor, observed size, and
  the fingerprint window and SHA-256 digest (both empty while the cursor is
  0). Agent names match `^[a-z0-9][a-z0-9._-]{0,63}$`, because they become
  file names. `watch.InspectSources` reads the file without polling or
  writing, for `ohya doctor`. An unusable file is `ErrStateLost`: restore
  it, or delete it to re-import every source under new source IDs. The
  re-import is retry-safe but duplicates history, because deduplication
  keys cannot merge two incarnations.

Watcher rules (`internal/watch`):

- The platform file identity is the device and inode numbers on Unix. An
  empty identity means the platform has none; then size and fingerprint
  are the only signals. Paths are normalized by resolving symlinks in the
  parent directories (macOS links `/tmp` to `/private/tmp`), so a file
  keeps one key whether or not it currently exists.
- A tracked path starts a fresh source when its identity changed, when the
  file is smaller than the acknowledged cursor, or when the fingerprint of
  the window ending at the cursor no longer matches. The outcome carries the
  reason and the replaced source with its cursor: bytes past that cursor
  were never acknowledged, so an uploader that had not sent them lost them.
  A fingerprint window that cannot be read right now keeps the checkpoint;
  `ReadPending` surfaces a real read failure.
- The fingerprint read runs only when the platform identity, size, or
  modification time changed since the watcher's previous poll. The watcher
  keeps the observed triple in memory only, so a restarted watcher
  re-verifies every source once. An in-place rewrite that restores the
  exact size and modification time of the previous poll is therefore
  invisible until a restart — the same accepted edge as the ingestor's
  pending-tail cache. Local ingestion also calls `RegisterSource` only for
  a source this process has not registered at that path: new, replaced, and
  moved sources, and every source once after a restart. A write-path
  `ErrSourceNotRegistered` drops that memory, so the next pass re-registers
  a source the hub lost, for example after a restored backup.
- A tracked incarnation that is live under another candidate path is
  rebound to that path. All moves are computed from the unchanged state and
  applied together, so two files that swap paths both keep their sources.
  A move whose target another staying or moving entry also claims is
  refused, and the check repeats until no target is claimed twice. A
  refused source stays missing with its checkpoint, and a later poll
  rebinds it once the conflict is gone.
- A path that is not observable is reported as missing, and its checkpoint
  stays. The consumers turn that outcome into one diagnostic line per
  disappearance — never a silent stop — and clear it when the file returns.
  Sources that are already missing when a process starts, with no
  unsent bytes known to that process, get one counted line per pass
  instead of one line each.
  A new path whose identity matches a source that is live at its own
  path is a hardlink and is not registered.
- `Rewind` moves a checkpoint back to an earlier cursor. It answers a hub
  cursor regression, for example after the hub restored a backup. It is
  refused unless the file still holds the same incarnation and the
  fingerprinted bytes, because only unchanged retained bytes can replay the
  lost history.
- A `Watcher` is not safe for concurrent use. One goroutine drives it, like
  the database writer.
- `Acknowledge` and `Rewind` update the in-memory checkpoint and mark it
  dirty; they do not write the state file. `Flush` saves it once if dirty,
  so a pass with many acknowledgments costs one rewrite, not one per
  source. `ohya ingest` flushes once after each pass's outcome loop; the
  uploader flushes once at the end of each `Sync` call, after every
  acknowledgment that call made, win or lose. A crash before a flush loses
  only the unflushed acknowledgment, exactly as if it had never reached
  the device — the next pass resumes from the older durable cursor and
  replays the unacknowledged bytes, which the hub's deduplication makes
  safe.

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
Do not silently skip bytes. Other sources continue. Recovery is user-owned and
the diagnostic states its remedy ([ADR 0004](docs/adr/0004-stopped-source-recovery.md)):
`ohya ingest --max-record-bytes` (default 32 MiB) raises the per-record bound, so a
size stop clears on the next pass from the checkpointed offset, and every other
stop needs the source file repaired at that offset; automatic skipping is rejected.

Every agent parser (`internal/claudecode`, `internal/codex`, `internal/pi`)
has the same contract:

- `Parse(data, base, Options)` reads bytes that start at absolute file offset
  `base`. It returns every complete `\n`-terminated record in order. A
  record's `End` is the offset just past its terminator. `Result.Cursor` is
  the `End` of the last accepted record, and Parse never consumes the bytes
  after it.
- On a stop, Parse returns the records before the bad line together with a
  `*ParseError`. The caller commits those records, advances the cursor, and
  shows the diagnostic. The reason is a fixed text and never quotes input,
  because JSON decoder errors can contain transcript characters.
- `RecordTooLarge` marks a stop that the size limit caused. The limit
  (`Options.MaxRecordBytes`, default 32 MiB) applies to the JSON text without
  the terminator. A record exactly at the limit passes. An incomplete tail
  that is already above the limit also stops the source, because an
  append-only file cannot make it valid.
- Each line must be valid UTF-8, because SQLite stores it as TEXT. Each line
  must be exactly one JSON object: `null`, arrays, and trailing content stop
  the source.
- Parsing is pure and deterministic, with no clock, network, or model input.
  `Raw` holds the exact line without its terminator. Numbers decode with
  `UseNumber`, so metadata keeps them as written. Metadata is compact JSON
  with sorted keys.
- A record of an unknown `type` gets kind `unknown`, with the observed type
  in metadata under `observedType`. No record is rejected for its type.
- A field of the wrong JSON type reads as absent. A timestamp that does not
  parse as RFC 3339 is an absent observation, not a stop.
- A record without a usable native session ID gets the synthetic ID
  `ohya-synthetic:` plus `Options.SourceScope` (the source ID). Native IDs
  are UUIDs and contain no colon, so they cannot collide with a synthetic
  ID. Without a source scope, such a record stops the source.
- Codex and Pi records do not name their session; the first record of the
  file does. A parse with `base > 0` therefore needs `Options.Native`, which
  is `Result.Native` of the parse that covered offset 0. Ingestion reads it
  from `store.SourceState.FirstNativeID`. A missing value returns
  `ErrNoNative`, a caller error and not a source stop. A later header never
  changes the established identity.
- Searchable text joins its parts in source order with newlines. A tool
  call contributes its name, a space, and the compact sorted-key JSON of its
  input. `ToolName` is set only when the record invokes exactly one named
  tool.

Each parser has a `FuzzParse` target. It checks contiguous boundaries,
verbatim raw text, a cursor on accepted boundaries, deterministic output, a
stop exactly at the cursor, prefix stability for any cut of the input, and
(for Codex and Pi) that a parse resumed at the cursor with the established
native ID reproduces the full parse.

`internal/events` maps parser records onto `store.Event` for local ingestion
and for hub-side batch normalization, so every path into the database
normalizes a record the same way. `events.Parse` wraps a parser stop in a
`*StopError` and dispatches on the agent name. `events.Derivers` binds each
parser to `store.ReDerive` with the same parse calls, so a re-derived row
equals a freshly ingested one. `internal/store/ingest_test.go` keeps a copy
of the Claude Code mapping, because an in-package store test cannot import
`internal/events` without an import cycle. Change both copies together. The
ingestion benchmarks in `internal/events` copy the 8 MiB batch bound of
`cmd/ohya` in the same way. The format notes under `testdata/<agent>/`
describe what each parser derives.

Implemented (T011): `internal/codex` parses Codex rollout files, and
`internal/events.Parse` selects the parser from the source's agent for both
local ingestion and the hub endpoint. Codex records carry no session field:
the `session_meta` at offset 0 names the thread, and every later record of
the source belongs to it (a first record without a thread id gives the
source a synthetic ID). A parse that starts past offset 0 takes the native
ID of the source's committed first record (`store.SourceState`), so a
restart, a split batch or a hub-side batch names the same session. A thread
spans several rollout files, and a resumed page re-enumerates ordinals after
a rollback instead of byte-replaying the earlier file's records: at a shared
ordinal the two files hold different records. Deduplication is keyed by
`(session, ordinal)` plus content — a byte-identical copy of a stored
original is stored as its duplicate: retained for retry validation, without
search text, and outside the aggregates — while a differing record at a
known ordinal is a distinct record and an original of its own, so nothing
the thread really wrote is suppressed. `event_msg` records mirror the
transcript and add no search text; `event_msg/user_message` marks the human
prompt for `sessions.first_prompt`, because Codex also sends injected
context as `user` messages. `ohya ingest` runs one watcher and checkpoint
file per agent; the Codex agent watches two default roots, `sessions/` and
`archived_sessions/` under `$CODEX_HOME` (or `~/.codex`), through that one
watcher, because Codex moves ended rollouts into `archived_sessions`. A
rollout moved between the two roots keeps its source and checkpoint — the
watcher rebinds by file identity — while watching all of `~/.codex` would
wrongly ingest `session_index.jsonl`, `history.jsonl`, and vendor JSONL,
so only the two named roots are watched.

Implemented (T037): `internal/pi` parses Pi agent session files, which are
global — one append-only JSONL per session under
`~/.pi/agent/sessions/<encoded-cwd>/` — never project-local `.pi/`. The
`session` header at offset 0 names the session (the same header-first rule
as Codex: parses past offset 0 take the id from the committed first record),
and the `<encoded-cwd>` directory encoding is lossy, so working-directory
identity comes from the header's `cwd` alone. Every user message is a fresh
prompt (the harness preamble is a separate `system` entry, which contributes
no search text). Assistant messages carry one usage object per response —
`input` excludes both `cacheRead` and `cacheWrite` — so token totals sum
directly with no message-id dedupe; a usage whose parts do not sum to
`totalTokens` counts nothing. The usage-row key is the `responseId`, or the
entry id when the provider emits none (verified 2026-11-14:
`openai-completions` records never carry one). Entries flush incrementally,
so partial files from failed model calls are ordinary input, not corruption:
the error assistant carries zero usage and no response id.

Each parser also carries an output version (`OutputVersion`), stamped per row
in `events.parser_version` and independent of the schema version: it versions
what the parser derives from one record, and any change to that output bumps
it in the same commit. Because the hub preserves every raw record, a bumped
parser re-derives stored rows inside the hub (`store.ReDerive`, the
`ohya rederive` command) in one transaction — normalized fields are rewritten
in place from `raw_json`, session aggregates, token totals and replay
duplicates are recomputed deterministically, and `events_fts` is rebuilt from
the content table — with no source-file access and no resync from devices.
Rows written by a newer parser than the running build are refused until the
binary is upgraded. See DB.md, "Re-derivation".

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

Implemented (T017): `internal/hubsync` serves this contract as a library on any
`net.Listener`, so the Tailcat adapter's listener plugs in without transport
code in the sync layer. One binary frame protocol, shared by the endpoint and
the future uploader, carries the registration (device, source, agent, path —
it maps onto `store.RegisterSource`, which binds the source to the
authenticated device) and the batch (version, device, source, byte range,
records with their offsets); acknowledgments carry the committed cursor, the
newly inserted count and, when the parser refused a record, the stop offset
and reason. Every declared length is checked against a bound before
allocation: 32 MiB per record, 64 MiB per batch, 65,536 records per batch. The
endpoint authenticates each connection through an `Authenticator` interface
and rejects any frame naming another device; the credential registry backs
the interface (T018, `internal/registry`), with a per-frame registry check
and revocation teardown through its `SessionAuthorizer` extension. `ohya
serve` (T034) plugs the endpoint into the Tailcat hub listener behind
exactly that authenticator. Normalization runs
the source agent's local parser and adapter (`internal/events.Parse`), so
hub-side normalization is byte-identical to local ingestion, and a record the
parser refuses commits its valid prefix and reports the stop, mirroring local
source-stop semantics. The write path's sentinels (`ErrBatchRejected`,
`ErrSourceOwnership`, `ErrSourceNotRegistered`) map to protocol error codes so
a device knows permanent rejections from transient ones; transient write
failures retry with capped backoff inside the hub. One in-flight batch per
source is enforced by a per-source gate across connections, and a bounded
connection count rejects the excess explicitly.
Implemented (T145): a device compresses its data batches as `zbatch` frames —
zstd at level 3 over the record section, carrying the uncompressed length —
after a per-connection capability negotiation (`caps`/`welcome`) that falls
back to plain frames against an older hub, so the upgrade order is hub first
([ADR 0014](docs/adr/0014-zstd-compressed-batch-upload.md)). The hub
decompresses before record validation with the allocation capped at the
declared length; corrupt or oversize decompression is a deterministic
protocol error, never an unbounded allocation. The capability changes no
batch/ack behavior, so the TLA+ sync model is unchanged.
[docs/sync-protocol.md](docs/sync-protocol.md) specifies the frames, their
bounds, the error codes, and the endpoint and uploader rules.

Implemented (T019): `internal/uploader` is the device side of this contract,
a library over a `Dialer` (for example `transport.Device.Dial`) and the
watcher's checkpoint state. One authenticated connection carries strictly
sequential exchanges, so each source has at most one batch in flight; a batch
always starts at the durable checkpoint and stays inside the protocol bounds,
and a record above the size limit stops its source locally instead of closing
the connection. Only a hub acknowledgment moves the checkpoint
(`watch.Acknowledge`): a timeout, a reset, a busy hub or a lost
acknowledgment leaves it in place, and `Run` reconnects with capped
exponential backoff. On every new connection each source is registered and
its hub cursor probed with an empty batch: a hub cursor ahead of the
checkpoint is a lost acknowledgment and is persisted without resending; one
behind it is a cursor regression, answered by `watch.Rewind` and a replay of
the retained file, or reported as an unrecoverable gap when the file is gone
or changed. Each pass first checks, without blocking, that an idle
connection is still open, so a hub restart is noticed even when nothing is
pending. The uploader's checkpoint state must be dedicated to one hub: its
cursors are that hub's acknowledgments. `ohya join` (T034) runs it, one
instance per agent over the Tailcat device node.

Implemented (T036): the same connection carries the device-facing read and
annotate path ([ADR 0003](docs/adr/0003-remote-query-annotate.md)). Two
request frames — a query (one SQL statement within the sql tool's 64 KiB
input bound) and an annotate batch (the tool's JSON arguments within its
1 MiB request bound) — are served through the endpoint's optional
`RemoteEngine`, which the `internal/mcpserver` engine implements, so the
remote path runs under the same read-only policy, the same row, byte,
deadline and concurrency bounds, and the same strict annotate decoding as
the stdio tools. Replies frame the bounded result exactly as the sql tool
shapes it (a 4 MiB wire bound; an engine configured past it produces an
explicit internal error, never a silently cut payload) or the committed
applied/unchanged counts. The annotation writer is the authenticated
device: the annotate frame carries no writer field at all, and the
per-frame registry check and revocation teardown of T018 gate queries and
annotate batches exactly as they gate registrations and batches — a
revocation that lands mid-query cancels the statement and closes the
connection before any result is delivered. Reads carry no ownership state
(`tla/Auth.tla`): every paired device reads all history. An endpoint built
without a `RemoteEngine` stays upload-only and answers these frames with an
explicit `CodeNoRemote`.

Workspace observations ([ADR 0009](docs/adr/0009-workspace-repository-observations.md))
give sessions a repository key that matches across devices, because one
project has a different `cwd` on each device. Parsers never read the
filesystem, so the device observes instead: the hub names the session
`cwd` values of that device that still lack a repository, the device
resolves each one from `.git` with `internal/workspace` (no git process,
only normalized URLs leave the device), and the hub stores the result
write-once in `workspaces` and fills the NULL `sessions.repo` values.
`ohya join` runs the exchange over the `workspaces` frame; `ohya ingest`
runs the same `workspace.Sync` loop against its own database.
Re-derivation reads the stored observations, so it stays file-free.

Implemented (T034; moved to the Tailcat address transport in T122): the
deployment commands. `ohya serve` runs the hub: on the first start it
creates the transport identity — node key, pre-shared key, and selected
DERP region — and saves it owner-only under `tailcat-hub/` in the state
directory, so the hub's address stays the same across restarts
([ADR 0011](docs/adr/0011-tailcat-address-transport.md)). The endpoint
listens on port 9027 inside the Tailcat network stack — the hub opens no
port on the host — behind the registry authenticator, with the bounded
engine attached as the `RemoteEngine`. `ohya devices invite` recomputes
the hub address from the saved identity and prints it with the registry
invitation as one `ohya1.<invitation>.<address>` code; the address holds
the pre-shared key, so the code is a secret and is never logged.
`ohya join` spends the invitation, pairs its identity, saves the hub
address in its join state next to `credential.json`, and runs one
uploader per agent from a state directory dedicated to the hub.
`ohya mcp --remote` speaks the same MCP stdio surface through
`hubsync.RemoteSession` (`internal/hubsync/session.go`): one authenticated
connection, the writer derived from the paired credential, reconnect and
one retry on transport failure, and a terminal error on a refused
credential. The end-to-end tests (`cmd/ohya/serve_e2e_test.go`,
`cmd/ohya/tailcat_join_test.go`) drive real serve, join, and remote-MCP
child processes over a local DERP relay.

Each device process dials the hub independently with its own fresh
transport key ([ADR 0011](docs/adr/0011-tailcat-address-transport.md)):
the registry credential, not the transport key, carries identity, so
`ohya join` and every `ohya mcp --remote` process connect with no shared
node and no owner lock. Transport admission is the pre-shared key inside
the hub address; authorization stays in the device registry. Hub
authentication, source ownership, commit ordering, and annotation writers
remain governed by the existing sync and authorization protocols.

Offline replay relies on retained source files. Replacement or deletion before
upload can lose uncommitted history; surface this limitation. There is no outgoing
spool or offline query replica in the MVP. Hub backup restoration may roll back
acknowledged history: detect cursor regression and replay retained files, or report
unrecoverable gaps. Durability assumes intact hub storage, not immunity to loss.
The uploader reports each loss it can see: a replaced, truncated or rewritten
file whose observed bytes passed its acknowledged cursor, and a deleted file
with unsent bytes. Bytes appended and lost between two polls stay invisible.

An owner purge adds one terminal state to a source
([ADR 0013](docs/adr/0013-owner-scoped-session-purge.md)): `ohya purge`
deletes whole sessions and every source that holds their events, one
transaction per source, and replaces each source row with a tombstone.
Registration and batches for a tombstoned source ID are refused
deterministically with the purge date and the `ohya sources reallow`
remedy — for any device, so the freed source ID cannot be re-registered —
while other sources on the same connection continue. Devices keep their
checkpoints and files; after the owner re-admits a source, it is restored
at its purged cursor and history resumes from that point, so purged bytes
never return through replay. `tla/Sync.tla` and `tla/Auth.tla` carry the
purged state, its refusal, and the re-admission rules.

Agents nominate garbage but never delete
([ADR 0016](docs/adr/0016-agent-marked-purge-scope.md)): they find it with
the read-only `sql` tool, mark it with `annotate` (namespace `tags`, key
`garbage`), and hand the owner the exact `ohya purge --marked
<namespace.key>` commands. The selector plans the sessions whose events
carry that mark from any writer; it changes selection only, not the
lifecycle above, so the models stay valid.

Model-check cursor advancement, retries, crashes and lost acknowledgments before
network sync implementation. State source-retention and durable-storage assumptions;
liveness assumes eventual reconnection and retries. Test implementation separately.
Done for this contract: `tla/Sync.tla` is the executable specification — TLC
checks its invariants and liveness property in CI through `make tla`, and
tla/README.md records the assumptions, the covered failure scenarios, and what
the model deliberately leaves to Go tests. Keep the specification in step with
any change to this contract.

## Authorization and SQL

For initial local use, stdio operates under the user's OS identity. Before enabling
remote access, persist a hub-owned device credential registry, separate from queryable
transcript tables. Deriving devices from sessions is insufficient authorization.
All paired devices may read all history and upload only their own sources in MVP.

Use one-time expiring pairing invitations and protected credential files. Check
revocation on every request; close existing connections and cancel active requests
on revocation. Already returned data cannot be revoked. Model-check authorization
and invitation consumption before shipping pairing; do not implement cryptography.
Done for this contract: `tla/Auth.tla` is the executable specification — TLC checks
its invariants (single-use invitations, no new session for a revoked device,
immovable source ownership, session-derived annotation writers) in CI through
`make tla`, and tla/README.md records the assumptions, the checked scenarios, and
the design decisions the model fixes: revocation is final for a device identity,
and orphaned sources stay locked rather than rebindable. Pairing implementation
(T018) follows this specification; keep it in step with any rule change here.

Implemented (T018): `internal/registry` owns the device credential registry as
its own SQLite database (`registry.db` in the owner-only state directory,
WAL, synchronous FULL, schema pinned by `PRAGMA user_version`) — a protected
store the transcript database and its `sql` tool can never read, so no
deny-list entry is needed. Invitations are 128-bit random secrets stored as
SHA-256 digests, one-time and expiring; consumption is one transaction that
consumes the invitation and admits only a stranger device, so a revoked
identity can never re-pair (re-admission means a new device identity). The
hub mints the device credential at pairing; the device keeps it in
`credential.json`, an owner-only durably replaced file that reports
unusable states instead of silently re-pairing. `ohya devices
invite|list|revoke` is the operator surface. On the wire, the first frame
of a connection is a pairing request (`P`) or a credential hello (`H`);
`internal/registry.Authenticator` backs the endpoint's `Authenticator`
interface, and its `SessionAuthorizer` extension carries the two
per-request rules: `Authorize` gates every request frame after it is read,
and `WatchRevocations` closes the revoked device's live connections and
cancels their in-flight request contexts (revocations from another process
reach the endpoint within the poll interval; a request that already
returned its data cannot be recalled). `ohya serve` (T034) runs the
endpoint behind exactly this authenticator; the registry decides admission
and revocation, and the transport only carries traffic.

Registry details (`internal/registry`):

- The schema version (1) lives in `PRAGMA user_version`. A database with
  another version, or with registry tables but no version, is refused, never
  silently upgraded, downgraded, or recreated.
- The state directory is owner-only (0700) and protects the write-ahead log
  files; `registry.db` itself is 0600. One writer connection serializes all
  operations, and across processes the 5 s busy timeout serializes them.
  Transactions are immediate (`_txlock=immediate`), so two racing consumes
  serialize on the write lock instead of deadlocking on a read-lock upgrade.
- Secrets are 16 random bytes, hex-encoded. Invitations expire after 15
  minutes by default. The secret is shown once at issue time.
- `Consume` is one transaction that consumes the invitation and admits the
  device, so no state exists where one happened without the other. Only a
  stranger device may consume an invitation.
- Each refusal has its own error, so the endpoint and the CLI can tell the
  cases apart: unknown, used, or expired invitation; device already paired,
  revoked, or unknown; already revoked; unknown credential.
- `Revoke` is one-way. The device's sources and annotation rows stay as
  history. `WatchRevocations` polls the registry (every second by default),
  because another process such as the operator CLI can revoke. It reports
  only devices revoked after the watch starts, because earlier revocations
  cannot hold connections. A transient read failure does not stop the watch.
- Transport admission is the pre-shared key in the hub address; a revoked
  device can still complete a handshake, and the registry rejects its hello.
  The registry alone decides admission and revocation.

SQL runs with a read-only connection, SQLite authorizer, one statement, execution
deadline, row/byte limits and bounded concurrency. Deny attachment, extension
loading, unsafe functions and state-changing pragmas. Verify driver support for
FTS5, authorizer and cancellation before selecting it. Output truncation is explicit.
Publish schema and core recall recipes in the MCP tool description; expose
specialized playbooks (garbage audit, analytics and review pagination) on
demand as MCP resources (`ohya://guides/garbage-audit`,
`ohya://guides/analytics-and-review`) so ordinary sessions do not pay for
them upfront; permit safe schema inspection through SQL. Credentials and
internal authorization state are not exposed.

Implemented in `internal/mcpserver` and served by `ohya mcp` on stdio (official
MCP Go SDK, pinned per ADR 0001). The initialize result identifies the server
as `ohya` with the build version from `internal/buildinfo` — the same source
`ohya version` prints — so every serving path (local stdio, remote stdio, and
the engine behind the sync frames) reports one build identity, never a
hardcoded constant. One statement per call is enforced by a local
scanner before execution, because the driver executes every statement in the
input and answers with the last one's rows. The authorizer deny-lists
`load_extension` and `fts3_tokenizer`, allow-lists the schema-inspection pragmas
(plus `data_version`, which FTS5 reads internally), and deny-lists named tables
through `EngineOptions.DenyTables`. The device-credential registry lives in its
own protected database (T018, `internal/registry`), so the tool's connection
cannot reach it and no deny-list entry is needed.
Defaults: 5 s deadline,
500 rows, 256 KiB of row JSON, 4 concurrent queries, 64 KiB query text; a
caller past the concurrency bound fails fast instead of queueing. The same
engine also serves paired devices remotely, through the endpoint's query
and annotate frames (T036, see the sync contract); the annotation writer
there is the authenticated device, never caller input. `mcpserver.New`
consumes this surface as a `Backend` interface (query plus writer-derived
annotate), so the local engine and the device-side `hubsync.RemoteSession`
(`ohya mcp --remote`, T034) attach to the same MCP tool handlers.

Engine details (`internal/mcpserver`):

- The authorizer allows `SELECT`, recursive CTEs, reads of tables outside
  `DenyTables`, the allow-listed pragmas (`collation_list`, `data_version`,
  `database_list`, `foreign_key_list`, `function_list`, `index_info`,
  `index_list`, `index_xinfo`, `module_list`, `pragma_list`, `table_info`,
  `table_list`, `table_xinfo`), and every function except `load_extension`
  and `fts3_tokenizer`. `fts3_tokenizer` rebinds tokenizer pointers, which
  is an execution primitive even on a read-only connection. Everything else
  is denied: writes, DDL, `ATTACH`, transactions, savepoints, virtual-table
  management, `REINDEX`, `ANALYZE`, and connection-state pragmas such as
  `query_only` or `busy_timeout`. The connection is also read-only, so a
  policy gap cannot become a write.
- Workaround: the driver (mattn/go-sqlite3 v1.14.53) does not export
  `SQLITE_RECURSIVE`, so the engine defines it as 33 from the bundled
  header. The recursive-CTE query test fails if a driver update renumbers
  it.
- SQLite reports authorizer denials with different texts per operation
  ("not authorized", "access to table.column is prohibited", "authorization
  denied" for `VACUUM`). The engine maps all of them to `ErrDenied`. Other
  sentinels are `ErrBusy` (retry later), `ErrMultipleStatements`,
  `ErrEmptyQuery`, and `ErrDeadline`.
- The statement scanner follows SQLite's lexical rules far enough to find
  terminators: quotes (with doubling), `--` and `/* */` comments, and
  bracketed identifiers never terminate a statement. Any token after the
  first terminator makes a second statement, so `select 1;;` is two.
- Output truncation drops whole rows, never parts of a row, and reports
  `row_limit` or `byte_limit`. BLOBs become base64 text, and non-finite
  floats become their names.
- Egress secret redaction: after the rows are read, every string cell
  passes the fixed pattern table in `internal/secrets`
  ([ADR 0015](docs/adr/0015-egress-secret-redaction.md)). A match becomes a
  kind-labeled marker such as `[redacted:github-token]`, the variable name of
  a matched assignment stays readable, and the result carries one additive
  `redactions` count (omitted when zero, so the wire shape is unchanged for
  clean results). The pass runs inside the engine, so the stdio tool, the
  remote query frames, and the operator recall commands all share it. It is
  best-effort risk reduction over untouched stored records, never a
  guarantee; `ohya secrets scan` reports the same table's findings per
  event, session, and pattern without ever printing matched text.
- The annotate handler decodes strictly: unknown fields and trailing data
  are refused, and pointer fields keep absent, null, and empty values apart.
  Content refusals wrap `ErrAnnotateRejected` before the database is
  touched. The tool advertises `readOnlyHint: false` and a destructive hint,
  because a delete discards a value; a retried batch converges.

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
annotation ownership in the authorization model before remote writes ship: done —
`tla/Auth.tla` pins the writer to the authenticated session, so a request can
never name another writer's row. Tests
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
