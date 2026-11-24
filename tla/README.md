# TLA+ protocol models

Two executable specifications live here. `Sync.tla` specifies the sync
cursor and acknowledgment protocol from the "Sync contract" in
[ARCHITECTURE.md](../ARCHITECTURE.md); `Auth.tla` specifies the
authorization, pairing, and revocation rules from its "Authorization and
SQL" and "Agent annotations" sections. Both carry the owner purge
lifecycle of [ADR 0013](../docs/adr/0013-owner-scoped-session-purge.md):
the tombstoned source, its refusal, and re-admission. `Auth.tla` also
carries the source workspace of a device
([ADR 0018](../docs/adr/0018-device-observed-source-workspace.md)).
TLC checks the models against the properties recorded here. The sync
endpoint (T017, `internal/hubsync`), the uploader (T019), and pairing
with revocation (T018, `internal/registry`) must implement these state
machines; change the specifications first when a contract changes.
`TransportOwner.tla`, which checked shared device transport ownership
(ADR 0006), was removed with the shared node it modeled when
[ADR 0011](../docs/adr/0011-tailcat-address-transport.md) replaced the
transport.

## Run

```sh
make tla
```

You can also run `./tla/run-tlc.sh` directly; it changes to its own
directory first. The runner needs Java 11 or later and nothing else: the
TLA+ tools jar (1.8.0) is vendored at `tla/vendor/`, every run verifies its
SHA-256 against the pin in the script, and no download happens — a fresh
checkout cannot break when upstream rebuilds a release tag
([ADR 0007](../docs/adr/0007-vendored-tlc-jar.md)). TLC output ends with
`Model checking completed. No error has been found.` per model.

To upgrade TLC, download the new release jar out of band, place it at
`tla/vendor/tla2tools-<version>.jar`, update `TLA_VERSION` and `TLA_SHA256`
in `tla/run-tlc.sh`, delete the superseded jar, and commit jar and pin
together.

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
under check. Frame encoding is likewise outside the model: a plain or
zstd-compressed batch frame (ADR 0014) is the same `SendBatch` action, and
the capability negotiation that selects the encoding happens before any
batch exists and degrades to plain frames, changing no batch/ack state.
Extend the model only if a capability changes the state machine itself.

| Domain concept | Model element |
|---|---|
| Source file (append-only) | `fileLen[s]`, grown by `AppendRecord` |
| Batch of contiguous records | `inflight[s] = <<start, end>>`, bounded by `MaxBatch` |
| Hub committed cursor (`sources.cursor_bytes`) | `hubCursor[s]` |
| Hub event storage | `stored[s]` (record indices) |
| Aggregate event count (inserts only) | `eventCount[s]` |
| Device's durable checkpoint (acknowledgment) | `ackCursor[s]` |
| Acknowledgment on the wire | `wireAck[s]` (highest value only; acks are monotone) |
| Purge tombstone (ADR 0013) | `purged[s]`, with the eviction mark `purgeMark[s]` and the owner action counters `purgeCount[s]`, `reallowCount[s]` |

| Constant | Meaning |
|---|---|
| `MaxLen` | Model bound: the most records one source file may reach |
| `MaxBatch` | The most records in one batch; batches may split a file |
| `Sources` | The set of sources; each source is one watched file |
| `ReliableNet` | `FALSE` enables message loss and crashes |
| `MaxPurges` | The most owner purges one source may receive; `0` disables the purge actions entirely |

`NoBatch` (the empty tuple) and `NoAck` (`MaxLen + 1`, above every real
cursor) mark an empty channel. They keep each channel single-typed,
because TLC rejects equality across types and TLA+ has no negative
numbers. `AckMax` keeps the newer of two acknowledgments; `NoAck` never
wins over a real one.

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
| `HubCommit` | One durable transaction: insert new records, count them once, advance the hub cursor, emit the ack; a repeated batch is a no-op that returns the existing cursor; refused (no transition) while the source is tombstoned |
| `DeliverAck` | The device durably persists the acknowledged cursor; a stale ack that arrives late changes nothing |
| `PurgeSource` | The owner purges a source (ADR 0013): storage empties, the eviction mark freezes at the committed cursor, the tombstone is set; the cursor itself never moves |
| `ReallowSource` | The owner re-admits a purged source: the tombstone clears, storage stays empty, sync resumes from the frozen cursor |
| `DropBatch` | Batch lost in transit, or connection timeout and reset; the device retries from its checkpoint |
| `DropAck` | Lost acknowledgment; the device retries, the hub replays the overlap |
| `CrashClient` | Device crash: durable checkpoint survives, in-flight messages for its source vanish — the window between hub commit and local persistence |
| `CrashHub` | Hub crash: committed state survives, all connections reset, no cursor rolls back |

