# 0013: Owner-scoped session purge with source eviction

Status: accepted, 2026-11-19 (T144).

## Context

ohya's stance so far has been "keep everything". Two facts broke that stance:

- Claude Code deletes local transcripts after 30 days by default, so the hub
  often holds the only copy — the hub is the retention point, and a retention
  point the owner cannot trim is a liability, not a feature.
- The deployed hub measures 16.3 GB at 1.66x the mirrored transcript bytes,
  with 0.5% of rows holding 27% of the bytes
  (NOTES.md, "Hub size breakdown", 2026-11-16). Atuin's most-cited complaint
  after shipping the same kind of store is the absence of any delete
  (atuin #4434: no delete, 482 MB and growing) — the same complaint was
  coming for us.

The operator decision (2026-11-19, competitive study
`/notes/2026-11-19-atuin-threads-vs-ohya.md`): build retention the ohya way —
owner-decided, loud, scoped, never automatic.

## Decision

**Purge evicts whole sessions and whole sources, and replaces each evicted
source row with a tombstone.** The tombstone makes the purge durable against
the write path: a device that re-registers or replays a purged source ID
receives a deterministic refusal naming the purge date and the remedy. Purged
bytes never return automatically; re-admission is an explicit owner action
(`ohya sources reallow`), which removes the tombstone and restores the source
row at its purged cursor, so history resumes from the purge point.

### Command surface

- `ohya purge` selects sessions with `--session <native-id>`, `--repo`,
  `--agent`, and `--before <RFC3339 date>` (all given selectors must match; a
  session matches `--before` when its observed activity — `last_event_at_ms`,
  falling back to `started_at_ms` — is before the date; sessions with no
  timestamps at all are never selected by `--before`, because an undatable
  session must not be purged on a guess).
- Dry run is the default and prints the affected sessions, sources, event
  counts, and bytes (the evicted sources' committed `cursor_bytes`, the same
  measure as the hub's mirrored-bytes accounting). `--apply` deletes.
  `--json` follows the [ADR 0005](0005-json-output.md) envelope.
- `ohya sources reallow <source-id>` removes one tombstone and restores the
  source row at the purged cursor.

### Execution

- One transaction per source, in source ID order: bounded WAL per commit, and
  an interrupted run leaves the already-purged sources durably purged; rerun
  with the same selectors and it plans from the current state and continues
  (pinned by a test that aborts the second source's tombstone insert and
  resumes).
- Delete order per source: re-read the source's authoritative `cursor_bytes`,
  `path`, and `first_native_id` inside the transaction (so concurrent writes
  never produce a stale tombstone cursor), verify no unselected real session
  shares the source, delete `event_annotations` of the source's events, detach
  any cross-source `events.duplicate_of` references pointing into this source,
  delete `events` (the external-content FTS delete trigger fires per row),
  then for each session that lost its last event delete `session_token_usage`,
  the `sessions` row, and any now-orphaned `workspaces` row for `(device_id,
  cwd)`, then delete the `sources` row, then insert the `purged_sources`
  tombstone. Token-usage rows are keyed by session, not source, so they are
  removed exactly when their session row is removed — deleting them per source
  would corrupt the totals of a session that still holds events from another
  source (a Codex thread spanning rollout files).
- The FTS index stays consistent through the per-row delete trigger; tests
  assert the FTS5 `integrity-check` after purge and after resumed sync.

### The tombstone

Migration 008 adds `sources.first_native_id` (backfilled from offset-0 events
on upgrade) and `purged_sources` (source ID, device, agent, path, the
committed cursor at eviction, the purged session's native ID, `first_native_id`,
the purge scope text, and the purge timestamp) with indexes on native ID and
(device, agent). `purged_sources` is denied to the MCP `sql` tool by default so
purged paths and scopes do not linger in agent queries.

- `store.RegisterSource`, `store.Ingest`, and `store.SourceState` check the
  tombstone whenever the source row is absent, and answer a deterministic
  refusal (`store.ErrSourcePurged`) whose text names the purge date, the
  scope, and the remedy (`ohya sources reallow <id>` plus restarting a running
  `ohya join` on the device). The endpoint maps it to `CodeRejected` — the
  connection stays open, so other sources on the same device are unaffected;
  the uploader blocks exactly the purged source for its process lifetime and
  logs the remedy.
- The refusal covers any device: the tombstone keeps the freed source ID
  from being re-registered by a stranger, exactly as ownership did while the
  row lived.
- Reallow restores `cursor_bytes` and `first_native_id` from the tombstone (so
  header-first parsers such as Codex and Pi continue to parse and re-derive
  past offset 0 without their deleted offset-0 event). After restarting
  `ohya join` on the device, its probe finds the hub cursor equal to (or ahead
  of) its checkpoint, so it never rewinds and never replays. A below-cursor
  replay is refused by the write path's existing rule — there are no stored
  records below the committed cursor — so the purged records cannot come back
  even then.

### Why evict-and-tombstone, not a digest table

The alternative design kept a digest (a hash per purged record, or per purged
source) instead of the source row, so a replay could be checked record by
record and rejected precisely. It was rejected for v1:

- The digest buys nothing for the shipped parsers, because a session purge
  always evicts whole sources (invariant below): there is no partial-source
  purge, so no record-level validation is ever needed to decide a replay.
  The cursor alone encodes everything the write path must know.
- A per-record digest is a new table at event cardinality (2.7 M rows on the
  deployed hub) that exists only to refuse replays the cursor already
  refuses. Per-record digests become necessary only for partial-source purge
  (deleting a time range inside a still-live session), which is deliberately
  out of scope for v1.

### The source-to-session invariant, verified

The eviction argument rests on one structural fact: **a source holds events
of at most one real session, plus at most the source-scoped synthetic
session** (`ohya-synthetic:<source-id>`, from records that carry no session
ID). Verified on the deployed hub (2026-11-19, 2.7 M events, read-only):

| Query (whole hub) | Result |
|---|---|
| Sources holding events of two real sessions | 0 |
| Sources holding a synthetic session of another source | 0 |
| Sources holding events of more than two sessions | 0 |
| Synthetic sessions spanning more than one source | 0 |
| Sources holding a real session plus their own synthetic session | 292 (all claude-code) |

The naive reading "one source, one session" is therefore true only for real
sessions: 292 Claude Code files mix a real session with their own synthetic
companion (subagent `started`/`result` records carry no session ID).
`internal/store` pins the invariant as a test over the fixture corpus
(`TestNoSourceHoldsTwoRealSessions`), including the mixed case.

Consequences for the design:

- A purge selected on a session evicts every source holding that session's
  events — never less (the session's history is gone as a unit) and never
  more (another session's events cannot share the evicted source).
- The source's synthetic companion events are evicted with the source, and
  the synthetic session row drops with its last event. It exists only inside
  that source, so this destroys nothing else.
- Synthetic sessions are not selectable (`ohya-synthetic:` native IDs are
  excluded from planning). Purging "the synthetic session" alone would evict
  the whole source and thereby the real session's history — a scope the
  operator did not name.

## Model checking

Both protocol models gained the terminal purged state; `make tla` checks them
([tla/README.md](../../tla/README.md) records the details).

- `Sync.tla` gains `PurgeSource`/`ReallowSource` (owner actions, bounded by
  `MaxPurges`) and a tombstone guard on `HubCommit`. New and restated
  invariants cover the required properties:
  `NoResurrection` (no record below the purge mark is ever stored again),
  `PurgedSourceStoresNothing`, `TombstoneConsistent` (a tombstone flips only
  through the paired owner actions), `HubCursorNeverRegresses` and
  `PurgeMarkNeverRegresses` (action properties), and the restated
  `AckedRecordsStored` (an acknowledged record is stored, or below a purge
  mark) and `StoredPrefixContiguous` (stored is the range above the purge
  mark up to the cursor). The purged source's refusal is per-source state, so
  with two sources TLC explores the purged source alongside the healthy one
  still syncing — the refusal cannot wedge unrelated sources.
- The hub cursor never regresses in the model because purge does not reset
  it: the model freezes `hubCursor` at the purge-time value (the row is gone
  in the implementation; the reallowed row returns at exactly that value),
  and a below-cursor replay stores nothing — which is precisely what the
  implementation's replay rejection achieves.
- `Auth.tla` gains the registration lifecycle (`srcReg`: registered ↔ purged
  through the same owner actions, `TombstoneMatchesOwnerActions`) and guards
  `Upload` on a registered source. Revocation still wins over re-admission:
  a reallowed source owned by a revoked device stays unreachable, because
  the registry check in `Authorized` is unaffected by purge state
  (`NoLiveSessionForRevoked` over the new interleavings).

### Model assumptions

- Purge and reallow are owner actions, not protocol actions: no fairness is
  assumed for them, and their counts are bounded (`MaxPurges`) to keep the
  state space finite; the liveness configuration keeps `MaxPurges = 0`
  because a purged source legitimately stops syncing.
- The uploader-level below-cursor replay cannot occur in practice (every new
  connection probes the hub cursor first and reconciles), so the store-level
  rejection is pinned by a direct write-path test, and the e2e pins the
  uploader behavior: after reallow the device resumes with only
  post-purge bytes.
- Device-side checkpoints and files are untouched by purge; restoring a
  device checkpoint from before the purge results in the same reconciliation
  as a lost acknowledgment (the device jumps to the hub cursor), never in a
  replay of purged bytes.

## Out of scope for v1 (deliberately)

- **Partial-source purge** (a time range inside a still-live session). Needs
  per-record digests to refuse replays precisely; no evidence of demand.
- **Time-based automatic retention policies.** Retention is owner-decided;
  `--before` is a selector an operator runs, not a daemon.
- **Device-side file deletion.** Purge is a hub-side decision about stored
  data; devices keep their files and checkpoints, so nothing device-side
  changes.
- **Un-purge.** Deleted rows are gone; recovery copies are the only undo
  (see below).
- **FTS-only compaction** and **VACUUM**: freed pages stay in the database
  file after a purge; the file shrinks only through SQLite's ordinary
  free-page reuse or an operator-run VACUUM. Documented, not built.

## Operations notes

- Migration 008 is a schema-only migration, but every migration forces a
  size-aware recovery copy at the next setup run; on a slow single disk the
  copy of a 16 GB hub costs hours
  (NOTES.md, 2026-11-16; [release-operations](../release-operations.md),
  "Recovery point cost").
- A recovery copy taken before a purge still contains the purged data, and
  restoring it undoes the purge entirely (rows and tombstones). Treat
  pre-purge recovery copies as still holding everything the purge removed.

## Consequences

Retention becomes an owner decision with a loud, scoped, resumable command
and a deterministic write-path refusal instead of an silent re-ingestion
loop. The cost is one small table and one new source lifecycle state
(registered → purged → registered-at-cursor), which both protocol models now
carry. `ohya purge --dry-run`-style safety (dry run is the default) and the
per-source transaction bound make the operation safe to rerun and safe to
interrupt.
