# TLA+ protocol models

Two executable specifications live here. `Sync.tla` specifies the sync
cursor and acknowledgment protocol from the "Sync contract" in
[ARCHITECTURE.md](../ARCHITECTURE.md); `Auth.tla` specifies the
authorization, pairing, and revocation rules from its "Authorization and
SQL" and "Agent annotations" sections. TLC model-checks both against the
safety invariants and liveness properties recorded here. The sync
endpoint (T017, `internal/hubsync`), the uploader (T019), and pairing
with revocation (T018, `internal/registry`) must implement these state
machines; change the specifications first when a contract changes.

## Run

```sh
make tla
```

The runner (`run-tlc.sh`) needs Java 11 or later. It downloads the pinned
TLA+ tools jar (1.8.0), verifies its SHA-256 checksum, and checks every
model below. TLC output ends with `Model checking completed. No error has
been found.` per model.

# Sync protocol model

`Sync.tla` is the executable TLA+ specification of the sync cursor and
acknowledgment protocol from the "Sync contract" in
[ARCHITECTURE.md](../ARCHITECTURE.md). The sync endpoint (T017) and the
uploader (T019) must implement this state machine; change this
specification first when the contract changes.

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
  outside this model; the uploader (T019, `internal/uploader`) detects it
  by probing every source on each new connection, then replays the retained
  file or reports the unrecoverable gap.
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
  batch. The registration step is T017: the 'R' frame of `internal/hubsync`
  performs it, bound to the authenticated device, and its tests cover it.
- **Fixed ownership.** Each source's device is fixed. Authentication,
  spoofing, revocation, and ownership binding are the authorization model
  (`Auth.tla`, below), not this one.

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
  validation (`internal/store`); the endpoint tests repeat them over the
  wire (`internal/hubsync`, T017).
- Content of a conflicting retry: excluded by the immutable-content
  assumption above.
- Transport behavior (Tailcat framing, reconnect mechanics): behind the
  `internal/transport` boundary; the model is transport-independent.
- Authorization, pairing, and revocation: `Auth.tla`, below.
- Hub cursor regression after backup restoration: detected and replayed
  by the uploader, tested in Go (`internal/uploader`, T019).
- Finite bounds only: TLC checks the models listed above, not all sizes.

# Authorization model

`Auth.tla` is the executable TLA+ specification of the authorization
rules in the "Authorization and SQL" and "Agent annotations" sections of
[ARCHITECTURE.md](../ARCHITECTURE.md): the hub-owned device credential
registry, one-time expiring pairing invitations, per-request revocation
checks, source ownership, and annotation writer ownership. Pairing,
credentials, and revocation (T018) implement this state machine in
`internal/registry` (the registry store and its endpoint authenticator)
with `internal/hubsync` carrying the per-request `Authorize` gate and the
revocation teardown; change this specification first when a rule changes.
`Sync.tla` assumes
fixed source ownership — this model is that assumption, checked.

## What the model covers

| Domain concept | Model element |
|---|---|
| Pairing invitation (one-time, expiring) | `invState[i]`: unissued → open → used or expired |
| Device that consumed an invitation | `invUsedBy[i]` |
| Device credential registry | `registry[d]`: absent → active → revoked |
| Credential provenance of a device | `pairedVia[d]`, set once at admission |
| Revocation history | `revokedEver`, the set of ever-revoked devices |
| Session (connection) | `sesDevice[c]`, permanent binding, and `sesLive[c]` |
| Source ownership | `srcOwner[s]`, with history `srcEverOwned[s]` |
| Annotation slot (event, namespace, key) | constant `Annotations`; the writer completes the identity |
| Annotation rows and their writers | `annRows[a]`, `annRowSes[a][d]` |

Actions and the scenarios they cover:

| Action | Scenario |
|---|---|
| `IssueInvitation` | The operator issues an invitation |
| `ExpireInvitation` | An invitation dies before consumption; unfair, so consumption must race it and can lose |
| `ConsumeInvitation` | One atomic hub transaction consumes the invitation and admits the device; only a stranger ("absent") device may consume |
| `RevokeDevice` | Registry flips to revoked and every live session of the device closes with its in-flight requests cancelled, in one step |
| `OpenSession` | Only an active device establishes a session; the slot binds to it permanently |
| `CloseSession` | A session ends; the device binding survives |
| `Upload` | The first accepted upload binds the source to the session's device; the same device's retries are accepted; any other device — stranger, paired, or the revoked former owner — is rejected |
| `Annotate` / `DeleteAnnotation` | Upsert or delete of the session device's own row on a slot; the request carries no writer |

A rejected request is the absence of a transition: the authorization
guard fails and no state changes.

## Safety invariants

Every configuration checks all of these.

- `InvitationSingleUse` — at most one device ever paired through one
  invitation. An invitation cannot be consumed twice.
- `PairingConsistent` / `InvitationUsedByPairer` — the registry admits
  devices only through invitation consumption, and what the pairing
  record claims is true: the invitation is used, by exactly this device.
  A pairing through an expired or still-open invitation breaks this.
