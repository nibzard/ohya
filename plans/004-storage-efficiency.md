# Storage efficiency

Status: in delivery. S1 (T100), S2 (T101), and S3 (T102) are implemented,
delivered 2026-11-16 to 2026-11-17. D1 (T110), D2 (T111), and D3 (T112)
remain open, deferred until the plan 002 acceptance gate. Priority P2 for
the cheap items, deferred for the rest. Planned against `d237c54`,
2026-11-16. Full analysis and sources: [`brainstorm.md`](../brainstorm.md).

## Outcome

The hub stays safe to migrate on slow disks, idle ingestion costs little
CPU and I/O, and the storage design for original records is chosen from
measured agent usage, not guesses.

## Evidence

- Hub 16.3 GB for 9.85 GB of raw transcripts; `events` holds 14.9 GB.
- Per-row zstd: 2.4x; with a dictionary: 3.1x; 1 MB session blocks:
  4.1x; whole session: 5.8x. 32.9% of raw bytes repeat earlier large
  strings.
- A setup recovery copy of 13.1 GB took 8.5 h and made the VM
  unresponsive (`NOTES.md`, migration recovery copy cost).
- Idle ingestion still reads ~13 MB/s from the page cache: a fingerprint
  read and a `RegisterSource` call for every source on every pass.
- `github.com/klauspost/compress` (zstd) is already in the module graph.

## Do now: cheap and quick

### S1. Size-aware setup recovery point

`internal/setup/engine.go`, `recoveryPoint`, copies the whole database
before a schema change.

1. Before copying, check free space on the target filesystem. Refuse with
   a clear remedy if free space is below the database size plus a margin.
2. Report the database size and progress during the copy.
3. Document the expected cost per GB in `docs/release-operations.md`.
4. Test: insufficient space refuses without changing anything; progress
   lines appear; an interrupted copy leaves no partial recovery point
   that pruning would keep.

### S2. Skip per-pass work for unchanged sources

1. In `internal/watch` `Poll`, skip the fingerprint read when a source's
   identity, size, and modification time match the last pass.
2. In `cmd/ohya/ingest.go` `poll`, call `RegisterSource` only for new
   sources or when path, device, or agent changes.
3. Keep replacement and truncation detection exact. Prove it with the
   existing replacement, truncation, and in-place repair tests.
4. Measure idle reads with `/proc/<pid>/io` before and after; record the
   result in `docs/performance.md`.

### S3. Offline storage analyses

Answer the remaining open questions in `brainstorm.md` with read-only
analysis scripts. Keep scripts and results in `perf/` and the summary in
`NOTES.md`. Never copy transcript content into the repository.

1. Share of the FTS index by event kind and tool output.
2. Duplication and compression ratios per device (VM and MacBook data).
3. Decode cost of `klauspost/compress` zstd per 64 KB and 1 MB block on
   this VM, with and without a dictionary.

## Deferred until plan 002 data exists

Start these only after the plan 002 acceptance gate: at least 200 `sql()`
calls from both agents over at least 2 weeks.

### D1. Choose the storage design for original records

1. Add a read-only `ohya store estimate` command that measures what each
   design would save on a user's hub.
2. From plan 002 data, compute the share of calls that read `raw_json`.
3. Write an ADR choosing between:
   - **O1 + O4:** sealed, compressed, immutable session segments for cold
     `raw_json`, with a narrow hot `events` table and an `ohya_raw(id)`
     SQL function that keeps `events_settled.raw_json` compatible.
   - **O3:** per-row zstd with immutable, versioned, trained dictionaries
     stored in the hub.
4. Kill criteria (from `brainstorm.md`): under 2x saving on three real
   hubs; or agents read `raw_json` in more than 20% of calls and p95
   latency rises by more than 20%.

### D2. Implement the chosen design

1. Model sealing in TLA+: idempotent, crash-safe, visible only in the
   transaction that clears hot `raw_json`.
2. Byte-exact round-trip tests on every agent's fixtures; block checksums;
   verify after seal.
3. Ship behind a flag; make S1 a hard prerequisite.

### D3. Later options

Evaluate only after D2:

- **O2:** content-addressed large strings (33% duplicate bytes measured).
- **O5:** compressed sync batches with a shared dictionary. Partially
  delivered 2026-11-19 (T145) by operator decision ahead of D2: batch
  upload frames are zstd-compressed with per-connection negotiation and
  plain-frame fallback
  ([ADR 0014](../docs/adr/0014-zstd-compressed-batch-upload.md)). The
  shared dictionary remains open, to be evaluated by T146 only on a
  material measured gain.
- **O6:** index fewer low-value bytes in FTS; needs the plan 003 R5
  recall evaluation first.

## Rejected

See the negative list in `brainstorm.md`: LLM-based compression, lossy
summaries instead of originals, dropping Codex `event_msg` or
`encrypted_content`, external columnar formats as the store, and `VACUUM`.