`HubCommit` requires `lo =< hubCursor[s]`. This guard restates the
contract rule "reject gaps ahead of the stored cursor". Batches start at
`ackCursor`, which never passes `hubCursor`, so the guard always holds in
the model. The hub checks it anyway.

`PurgeSource` and `ReallowSource` model the owner actions of ADR 0013.
Records below the eviction mark are the purged content: `HubCommit` never
stores them again, because it only stores records at or beyond the
committed cursor and purge never lowers that cursor. The bound `MaxPurges`
keeps the counters (and the state space) finite; the liveness configuration
sets it to `0`, because a purged source legitimately stops syncing — its
refusal is required behavior, not a starvation bug. The purged state is
per-source, so with two sources every interleaving of a refused source and
a healthy one is explored: the refusal cannot wedge unrelated sources.

## Safety invariants

Every configuration checks all of these.

- `AckLeCommit` — no acknowledged cursor passes the hub's committed
  cursor. Cursor advancement never precedes durable storage. Holds for
  purged sources too, because purge never lowers the hub cursor.
- `AckedRecordsStored` — every record below an acknowledged cursor is
  durably stored at the hub, or lies below a purge mark: the owner's purge
  is the only way an acknowledged record stops being stored. Events are
  not lost after acknowledged commits by any other path.
- `StoredPrefixContiguous` — the hub stores exactly the contiguous range
  from the purge mark to the committed cursor (the mark is 0 for a source
  that was never purged, so this is the original prefix property).
  Records of one source become visible in file order, whatever the
  interleaving across sources. No phantom record sits beyond the cursor,
  and none sits below a purge mark.
- `NoResurrection` — no record below a purge mark is ever stored again.
  Named separately from `StoredPrefixContiguous` (which implies it) because
  it is the property the tombstone exists for.
- `PurgedSourceStoresNothing` — a tombstoned source stores nothing; only
  `ReallowSource` reopens the write path.
- `TombstoneConsistent` — the tombstone is set exactly when the purge
  count exceeds the reallow count, so it flips only through the paired
  owner actions: an acknowledged-then-purged source stays purged until the
  owner re-admits it.
- `EventsCountedOnce` — aggregates count each stored record exactly once.
  Retries and reconnections create no duplicate events.
- `HubWithinFile` — the hub never commits records the file does not hold.
- `InflightFromAck` — a batch in flight never starts ahead of the durable
  checkpoint. The device never infers success from having sent a batch.
  A batch may start at an older checkpoint than the current one, because
  an earlier ack can arrive while the next batch is already on the wire.
- `WireAckLeCommit` — an acknowledgment on the wire never passes the
  committed cursor.

Two action properties hold as well: `HubCursorNeverRegresses` (no cursor
regression — not even a purge moves the hub cursor) and
`PurgeMarkNeverRegresses` (the eviction mark only grows, so re-purging
after a reallow can only widen the purged range, never shrink it).

## Liveness property

`EventualSync`: every appended record is eventually stored at the hub — or
lies below a purge mark, the owner's terminal decision for it — and every
stored record is eventually acknowledged. Checked under weak
fairness per source on retries (`SendBatch`), commits (`HubCommit`), and
checkpoint persistence (`DeliverAck`). Appends stay unfair: the protocol
must sync whatever has arrived, on any schedule. The liveness configuration
disables the purge actions (`MaxPurges = 0`); under purge, the escape
clause is what the property means.

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
  cursor atomically (`internal/durablefile`). An acknowledgment the device
  has processed but not yet flushed (`internal/watch.Watcher.Flush`,
  called once per upload pass) is exactly `wireAck` above, not `ackCursor`:
  a crash before that flush loses it as if it had never left the wire, and
  `DeliverAck` never ran. The device re-sends the batch, and the hub's
  replay is idempotent. A crash after a flush loses only in-flight
  messages, never the flushed checkpoint; the device keeps no other
  volatile protocol state beyond what is on the wire.
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
  The uploader may probe the hub cursor of a source before the registration;
  the hub answers such a probe with `CodeUnknownSource`, and no batch comes
  before the registration. The keepalive of an idle connection is a `caps`
  exchange, not a sync action: it changes no checkpoint, batch, or
  acknowledgment.
