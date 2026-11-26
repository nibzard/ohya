# Implicit State Machines Audit

**Date:** 2026-11-26

**Scope:** Full `ohya` Go codebase (`cmd/ohya/`, `internal/`) and macOS menu bar app (`macos/OhyaMenuBar/`), contrasted with the formal TLA+ specifications ([`Sync.tla`](../tla/Sync.tla), [`Auth.tla`](../tla/Auth.tla)).

**Summary:** **53** verified per-subsystem implicit state machines (**10 high**, **29 medium**, **14 low**), **5** cross-package implicit state machines (**3 high**, **2 medium**), and **3** rejected stateless false positives.

> This audit describes the code at `c5ae9c1`. The findings and line numbers refer to that revision.

---

## 1. Executive Summary

### Contrast with Explicit & TLA+-Modeled State
- **Formally modeled in TLA+:** Hub cursor/acknowledgment monotonicity, duplicate handling, purge tombstones, pairing invitation lifecycles, device revocation, and write-once source workspaces are specified in [`tla/Sync.tla`](../tla/Sync.tla) and [`tla/Auth.tla`](../tla/Auth.tla).
- **Explicit enums with implicit sidecar state:** A few components define explicit enums (`ServiceAction` in `internal/setup`, `MenuBarIconState` in `OhyaMenuBarCore`), yet carry critical implicit state alongside them (receipt/unit/running combinations on disk; stale `status` retained alongside `lastError`, `setupNeeded`, and `isPaused` in SwiftUI).
- **Highest-risk drift & failure modes:**
  1. **Purge → Reallow wedge:** `Sync.tla` and `Auth.tla` require a reallowed source to resume syncing from its frozen cursor, yet both [`Uploader`](../internal/uploader/uploader.go#L379-L384) (`u.blocked`) and [`ingestor`](../cmd/ohya/ingest.go#L360-L362) (`r.purged`, `r.noProgress`) permanently cache purge/stop state in memory until daemon restart.
  2. **`Opening.Close()` lock-wait hang:** [`Opening.Close`](../internal/mcpserver/opening.go#L66-L73) waits on `<-o.done` *before* calling `o.cancel()`, hanging shutdown whenever `open(ctx)` is blocked on a SQLite write/migration lock.
  3. **`address.Device` double-`Close()`:** [`releaseSlotLocked`](../internal/transport/address/address.go#L304-L314) returns `slot.client` without setting `slot.closed = true` when `d.closed && slot.dialing > 0`, causing repeated `slot.client.Close()` calls as in-flight dials finish.
  4. **`RemoteSession` desynchronized stream reuse:** [`RemoteSession.call`](../internal/hubsync/session.go#L194-L200) does not call `dropLocked()` when a retry fails with a transport or timeout error, leaving a half-read socket active.
  5. **Non-atomic `ApplyPurge` & `ReDerive` hazards:** Multi-transaction [`ApplyPurge`](../internal/store/purge.go#L304-L332) can abort mid-loop leaving sibling duplicate rows orphaned (`duplicate_of = NULL`, `text = NULL`); [`ReDerive`](../internal/store/rederive.go#L44-L140) uses a self-referential `duplicate_of = id` sentinel and fails on purged+reallowed sources with empty `first_native_id`.
  6. **Unrolled setup/install failure states:** [`parseInstallArgs`](../cmd/ohya/install.go#L64-L93) misses single-dash `-dry-run` (swapping the binary while child `ohya setup` dry-runs), and [`configureOwnedService`](../internal/setup/service_setup.go#L19-L102) commits DB schema migrations before service start/restore.

---

## 2. Cross-Package Implicit State Machines

### 1. Local Ingestion Pipeline (`cmd/ohya/ingest.go` ↔ `internal/watch` ↔ `internal/store`) — `high`
- **Implicit encoding:** Three uncoordinated state layers: `watch.Watcher` (`sources/<agent>.json`, `w.state.Sources`, `w.lastPass`, `w.dirty`), `ingestor` (7 `SourceID`-keyed maps + `workspaceTried`/`workspacesDue`), and `store.DB` (`sources`, `purged_sources`, `events`).
- **Invariants & risks:**
  - *Missing cursor-regression reconciliation:* Unlike `uploader.reconcile`, [`ingestor.ingestSource`](../cmd/ohya/ingest.go#L533-L594) never queries `store.SourceState` for `cursor_bytes` and never calls `watch.Rewind`. If `hub.db` is restored to an earlier cursor below `watch.CursorBytes`, [`store.Ingest`](../internal/store/ingest.go#L408-L411) rejects every batch (`ErrBatchRejected`) while `ingestor` retries endlessly.
  - *Triple-layer purge/reallow & stop deadlock:* `store.ApplyPurge` moves rows from `sources` into `purged_sources`, `ingestor` caches `r.purged[id] = true` for process lifetime ([`ingest.go`](../cmd/ohya/ingest.go#L360-L362)), and `r.noProgress[id]` ([`ingest.go`](../cmd/ohya/ingest.go#L386-L397)) skips `ingestSource` whenever `end`/`cursor`/`modTime` are unchanged — hiding both `ErrSourcePurged` and `store.ReallowSource` and keeping `watch_sources{state="stopped"}` inflated ([`ingest.go`](../cmd/ohya/ingest.go#L455-L462)).
  - *Unatomic checkpointing & `first_native_id` / `ReDerive` coupling:* `r.tails` mutates before `Acknowledge` ([`ingest.go`](../cmd/ohya/ingest.go#L431-L437)), `Acknowledge` mutates `w.state` before `Flush()` ([`ingest.go`](../cmd/ohya/ingest.go#L464)), and `store.Ingest` commits independently. On `codex`/`pi`, an offset-0 non-header locks `sources.first_native_id = ohya-synthetic:...` ([`codex/parser.go`](../internal/codex/parser.go#L175)), while a purged+reallowed source with empty `first_native_id` causes `store.ReDerive` ([`rederive.go`](../internal/store/rederive.go#L199-L230)) to fail with `ErrNoNative`.

### 2. Distributed Sync & Auth Protocol (`internal/uploader` ↔ `internal/hubsync` ↔ `internal/registry` / `internal/store`) — `high`
- **Implicit encoding:** Client `watch.Watcher` + `Uploader` (9 maps + `capsOff`/`hubCaps`), `hubsync` (`RemoteSession` + `Endpoint` `conns`/`byDev`/`sourceGates`), `registry.DB` (`devices`/`invitations` + `WatchRevocations.seen` + `pathRecorder`), and `store.DB` (`sources`/`purged_sources`).
- **Invariants & risks:**
  - *`PurgeSource` → `ReallowSource` TLA+ drift:* [`Sync.tla`](../tla/Sync.tla#L103-L109) and [`Auth.tla`](../tla/Auth.tla#L185-L192) guarantee sync resumes from the frozen cursor after owner re-admission. Across the boundary, `Uploader` ([`uploader.go`](../internal/uploader/uploader.go#L379-L746)) permanently latches `u.blocked[id] = true` on `ProbePurged`/`CodeRejected` and never clears it on reconnect, requiring a daemon restart (masked by `alice.restart(t)` in [`purge_e2e_test.go`](../internal/uploader/purge_e2e_test.go#L172)).
  - *Non-atomic pairing vs `Auth.tla` `ConsumeInvitation`:* `registry.Authenticator.Authenticate` ([`authenticator.go`](../internal/registry/authenticator.go#L29-L39)) commits `DB.Consume` before sending `CredentialReply`, and [`join.go`](../cmd/ohya/join.go#L147-L163) writes `device.json`, `credential.json`, and `hub-address.json` via 3 separate `durablefile.Write` calls — stranding `device.json` permanently paired at the hub on mid-flight failure.
  - *Revocation & `pathRecorder` divergence vs `Auth.tla` `RevokeDevice`:* `WatchRevocations` polls every 1s ([`registry.go`](../internal/registry/registry.go#L25-L425)) and `Endpoint` checks `Authorize` per frame, leaving an in-flight handler window after `DB.Revoke`; `pathRecorder` ([`hubpath.go`](../cmd/ohya/hubpath.go#L15-L87)) overwrites `remote[deviceID]` when `join` + `mcp --remote` run concurrently and throttles `RecordSync` to 30s.
  - *Capability-skip & `RemoteSession` / `Endpoint` framing desync:* `Uploader` ([`uploader.go`](../internal/uploader/uploader.go#L850-L852)) skips `Capabilities` when compression is off and `SourceWorkspace == nil`, silently disabling `CapBatchProbes`; `Endpoint.handleQuery` ([`endpoint.go`](../internal/hubsync/endpoint.go#L586-L600)) maps `mcpserver.ErrDeadline` to `CodeRejected`, which `RemoteSession.finishAttemptLocked` ([`session.go`](../internal/hubsync/session.go#L211-L221)) does not drop, compounding `session.go#L194-L200`'s unread-buffer retention on retry failure.

### 3. Provisioning, Backup, Migration & Service Ownership (`internal/setup` ↔ `cmd/ohya/setup*.go` / `install.go` ↔ `store.Open` / `migrate.go`) — `high`
- **Implicit encoding:** `parseInstallArgs` `dry` vs Go `flag`, `setupPreferences` + `CLAUDE_CONFIG_DIR`, `Options.Prepare`, `service.json`/`hub-service.json`, `.recovery-*`/`recovery-*` backups, `schema_migrations` (`EmptyTable` vs `AppliedSchemaVersion`), and service-manager polling.
- **Invariants & risks:**
  - *Irreversible schema migration before service start/restore:* `configureOwnedService` ([`service_setup.go`](../internal/setup/service_setup.go#L63-L93)) commits `migrateDatabase` (`store.Open` → `migrate.go`) *before* `ApplyFile`/`Start`/`waitService`. On startup failure, `restoreService` (`L104-L121`) rolls back the service unit (`original`) without rolling back the database schema.
  - *`EmptyTable` migration bypass:* [`migrate.go`](../internal/store/migrate.go#L110) creates `schema_migrations` outside a tx; if a crash leaves `schema_migrations` empty, `appliedSchemaVersion` ([`engine.go`](../internal/setup/engine.go#L504-L518)) returns `(0, false)` and `migrateDatabase` (`L484`) silently skips migration.
  - *Pre-lock/pre-backup side effects & `install -dry-run` split:* [`install.go`](../cmd/ohya/install.go#L84-L88) fails to recognize single-dash `-dry-run` (swapping the binary via `install.Binary` while child `ohya setup` dry-runs) and never rolls back `install.Binary` when `ohya setup` fails; `cmd/ohya/setup.go` clobbers `CLAUDE_CONFIG_DIR` via `setupPreferences`, unsets `--join-code-env` before `setup.Lock`, and runs `Prepare` (persisting preferences, pairing, and `connection.json`) before `ApplyProfile`/`recoveryPoint`/`migrateDatabase`.

### 4. Observability & UI Polling Bridge (`macos/OhyaMenuBar` ↔ `internal/observe` ↔ CLI Processes) — `medium`
- **Implicit encoding:** Daemons publish `<stateDir>/observe/<id>/snapshot.json` via `observe.Publisher` (`Stopped`, `PID`, `SampledWallMS`, `BuildRevision`, `BuildDirty`); `cmd/ohya/status.go` & `doctor_mcp.go` interpret snapshots via `ProbePID` + `SnapshotStaleAge`; `AppModel` polls `status --json` / `doctor --json` + `ObserveWatcher` (`O_EVTONLY` on `observe/`).
- **Invariants & risks:**
  - *Unlinked-FD watcher stall + uncancelled CLI subprocesses:* Deleting/recreating `observe/` wedges `ObserveWatcher.watch()` ([`ObserveWatcher.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/ObserveWatcher.swift#L14)) on an unlinked fd; cancelling `pollTask`/`searchTask`/`binaryOverrideTask` never terminates running `OhyaRunner.execute` subprocesses ([`OhyaRunner.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBarCore/OhyaRunner.swift#L182-L244), up to 180s).
  - *Hybrid UI state across CLI boundaries:* `AppModel` retains stale `status` across `lastError != nil`, carries `remoteHub` across mode switches ([`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L350-L352)), hides `setupNeeded` updates behind `try?` (`L247`), and displays old-binary `status`/`doctor`/`recall` after `binaryOverridePath` changes; meanwhile `Publisher` omits `Stopped: true` from `history.jsonl` on fast shutdowns ([`publisher.go`](../internal/observe/publisher.go#L221)), `CleanStaleInstances` preserves PID-reused dead dirs forever (`publisher.go#L452`), and `mcp_build` warnings lag up to 60s behind `AppModel.refreshSecondary`.

### 5. Dual-State Topology & Directory Resolution (`internal/setup/connection.go` ↔ `cmd/ohya`) — `medium`
- **Implicit encoding:** Absent `setup/connection.json` decodes as `Mode: "local"` ([`connection.go`](../internal/setup/connection.go#L25-L27)), while remote mode splits state between `setupState` (`~/.ohya`) and `join` (`~/.ohya/join`) with a second `connection.json` written via `BindRemote` ([`setup.go`](../cmd/ohya/setup.go#L247)).
- **Invariants & risks:** Passing `--state ~/.ohya` explicitly on a remote-mode host causes [`doctor.go`](../cmd/ohya/doctor.go#L125-L128) to set `remote = true` (skipping local DB checks) while reading `observe/`, `device.json`, `credential.json`, and `sources/*.json` from `~/.ohya` instead of `~/.ohya/join`; [`status.go`](../cmd/ohya/status.go#L287-L304) mixes `observe/` from `~/.ohya` with `credential.json` from `~/.ohya/join`; and switching `--remote` → `--local` leaves `join/setup/connection.json` pinned to `"remote"`.

---

## 3. Subsystem Inventory

### A. Sync, Watch, Transport & Auth

#### A1. `Uploader` per-source maps & purge/reallow stuck state (`high`)
- **Location:** [`internal/uploader/uploader.go`](../internal/uploader/uploader.go#L81-L90) (`Uploader`), [`L309-L755`](../internal/uploader/uploader.go#L309-L755), [`L894-L905`](../internal/uploader/uploader.go#L894-L905); contrasted with [`tla/Sync.tla`](../tla/Sync.tla#L92-L110) and [`tla/Auth.tla`](../tla/Auth.tla#L176-L192); confirmed in [`purge_e2e_test.go`](../internal/uploader/purge_e2e_test.go#L172).
- **How state is implicitly encoded:** Nine `SourceID`-keyed maps: connection-scoped `ready`, `unregistered` (reset in `dialHello` `L901-L902`); process-scoped `blocked`, `stops`, `idle`, `sourceTold`, `seen`, `lossReported`, `gapReported` (sentinel `hub + 1` to distinguish `hub == 0` from unset `0`).
- **States & transitions:** `Unprobed` → `Unregistered` (`unregistered[id]=true` via `ProbeUnknown`) → `Ready` (`ready[id]=true` via `probe`/`reconcile`/`recordMissingGap`) → `Idle` (incomplete trailing line) / `Stopped` (`stops[id]`, cleared on `ack.Cursor > o.Cursor` or `(End, ModTime)` change) / `Blocked` (`blocked[id]=true` on `CodeRejected`, `CodeOwnership`, `CodeUnsupported`, `CodeUnknownSource` post-reg, `ProbePurged`, or `reconcile` error; cleared only on `ReplacedSourceID`).
- **Invariants & risks:** `Sync.tla` and `Auth.tla` model `PurgeSource` → `ReallowSource` resuming from the frozen cursor, whereas `Uploader` permanently skips `u.blocked[o.SourceID]` across reconnects until process restart. `sourceTold` is also process-sticky (hub restore before `SetSourceWorkspace` never re-sends workspace), and `stops` is not cleared in `reconcile()` on hub cursor regression.

#### A2. `RemoteSession` & `RemoteClient` connection, negotiation & retry lifecycle (`high`)
- **Location:** [`internal/hubsync/session.go`](../internal/hubsync/session.go#L27-L70) (`RemoteSession`), [`L110-L255`](../internal/hubsync/session.go#L110-L255); [`internal/hubsync/client.go`](../internal/hubsync/client.go#L23-L31) (`RemoteClient`), [`L44-L215`](../internal/hubsync/client.go#L44-L215).
- **How state is implicitly encoded:** `RemoteSession`: `client *RemoteClient` (`nil` vs non-nil), `keepaliveOff bool`, `closed bool`, `lastUsed time.Time`, `stopOnce sync.Once`, `doneCh chan struct{}` (`nil` if `KeepaliveInterval <= 0`). `RemoteClient`: `negotiated bool`, `caps Capabilities`.
- **States & transitions:** `RemoteSession`: `Disconnected` (`client == nil`) → `connectLocked()` (dials + writes `Hello`, no server ack) → `Connected` (`client != nil`) → `Closed` (`closed=true, client=nil`). `RemoteClient`: `Unnegotiated` (`!negotiated`) → `NegotiatedSupported` (`caps&CapRemoteStatus != 0`) or `NegotiatedUnsupported` (`caps == 0`).
- **Invariants & risks:** On a second `exchange()` transport/timeout error ([`session.go`](../internal/hubsync/session.go#L194-L200)), `finishAttemptLocked` returns `retry=true` without calling `s.dropLocked()`. `s.client` stays non-nil on a half-read `bufio.Reader`; on read timeouts, `Alive()` (`Peek(1)` 1ms) returns `true`, reusing a desynchronized framing stream. `Keepalive` treats `CodeMalformed || CodeVersion` as unsupported (`client.go#L70`), while `Diagnostics` checks only `CodeMalformed` (`L141`). `roundTrip()` sets `SetDeadline` without clearing it, and `stepKeepalive` holds `s.mu` across up to 60s of network I/O.

#### A3. `address.Device` / `clientSlot` lifecycle & double-`Close` bug (`high`)
- **Location:** [`internal/transport/address/address.go`](../internal/transport/address/address.go#L197-L380) (`clientSlot`, `Device`, `Dial`, `finishDialFailure`, `releaseSlotLocked`, `deviceConn`, `Drain`, `Close`); [`internal/transport/address/path.go`](../internal/transport/address/path.go#L39-L96) (`PathInfo`).
- **How state is implicitly encoded:** `Device`: `addr tailcat.Addr` (`""` = unbound, non-empty = write-once pinned hub), `closed bool`, `started bool`, `slot *clientSlot`, `retired map[*clientSlot]struct{}`. `clientSlot`: `open int`, `dialing int`, `retired bool`, `closed bool`.
- **States & transitions:** `Device`: `Unbound` (`addr == ""`) → `Bound` (`addr != ""`) → `Started` (`started = true`) → `Closed` (`closed = true`). `clientSlot`: `Active` → `Retired` (on non-canceled `Dial` failure in `finishDialFailure`, replaces `d.slot`, resets `d.started = false`) → `Closed` (`releaseSlotLocked`).
- **Invariants & risks:** When `d.closed == true` and `slot.dialing > 0`, [`releaseSlotLocked`](../internal/transport/address/address.go#L304-L314) leaves `slot.closed == false` yet still returns `slot.client`. `Device.Close()` closes `slot.client`, and every in-flight `Dial()`/`PathInfo()` finishing afterward calls `slot.client.Close()` repeatedly. `PathInfo()` sets `d.started = true` on success without retiring failed slots; `finishDialFailure` resets `d.started = false`, allowing `Drain()` (`L344-L354`) to return `nil` while older retired slots still have `open > 0`.

#### A4. `Watcher` verification, missing-edge & dirty-checkpoint state (`medium`)
- **Location:** [`internal/watch/watch.go`](../internal/watch/watch.go#L28-L40), [`L239-L393`](../internal/watch/watch.go#L239-L393), [`L438-L514`](../internal/watch/watch.go#L438-L514); [`internal/watch/state.go`](../internal/watch/state.go#L22-L99); [`internal/durablefile/durablefile.go`](../internal/durablefile/durablefile.go#L9-L50).
- **How state is implicitly encoded:** `w.state.Sources[path]` (`CursorBytes == 0` vs `> 0` requiring `FingerprintWindow > 0 && FingerprintSHA256 != ""`, plus `FileIdentity`), in-memory `w.lastPass[path]` (`probe{dev, ino, hasID, identity, size, modTime, ctime}`), `w.missing[path]` (`map[string]bool`), and `w.dirty bool`.
- **States & transitions:** `Untracked` → `Active/Cursor=0` (`register()`) → `Active/Verified` (`lastPass[key] == pr`) ↔ `Active/Unverified` (`lastPass` empty at startup or cleared on missing/unreadable fingerprint) → `Missing` (`!live` or claimed hardlink) → `Replaced` (`register()` generates new `SourceID`). `Acknowledge`/`Rewind` mutate memory and set `dirty=true`; `Flush()` writes `sources/<agent>.json` and clears `dirty=false`.
- **Invariants & risks:** `tla/README.md` (`L183-L191`) maps un-flushed `Acknowledge` to `wireAck` and `Flush` to `ackCursor`; `Rewind` is outside `Sync.tla`. In `verifyFingerprint()` (`L390-L392`), non-EOF read errors clear `lastPass[key]` while still emitting a normal active `Outcome`. If `Flush()`/`syncDir` fails after `os.Rename`, `w.dirty` remains `true` while disk state changed.

#### A5. `Watcher.Poll` move-conflict resolution & hardlink-claiming state (`medium`)
- **Location:** [`internal/watch/watch.go`](../internal/watch/watch.go#L129-L237), [`L254-L266`](../internal/watch/watch.go#L254-L266), [`L306-L342`](../internal/watch/watch.go#L306-L342).
- **How state is implicitly encoded:** `unclaimedProbes int` gating `byIdentity`, `moves`, `claims`, `renamedFrom`, and `claimed`.
- **States & transitions:** `FastPath` (`unclaimedProbes == 0`) vs `ReconcileMovesAndHardlinks` (`unclaimedProbes > 0`): builds `byIdentity` → computes `moves` → fixed-point loop deletes moves where `claims[target] > 1` → applies surviving moves → marks hardlinked tracked paths `Missing` → skips `claimed[id]`.
- **Invariants & risks:** If tracked source `A` disappears (`!live`, `claims[A]++` at `L187`) and tracked source `B` is renamed onto `A`'s path, `claims[A] == 2` prunes `moves[B]`, marking `B` `Missing` and replacing `A` (`Fresh: true, Cursor: 0`), losing `B`'s checkpoint. Empty `FileIdentity` bypasses move/hardlink logic entirely.

#### A6. `Uploader` connection, capability negotiation, keepalive & offline/online lifecycle (`medium`)
- **Location:** [`internal/uploader/uploader.go`](../internal/uploader/uploader.go#L80-L98), [`L182-L285`](../internal/uploader/uploader.go#L182-L285), [`L809-L935`](../internal/uploader/uploader.go#L809-L935), [`L1018-L1060`](../internal/uploader/uploader.go#L1018-L1060).
- **How state is implicitly encoded:** `u.conn *conn`, `u.offline bool`, `u.hubCaps hubsync.Capabilities`, sticky downgrade flags (`capsOff`, `sourceWorkspaceOff`, `workspacesOff`, `workspacesDue`), and `failures int`.
- **States & transitions:** `Disconnected` (`conn==nil`) → `dialHello()` → `ConnectedUnnegotiated` (`hubCaps==0, workspacesDue==true`) → `negotiate()` → `ConnectedNegotiated` (or `ConnectedLegacy` with `capsOff=true` on `CodeMalformed`/`CodeVersion`) → `Disconnected` (`disconnected()`, `offline=true`).
- **Invariants & risks:** [`uploader.go`](../internal/uploader/uploader.go#L850-L852) skips `negotiate()` when compression is disabled and `SourceWorkspace == nil`, leaving `u.hubCaps == 0` (silently disabling `CapBatchProbes` at `L563`) and skipping `u.noteOnline()`. `keepalive()` (`L814`) discards refreshed `caps`, and `negotiate()` error fallback (`L918`) calls `ReadReply` instead of `ReadWelcome`.

#### A7. `address.Hub` identity & listen/close lifecycle (`medium`)
- **Location:** [`internal/transport/address/address.go`](../internal/transport/address/address.go#L48-L191) (`Hub`, `NewHub`, `Listen`, `Drain`, `Close`); [`internal/transport/address/path.go`](../internal/transport/address/path.go#L111-L145) (`PeerPath`).
- **How state is implicitly encoded:** `fresh bool`, `started bool`, `closed bool`, `addr string`, `ident.Region *tailcfg.DERPRegion`.
- **States & transitions:** `Fresh` (`fresh=true`) or `Loaded` (`fresh=false`) → `Listening` (`started=true, addr!="", fresh=false`) → `Closed` (`closed=true`).
- **Invariants & risks:** If `Listen()` fails after `h.srv.Listen` succeeds (e.g. `durablefile.Write` fails at `L139`), `failListen` closes `h.srv` and resets `started=false, addr=""` while leaving `fresh=true, closed=false`. `Listen()` lacks an `if h.started` guard, and `PeerPath()` (`path.go#L115-L130`) reads `h.srv.Status()` after unlocking `h.mu`, racing `Close()`.

#### A8. Multi-file device identity & registry pairing/revocation vs `Auth.tla` (`medium`)
- **Location:** [`internal/device/device.go`](../internal/device/device.go#L29-L60), [`credential.go`](../internal/device/credential.go#L35-L88), [`hubaddr.go`](../internal/device/hubaddr.go#L27-L65); [`internal/registry/registry.go`](../internal/registry/registry.go#L200-L443), [`authenticator.go`](../internal/registry/authenticator.go#L17-L40).
- **How state is implicitly encoded:** Client presence and `DeviceID` equality across `device.json`, `credential.json`, and `hub-address.json`; hub `invitations.state` (`'open'|'used'`) + wall-clock `expires_at_ms`; `WatchRevocations.seen`; `Authenticator` dispatch (`hello != nil` vs `pair != nil`).
- **States & transitions:** Client: `Absent` → `Unpaired` (`device.json`) → `Paired` (`device.json` + `credential.json` + `hub-address.json`) or `Partial/Mismatched`. Hub invitation: `'open'` (`now < expires_at_ms`) → implicitly expired on read or `'used'`.
- **Invariants & risks:** `Auth.tla` models `ConsumeInvitation` atomically, whereas `Authenticator.Authenticate` (`L29-L39`) commits `DB.Consume` before `WriteCredentialReply`, and the client writes 3 files separately; mid-flight failure wedges `device.json` permanently paired on the hub without a local credential. `CheckPairedIdentity` (`L79-L80`) ignores missing `device.json`, and invitation expiry uses non-monotonic wall clock (`L230, L389`).

#### A9. `Uploader` backlog drain tracker (`low`)
- **Location:** [`internal/uploader/uploader.go`](../internal/uploader/uploader.go#L99-L116), [`L1066-L1167`](../internal/uploader/uploader.go#L1066-L1167).
- **How state is implicitly encoded:** `u.drain *backlogDrain` (`nil` vs non-nil), `logged bool`, `samples []drainSample`, `left`/`sent`/`total`.
- **States & transitions:** `Idle` (`drain == nil`) → `DrainingSilent` (`drain != nil, !logged`) → `DrainingLogged` (`logged = true` when `total >= 1MiB && sent > 0`) → `Idle` (next `Sync` sees `pending == 0`).
- **Invariants & risks:** Excludes `blocked` and `stops`, so stopping/blocking a source mid-drain drops `left` abruptly (`sent < total`), and completion is detected one `Sync()` pass later.

#### A10. Hub `Endpoint` session binding, revocation teardown & `sourceGates` (`low`)
- **Location:** [`internal/hubsync/endpoint.go`](../internal/hubsync/endpoint.go#L62-L88), [`L148-L403`](../internal/hubsync/endpoint.go#L148-L403), [`L640-L834`](../internal/hubsync/endpoint.go#L640-L834).
- **How state is implicitly encoded:** `connSession.deviceID == ""` vs non-empty + `e.conns` vs `e.byDev[deviceID]` + `e.closed`; `e.slots` semaphore; `sourceGates.locks[sourceID]`; `e.diagLast[deviceID]`; `opts.FirstNative`.
- **States & transitions:** `TrackedUnbound` (`AuthTimeout`) → `Bound` (`bind()` + `authz.Authorize`) → `ServingFrames` (`IdleTimeout` + per-frame `Authorize`) → `TornDown`.
- **Invariants & risks:** Contrasts with `Auth.tla` atomic `RevokeDevice`: relies on per-frame `Authorize` plus 1s polling in `WatchRevocations`. Frame ordering (`Registration` before `Batch`, `Capabilities` before `ZBatch`) is enforced by DB/codec errors rather than explicit connection state.

---

### B. Store, Parsers, Workspace & Secrets

#### B1. Source registration, tombstone, cursor-freeze, reallow & write-once workspace (`high`)
- **Location:** [`internal/store/ingest.go`](../internal/store/ingest.go#L77-L446), [`purge.go`](../internal/store/purge.go#L18-L561), [`source_workspace.go`](../internal/store/source_workspace.go#L18-L110); migrations [`001`](../internal/store/migrations/001_initial.sql), [`008`](../internal/store/migrations/008_purged_sources.sql), [`009`](../internal/store/migrations/009_source_workspace.sql).
- **How state is implicitly encoded:** Row presence across `sources` vs `purged_sources` + nullable `cursor_bytes`, `first_native_id`, `workspace_path`, and `purged_sources.scope`. `sourceState` (`L309-L313`) infers `FirstNativeID` via `COALESCE(offset-0 session.native_id, src.first_native_id)`. `SetSourceWorkspace` (`L67-L69`) uses `if stored.Valid { return nil }` for write-once.
- **States & transitions:** `Unregistered` → `Registered` (`sources` row, `cursor_bytes=0`) → `Active` (`cursor_bytes > 0`, `first_native_id` populated) → `Active+WorkspaceSet` → `Tombstoned` (moved to `purged_sources`, `events`/`annotations` deleted, `cursor_bytes` frozen) → `Reallowed` (moved back to `sources` at frozen `cursor_bytes`, preserving `first_native_id` & `workspace_path`).
- **Invariants & risks:** `ApplyPurge` executes `PlanPurge` (read-only snapshot), per-source `purgeOneSource` (separate write tx per source), and `dropEmptySelectedSessions` (another write tx) outside one transaction, drifting from `Sync.tla` `PurgeSource` atomicity. Mid-loop abort after clearing `duplicate_of = NULL` on sibling sources (`L412-L418`) leaves surviving sibling duplicate events with `duplicate_of = NULL`, `text = NULL`, missing `event_count`, and missing token usage. `PurgeSelector.sessionFilter` (`L83`) excludes `ohya-synthetic:%`, making synthetic-only sources untargetable.

#### B2. `events.duplicate_of` self-referential sentinel & `ReDerive` / `events_fts` rebuild (`high`)
- **Location:** [`internal/store/rederive.go`](../internal/store/rederive.go#L44-L409); migrations [`001`](../internal/store/migrations/001_initial.sql#L44-L55), [`003`](../internal/store/migrations/003_event_ordinal.sql), [`004`](../internal/store/migrations/004_parser_output_version.sql), [`005`](../internal/store/migrations/005_ordinal_dedup_by_content.sql).
- **How state is implicitly encoded:** `events.parser_version` classifies agents (`Empty`, `TooNew`, `UpToDate`, `NeedsRebuild`). `clearDerived` (`L143-L146`) temporarily writes a **third sentinel state** `duplicate_of = id` ("pending re-derivation") so `rewriteEvent`'s `WHERE duplicate_of IS NULL` skips unwalked rows. `rederiveAgent` threads `native := src.firstNative` sequentially.
- **States & transitions:** `OldDerived` → `PendingRederive` (`duplicate_of = id`) → `Canonical` (`duplicate_of = NULL`) or `Duplicate` (`duplicate_of = original_id`), followed by global `events_fts` delete-all + rebuild (`L128-L134`).
- **Invariants & risks:** `events_fts` has `AFTER INSERT`/`AFTER DELETE` triggers but **no `AFTER UPDATE` trigger**, so any future `UPDATE events SET text` outside `ReDerive` silently corrupts FTS. `ReDerive` wipes/rebuilds `events_fts` across all agents even when rebuilding one agent. Purged+reallowed sources (missing offset-0 event) with empty `first_native_id` fail `ReDerive` at `offset > 0` with `ErrNoNative`. Canonical duplicate winner depends on lexicographic `sources.id` ordering (`ORDER BY id`, `L175`).

#### B3. `DB` handle open-mode split & busy-lock retry loop (`medium`)
- **Location:** [`internal/store/store.go`](../internal/store/store.go#L25-L199); [`internal/store/doctor.go`](../internal/store/doctor.go#L18-L69).
- **How state is implicitly encoded:** `DB` encodes mode via `(writer != nil, readers == nil)` vs `(writer == nil, readers != nil)`; `retryWriteLock`/`retryBusy` encode wait policy via `waitForLock bool` + `wait == 0` sentinel + `deadline.IsZero()`.
- **States & transitions:** `Uninitialized` → `ReadWrite` (`Open` → `verifyWriter` → `migrate`) or `ReadOnly` (`OpenReadOnly` → `verifyReader`) → `Closed`. `retryBusy`: `Initial` → `Waiting` → `Succeeded` / `NonBusyError` / `TimedOut` / `Canceled`.
- **Invariants & risks:** `doctor.go` methods (`AppliedSchemaVersion`, `MaxParserVersions`, `ReadOnlySourceState`) dereference `d.readers` without nil-checking (panic on `Open()`), while write methods panic on `OpenReadOnly()`. Because `writerDSN` sets `_busy_timeout=5000`, `wait == busyTimeout` (5s) trips after one attempt and SQLite's C busy handler delays cancellation up to 5s.

#### B4. Schema migration lifecycle & `schema_migrations` bootstrap states (`medium`)
- **Location:** [`internal/store/migrate.go`](../internal/store/migrate.go#L105-L216); [`internal/store/doctor.go`](../internal/store/doctor.go#L18-L38).
- **How state is implicitly encoded:** `schema_migrations` table existence, `max(version)` NULLness, `(version, checksum)` rows, and `newest`/`committed`/`recorded` booleans (`AppliedSchemaVersion` encodes `NoTable`, `EmptyTable`, and `Migrated(v)`).
- **States & transitions:** `NoTable` → `EmptyTable` (`CREATE TABLE IF NOT EXISTS` outside tx at `L110`) → `Migrated(v)` (or `Drifted` on checksum/order mismatch).
- **Invariants & risks:** `schemaMigrationsDDL` runs outside a transaction, leaving an observable `EmptyTable` state `(0, false, nil)` on crash. `applyMigration` (`L179-L181`) runs `ROLLBACK` ignoring errors even when `BEGIN IMMEDIATE` failed.

#### B5. Multi-actor session aggregate convergence (`cwd`, `repo`, `first_prompt`) (`medium`)
- **Location:** [`internal/store/ingest.go`](../internal/store/ingest.go#L527-L758), [`rederive.go`](../internal/store/rederive.go#L333-L409), [`source_workspace.go`](../internal/store/source_workspace.go#L70-L78), [`workspace.go`](../internal/store/workspace.go#L25-L157); contrasted with [`tla/Auth.tla`](../tla/Auth.tla).
- **How state is implicitly encoded:** SQL `NULL` columns on `sessions`, `sessions.metadata["firstPromptRef"]`, and `workspaces` (`ON CONFLICT DO NOTHING` write-once).
- **States & transitions:** `cwd`/`repo` start `NULL` and lock on first write (`Ingest` vs `SetSourceWorkspace` vs `ObserveWorkspaces`); `first_prompt` is overwritten only when `candidate.before(*stored)` (`SourceID`, then `SourceOffset`).
- **Invariants & risks:** Early `ObserveWorkspaces` inserting `repo = NULL` permanently locks out later repo resolution (`PendingWorkspaces` skips existing rows, matching `Auth.tla` `ObservationWriteOnce`). `SetSourceWorkspace` running before an event with `e.Cwd` locks `sessions.cwd` onto `sourceWorkspace`. `firstPromptRef` orders across sources by `SourceID` string rather than timestamp.

#### B6. Header-first native-ID establishment & embedded-meta detection (`medium`)
- **Location:** [`internal/codex/parser.go`](../internal/codex/parser.go#L80-L263), [`internal/pi/parser.go`](../internal/pi/parser.go#L75-L214), [`internal/antigravity/parser.go`](../internal/antigravity/parser.go#L99-L262), [`internal/claudecode/parser.go`](../internal/claudecode/parser.go#L88-L161), [`internal/events/events.go`](../internal/events/events.go#L42-L83).
- **How state is implicitly encoded:** `(base, o.Native, res.Native, o.SourceScope)`, `ohya-synthetic:` prefix re-inference, and `id != rec.Native` → `md["embedded"] = true` in `codex.normalizeSessionMeta`.
- **States & transitions:** At `base == 0`, line 0 locks `res.Native` to `RealNativeLocked` or `SyntheticLocked` (`ohya-synthetic:<scope>`); `base > 0 && o.Native == ""` returns `ErrNoNative`.
- **Invariants & risks:** In `codex`/`pi`, a non-header line 0 irrevocably locks the source to `ohya-synthetic:...` and misclassifies real `session_meta` on line 1+ as `embedded=true` (dropping `cwd`/`gitBranch`/`repo`). `codex`/`pi` fail with `ErrNoNative` when `base > 0 && o.Native == "" && len(data) == 0`, unlike `antigravity.Parse` (`L119`).

#### B7. Sequential pattern-mutation pipeline in `secrets.apply` (`medium`)
- **Location:** [`internal/secrets/secrets.go`](../internal/secrets/secrets.go#L14-L105).
- **How state is implicitly encoded:** Sequential 8-pass regex replacement mutating `s = b.String()` in place (`L70-L91`), so Pass 7 (`env-assignment`) inspects `[redacted:<id>]` markers emitted by Passes 0–6.
- **States & transitions:** `Unchecked` → Pass 0..6 (specific tokens) → Pass 7 (`env-assignment`).
- **Invariants & risks:** Quoted env assignments containing a known key (e.g., `OPENAI_API_KEY="sk-..."`) match `openai-api-key` first → `OPENAI_API_KEY="[redacted:openai-api-key]"`, then match Pass 7's `"[^"\n]{8,}"` branch — overwriting the specific marker with `[redacted:env-assignment]` and double-counting `onMatch`.

#### B8. JSONL framing, incomplete-tail & halted-source (`StopError`) protocol (`low`)
- **Location:** [`internal/claudecode/parser.go`](../internal/claudecode/parser.go#L78-L126), [`codex/parser.go`](../internal/codex/parser.go#L70-L125), [`pi/parser.go`](../internal/pi/parser.go#L65-L120), [`antigravity/parser.go`](../internal/antigravity/parser.go#L69-L155), [`internal/events/events.go`](../internal/events/events.go#L33-L83).
- **How state is implicitly encoded:** `rel` + `bytes.IndexByte('\n')` + `limit` + `ParseError{Offset, RecordTooLarge}`.
- **States & transitions:** `StreamingCompleteLines` → `IncompleteTailPaused` / `StoppedOversized` / `StoppedMalformed`.
- **Invariants & risks:** Well-contained partial-result contract; relies on callers honoring `Cursor` without skipping the stopped offset.

#### B9. SQLite backup / FTS snapshot dual-connection protocol (`low`)
- **Location:** [`internal/store/backup.go`](../internal/store/backup.go#L24-L140), [`internal/store/doctor.go`](../internal/store/doctor.go#L71-L140).
- **How state is implicitly encoded:** Sequential `*sqlite3.SQLiteConn` calls (`Begin()` → `sqlite_schema` → `Backup.Step(128)` → `Rollback()` + `driver.ErrBadConn`) + LIFO cleanup.
- **States & transitions:** `Opened` → `SnapshotPinned` → `SteppingPages` → `Completed` / `Aborted`.
- **Invariants & risks:** `FTSIndexConsistent` omits `backupBudget`, relying solely on caller `ctx`.

#### B10. `workspace.Sync` multi-round piggyback loop (`low`)
- **Location:** [`internal/workspace/sync.go`](../internal/workspace/sync.go#L16-L51).
- **How state is implicitly encoded:** `round` (`0..MaxRounds-1`), `tried map[string]bool`, `observed []Observation`.
- **States & transitions:** `InitialExchange` → `ResolveAndReply` loop → `FinalFlushExchange`.
- **Invariants & risks:** Transient `resolve` errors mark `tried[path] = true` for all 64 rounds of that `Sync` call; final flush discards returned `pending`.

---

### C. MCP Server & Observability

#### C1. `Opening` background DB open lifecycle (`high`)
- **Location:** [`internal/mcpserver/opening.go`](../internal/mcpserver/opening.go#L14-L73).
- **How state is implicitly encoded:** Unsynchronized `done chan struct{}`, `cancel context.CancelFunc`, nullable `backend Backend`, nullable `close func()`, and `err error` (no phase enum, `closed` flag, or `sync.Once`).
- **States & transitions:** `opening` (`done` open) → `ready` / `failed` (`done` closed) → `closed` (`Close()`).
- **Invariants & risks:** `Close()` waits `<-o.done` (`L67`) *before* calling `o.cancel()` (`L68`), hanging shutdown when `open(ctx)` is blocked on a hub write lock. Calling `Close()` twice invokes `o.close()` twice; calling `Query`/`AnnotateAs` after `Close()` still passes `ready()`; `open` returning `(nil, _, nil)` panics.

#### C2. `Engine` concurrency, tracing & pooled-connection guard (`medium`)
- **Location:** [`internal/mcpserver/query.go`](../internal/mcpserver/query.go#L95-L246), [`usage.go`](../internal/mcpserver/usage.go#L38-L160), [`annotate.go`](../internal/mcpserver/annotate.go#L50-L127), [`observer.go`](../internal/mcpserver/observer.go#L11-L23).
- **How state is implicitly encoded:** `slots chan struct{}`, `active int64`, nullable `writer *sql.DB`, `traced bool`, `queryTrace.closed`, `annotateOps != nil`, and sticky `RegisterAuthorizer`/`SetLimit` on pooled `*sqlite3.SQLiteConn`s in `e.readers`.
- **States & transitions:** `queryTrace`: `collecting` → `finished` (`closed=true`); pooled `SQLiteConn`: `unconfigured` → `bound_to_trace_N` → returned to pool via `defer conn.Close()` without clearing `RegisterAuthorizer(nil)`.
- **Invariants & risks:** Pooled reader connections retain the prior query's `trace.authorizer` + `e.authorize` + `SetLimit`. `annotateAs` shares `e.slots` without updating `sql_busy_rejections` or `sql_active`, and `e.readers.Conn(ctx)` timeouts (`L176`) bypass `queryError` and `ErrDeadline`.

#### C3. On-disk `observe/` instance lifecycle & stale cleanup (`medium`)
- **Location:** [`internal/observe/instance.go`](../internal/observe/instance.go#L22-L45), [`publisher.go`](../internal/observe/publisher.go#L401-L477), [`discovery.go`](../internal/observe/discovery.go#L12-L120), [`process.go`](../internal/observe/process.go#L11-L30).
- **How state is implicitly encoded:** Directory existence, `snapshot.json` presence/validity, `Stopped`, `PID`, `HasUsage`, `ModTime`, `staleCandidate.{terminated, live}`, and `ProbePID` strings (`"running"|"dead"|"unknown"`).
- **States & transitions:** `starting` → `running` (`live=true`) → `stopped`/`crashed` (`terminated=true`) or `corrupt/unclassified` (`!terminated && !live`) → `evicted` (`RetentionAge` 7d or `RetainedInstances` 8 per role).
- **Invariants & risks:** Unclassified dirs evade the per-role cap and crowd out `MaxLiveInstances = 64` for 7 days. PID reuse makes crashed instances immortal (`case candidate.live: continue`, `L452`). `LoadInstanceView` (`L98`) checks `InstanceID != instanceID`, whereas `CleanStaleInstances` (`L432`) does not.

#### C4. `Publisher` snapshot & history lifecycle (`medium`)
- **Location:** [`internal/observe/publisher.go`](../internal/observe/publisher.go#L48-L342).
- **How state is implicitly encoded:** `stop`/`done`/`closeOnce`, `hasHistory`, `nextHistoryMono`, `lastHistoryWallMS`/`lastHistoryUptime`, `lastPruneWallMS`/`lastCleanWallMS` (`0` = never), and write-once `lastErr`.
- **States & transitions:** `init` → `running` (`tick()`) → `stopped` (`Close()` writes final snapshot and forced `recordHistory`).
- **Invariants & risks:** `recordHistory` (`L221`) drops the final `Stopped: true` record whenever `SampledWallMS` and `UptimeMS` match the previous entry (sub-1ms start/stop). Wall-clock regressions stall `cleanStale` (`L152`) and `pruneHistory` (`L271`). Sticky `lastErr` (`L339`) reports transient startup errors even after recovery.

#### C5. `LogWriter` bounded async coalescing & rotation (`medium`)
- **Location:** [`internal/observe/logwriter.go`](../internal/observe/logwriter.go#L59-L403).
- **How state is implicitly encoded:** `stop`/`done`/`closeOnce`, `closed` under `mu`, `pending map[string]LogEntry` + `queue chan`, write-once `err`, `everNoticed`/`lastStderrMS`/`droppedSince`, `lastSummary`, `lastEntryMS` (`0` = no writes).
- **States & transitions:** `running` → `draining` (`stop` closed while `closed == false` so `emitSummary(true)` can enqueue at `L243`) → `closed` / `timed_out`.
- **Invariants & risks:** External `Event()` calls arriving between `drain()` returning (`L244`) and `closed = true` (`L246`) are dropped in `Close()` without clearing `pending`. `LogFlushTimeout` (`L225`) returns while `run()` is still writing/rotating. `droppedSince` is only printed inside `recordFailure()` (`L323-L324`), hiding overflow drops during healthy disk writes. `lastEntryMS == 0` suppresses heartbeats, and partial `rotate()` failure overwrites `.2` on retry.

#### C6. `UsageWriter` cached-FD & rotation lifecycle (`medium`)
- **Location:** [`internal/observe/usagewriter.go`](../internal/observe/usagewriter.go#L31-L225).
- **How state is implicitly encoded:** `queue chan`, `done`, `closed`, write-once `err`, worker-local `file *os.File` (`nil` vs open) and `size int64`.
- **States & transitions:** `unopened` → `open` → `rotating` → `unopened` (on error) → `closed`.
- **Invariants & risks:** `UsageFlushTimeout` (`L122`) returns while `run()` continues draining/writing. Caching `file`/`size` without re-statting writes to an unlinked inode after external deletion until `size > MaxFileBytes`. `w.err` in `fail()` (`L221`) never resets.

#### C7. `GaugeGroup` per-slot gauge aggregator (`low`)
- **Location:** [`internal/observe/observer.go`](../internal/observe/observer.go#L46-L117).
- **How state is implicitly encoded:** `slots map[string]map[string]float64` with no removal/close API.
- **States & transitions:** `unregistered` → `registered` on first `Set`.
- **Invariants & risks:** Retired slots permanently inflate summed gauges (`watch_sources`, `watch_partial_tail_bytes`, `watch_pending_complete_bytes`) unless zeroed; `mu.Unlock()` before `base.Set` (`L99-L100`) permits out-of-order sum publication.

---

### D. CLI Commands, Setup Engine & Installer

#### D1. `ingestor` per-source registration, stop/no-progress, missing/excluded & sticky-purge (`high`)
- **Location:** [`cmd/ohya/ingest.go`](../cmd/ohya/ingest.go#L213-L259), [`L309-L478`](../cmd/ohya/ingest.go#L309-L478), [`L530-L666`](../cmd/ohya/ingest.go#L530-L666).
- **How state is implicitly encoded:** Seven parallel `SourceID` maps (`registered`, `purged`, `missingReported`, `present`, `noProgress`, `tails`, `reported`) + per-pass `excludedPaths`.
- **States & transitions:** `unseen` → `missing_at_startup` / `present` → `missing` / `excluded` → `returned`; `unregistered` → `registered` → `active` → `no_progress` / `stopped` → `purged` / `replaced`.
- **Invariants & risks:** `ErrSourcePurged` (`L369-L374`, `L404-L409`) deletes `registered` and `tails` but leaves `noProgress` and `reported` intact, keeping purged-after-stop sources in `watch_sources{state="stopped"}`. `noProgress` (`L386-L397`) skips `ingestSource`, hiding hub purge/unregistration. `purged[id]` stays sticky until restart. `ReplacedSourceID` cleanup (`L349-L354`) omits `reported`, `missingReported`, `present`, `workspaceTried`, and `workspaceFailed`. `r.tails` mutates before `Acknowledge` (`L429-L443`).

#### D2. `runJoin` / `rejoin` multi-file pairing & credential state (`high`)
- **Location:** [`cmd/ohya/join.go`](../cmd/ohya/join.go#L139-L176), [`L243-L301`](../cmd/ohya/join.go#L243-L301).
- **How state is implicitly encoded:** On-disk `credential.json`, `device.json`, `hub-address.json` + CLI `o.code`, `o.pairOnly`.
- **States & transitions:** `unpaired` → `pair()` RPC → `SaveCredential` → `SaveHubAddress` → `paired`; `paired_missing_addr` → `verifyCredential` → `SaveHubAddress`.
- **Invariants & risks:** Crash/error between `pair()` RPC (`L151`) and `SaveCredential` (`L158`) burns the one-time invitation and leaves the local `device.json` ID permanently paired on the hub without a local credential. `runJoin` never runs `device.CheckPairedIdentity` when `LoadCredential` succeeds.

#### D3. `runInstallCommand` & `parseInstallArgs` dry-run split & unrolled binary swap (`high`)
- **Location:** [`cmd/ohya/install.go`](../cmd/ohya/install.go#L15-L93).
- **How state is implicitly encoded:** Manual string-matching `dry bool` in `parseInstallArgs` (`L84-L88`) vs Go `flag` parsing in child `ohya setup` (`L54`), plus unrolled `install.Binary` → `exec.Command(target, "setup", ...)`.
- **States & transitions:** Manual parse → optional version check → `install.Binary` (if `!dry`) → exec `setup`.
- **Invariants & risks:** `ohya install -dry-run` fails to set `dry = true` (only matches `--dry-run`), overwriting `~/.local/bin/ohya` while child `ohya setup -dry-run` executes as a dry run. No rollback runs if child `setup` fails (`L58-L60`). `--help` is only honored when `len(args) == 1` (`L16`).

#### D4. `setup.Run` & `configureOwnedService` multi-phase setup, backup & migration (`high`)
- **Location:** [`internal/setup/engine.go`](../internal/setup/engine.go#L40-L48), [`L88-L288`](../internal/setup/engine.go#L88-L288), [`L412-L593`](../internal/setup/engine.go#L412-L593); [`internal/setup/service_setup.go`](../internal/setup/service_setup.go#L19-L121).
- **How state is implicitly encoded:** `Options` booleans, `blocked`, `report.Complete()`, `withRecovery`/`active`/`stopped`/`len(original) > 0`, and `.recovery-*` vs `recovery-*` backup directories.
- **States & transitions:** `DiscoverClients`/`PreflightClient` → `Lock` → `Prepare` → `ApplyProfile` → `configureService` (stop → `recoveryPoint` → `migrateDatabase` → `ApplyFile` → start → `waitService`, or `restoreService`) → `hub-service` → `CheckHub` → `Verify` → `ApplyClient` + `ApplySkill`.
- **Invariants & risks:** `migrateDatabase` commits schema changes before `ApplyFile`/`Start`/`waitService`; failure restores the old unit against a migrated DB. Initial service install failure (`len(original) == 0`) leaves partial unit/receipt behind. `Prepare` persists `preferences.json`, pairing, and `connection.json` before backup/migration. `--no-service` creates and prunes recovery backups without migrating (`L217-L225`). `ApplyProfile` failure continues into `configureService` (`L189-L200`).

#### D5. `ingestor` workspace-sync & Antigravity index retry state (`medium`)
- **Location:** [`cmd/ohya/ingest.go`](../cmd/ohya/ingest.go#L231-L234), [`L471-L528`](../cmd/ohya/ingest.go#L471-L528); [`cmd/ohya/antigravity_workspace.go`](../cmd/ohya/antigravity_workspace.go#L22-L63).
- **How state is implicitly encoded:** `workspacesDue`, `workspaceTried`, `workspaceFailed`, per-pass `grew`, and closure-local `reported`.
- **Invariants & risks:** `immutable=1` index read misses uncheckpointed Antigravity workspaces once while `workspaceTried[s.ID]` latches `true` permanently if the session stops growing. `observeWorkspaces` clears `workspacesDue` even when `len(res.Failures) > 0` (`L518-L527`).

#### D6. `pathRecorder` per-device auth/sync-path cache & error suppression (`medium`)
- **Location:** [`cmd/ohya/hubpath.go`](../cmd/ohya/hubpath.go#L15-L87).
- **How state is implicitly encoded:** `remote map[string]net.Addr`, `recorded map[string]time.Time`, global `reported bool` under `mu`.
- **Invariants & risks:** Concurrent `join` + `mcp --remote` connections overwrite `remote[deviceID]`; maps grow unboundedly; a single `RecordSync` error suppresses all future error logs across all devices.

#### D7. `runSetupCommandWith` & `setupPreferences` flag/env/mode resolution (`medium`)
- **Location:** [`cmd/ohya/setup.go`](../cmd/ohya/setup.go#L82-L278); [`setup_preferences.go`](../cmd/ohya/setup_preferences.go#L10-L36); [`setup_hub.go`](../cmd/ohya/setup_hub.go#L26-L70).
- **How state is implicitly encoded:** Pre-parse mutation of `*roots.claude` from `CLAUDE_CONFIG_DIR` (`setup.go#L118-L120`), `fs.Visit` override in `setupPreferences`, `connection.Mode`, early `unsetenv(*codeEnv)`, and `hubReceipt`.
- **Invariants & risks:** `CLAUDE_CONFIG_DIR` is clobbered by saved `preferences.Flags["claude-root"]` because `*roots.claude` bypasses `fs.Set`. `setupPreferences` calls `os.Getenv` directly instead of `rt.getenv`. Mode switching leaves stale `hub-service.json`/`hub.db`/`join/`. `probeHubWithRetry` (`setup_hub.go#L40`) aborts immediately on `ErrRelayUnreachable`.

#### D8. Divergent mode & state-dir resolution across read/diagnostic commands (`medium`)
- **Location:** [`cmd/ohya/status.go`](../cmd/ohya/status.go#L287-L304), [`L1327-L1336`](../cmd/ohya/status.go#L1327-L1336); [`doctor.go`](../cmd/ohya/doctor.go#L109-L128); [`diagnose.go`](../cmd/ohya/diagnose.go#L115-L129); [`recall.go`](../cmd/ohya/recall.go#L382-L401).
- **How state is implicitly encoded:** Combinations of `*state != ""`, `explicitDB`, `setup.LoadConnection(setupState).Mode`, and `--remote`.
- **Invariants & risks:** `doctor --state ~/.ohya` on a remote host skips local DB checks while inspecting `~/.ohya` instead of `~/.ohya/join`; `status --state ~/.ohya` mixes `observe/` from `~/.ohya` with `credential.json` from `~/.ohya/join`.

#### D9. Two-phase receipt-before-target ownership & edit-detection protocol (`medium`)
- **Location:** [`internal/setup/ownedfile.go`](../internal/setup/ownedfile.go#L15-L127), [`jsonentry.go`](../internal/setup/jsonentry.go#L145-L231), [`tomlentry.go`](../internal/setup/tomlentry.go#L168-L252), [`profile.go`](../internal/setup/profile.go#L75-L233), [`configbackup.go`](../internal/setup/configbackup.go#L13-L40), [`inspect.go`](../internal/setup/inspect.go#L12-L142), [`skill.go`](../internal/setup/skill.go#L130-L184).
- **How state is implicitly encoded:** Separate receipt JSON (`Content`/`Value`/`Block` + `Previous`) and target file on disk; write writes receipt first, remove deletes target first.
- **Invariants & risks:** `Remove*` leaves orphaned receipts on receipt-delete failure; missing target on `ApplyFile` bypasses edit checks; `tomlentry.go#L209-L213` drifts `Previous`; `profileReceiptPath` keys solely by basename; `sharedSkillOwner` lets one client overwrite another's receipt (`skills/claude.json`).

#### D10. `ControlService` & `service.go` lifecycle, string-sniffing & polling (`medium`)
- **Location:** [`internal/setup/service_control.go`](../internal/setup/service_control.go#L47-L251); [`internal/setup/service.go`](../internal/setup/service.go#L89-L121), [`L228-L284`](../internal/setup/service.go#L228-L284).
- **How state is implicitly encoded:** Explicit `ServiceAction` paired with implicit `(receipt, !unitMissing, edited, loaded, running, changed)`, `successes == 2`, and `args` substring matching.
- **Invariants & risks:** `RunServiceCommand` dispatches by `args` substrings; `waitLaunchdRelease` treats any non-nil `launchctl print` error as release; `ControlService` patches `planFn(..., "/ohya")` by slice index.

#### D11. Pi native-vs-legacy compatibility switching & two receipts (`medium`)
- **Location:** [`internal/setup/engine.go`](../internal/setup/engine.go#L240-L267), [`probe.go`](../internal/setup/probe.go#L26-L53), [`pi.go`](../internal/setup/pi.go#L16-L48).
- **How state is implicitly encoded:** Runtime probe (`pi mcp --help` / `pi --version == 0.84.2`) switches between `clients/pi.json` and dual receipts (`pi-bridge-receipt.json` + `pi-extension.json`).
- **Invariants & risks:** Non-atomic switch deletes the old integration before `ApplyPiExtension`/`ApplyClient` succeeds; `RemovePiExtension` handles two receipts sequentially without rollback.

#### D12. `install.Binary` / `install.Remove` receipt-first swap & backups (`medium`)
- **Location:** [`internal/install/install.go`](../internal/install/install.go#L18-L22), [`L66-L258`](../internal/install/install.go#L66-L258).
- **How state is implicitly encoded:** `ohya`, `.ohya-install.json`, `.ohya-install.lock`, `.ohya-stage-*`, `.ohya-backups/install-*`, `result.Changed`.
- **Invariants & risks:** `parent.Sync()` failure (`L164-L167`) returns `Changed: false` after updating both binary and receipt; `.ohya-backups/` is created before staging and never pruned; `Remove` deletes the binary before the receipt.

#### D13. Directory stat-stamp discovery cache (`low`)
- **Location:** [`cmd/ohya/ingest.go`](../cmd/ohya/ingest.go#L687-L730), [`L760-L848`](../cmd/ohya/ingest.go#L760-L848).
- **How state is implicitly encoded:** `discoveryCache.dirs`, `StableAt`, `len(excludes) == 0` gate, threshold `clear()`.
- **Invariants & risks:** Any `--exclude` disables directory caching completely.

#### D14. `joinEventGate` multi-uploader offline/online & `joinDiscover` (`low`)
- **Location:** [`cmd/ohya/join.go`](../cmd/ohya/join.go#L328-L351), [`L390-L442`](../cmd/ohya/join.go#L390-L442).
- **How state is implicitly encoded:** `offline map[string]bool`, `reportedOffline bool`, `lastExcluded = -1`.
- **Invariants & risks:** One stalled offline agent suppresses `EventTransportOnline` globally; `lastExcluded` misses equal-cardinality swaps.

#### D15. `runMcp` dual-goroutine open & shutdown coordination (`low`)
- **Location:** [`cmd/ohya/mcp.go`](../cmd/ohya/mcp.go#L74-L104).
- **How state is implicitly encoded:** `ctx`, `serverCtx`/`stopServer`, `opening`, unjoined watcher goroutine (`L93-L97`), error precedence (`L98-L103`).
- **Invariants & risks:** Watcher goroutine is not joined on return; `openErr` shadows `runErr`.

#### D16. `status` instance retention inference & `logs --follow` tailer (`low`)
- **Location:** [`cmd/ohya/status.go`](../cmd/ohya/status.go#L357-L401), [`L506-L578`](../cmd/ohya/status.go#L506-L578), [`L747-L793`](../cmd/ohya/status.go#L747-L793); [`logs.go`](../cmd/ohya/logs.go#L375-L587).
- **How state is implicitly encoded:** Inferred instance states + `unreplacedDeadSeen`, `logTailer`.
- **Invariants & risks:** `isRetainedStatusInstance` depends on slice discovery order; `followLogs` has a replay-to-`SeekEnd` gap and drops partial lines on `io.EOF`.

#### D17. `Connection` absence-as-local & dual-state binding (`low`)
- **Location:** [`internal/setup/connection.go`](../internal/setup/connection.go#L23-L71); [`cmd/ohya/setup.go`](../cmd/ohya/setup.go#L235-L251).
- **How state is implicitly encoded:** Absent `setup/connection.json` decodes as `Mode: "local"`; remote setup writes `connection.json` in both `~/.ohya` and `~/.ohya/join`.
- **Invariants & risks:** Unconfigured state dirs are indistinguishable from explicit `local`; switching `~/.ohya` to `local` leaves `join/setup/connection.json` pinned to `remote`.

---

### E. macOS Menu Bar App (`macos/OhyaMenuBar/`)

#### E1. `AppModel` polling cadence, refresh coalescing & mixed-epoch cache (`medium`)
- **Location:** [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L53), [`L93-L94`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L93-L94), [`L121-L198`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L121-L198), [`L314-L405`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L314-L405); [`OhyaMenuBarApp.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/OhyaMenuBarApp.swift#L13-L18).
- **How state is implicitly encoded:** `isPopoverVisible`, `isRefreshing`, `statusRefreshPending`, `pollTask`, `.distantPast` sentinels (`lastSecondaryFetch`, `lastRemoteHubFetch`), `next.remoteHub = status?.remoteHub`.
- **Invariants & risks:** `restartPollCadence()` (`L145-L146`) cancels `refreshAll()` mid-flight when opening the popover during startup. `refreshStatus()` returns immediately when `isRefreshing == true` (`L315-L318`), leaving `toggleService()`/`setPopoverVisible(true)` with stale status. Cooldown timestamps are stamped before CLI success, and `L350-L352` carries stale `remoteHub` across mode switches.

#### E2. `ObserveWatcher` directory watcher lifecycle (`medium`)
- **Location:** [`ObserveWatcher.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/ObserveWatcher.swift#L4-L41); [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L356-L361).
- **How state is implicitly encoded:** Nullable `source: DispatchSourceFileSystemObject?`, `watchedPath: String?`, `O_EVTONLY` fd.
- **Invariants & risks:** `L14` (`watchedPath == directory && source != nil`) never re-arms if `observe/` is deleted/recreated, holding an unlinked fd indefinitely. Lacks a `stop()` teardown method.

#### E3. `OhyaRunner.execute` subprocess, timeout & pipe-drain lifecycle (`medium`)
- **Location:** [`OhyaRunner.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBarCore/OhyaRunner.swift#L176-L244), [`L271-L315`](../macos/OhyaMenuBar/Sources/OhyaMenuBarCore/OhyaRunner.swift#L271-L315).
- **How state is implicitly encoded:** `LockedExecutionState.finished`, `exitCode = -1`, `DispatchGroup` (3 entries), `DispatchSourceTimer`, `CheckedContinuation`.
- **Invariants & risks:** Missing `withTaskCancellationHandler` (cancelling `pollTask`/`searchTask`/`binaryOverrideTask` leaves subprocesses running up to 180s). On timeout, `readDataToEndOfFile()` on `.utility` global queue blocks until grandchildren close pipe FDs.

#### E4. Recall search debounce, generation tracking & unguarded inline preview (`medium`)
- **Location:** [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L100-L109), [`L439-L510`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L439-L510); [`RecallView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/RecallView.swift#L60-L116), [`L261-L293`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/RecallView.swift#L261-L293).
- **How state is implicitly encoded:** `recallGeneration`, `searchTask`, `isLoadingRecall`, `isLoadingPreview`, `expandedSessionID`/`expandedShowData`, `recentSessions`/`searchHits`.
- **Invariants & risks:** `toggleSessionPreview` lacks generation/cancellation (`try?` `L501` + `defer { isLoadingPreview = false }` `L499`). `refreshRecall()` does not cancel `searchTask` (clear/Enter schedules duplicate searches). Query/filter changes leave `expandedSessionID`/`expandedShowData` stale.

#### E5. Binary resolution, `setupNeeded`, first-run wizard & update lifecycle (`medium`)
- **Location:** [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L55-L91), [`L189-L301`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L189-L301); [`FirstRunView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/FirstRunView.swift#L6-L182); [`PopoverRootView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/PopoverRootView.swift#L17-L31); [`StatusView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/StatusView.swift#L11-L29).
- **How state is implicitly encoded:** `setupNeeded`, `isRunningSetup`, `isUpdating`, `@State joining`, nullable `resolvedBinaryPath`/`setupReport`/`setupError`/`bundledUpdate`/`updateError`, `binaryOverridePath`/`joinCode`.
- **Invariants & risks:** `FirstRunView.join()` clears `joinCode` and resets `joining = false` before `runSetup` (`L100-L106`), and `"Run Setup Again"` (`L168-L170`) hardcodes `joinCode: nil`, trapping failed joins. `setupNeeded` defaults `false` and swallows errors via `try?` (`L247`). Changing `binaryOverridePath` leaves prior binary's cached data visible beneath the error card (`StatusView.swift#L17`).

#### E6. Stale-status/error hybrid & unmodeled sub-states (`StatusSummary`, `LoginItemStore`) (`medium`)
- **Location:** [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L353-L368), [`L701-L708`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L701-L708); [`DomainLogic.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBarCore/DomainLogic.swift#L137-L205); [`PopoverRootView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/PopoverRootView.swift#L123-L184); [`LoginItemStore.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/LoginItemStore.swift#L7-L34).
- **How state is implicitly encoded:** Explicit `MenuBarIconState` paired with retained `status`, `lastError`, `setupNeeded`, `isPaused`, and `LoginItemStore.(isEnabled, requiresApproval, lastError)`.
- **Invariants & risks:** Hybrid `.unavailable` + stale `isPaused`/`totalBacklogBytes`/`stoppedSources`; `isPaused` (`"Resume"`) while `iconState != .paused`; popover `"Not set up"` vs stale menu bar icon; `LoginItemStore.refresh()` (`L16-L20`) drops `.notFound` and retains stale `lastError` in `SettingsView`.

#### E7. Modal sheets, purge execution, diagnostics export, logs & device actions (`medium`)
- **Location:** [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L96-L124), [`L544-L699`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L544-L699); [`PurgeReviewSheet.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/PurgeReviewSheet.swift#L7-L67); [`LogsSheetView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/LogsSheetView.swift#L25-L60); [`DevicesView.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Views/DevicesView.swift#L7-L166).
- **How state is implicitly encoded:** `showingLogs`, `showingPurgeReview`, view-local `@State applying`/`revokingDevice`, missing in-flight locks, `NSSavePanel` before `isExportingDiagnostics = true`.
- **Invariants & risks:** Dismissing `PurgeReviewSheet` resets `@State applying`, allowing concurrent `purge --apply` while `refreshSecondary` mutates `purgePlan` underneath. `openLogs()` uses `try?` without generation guard. `showingLogs && showingPurgeReview` can both be `true`. `NSSavePanel.runModal()` allows overlapping exports. Expired `activeInvite` remains copyable.

#### E8. `Notifier` transition alerting & cooldown (`low`)
- **Location:** [`Notifier.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/Notifier.swift#L5-L52); [`AppModel.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/AppModel.swift#L67-L74); [`DomainLogic.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBarCore/DomainLogic.swift#L207-L226).
- **How state is implicitly encoded:** `previousSummary`, `[String: Date] lastSentAt`, `notificationsEnabled`.
- **Invariants & risks:** Stamps `lastSentAt` (`L38`) before `UNUserNotificationCenter.add` without checking authorization; `init()` omits `requestAuthorizationIfNeeded()`; `0 -> >0` edge misses `1 -> 2`; stale `status` freezes baseline.

#### E9. `ClipboardGuard` & `CopyFeedbackButton` timers (`low`)
- **Location:** [`ClipboardGuard.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/State/ClipboardGuard.swift#L4-L39); [`GlassStyle.swift`](../macos/OhyaMenuBar/Sources/OhyaMenuBar/Design/GlassStyle.swift#L181-L228).
- **How state is implicitly encoded:** `clearTask` + `capturedChangeCount`; `@State copied` + untracked `Task`.
- **Invariants & risks:** `NSApp.terminate(nil)` drops `clearTask` leaving secrets on pasteboard; double-clicking within 1.5s lets the first `Task` prematurely reset `copied`.

---

## 4. Rejected False Positives

1. [`internal/mcpserver/statement.go`](../internal/mcpserver/statement.go#L8-L80) (`singleStatement` / `mark`): Pure single-call lexical scanner over a query string, not a persistent component lifecycle (edge note: `(false, true, false)` rejects `; SELECT 1` as `ErrMultipleStatements`).
2. [`internal/workspace/config.go`](../internal/workspace/config.go#L10-L128) (`parseRemotes` / `parseSection` / `parseValue`): Stateless git INI config parser.
3. [`internal/buildinfo/buildinfo.go`](../internal/buildinfo/buildinfo.go#L8-L37) (`Identity`) & [`cmd/ohya/doctor_mcp.go`](../cmd/ohya/doctor_mcp.go#L12-L90) (`buildIdentity` / `checkMCPBuilds`): Stateless sentinel string evaluation (`"unknown"`, `"devel"`).

---

## 5. Suggested Next Steps

1. **Fix [`Opening.Close()`](../internal/mcpserver/opening.go#L66-L73) cancellation order and explicit closed state:** Cancel `o.cancel()` before waiting on `<-o.done`, guard close/ready transitions explicitly, and reject post-close `Query`/`AnnotateAs`.
2. **Fix [`address.Device.releaseSlotLocked`](../internal/transport/address/address.go#L304-L314) & [`RemoteSession.call`](../internal/hubsync/session.go#L194-L200):** Ensure `slot.client` is closed at most once when in-flight dials finish after `Device.Close()`, and call `dropLocked()` when a retried RPC fails on a transport/timeout error.
3. **Align `Uploader` & `ingestor` purge/reallow behavior with `Sync.tla` / `Auth.tla`:** Replace process-sticky `blocked`/`purged` latches with explicit per-source state (re-probing reallowed sources on reconnect or poll), clear `noProgress`/`reported` on purge, and add cursor-regression reconciliation to `ingestor`.
4. **Make [`ApplyPurge`](../internal/store/purge.go#L304-L332) transactional and harden [`ReDerive`](../internal/store/rederive.go#L44-L140):** Execute `PlanPurge`, `purgeOneSource`, and `dropEmptySelectedSessions` in one `BEGIN IMMEDIATE` transaction, and handle purged+reallowed sources with empty `first_native_id` safely.
5. **Fix [`parseInstallArgs`](../cmd/ohya/install.go#L64-L93) `-dry-run` handling & harden [`internal/setup`](../internal/setup/service_setup.go#L19-L102):** Match single-dash `-dry-run` consistently with Go `flag` and make DB migration / service rollback / recovery point ordering explicit.


## 6. Follow-up

The five fixes in section 5 are implemented. Subsequent findings remain
historical audit items unless a fix above covers them.

- Opening cancels a pending database open before it waits. It closes the
  backend once and returns an error for tool calls after closure.
- Device shutdown cancels active dials. Each transport client closes once,
  after its last dial returns. Drain also checks retired clients. The
  remote session discards a connection after a failed transport retry.
- Re-admitted sources resume at the frozen cursor without a restart.
  Local ingestion also reconciles a restored database cursor when it
  prepares a source or finds pending input.
- Purge plans and applies changes in one write transaction. Re-derive
  recovers a missing native ID from the earliest retained event.
- Install and setup share flag parsing. Setup removes a failed new service
  or restores the previous unit. The recovery point remains available
  after a committed migration.
