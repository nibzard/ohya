# Sync wire protocol

This document specifies the binary frame protocol between a device and the
hub. `internal/hubsync` implements it, and `internal/uploader` and
`hubsync.RemoteSession` speak it from the device side. ARCHITECTURE.md,
"Sync contract", states the guarantees; `tla/Sync.tla` and `tla/Auth.tla`
model them.

## Frames

Every frame starts with a one-byte marker and the protocol version byte. The
version is 1. A frame with another version is refused; there is no
negotiation. All integers are big-endian. A length prefix of `n` bytes is
written as `name(n)`.

```text
registration = 'R' ver(1) devLen(2) dev srcLen(2) src agentLen(2) agent pathLen(4) path
batch        = 'B' ver(1) devLen(2) dev srcLen(2) src start(8) end(8) count(4)
                 count * [ offset(8) len(4) json ]
ack          = 'A' ver(1) status(1) cursor(8) inserted(8) stopOffset(8) stopLen(2) stop
error        = 'E' ver(1) code(1) msgLen(2) msg
hello        = 'H' ver(1) credLen(2) cred
pair         = 'P' ver(1) invLen(2) inv devLen(2) dev
credential   = 'C' ver(1) devLen(2) dev credLen(2) cred
query        = 'Q' ver(1) sqlLen(4) sql
queryResult  = 'S' ver(1) jsonLen(4) json
annotate     = 'N' ver(1) reqLen(4) json
annotateRes  = 'K' ver(1) applied(8) unchanged(8)
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
- `ack` acknowledges a registration or a batch. The hub writes it only after
  the durable commit. `cursor` is the committed cursor, and `inserted`
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
| Query text | 64 KiB | The `sql` tool's input limit |
| Query result | 4 MiB | 16 times the engine's default output bound |
| Annotate request | 1 MiB | The `annotate` tool's request limit |

The writers refuse to build a frame past these bounds. An engine configured
past the query result bound gets `CodeInternal` instead of a cut payload.
String fields with a 2-byte length must not be empty.

## Error codes

| Code | Value | Meaning | Connection |
|---|---|---|---|
| `CodeAuth` | 1 | The peer is not a paired device, or a frame names another device | Closes |
| `CodeVersion` | 2 | Unsupported protocol version | Closes |
| `CodeMalformed` | 3 | Undecodable frame | Closes |
| `CodeTooLarge` | 4 | A record or batch above its bound | Closes |
| `CodeRejected` | 5 | Deterministic rejection: a gap ahead of the cursor, conflicting content, invalid boundaries, or a denied query | Stays open |
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
- Frames on one connection are strictly sequential, so a connection holds at
  most one decoded frame in memory. With a `SessionAuthorizer`, the endpoint
  checks the registry before it processes each frame. A revocation closes the
  device's connections and cancels their requests; data already returned
  cannot be recalled. If the revocation watch fails, the per-frame check
  stays in force.
- A per-source gate keeps one batch in flight per source across all
  connections. A second uploader of the same source waits and validates
  against the first commit.
- Before parsing, the endpoint checks that the records are contiguous, start
  at the batch start, and end at the batch end. Normalization uses
  `events.Parse` with the record bound passed explicitly, so it cannot drift
  from the wire bound. Each framed record must parse to exactly one record at
  its framed boundaries; JSON with an embedded line terminator fails this
  check. A batch that holds offset 0 supplies the first native ID for the
  rest of the batch.
- Transient write failures, such as an external lock past the busy timeout,
  retry with the hub policy: 3 attempts, waits from 100 ms doubling up to
  2 s, no jitter. Deterministic rejections (`ErrBatchRejected`,
  `ErrSourceOwnership`, `ErrSourceNotRegistered`) never retry.
- Query and annotate outcomes are always answered in band, and the connection
  stays open. Policy and input rejections map to `CodeRejected`, contention
  to `CodeBusy`, and other failures to `CodeInternal`.
- Cancelling `Serve` closes every connection and waits for the handlers. A
  batch in flight either commits or leaves the cursor unchanged.

## Device side

The uploader (`internal/uploader`) implements the `SendBatch` and
`DeliverAck` actions of `tla/Sync.tla`:

- A batch always starts at the durable checkpoint. One connection carries
  strictly sequential exchanges, so each source has at most one batch in
  flight. Only an acknowledgment moves the checkpoint; a timeout, reset, or
  lost acknowledgment leaves it, and the next pass sends the same bytes.
- Offline catch-up needs no queue, because the retained source files are the
  backlog. On every new connection, each source is registered and probed with
  an empty batch. A hub cursor ahead of the checkpoint is a lost
  acknowledgment and is persisted. A hub cursor behind it is a regression,
  for example after a backup restore: `watch.Rewind` replays the retained
  file, or the uploader reports an unrecoverable gap when the file is gone or
  changed. A `CodeRejected` gap during a pass triggers a new probe before the
  uploader declares a conflict.
- A record above 32 MiB, or an incomplete tail already above it, stops the
  source locally, because sending it would close the connection. A parse
  stop is not sent again until the file grows. A source refused for a
  reason retrying cannot fix is blocked for the rest of the process; a
  changed file is a new source and starts clean.
- The uploader reports bytes it can see were lost before upload: a replaced
  file whose observed end passed its acknowledged cursor, and a deleted file
  with unsent bytes. Each loss and gap is reported once.
- Each exchange has a 60 s timeout. Failed passes reconnect with backoff from
  1 s doubling up to 1 min. `Run` stops only on `ErrUnauthorized` (the
  credential was refused, for example after revocation), `ErrIncompatible`
  (the hub refused the uploader's frames), a discovery failure, or lost local
  state. When the hub closes a connection after a refusal, the uploader reads
  the hub's error frame first, so it reports the hub's reason.
- Before each pass, a non-blocking read checks that an idle connection is
  still open. The hub never sends unsolicited frames, so a closed connection
  means a hub restart. This is how an idle device notices a restored hub.
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