- **Fixed ownership.** Each source's device is fixed. Authentication,
  spoofing, revocation, and ownership binding are the authorization model
  (`Auth.tla`, below), not this one.
- **Purge and reallow are owner actions** (ADR 0013), assumed correct and
  not subject to protocol fairness; `MaxPurges` bounds their counts so the
  state space stays finite, and the liveness configuration disables them,
  because a purged source legitimately stops syncing. The device-side
  below-cursor replay they must survive cannot arise through the uploader
  (every new connection probes the hub cursor first and reconciles), so
  its deterministic rejection is pinned by write-path tests in
  `internal/store`, and the resumed-sync behavior by the uploader e2e.

## Models

| Configuration | Bounds | What it exercises | Size (checked 2026-11-19) |
|---|---|---|---|
| `SyncConcurrent.cfg` | 2 sources, `MaxLen` 5, `MaxBatch` 2, 1 purge per source, loss and crashes | Cross-device interleaving under every failure action, including purge and reallow against a still-syncing neighbor | 929,296 states, ~30 s |
| `SyncFailures.cfg` | 1 source, `MaxLen` 12, `MaxBatch` 3, 1 purge, loss and crashes | Long retry chains, many partial batches, every loss/crash window, purge between commit and acknowledgment | 13,711 states, <1 s |
| `SyncLiveness.cfg` | 2 sources, `MaxLen` 4, `MaxBatch` 2, no purge, reliable network | `EventualSync` under fairness, plus all invariants | 12,100 states, ~13 s |

All runs use `-deadlock`: the drained system legitimately reaches a
quiescent fixed point where no action changes state, and that is not an
error. The `Quiescent` operator describes that fixed point for readers of
traces; no configuration checks it.

## Non-vacuousness

The invariants are not true by accident of formulation. Three one-line
mutations, run against `SyncFailures.cfg` during review of this model,
each produced a TLC violation:

- Device acknowledges optimistically at send time: `AckLeCommit` violated.
- Hub counts every record in a batch instead of only fresh ones:
  `EventsCountedOnce` violated.
- Hub advances the cursor while skipping one record insert:
  `StoredPrefixContiguous` violated.

Three further mutations, run 2026-11-19 against `SyncConcurrent.cfg` when
the purge actions were added (ADR 0013), were each caught:

- `HubCommit` drops the tombstone guard (a purged source keeps committing):
  `PurgedSourceStoresNothing` violated.
- `PurgeSource` sets the eviction mark one below the committed cursor (a
  purge that leaves the newest record resurrectable):
  `StoredPrefixContiguous` violated, because the still-stored records no
  longer match the range above the mark.
- `PurgeSource` resets `hubCursor` to 0 instead of freezing it: the
  in-flight acknowledgment passes the reset cursor, so `WireAckLeCommit`
  reports the violation first and `HubCursorNeverRegresses` holds the same
  step — the modeled equivalent of restoring a reallowed source at cursor
  0, which would invite a full replay.

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
checks, source ownership, annotation writer ownership, and the source
workspace of a device
([ADR 0018](../docs/adr/0018-device-observed-source-workspace.md)). Pairing,
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
| Source registration lifecycle (ADR 0013) | `srcReg[s]`: registered ↔ purged through the owner actions, with counters `srcPurges[s]`, `srcReallows[s]` |
| Source ownership | `srcOwner[s]`, with history `srcEverOwned[s]` |
| Annotation slot (event, namespace, key) | constant `Annotations`; the writer completes the identity |
| Annotation rows and their writers | `annRows[a]`, `annRowSes[a][d]` |
| Workspace observation (ADR 0009) | `wsObs[d][w]`, written once, and its writer session `wsWriter[d][w]` |
| Source workspace (ADR 0018) | `srcWs[s]`, written once, and its writer session `srcWsWriter[s]` |

