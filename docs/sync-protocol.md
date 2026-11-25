# Sync wire protocol

This document specifies the binary frame protocol between a device and the
hub. `internal/hubsync` implements it, and `internal/uploader` and
`hubsync.RemoteSession` speak it from the device side. ARCHITECTURE.md,
"Sync contract", states the guarantees; `tla/Sync.tla` and `tla/Auth.tla`
model them.

## Transport

The frames ride the Tailcat address transport ([ADR 0011](adr/0011-tailcat-address-transport.md)):
the hub listens on TCP port 9027 inside the transport's network stack and
opens no host port, and each device process dials the hub address on its
own with a fresh transport key. Admission is the pre-shared key inside the
hub address — only a holder of the address completes a handshake — while
authorization stays in the device registry (`tla/Auth.tla`). A join code
(`ohya1.<invitation>.<tailcat-address>`) carries both halves to the device,
which saves the address in its join state next to `credential.json`. A
device that re-presents a join code proves its credential with one hello
and one read-only query before it uses the invitation; no new frame or
state transition exists for that probe ([ADR 0012](adr/0012-two-step-multi-machine-setup.md)).

## Frames

Every frame starts with a one-byte marker and the protocol version byte. The
version is 1. A frame with another version is refused; there is no
negotiation. All integers are big-endian. A length prefix of `n` bytes is
written as `name(n)`.

```text
registration = 'R' ver(1) devLen(2) dev srcLen(2) src agentLen(2) agent pathLen(4) path
batch        = 'B' ver(1) devLen(2) dev srcLen(2) src start(8) end(8) count(4)
                 count * [ offset(8) len(4) json ]
zbatch       = 'Z' ver(1) devLen(2) dev srcLen(2) src start(8) end(8) count(4)
                 rawLen(8) compLen(4) payload
caps         = 'V' ver(1) caps(2)
welcome      = 'v' ver(1) caps(2)
ack          = 'A' ver(1) status(1) cursor(8) inserted(8) stopOffset(8) stopLen(2) stop
error        = 'E' ver(1) code(1) msgLen(2) msg
hello        = 'H' ver(1) credLen(2) cred
pair         = 'P' ver(1) invLen(2) inv devLen(2) dev
credential   = 'C' ver(1) devLen(2) dev credLen(2) cred
query        = 'Q' ver(1) sqlLen(4) sql
queryResult  = 'S' ver(1) jsonLen(4) json
annotate     = 'N' ver(1) reqLen(4) json
annotateRes  = 'K' ver(1) applied(8) unchanged(8)
workspaces   = 'W' ver(1) devLen(2) dev agentLen(2) agent count(2)
                 count * [ pathLen(2) path repoLen(2) repo ]
pending      = 'D' ver(1) applied(8) unchanged(8) ignored(8) count(2)
                 count * [ pathLen(2) path ]
sourceWorkspace = 'F' ver(1) devLen(2) dev srcLen(2) src pathLen(4) path
```

- `registration` announces one source before its first batch: the source ID
  that the device persisted, the agent whose parser owns the records, and the
  device-local path. The hub maps it onto `store.RegisterSource`, which binds
  the source to the authenticated device.
- `batch` carries one source's contiguous records. `start` is the first
  record's offset, which is the device checkpoint when the batch was built.
  `end` is the offset just past the last record's terminator. Each record is
  the JSON text without its terminator; its end is `offset + len + 1`. A
  batch never splits a record. A batch with no records is a cursor probe,
  and its `start` must equal its `end`.
- `zbatch` is the same batch with its record section zstd-compressed
  ([ADR 0014](adr/0014-zstd-compressed-batch-upload.md)). The header is the
  plain batch header; `payload` is one zstd stream (encoder level 3, frame
  CRC on) over exactly the bytes a plain batch carries after its header —
  `count` times `offset(8) len(4) json` — and `rawLen` is that section's
  uncompressed length. The writer sends `zbatch` only when compression wins
  and only after the hub's `welcome` offered it; probes and incompressible
  batches travel as plain `batch` frames. The hub decompresses before
  validating the records: `rawLen` must fit the maximum plain record section
  (64 MiB of records plus 12 framing bytes per record), `compLen` must fit
  the 64 MiB batch bound, decompression allocates exactly `rawLen` and fails
  deterministically on any expansion past it, and the decoded section then
  passes the plain batch's per-record, total, and exact-consumption checks.
