# Sync protocol model

`Sync.tla` is the executable TLA+ specification of the sync cursor and
acknowledgment protocol from the "Sync contract" in
[ARCHITECTURE.md](../ARCHITECTURE.md). TLC model-checks it against the
safety invariants and the liveness property recorded here. The sync
endpoint (T017) and the uploader (T019) must implement this state
machine; change this specification first when the contract changes.

## Run

```sh
make tla
```

The runner (`run-tlc.sh`) needs Java 11 or later. It downloads the pinned
TLA+ tools jar (1.8.0), verifies its SHA-256 checksum, and checks every
model below. TLC output ends with `Model checking completed. No error has
been found.` per model.

## What the model covers

The model abstracts one source to record indices: byte offsets and
record-boundary validation are implementation detail (Go tests cover
them), while the cursor/acknowledgment state machine is the protocol
under check.

| Domain concept | Model element |
|---|---|
| Source file (append-only) | `fileLen[s]`, grown by `AppendRecord` |
| Batch of contiguous records | `inflight[s] = <<start, end>>`, bounded by `MaxBatch` |
| Hub committed cursor (`sources.cursor_bytes`) | `hubCursor[s]` |
| Hub event storage | `stored[s]` (record indices) |
| Aggregate event count (inserts only) | `eventCount[s]` |
| Device's durable checkpoint (acknowledgment) | `ackCursor[s]` |
| Acknowledgment on the wire | `wireAck[s]` (highest value only; acks are monotone) |

Each source models one device's uploader; several sources in one model
synchronize concurrently on separate connections. A real device crash
resets every one of its connections at once; the model allows each
source's connection to reset independently, which explores a superset of
those grouped behaviors.

Actions and the failure scenarios they cover:

| Action | Scenario |
|---|---|
| `AppendRecord` | The agent appends records; appends interleave with sync arbitrarily |
| `SendBatch` | Batch upload from the durable checkpoint; one in-flight batch per source |
| `HubCommit` | One durable transaction: insert new records, count them once, advance the hub cursor, emit the ack; a repeated batch is a no-op that returns the existing cursor |
| `DeliverAck` | The device durably persists the acknowledged cursor |
| `DropBatch` | Batch lost in transit, or connection timeout and reset; the device retries from its checkpoint |
| `DropAck` | Lost acknowledgment; the device retries, the hub replays the overlap |
| `CrashClient` | Device crash: durable checkpoint survives, in-flight messages for its source vanish — the window between hub commit and local persistence |
| `CrashHub` | Hub crash: committed state survives, all connections reset, no cursor rolls back |

## Safety invariants

Every configuration checks all of these.

- `AckLeCommit` — no acknowledged cursor passes the hub's committed
  cursor. Cursor advancement never precedes durable storage.
- `AckedRecordsStored` — every record below an acknowledged cursor is
  durably stored at the hub. Events are not lost after acknowledged
  commits.
- `StoredPrefixContiguous` — the hub stores exactly the contiguous prefix
  below its cursor. Records of one source become visible in file order,
  whatever the interleaving across sources. No phantom record sits beyond
  the cursor.
- `EventsCountedOnce` — aggregates count each stored record exactly once.
  Retries and reconnections create no duplicate events.
- `HubWithinFile` — the hub never commits records the file does not hold.
- `InflightFromAck` — a batch in flight never starts ahead of the durable
  checkpoint. The device never infers success from having sent a batch.
- `WireAckLeCommit` — an acknowledgment on the wire never passes the
  committed cursor.

## Liveness property

`EventualSync`: every appended record is eventually stored at the hub, and
every stored record is eventually acknowledged. Checked under weak
fairness per source on retries (`SendBatch`), commits (`HubCommit`), and
checkpoint persistence (`DeliverAck`). Appends stay unfair: the protocol
must sync whatever has arrived, on any schedule.

## Assumptions

- **Retained source files.** A source file is append-only and never loses
  bytes. Replacement and truncation create a new source incarnation
  (watcher logic, T005/T028), not a protocol transition.
- **Intact hub storage.** A hub commit is atomic and durable (WAL,
  `synchronous = FULL`). Committed cursors and events never roll back.
  Backup restoration that regresses the hub cursor is a recovery problem
  outside this model; T019 owns its detection.
- **Durable local checkpoints.** The device persists its acknowledged
  cursor atomically (`internal/durablefile`), so a crash loses only
  in-flight messages, never the checkpoint. The device keeps no volatile
  protocol state beyond what is on the wire.
- **Immutable record content.** A record's bytes never change, so an
  overlapping retry always matches stored content. The hub's
  conflicting-content rejection (`internal/store` validation) guards the
  cases this assumption excludes.
- **Eventual reconnection (liveness only).** Loss and crashes stop, and
  the device retries. The liveness model states this by disabling loss
  and crash actions (`ReliableNet = TRUE`); the safety models enable them.
- **Registered sources.** Every source exists at the hub before its first
  batch. The registration step is T017.
- **Fixed ownership.** Each source's device is fixed. Authentication,
  spoofing, revocation, and ownership binding are the authorization model
  (T016), not this one.

## Models

| Configuration | Bounds | What it exercises | Size (checked 2026-11-13) |
|---|---|---|---|
| `SyncConcurrent.cfg` | 2 sources, `MaxLen` 5, `MaxBatch` 2, loss and crashes | Cross-device interleaving under every failure action | 53,824 states, ~2 s |
| `SyncFailures.cfg` | 1 source, `MaxLen` 12, `MaxBatch` 3, loss and crashes | Long retry chains, many partial batches, every loss/crash window | 2,057 states, <1 s |
| `SyncLiveness.cfg` | 2 sources, `MaxLen` 4, `MaxBatch` 2, reliable network | `EventualSync` under fairness, plus all invariants | 12,100 states, ~13 s |

All runs use `-deadlock`: the drained system legitimately reaches a
quiescent fixed point where no action changes state, and that is not an
error.

## Non-vacuousness

The invariants are not true by accident of formulation. Three one-line
mutations, run against `SyncFailures.cfg` during review of this model,
each produced a TLC violation:

- Device acknowledges optimistically at send time: `AckLeCommit` violated.
- Hub counts every record in a batch instead of only fresh ones:
  `EventsCountedOnce` violated.
- Hub advances the cursor while skipping one record insert:
  `StoredPrefixContiguous` violated.

## Not proved here

- Byte-boundary arithmetic and JSONL framing: the model uses record
  indices. The Go write path tests boundaries, contiguity, and overlap
  validation (`internal/store`).
- Content of a conflicting retry: excluded by the immutable-content
  assumption above.
- Transport behavior (Tailcat framing, reconnect mechanics): behind the
  `internal/transport` boundary; the model is transport-independent.
- Authorization, pairing, and revocation: T016.
- Hub cursor regression after backup restoration: T019.
- Finite bounds only: TLC checks the models listed above, not all sizes.