| Constant | Meaning |
|---|---|
| `Devices` | Device identity strings, for example `{"d1", "d2"}` |
| `Invitations` | Invitation identity strings |
| `Sessions` | Session (connection) slot strings |
| `Sources` | Source identity strings |
| `Annotations` | Annotation slots: the `(event_id, namespace, key)` part of the annotation identity. The `writer_id` part is state (the row owner), not a constant |
| `Workspaces` | Workspace paths, observed by a device or reported for a source. The device part of the `(device_id, path)` identity is state, not a constant |
| `Repos` | Observed values, including the "no repository" value |
| `MaxPurges` | The most owner purges one source may receive; bounds the counters so the state space stays finite |

`None` is one sentinel string for "no device", "no invitation", and "no
session". The domains are disjoint, and no predicate compares sentinels
across domains, so one sentinel keeps the value sets small.

A session slot is single-use. It binds to its device when the session
opens and stays bound after the session closes, because annotation
history refers to it. Only a fresh slot can serve the device again.

`revokedEver`, `srcEverOwned`, and `annRowSes` are history variables.
They record what happened and never forget it, so "revocation is final",
"at most one registrar", and "the writer comes from the session" are
checkable as state invariants. Without them, a rebinding or a reversal
would leave no trace in the state. The credential secret is not modeled
state; it is an assumption below.

Actions and the scenarios they cover:

| Action | Scenario |
|---|---|
| `IssueInvitation` | The operator issues an invitation |
| `ExpireInvitation` | An invitation dies before consumption; unfair, so consumption must race it and can lose |
| `ConsumeInvitation` | One atomic hub transaction consumes the invitation and admits the device; only a stranger ("absent") device may consume |
| `RevokeDevice` | Registry flips to revoked and every live session of the device closes with its in-flight requests cancelled, in one step |
| `OpenSession` | Only an active device establishes a session; the slot binds to it permanently |
| `CloseSession` | A session ends; the device binding survives |
| `Upload` | The first accepted upload binds the source to the session's device; the same device's retries are accepted; any other device — stranger, paired, or the revoked former owner — is rejected; refused while the source is tombstoned (ADR 0013) |
| `PurgeSource` / `ReallowSource` | The owner's purge and re-admission of a source (ADR 0013): the registration flips registered ↔ purged, nothing else moves — ownership, the source workspace, the registry, and sessions are untouched |
| `Annotate` / `DeleteAnnotation` | Upsert or delete of the session device's own row on a slot; the request carries no writer, so another device's row is not addressable. Deleting an absent row succeeds and removes nothing |
| `Observe` | A workspace observation for the session device's own workspace. The first one is stored; a repeat, with the same or another value, changes nothing |
| `SetSourceWorkspace` | A session reports the workspace of a registered source of its own device (ADR 0018). The first report is stored with the session as its writer; a repeat, with the same or another path, changes nothing; refused while the source is tombstoned |

A rejected request is the absence of a transition: the authorization
guard fails and no state changes.

## Safety invariants

Every configuration checks all of these.

- `SessionLiveHasDevice` — a live session always belongs to a bound
  session slot.
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
  ownership cannot move between devices, and a purge or reallow moves it
  nothing: the tombstone keeps a purged source's ID bound exactly as the
  row did.
- `TombstoneMatchesOwnerActions` — the registration is purged exactly when
  the purge count exceeds the reallow count, so the tombstone flips only
  through the paired owner actions. Together with the `Upload` guard this
  is the checked half of "revocation still wins over re-admission": after
  `RevokeDevice`, no reallow can make the revoked owner's uploads pass, and
  before a reallow no device's uploads pass.
- `AnnotationWriterFromSession` / `AnnotationRowTouched` — every recorded
  annotation write or delete went through a session bound to the row's
  writer. The writer comes from the session, never from the request.
- `ObservationWriterFromSession` / `ObservationHasWriter` — every stored
  workspace observation was written through a session of the device that
  owns the workspace.
- `SourceWorkspaceWriterFromSession` / `SourceWorkspaceHasWriter` — every
  stored source workspace was written through a session of the device that
  owns the source.