- `caps` offers a 16-bit capability mask as the first request frame after a
  successful `hello`; `welcome` answers with the mask the hub honors, the
  intersection of the offer and the hub's support. Bit 0 (`0x0001`) is zstd
  batch compression; bit 1 (`0x0002`) is remote hub status aggregates
  (`CapRemoteStatus`, advertised only when the hub enables its diagnostics
  provider); bit 2 (`0x0004`) is the `sourceWorkspace` frame
  (`CapSourceWorkspace`). The mask must be nonzero, and a hub supporting
  nothing offered answers with zero. An old hub answers `caps` with `CodeMalformed` like any
  unknown marker, which is how a device detects it and falls back cleanly; an
  old device sends no `caps` frame and is unaffected. The uploader also sends
  one `caps` frame as a keepalive on an idle connection ("Device side").
- `diagnostics` (`G`, 1-byte format version `1`) requests cached hub aggregate
  status on a short-lived authenticated connection after negotiating
  `CapRemoteStatus`. `diagnosticsRes` (`g`) replies with at most 256 KiB of
  JSON carrying hub build identity, the hub's own device ID (optional; a hub
  that predates the field omits it), monotonic uptime, cached snapshot timestamp,
  storage file sizes (without filesystem paths), resource readings, and
  configured bounds — never logs, source details, other devices, SQL, or
  transcripts. Per-frame authorization and revocation apply; the hub enforces
  a 5 s deadline, 2 concurrent diagnostic requests globally, and a 10 s
  per-device rate limit (`CodeBusy` on excess).
- `ack` acknowledges a registration, a source workspace, or a batch. The hub
  writes it only after the durable commit. `cursor` is the committed cursor,
  and `inserted`
  counts only new events, so a replayed batch reports 0. `status` 1 means
  the parser refused a record: the valid prefix before `stopOffset`
  committed, and `stop` holds the parser's diagnostic.
- `hello` presents a paired device's credential. `pair` presents a pairing
  invitation and the device's stable ID. The hub answers a successful pairing
  with `credential`, exactly once.
- `query` carries one SQL statement. `queryResult` carries the bounded result
  as JSON, in the same shape the local `sql` tool returns.
- `annotate` carries the `annotate` tool's JSON arguments
  (`{"operations":[...]}`) verbatim. No writer travels in the frame; the hub
  uses the authenticated device. `annotateRes` carries the committed
  `applied` and `unchanged` counts.
- `workspaces` carries a device's workspace observations for one agent
  ([ADR 0009](adr/0009-workspace-repository-observations.md)): a session
  `cwd` and the normalized repository the device found there, or an empty
  `repo` for none. The count can be 0. The hub takes the writer from the
  authenticated device, stores each new observation write-once, and answers
  with `pending`: the counts, then up to 256 `cwd` values of that device and
  agent whose sessions have no repository and no observation yet. The hub
  ignores an observation for a path that is not a `cwd` of the device's
  sessions, and rejects the frame when a `repo` is not in normalized form.
- `sourceWorkspace` carries the workspace of one registered source
  ([ADR 0018](adr/0018-device-observed-source-workspace.md)): the device, the
  source ID, and a path with the bound of a registration path. The hub takes
  the device from the authenticated connection, and the frame must name it.
  The hub maps the frame onto `store.SetSourceWorkspace`, which stores the
  first path of a source and ignores each later path, and answers with `ack`.
  A device sends the frame only after the hub's `welcome` offered
  `CapSourceWorkspace`, so the supported upgrade order is hub first: a hub
  without the bit gets no frame, and the sessions of the device that carry
  no working directory stay without one until the hub is upgraded.

## Bounds

Every declared length is checked against its bound before the reader
allocates a buffer, so a hostile or broken frame cannot make the peer
allocate without limit. A frame past its bounds cannot be resynchronized, so
the connection closes.