- `NoLiveSessionForRevoked` — no live session belongs to a device that is
  not active. A revoked device cannot establish a new authorized session.
- `RevocationIrreversible` — a device revoked at any time is revoked now.
- `OwnershipImmobile` / `OwnerIsRegistrar` — a source is bound by at most
  one device ever, and the live owner is exactly that registrar. Source
  ownership cannot move between devices.
- `AnnotationWriterFromSession` / `AnnotationRowTouched` — every recorded
  annotation write or delete went through a session bound to the row's
  writer. The writer comes from the session, never from the request.

## Design decisions fixed by the model

- **Revocation is final for the device identity.** Re-pairing a revoked
  device with a fresh invitation would establish a new authorized
  session, which the safety property forbids — so consumption requires
  registry state "absent", which revocation never returns to. Re-admitting
  a user means enrolling a new device identity, not re-inviting the old
  one.
- **Orphaned sources stay locked.** Revocation does not unbind a device's
  sources, and no other device may take them over. This is the
  conservative reading of the sync contract's "reject … change source
  ownership".
- **Revocation closes sessions atomically in the model.** The per-request
  registry check in `Authorized` is redundant in-model (the atomic close
  already ends the sessions) but stays in the specification, because a
  real hub cannot tear a connection down atomically with a request
  already in flight — the check is the rule every request applies, and
  the single hub writer serializes it against revocation.

## Assumptions

- **Unguessable, protected credentials.** Authentication yields the true
  device identity; spoofing and credential theft are excluded.
  Cryptography is explicitly out of scope (ARCHITECTURE.md).
- **Correct transport identity.** The mapping from the transport-level
  peer identity (Tailcat `WhoIs`) to the registry device is correct.
- **One serialization point.** Every request and every revocation commits
  through the single hub writer, so each atomic model action is one
  implementation transaction.
- **Requests ride live connections.** A closed connection cannot carry a
  request; the transport guarantees it, so the model does not re-check
  `sesLive` outside `Authorized`.
- **Already returned data cannot be revoked.** An acknowledged limit, not
  a property. Revocation also does not erase history: revoked devices'
  annotation rows stay written.
- **Expiry is a nondeterministic action, not a clock.** The protocol
  property is that consumption must race expiry and can lose; the TTL
  length is configuration, not protocol.
- **Annotation value semantics are out of model.** Atomic batches,
  identical-retry no-ops, limits, and foreign keys are Go tests
  (`internal/store`, T009). This model covers only ownership and
  authorization.
- **Reads are out of model.** The same `Authorized` guard applies to
  queries; reads carry no ownership state to check.

## Models

| Configuration | Bounds | What it exercises | Size (checked 2026-11-13) |
|---|---|---|---|
| `Auth.cfg` | 2 devices, 2 invitations, 3 sessions, 2 sources | Full interleaving: pairing races, revocation under live sessions, cross-writer annotations on one slot | 46,289 states, ~1 s |
| `AuthWide.cfg` | 3 devices, 2 invitations, 3 sessions, 2 sources | Pairing scarcity: one device stays a stranger whatever happens; one device owning both sources while another owns none | 126,933 states, ~3 s |

Both run with `-deadlock`: the drained system (every invitation dead,
every paired device revoked, no live session) is a legitimate fixed
point.

## Non-vacuousness

Six one-line mutations, run against `Auth.cfg` during review of this
model, each produced a TLC violation (TLC reports the first violated
invariant):

- Consume a used or expired invitation: `InvitationUsedByPairer` violated.
- Upload rebinds another device's source: `OwnershipImmobile` violated.
- Revoke without closing the device's sessions:
  `NoLiveSessionForRevoked` violated.
- The hub writes the row the request names instead of the session's
  device (spoofed `writer_id`): `AnnotationWriterFromSession` violated.
- A revoked device consumes a fresh invitation:
  `RevocationIrreversible` violated.
- A session opens without the registry check:
  `NoLiveSessionForRevoked` violated.

One further mutation survived by design: an already-active device
consuming a second invitation violates no stated property (each
invitation is still single-use); the specification blocks it anyway with
the "absent" guard.

## No liveness property

Every property in scope is safety. Pairing and revocation are single
atomic decisions with no retry protocol at this abstraction; the retry
loop that makes pairing eventually succeed is device reconnection, whose
liveness assumption (eventual reconnection) belongs to `Sync.tla`. An
invitation may always expire first — that is required behavior, not a
starvation bug.

## Not proved here

- Cryptography, credential storage, and transport security: assumptions
  above.
- Annotation value and batch semantics: Go tests (`internal/store`, T009).
- Batch validation, cursors, and replay: `Sync.tla` and Go tests (T017).
- Hub-side rate limiting, connection bounds, request quotas: operational
  limits, not protocol states.
- Finite bounds only: TLC checks the models listed above, not all sizes.