The action properties `ObservationWriteOnce` and `SourceWorkspaceWriteOnce`
check that a stored observation, or a stored source workspace, and its
writer never change.

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
  peer identity (Tailcat pre-shared / node-key authentication) to the
  registry device is correct.
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
- **A source workspace follows its registration.** `Upload` folds the
  registration of a source into its first accepted batch, so in the model
  `SetSourceWorkspace` is enabled only after an `Upload` by the owner
  device. On the wire, the device sends the `sourceWorkspace` frame after
  the hub acknowledged the registration frame; the uploader tests pin that
  order (`internal/uploader`). The hub checks the path, and sets the
  working directory of the sessions that have none, in Go tests
  (`internal/store`, `internal/hubsync`).
- **Reads and remote diagnostics.** `ReadDiagnostics(c)` models the bounded
  remote-status request on an authenticated session: it requires the exact
  same `Authorized(c)` guard (live session + active registry state, so
  revocation blocks it) and leaves every authorization, ownership, annotation,
  and workspace variable unchanged (`UNCHANGED vars`), proving diagnostic reads
  cannot mutate committed state or bypass revocation while keeping the
  explored state space unchanged. SQL queries share the same read-only
  `Authorized(c)` guard.

## Models

| Configuration | Bounds | What it exercises | Size (checked 2026-11-24) |
|---|---|---|---|
| `Auth.cfg` | 2 devices, 2 invitations, 3 sessions, 2 sources, 1 workspace, 2 values, 1 purge per source | Full interleaving: pairing races, revocation under live sessions, cross-writer annotations on one slot, competing observations of one workspace, a source workspace racing purge and revocation, purge and reallow racing uploads | 18,286,389 states, ~3.5 min |
| `AuthWide.cfg` | 3 devices, 2 invitations, 3 sessions, 2 sources, 1 workspace, 2 values, 1 purge per source | Pairing scarcity: one device stays a stranger whatever happens; one device owning both sources while another owns none | 51,982,533 states, ~11 min |

The action `SetSourceWorkspace` raised both state counts by a factor of
3.9. Each configuration stays below 30 minutes, so no constant bounds the
action.

In `Auth.cfg`, three session slots let both devices hold live sessions
and still close a session and open a new one on a fresh slot. In
`AuthWide.cfg`, a race between consumption and expiry can leave
invitations dead before any device wins them.

Both run with `-deadlock`: the drained system (every invitation dead,
every paired device revoked, no live session) is a legitimate fixed
point. The `Quiescent` operator describes it for readers of traces; no
configuration checks it.

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

Two mutations of `Observe`, run on 2026-11-16, were also caught:

- The hub overwrites a stored observation: `ObservationWriteOnce`
  violated.
- The hub writes the observation for any device instead of the session's
  device: `ObservationWriterFromSession` violated.

One further mutation survived by design: an already-active device
consuming a second invitation violates no stated property (each
invitation is still single-use); the specification blocks it anyway with
the "absent" guard.

A purge mutation also survived by design (checked 2026-11-19, when the
registration lifecycle was added): dropping the `srcReg` guard from
`Upload` violates no property here, because an upload to an owned purged
source changes no authorization state. The refusal's observable content —
no recreated source row, no stored records — is what `Sync.tla` checks
(`PurgedSourceStoresNothing` and its mutations above); this model pins
that purge and reallow never move ownership or registry state, so
revocation still wins over re-admission.

Three mutations of `SetSourceWorkspace`, run on 2026-11-24 against
`Auth.cfg` when the action was added (ADR 0018):

- The hub stores the workspace through a session of another device:
  `SourceWorkspaceWriterFromSession` violated.
- The hub overwrites a stored source workspace:
  `SourceWorkspaceWriteOnce` violated.
- Dropping the `srcReg` guard survived by design, like the `Upload` purge
  mutation above: a report for an owned purged source changes no
  authorization state. The refusal is pinned by Go tests
  (`internal/hubsync`, `internal/store`).

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
- The path of a source workspace: the device observes it, and the hub
  checks only its length and its text (ADR 0018).
- Batch validation, cursors, and replay: `Sync.tla` and Go tests (T017).
- Hub-side rate limiting, connection bounds, request quotas: operational
  limits, not protocol states.
- Finite bounds only: TLC checks the models listed above, not all sizes.