| Field | Bound | Reason |
|---|---|---|
| Device and source IDs | 64 bytes | IDs are 32 hex characters |
| Agent name | 64 bytes | Agent names are short slugs |
| Path | 4,096 bytes | One filesystem path |
| Error and stop text | 1,024 bytes | Never carries transcript content |
| Invitation and credential | 128 bytes | Secrets are 32 hex characters |
| Record | 32 MiB | The parser's own record limit |
| Batch record bytes | 64 MiB | Holds the largest record and its newline (ADR 0004 amendment) |
| Batch record count | 65,536 | Bounds normalization work for tiny records |
| Uncompressed zbatch section | 64 MiB + 12 B per record | The plain record section's own maximum, given the validated count (ADR 0014) |
| Compressed zbatch payload | 64 MiB | The batch bound; honest writers send plain when compression does not shrink |
| Query text | 64 KiB | The `sql` tool's input limit |
| Query result | 4 MiB | 16 times the engine's default output bound |
| Annotate request | 1 MiB | The `annotate` tool's request limit |
| Workspaces per frame | 256 | Both in `workspaces` and in `pending` |
| Repository | 1,024 bytes | A normalized `host/path`; it can be empty |

The writers refuse to build a frame past these bounds. An engine configured
past the query result bound gets `CodeInternal` instead of a cut payload.
String fields with a 2-byte length must not be empty, except `repo` in `workspaces`.

## Error codes

| Code | Value | Meaning | Connection |
|---|---|---|---|
| `CodeAuth` | 1 | The peer is not a paired device, or a frame names another device | Closes |
| `CodeVersion` | 2 | Unsupported protocol version | Closes |
| `CodeMalformed` | 3 | Undecodable frame | Closes |
| `CodeTooLarge` | 4 | A record or batch above its bound | Closes |
| `CodeRejected` | 5 | Deterministic rejection: a gap ahead of the cursor, conflicting content, invalid boundaries, a purged source ([ADR 0013](adr/0013-owner-scoped-session-purge.md)), a source workspace that fails its checks, or a denied query | Stays open |
| `CodeOwnership` | 6 | The source belongs to another device | Stays open |
| `CodeUnknownSource` | 7 | The hub never registered the source | Stays open |
| `CodeUnsupported` | 8 | The hub has no parser for the agent | Stays open |
| `CodeBusy` | 9 | The hub is at its connection bound, or all query slots are taken | Retry later |
| `CodeInternal` | 10 | A hub-side failure that survived retries | Retry later |
| `CodeNoRemote` | 11 | The endpoint serves uploads only | Stays open |

## Hub endpoint

- The first frame of a connection is `hello` or `pair`, read under a 30 s
  authentication timeout, so a silent stranger cannot hold a slot. The auth
  reader reads exactly the frame's bytes. Frames inside a connection never
  establish an identity; the `Authenticator` derives it.
- The endpoint serves at most 16 connections by default. The next one gets
  `CodeBusy` and closes.
- The hub closes a connection that sends no frame for 5 minutes
  (`hubsync.DefaultIdleTimeout`). The read deadline is set before each frame.
- Frames on one connection are strictly sequential, so a connection holds at
  most one decoded frame in memory. With a `SessionAuthorizer`, the endpoint
  checks the registry before it processes each frame. A revocation closes the
  device's connections and cancels their requests; data already returned
  cannot be recalled. If the revocation watch fails, the per-frame check
  stays in force.
- A per-source gate keeps one batch in flight per source across all
  connections. A second uploader of the same source waits and validates
  against the first commit.
- A `caps` frame is answered by one `welcome` with the offered capabilities
  this hub honors, and holds no connection state: a `zbatch` is
  self-describing and bounded per frame, so the hub accepts it on any
  connection. Decompression failures are deterministic decode errors —
  `CodeTooLarge` for a declared or expanded size past its bound,
  `CodeMalformed` for a corrupt stream, a length mismatch, or trailing
  bytes — and close the connection; the hub never allocates beyond the
  declared uncompressed length (ADR 0014).
- Before parsing, the endpoint checks that the records are contiguous, start
  at the batch start, and end at the batch end. Normalization uses
  `events.Parse` with the record bound passed explicitly, so it cannot drift
  from the wire bound. Each framed record must parse to exactly one record at
  its framed boundaries; JSON with an embedded line terminator fails this
  check. A batch that holds offset 0 supplies the first native ID for the
  rest of the batch.
- Transient write failures, such as `database is locked` while another
  process holds the write lock for longer than the busy timeout of 5 s,
  retry with the hub policy: 3 attempts, waits from 100 ms doubling up to
  2 s, no jitter. A store transaction waits for the lock first, so a held
  lock fails a batch after about 15 s.
  Deterministic rejections (`ErrBatchRejected`,
  `ErrSourceOwnership`, `ErrSourceNotRegistered`, `ErrSourcePurged`) never
  retry. A purged source's registration and batches are answered with
  `CodeRejected` and the refusal text names the purge date and the
  `ohya sources reallow` remedy; the connection stays open, so the device's
  other sources are unaffected.
- A `workspaces` frame that names another device closes the connection, like
  a batch. An unknown agent gets `CodeUnsupported` and a value that is not
  in normalized form gets `CodeRejected`; both stay open.
- A `sourceWorkspace` frame that names another device closes the connection,
  like a batch. A source that the hub never registered gets
  `CodeUnknownSource`, a source of another device gets `CodeOwnership`, and a
  purged source or a path that fails the workspace checks gets
  `CodeRejected`. For these three the connection stays open.
- Query and annotate outcomes are always answered in band, and the connection
  stays open. Policy and input rejections map to `CodeRejected`, contention
  to `CodeBusy`, and other failures to `CodeInternal`.
- Cancelling `Serve` closes every connection and waits for the handlers. A
  batch in flight either commits or leaves the cursor unchanged.

## Device side

The uploader (`internal/uploader`) implements the `SendBatch` and
`DeliverAck` actions of `tla/Sync.tla`:

- A batch always starts at the checkpoint. One connection carries strictly
  sequential exchanges, so each source has at most one batch in flight.
  Only an acknowledgment moves the checkpoint; a timeout, reset, or lost
  acknowledgment leaves it, and the next pass sends the same bytes. The
  move happens in memory immediately and reaches disk once, at the end of
  the `Sync` call that made it — a crash before that flush loses only the
  unflushed acknowledgment, equivalent to one that never reached the
  device, and the retained file replays it.
- Offline catch-up needs no queue, because the retained source files are the
  backlog. On every new connection, the uploader prepares each source once. A
  source with an acknowledged checkpoint is probed first with an empty batch.
  The hub cursor in the answer shows that the hub knows the source, and no
  registration follows. `CodeUnknownSource` means that the hub has no row for
  the source, for example after the hub was set up again: the uploader
  registers the source and probes again. A source without a checkpoint, and a
  file that was renamed, is registered first and probed after the
  acknowledgment. A hub cursor ahead of the checkpoint is a lost
  acknowledgment and is persisted. A hub cursor behind it is a regression,
  for example after a backup restore: `watch.Rewind` replays the retained
  file, or the uploader reports an unrecoverable gap when the file is gone or
  changed. A `CodeRejected` gap during a pass triggers a new probe before the
  uploader declares a conflict.
- A record above 32 MiB, or an incomplete tail already above it, stops the
  source locally, because sending it would close the connection. A parse
  stop is not sent again until the file grows. A source refused for a
  reason retrying cannot fix is blocked for the rest of the process; a
  changed file is a new source and starts clean. A purged source is exactly
  such a refusal: the hub's message names the purge and the remedy, the
  uploader blocks that one source and keeps uploading the others, and the
  device's checkpoint and file stay untouched
  ([ADR 0013](adr/0013-owner-scoped-session-purge.md)). After the owner
  re-admits the source, the next registration succeeds and the probe finds
  the hub cursor at the purge point, so history resumes from there and the
  purged bytes are never replayed.
- The uploader reports bytes it can see were lost before upload: a replaced
  file whose observed end passed its acknowledged cursor, and a deleted file
  with unsent bytes. Each loss and gap is reported once.
- After the hello, the uploader offers its capabilities with one `caps`
  frame (zstd batches and source workspaces) and compresses data batches
  with `zbatch` while the hub's `welcome` offers it
  ([ADR 0014](adr/0014-zstd-compressed-batch-upload.md)). A hub
  that refuses the offer with `CodeMalformed` or `CodeVersion` is old: the
  uploader marks it plain for the rest of the process, reconnects once with
  a plain hello, and never offers again. The capability changes no
  checkpoint, batch, or acknowledgment behavior — a compressed batch is the
  same `SendBatch` action with a different encoding. Because only a new hub
  answers `welcome` and only a new device asks, the supported upgrade order
  is hub first: devices then pick up compression as they are upgraded, and
  no mixed pair ever exchanges a compressed frame. `ohya mcp --remote` and
  pairing send the plain hello and no `caps` frame; remote queries and
  annotate stay plain in protocol version 1. Setting the environment
  variable `OHYA_UPLOAD_COMPRESSION=off` in the joining process removes the
  zstd bit from the `caps` offer for its lifetime, so the device sends plain
  frames even against a compression-capable hub — the escape hatch for
  diagnosis and A/B measurement. A process that reports no source workspace
  then sends no `caps` offer at all, the same behavior an old device shows.
- When the joining process has a workspace lookup
  (`uploader.Options.SourceWorkspace`), the uploader looks the workspace of
  a source up once per connection, when the hub confirms the source. A
  source that the hub does not know is registered first, and the lookup
  follows the acknowledged registration. A source that the hub knows gets a
  cursor probe and no second registration, and the lookup follows that
  probe. In both cases the frame comes before the next batch. It sends one
  `sourceWorkspace` frame only when the hub's `welcome` offered
  `CapSourceWorkspace`. The hub's acknowledgment counts for the rest of the
  process, so a source is told once. A source without a workspace is looked
  up again on each new connection and never on each pass. A hub error for
  the frame is reported once per connection and never blocks the batches of
  the source. A transport error ends the pass like any other frame does. A
  hub that offers no `CapSourceWorkspace` gets no frame, and the uploader
  reports that once per process.
- Each exchange has a 60 s timeout. Failed passes reconnect with backoff from
  1 s doubling up to 1 min. `Run` stops only on `ErrUnauthorized` (the
  credential was refused, for example after revocation), `ErrIncompatible`
  (the hub refused the uploader's frames), a discovery failure, or lost local
  state. When the hub closes a connection after a refusal, the uploader reads
  the hub's error frame first, so it reports the hub's reason.
- Before each pass, a non-blocking read checks that an idle connection is
  still open. The hub never sends unsolicited frames, so a closed connection
  means a hub restart. This is how an idle device notices a restored hub.
- A connection that is still open after 3 minutes without an exchange
  (`uploader.DefaultKeepaliveInterval`) receives one `caps` frame before the
  pass, and the `welcome` proves that it is open. The hub closes an idle
  connection after 5 minutes, so the keepalive holds one connection open from
  pass to pass. An idle device sends one frame in 3 minutes. It does not
  register and probe each source at each pass, and a pass with nothing pending
  makes no other exchange. A keepalive that fails ends the connection, and the
  pass connects again with the backoff below. A hub that refuses the `caps`
  frame is old (see the capability bullet above): the uploader reconnects in
  the same way and sends no keepalive for the rest of the process.
- After it connects, and after a pass that inserted events, the uploader
  runs the workspace exchange: it sends its observations, resolves the
  returned paths with `workspace.Resolve`, and repeats until no new path
  resolves. A path it cannot read stays pending for a later exchange.
  Only normalized repositories leave the device, so remote credentials do
  not. An older hub answers the unknown `W` marker with `CodeMalformed` and
  closes the connection; the uploader then stops workspace exchanges for
  the process, reports it once, and keeps uploading.
- The checkpoint state must belong to one hub. Its cursors are that hub's
  acknowledgments, so a state directory that local ingestion also writes to
  would skip bytes the hub never received.

`hubsync.RemoteClient` runs query and annotate exchanges on one
authenticated connection, serialized like the uploader's exchanges, with a
60 s timeout per exchange. `hubsync.RemoteSession` backs `ohya mcp --remote`:
it dials and authenticates on first use and replaces a dead connection once
per call. A retry is safe, because a query is read-only and an identical
annotate batch converges to a no-op. A protocol rejection is returned as the
hub's answer, not retried. A refused credential is terminal
(`ErrRemoteUnauthorized`).
