# Runtime observability for ohya

Status: in delivery. Milestones 1 through 4 are implemented (T090–T093,
T096, delivered 2026-11-16 to 2026-11-17): recorder and schemas, path
instrumentation, snapshots with `ohya status` and `ohya metrics`, and
logs with `ohya logs` and `ohya diagnose`. Milestone 5, remote aggregates
(T098), and milestone 6, the overhead baseline (T097), are open.
Priority P1; effort L, delivered in small increments. Planned against
`33df75d`, 2026-11-15.

## Outcome

An agent with shell access can discover the installed version and topology, determine whether memory is current, explain resource use and slow operations, inspect recent failures, and collect reproducible before/after evidence without reading transcripts, guessing state paths, restarting services, or changing SQLite policy.

The primary workflow is:

```sh
ohya status --json
ohya status --remote --json
ohya metrics --window 5m --json
ohya logs --since 10m --limit 100 --json
ohya diagnose --output ./ohya-diagnostics.json
```

The local commands exist since milestones 3 and 4, with their help, documentation, and tests. Remote status is milestone 5 and is not delivered yet; `ohya status --remote` reports that capability gap. Older installations must report unsupported capabilities honestly.

## Constraints and current evidence

Read MANIFESTO.md, AGENTS.md, CONTRIBUTING.md, README.md, DB.md, docs/performance.md, docs/sync-protocol.md, and applicable nested instructions before implementation. Keep the single Go binary, hub-owned SQLite WAL database, deterministic ingestion, bounded read-only SQL, and encrypted authenticated transport. No external telemetry backend, mandatory collector, new database, or new dependency is required.

Current integration points:

| Path / symbol | Responsibility and change boundary |
| --- | --- |
| `cmd/ohya/jsonout.go`, `jsonEnvelope` | Existing versioned CLI JSON envelope; reuse it |
| `cmd/ohya/cli.go`, `main.go` | Command registration, flag and help conventions |
| `cmd/ohya/doctor.go` | Read-only health checks; health remains separate from performance |
| `cmd/ohya/ingest.go`, `ingestor.poll`, `ingestSource` | Local discovery, parsing, commits, checkpointing |
| `internal/watch/watch.go`, `Poll`, `Acknowledge` | Source observation and durable local cursor ownership |
| `internal/uploader/uploader.go`, `Run`, `Sync`, `syncSource`, `connect` | Upload passes, reconnection and acknowledgment handling |
| `internal/hubsync/endpoint.go`, `handleBatch`, `handleQuery`, `handleAnnotate` | Authenticated server boundaries and source gates |
| `internal/hubsync/client.go`, `protocol.go` | Bounded wire framing; currently protocol version 1 |
| `internal/store/ingest.go`, `DB.Ingest`, `RegisterSource` | Transactional durable event and Source cursor updates |
| `internal/mcpserver/query.go`, `Engine.Query` | Query validation, admission, reader acquisition, iteration and bounds |
| `internal/mcpserver/annotate.go` | Annotation validation and transactional outcome |
| `internal/setup/service.go`, `ServicePlan` | Owned launchd/systemd service lifecycle |
| `internal/transport/address/address.go` | Transport-specific facts; expose only verified supported observations |

Existing uploader stats contain `Sources`, `Inserted`, and `Acknowledged`; preserve their actual semantics rather than renaming them into speculative metrics. `Run` discards each successful pass's stats. The query engine has a five-second deadline, four admission slots and explicit row/byte truncation. The settled-events view excludes sessions active within ten minutes.

Observed motivation, not performance acceptance baselines:

- Targeted remote recall took tens to hundreds of milliseconds in an informal trial; `count(*)` over settled events exceeded the query deadline.
- `doctor --json` reports health and readable checkpoints, not backlog or throughput.
- The installed macOS launchd template does not configure persistent stdout/stderr files. Linux uses the journal with rate limiting.
- A local process snapshot showed about 301 MiB RSS and 11.7% CPU; a snapshot cannot establish idle cost or a leak.
- A local database file was about 6.2 GiB. This was not a measurement of the remote hub.
- Existing performance documentation attributes normal freshness mostly to the two-second polling interval. It also records durability-bound bulk import. Instrumentation must test those explanations on real workloads rather than assume them forever.

## Scope and non-goals

Implement local snapshots, metrics, bounded structured logs, safe diagnostic export, remote aggregate inspection, and documented agent workflows. Add on-demand storage analysis and explicit controlled profiling only after the basic measurements exist.

Do not tune polling, batching, durability, SQLite indexes, compression, or search ranking in this work. Do not add embeddings, automatic interpretation of transcript content, a dashboard, Prometheus/OpenTelemetry infrastructure, or another MCP tool. Agents initially inspect operational state through the CLI; `sql` and `annotate` retain their existing purpose and response contracts.

No transcript-schema migration is required for the baseline. If implementation appears to require one, explain the need in an ADR before proceeding. Preserve unrelated working-tree changes, including existing NOTES.md additions.

## Architecture

### Collection

Add `internal/observe` as a small concrete recorder and snapshot model, injected through existing option structs. Domain operations emit typed outcomes and durations; the recorder owns aggregation and serialization. Do not create a generic tracing framework, global registry, or observer interface per operation.

Use standard-library atomics or short locks for counters. Update fixed-size histograms and current-operation gauges without disk I/O on ingest/query paths. The background publisher copies bounded state, releases locks, then performs file writes. Instrumentation failure must never change a Batch, Acknowledgment, committed Cursor, parser outcome, authorization decision, or query result.

Metrics are process-local observations, not a second authoritative state machine. Reset them on process start. Durable domain facts continue to come from the database and checkpoints. Never restore performance counters from disk as authoritative facts.

### Publication and discovery

Each long-running process publishes under `<resolved-state>/observe/<random-instance-id>/`. Roles are `serve`, `join`, `ingest`, and `mcp`; a process can contain multiple agent components. Include PID, random instance ID, role, process start time, build revision, monotonic uptime, and resolved scope identity. Never key identity by PID alone.

Each instance owns `snapshot.json`, `history.jsonl` and rotated `events.jsonl` files. Directories are 0700 and files 0600. Verify ownership, reject symlink traversal and unsafe existing paths, and use bounded readers. Do not log credentials or publish command-line arguments verbatim.

Publish an atomic replaceable snapshot every ten seconds, plus startup and clean shutdown. No fsync is required for this disposable data. On startup clean only owned stale instances, retaining at most eight terminated instances per role and seven days of files; never delete a live instance to satisfy retention. If more than 64 live instances exist, report bounded-discovery truncation explicitly and select the requested instance where possible.

Snapshot maximum is 256 KiB; preserve aggregate counters and include truncation metadata if details exceed it. Validate size before rename. Temporary files are private, same-directory, and cleaned after failures. Do not use a durability helper that adds fsync work merely to preserve expendable telemetry.

`status` resolves state the same way setup/doctor resolve local versus joined configuration. Inspect owned service receipts and the saved connection configuration; never guess that an existing local hub.db is the active hub. Support `--state`, `--instance`, and `--role` to resolve ambiguity. Multiple matching processes are listed explicitly; never silently merge their cumulative counters.

A snapshot older than thirty seconds is stale. A valid recent snapshot is evidence of recent activity, not proof of current process liveness. Report service manager state and process liveness separately; PID reuse or inaccessible process metadata yields unknown. Readers never start services, migrate databases, checkpoint WAL, repair files, or change ownership.

### History

Persist one bounded aggregate sample per minute. Retain 24 hours and at most 16 MiB per instance, with a single writer. Prune on the publisher, not on request paths. Samples contain timestamps, monotonic durations, counters, gauges and histogram bucket counts. They contain no Source details, SQL, paths, or free-form errors. Counter deltas spanning restarts or gaps are unavailable rather than joined together.

MCP processes may be short lived: emit one final summary on clean exit and use the same ten-second cadence when long lived. Local status makes unobserved/crashed short operations explicit; hub measurements cover remote operations independently. Never label sampled MCP totals as all-time totals.

## Public CLI contract

All non-streaming `--json` output uses the existing `schema_version`, `command`, `data`, `error` envelope. Add `data.format_version: 1`, explicit units, sample timestamps, completeness, provenance and capabilities. Additive fields are compatible; changing a field's meaning or type requires a format version change. Unknown future fields must be ignored by readers; unsupported versions fail clearly.

### `ohya status [--remote] [--state DIR] [--role ROLE] [--instance ID] [--json]`

Default reads local cached snapshots and current service state; it does not contact the hub. Output contains:

- Installed CLI identity and each observed runtime's identity separately: version, revision, dirty flag, Go version, OS/architecture, schema/parser versions when known.
- Topology: local or joined mode, state scope, configured agent roots as count/agent only by default, service loaded/running facts, instance IDs and configured runtime limits.
- Runtime: uptime, sample age, last completed discovery/pass, current operation with elapsed duration, last successful durable progress, last connection/authentication success, last failure code and timestamp.
- Source summary: discovered, caught up, pending, blocked, stopped, missing and unknown counts; last complete scan time and completeness. These describe sampled observations.
- Observed pending bytes and pending complete-record bytes, acknowledgment counters, and rate windows where available.
- Resource gauges and DB/WAL/SHM byte sizes with location and observation time. A joined client does not report its old local DB as the hub DB.
- Findings with stable code, severity, evidence, and actionable hint. Healthy findings do not carry repair instructions.
- `coverage`: present components, missing components, stale samples, unsupported probes and remote visibility limitations.

Exit codes: 0 for a valid complete snapshot with no degraded component, 3 for a valid but partial/stale/degraded result, 1 for a command failure, 2 for usage errors. This new code-3 meaning applies only to these new observation commands; update global help without changing existing command exit semantics. JSON remains parseable for degraded results with data present and no top-level command error.

### `ohya metrics --window 1m|5m|1h|24h [--remote] [--json]`

Read bounded samples, not transcripts. Return observed duration, requested duration, sample count, gaps, counters/deltas, rates, histogram buckets, quantiles, and completeness. Do not manufacture a requested five-minute window from ten seconds of data. Rate denominator is actual monotonic elapsed time; identify whether it is wall throughput or active-operation throughput. Quantiles require at least twenty observations; otherwise return null and counts. Never average percentiles between instances.

### `ohya logs [--since DURATION] [--limit N] [--level LEVEL] [--component NAME] [--follow] [--json]`

Default limit 100, maximum 1,000 per finite request; default lookback ten minutes. Finite JSON returns the normal envelope with entries, coverage and truncation. `--follow --json` emits one independently versioned envelope per line. Bound a line to 8 KiB; maximum finite payload is 1 MiB, with explicit truncation. Follow handles rotation without re-emitting the full file and terminates on cancellation. Scope filters use the same state/role/instance selection as status. Baseline logs are local only; do not imply `--remote` support.

### `ohya diagnose --output FILE [--remote]`

Create a private, single JSON artifact containing status, selected 1m/5m/1h metrics, at most 100 recent sanitized warnings/errors, build/configuration limits and capability information. Cap artifact size at 2 MiB with explicit omitted sections. Refuse overwrite unless `--force`; reject symlink destinations. Never include transcripts, raw SQL, annotation values, invitations, credentials, environment dumps, private keys, full source paths or raw third-party errors. Export is explicit and local; never upload automatically. Missing hub access produces a partial artifact identifying the failure rather than aborting the useful local collection.

## Measurement dictionary

All counters are unsigned, cumulative since the identified process start. Timing uses monotonic clocks. Timestamps use UTC milliseconds for display/correlation only. Bounded labels: role, agent (`claude-code`, `codex`, `pi`, `unknown`), operation, result code, and query class. No Session, Event, Source, repository, device, path, SQL fingerprint or request ID in metric labels.

| Family | Required measurements and boundaries |
| --- | --- |
| Discovery | scans, candidate files, stat/read errors, scan duration, last completed scan; don't call candidate count valid-source count |
| Watch | observed file bytes, committed/acknowledged cursor, pending complete bytes, partial-tail bytes, replacement/truncation/missing observations, active/stopped/blocked counts |
| Parse | input bytes, records attempted/normalized/stopped, durations by agent, oversize/invalid record codes; bounded error offset in diagnostic details only |
| Local ingest | attempted batches, committed new events, recognized duplicate events, accepted byte spans, transactions committed/rolled back, parse and checkpoint durations |
| Upload | attempted/successful batches, attempted payload bytes, acknowledged byte advancement, locally persisted checkpoint advancement, inserted events reported by hub, round-trip duration, reconnect/auth failures, retry/backoff duration |
| Hub | active/admitted/rejected connections, authorization outcomes, Source-gate wait, normalization time, store transaction time, acknowledgment write time and failures |
| SQLite | pool DBStats, connection wait deltas, transaction total/commit duration, busy/locked outcomes, file sizes, page size/page count/freelist when cheaply available |
| SQL | requests and outcomes, active/admission limit, busy rejections, reader acquisition, execute-to-first-row, iteration/materialization, total engine time, returned rows and encoded bytes, row/byte truncation and timeouts |
| Annotation | attempted batches/operations, applied/unchanged, validation failures, transaction duration; no values or namespaces in telemetry |
| Runtime | Go heap allocation/in-use/reserved, goroutines, GC count/pause histogram, process CPU seconds, RSS where available, sampling duration/errors |
| Instrumentation | dropped log entries, coalesced entries, snapshot/history write errors, queue depth, last successful publication and publisher duration |

Duration histogram upper bounds in milliseconds: 0.1, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, +Inf. Publish count, sum and noncumulative bucket counts with documented bounds. Percentiles are bucket estimates, explicitly labeled as such. Use a fixed metric catalogue and fixed labels to bound memory.

Do not parse arbitrary SQL with regex to infer reliable semantics. Query classes are `sql` and `annotate` initially. A caller's locally selected probe name may distinguish synthetic benchmark cases, but normal SQL text or literal values must never enter logs. Do not alter query results to attach telemetry.

CPU utilization is derived from process CPU seconds divided by monotonic sample duration, expressed as percent of one core (can exceed 100%). RSS and Go heap are distinct. Prefer standard OS APIs/build-tagged implementations and report unsupported metrics as null with reason. Reading metrics must not force GC, inspect all stacks, or obtain expensive heap profiles.

### Cursor and freshness semantics

- Increment committed-event counters only after transaction commit succeeds. A duplicate replay does not increment newly inserted events.
- Acknowledged bytes means forward movement of the authoritative acknowledgment cursor within one Source incarnation; attempted/resubmitted payload bytes are separate.
- Track checkpoint persistence after acknowledgment separately. A failed local checkpoint write must not be represented as complete local progress.
- Compute pending bytes from file size and its matching Source cursor observed during the existing scan. Replacement/truncation invalidates the old subtraction; never produce negative backlog or silently clamp an identity mismatch into caught-up state.
- An incomplete JSONL tail is pending input but not a complete record ready to ingest. Missing/stopped/blocked sources remain visible and prevent an unconditional caught-up claim.
- State `caught_up_as_of_scan` only after a complete successful scan/pass with no known complete-record backlog or unresolved sources. It is not proof that all agent files everywhere are discovered. Show configured discovery roots' coverage and scan time; explicitly surface excluded/unsupported roots in docs.
- Local observation-to-durable-ack duration starts when a complete record is first observed and ends at acknowledgment receipt, using one process clock. Keep only bounded tracking for pending batches; old/untracked samples count as unavailable. This excludes pre-discovery delay and is not end-to-end transcript freshness.
- Timestamp age of an Event is not ingestion lag. Device/hub wall-clock differences are not a latency measurement. Measure true append-to-query-visible latency only in controlled synthetic probes with a single coordinator clock, not from arbitrary real transcripts.
- The ten-minute settled-view delay is a recall policy, reported separately from ingestion/query visibility. A caught-up device can still have records absent from `events_settled`.
- Stage timing spans can nest and overlap. Identify inclusive versus exclusive durations; never sum overlapping totals into a claimed critical path. Client round trip includes hub work and transport; no one-way network latency is inferred.

## Structured logs

Use `log/slog` or equivalent small standard-library implementation with a typed allowlist schema: format version, UTC timestamp, instance ID, role/component, level, stable event code, outcome code, duration, numeric counts and optional bounded correlation ID. Error messages are curated by event code; raw `err.Error()` is not safe by default because parsers, SQL and transport errors can embed input.

Log startup/shutdown, configuration capability summaries, online/offline/auth/revocation transitions, stopped sources, recoveries, checkpoint/commit failures, query timeout/busy/truncation summaries and instrumentation failures. Emit a summary once per minute when active and a heartbeat at most once per five minutes when idle. Do not log every successful event or query. Coalesce repeated errors by stable code and report counts; no unbounded map of arbitrary errors.

Each process writes a bounded asynchronous queue (capacity 256) to its own 8 MiB file plus two rotated files, maximum 24 MiB. Producers never block on log I/O; dropped entries are counted. Flush on clean shutdown for at most one second. Disk-full/read-only failures fall back to bounded sanitized stderr notices no more than once per minute. Process status reports degraded telemetry independently from healthy ingestion.

Application logs are the portable canonical source for `ohya logs`. Preserve systemd journal stderr output for startup failures. On macOS setup configures a private stderr startup file and safe bounded rotation using the owned installer/service lifecycle; do not introduce an unbounded stdout file. Application log writing failures remain visible on stderr. MCP stdout is exclusively protocol traffic. Redirect or sanitize third-party logging before enabling it; third-party debug modes are not included in default diagnostics.

Source-specific troubleshooting is a separate local `status --sources --limit N` view (default 20, maximum 100) selecting worst known blocked/pending Sources from an in-memory bounded top-N sample. Output Source IDs, safe reason codes, offsets and byte counts, never transcript excerpts or full paths. Return total counts, detail coverage and truncation. Do not retain all Source details indefinitely solely for telemetry.

## Remote inspection and authorization

Remote hub aggregates are required for a useful joined-device workflow, but deliver them after local instrumentation. Add a bounded read-only diagnostic request/response to the authenticated hubsync connection, not a public HTTP endpoint or SQL PRAGMA bypass. It returns cached aggregate status/metric windows only, never logs, Source details, other-device identifiers, configuration paths or profiling data.

Existing paired-device authorization permits aggregate diagnostics wherever it permits shared read-only querying. Recheck revocation for each request using the existing session authorization boundary. Limit to one request per connection at a time, two simultaneous diagnostic requests globally, five-second timeout, and 256 KiB response. Reject malformed/oversized windows before allocation. Rate-limit each authenticated device to one request per ten seconds; return cached timestamps, not a newly scanned database.

Design an explicit compatibility boundary: bump the wire protocol version with new diagnostic frame types; reuse the existing version rejection to give a clear incompatible/unsupported result. Do not probe an unknown frame on a live uploader stream, silently downgrade, reconnect repeatedly, or break an existing upload. Open a separate short-lived diagnostic connection using saved pairing state. Support testing old/new peer combinations; older binaries need upgrade guidance. Broader rolling-version negotiation is outside this spec.

`status --remote` includes both local connection evidence and a separately timestamped hub section, including hub version/schema/parser/limits and service role. A failed fetch never substitutes local database facts for the hub. Remote runtime duration is hub monotonic time; do not compute cross-host latency by subtracting timestamps. Transport direct/relay/path RTT is included only if an existing supported Tailcat API exposes it; otherwise publish unavailable. Do not shell out to a separately installed Tailscale CLI to invent embedded-node observations.

Because the diagnostic protocol extends authenticated behavior, update the relevant authorization TLA+ specification and run TLC. Prove diagnostic requests cannot mutate committed data/cursors or bypass revocation. Instrumentation alone must not alter sync transitions; retain existing sync model regression checks.

## Storage analysis and controlled experiments

Default status obtains file sizes through stat and cheap read-only facts only. No `COUNT(*)` over events, payload scans, `VACUUM`, WAL checkpoint, integrity scan, recovery copy, or migration is allowed implicitly.

Add explicit `ohya diagnose --storage --output FILE` after baseline delivery. It may inspect SQLite page/freelist metadata and `dbstat` if compiled/available. Report table/index allocated bytes separately from logical payload bytes and filesystem allocated versus apparent bytes. Unsupported dbstat is a capability result. Default storage analysis deadline thirty seconds with cancellation; report partial results. Full raw/text payload scanning requires an additional explicit `--scan-payloads` and configurable deadline, with progress on stderr, because it can read the entire DB. Never claim compression savings from file size alone.

Profiling is a later opt-in milestone: `ohya serve|join|ingest --cpu-profile FILE --profile-duration 30s` captures the actual process, not a new empty client. Default duration 30 seconds, maximum 120. Add explicit heap capture on shutdown only if requested. Private files, refuse overwrite, no public pprof listener, no automatic profile inclusion in diagnose. Profiles can expose sensitive stack/function/path information; label them sensitive. Do not enable block/mutex profiling continuously. A future attach-to-running-process control endpoint requires a separate security/design proposal; baseline observation works without one.

Controlled probes use synthetic fixtures in a private throwaway state directory and never append marker transcripts to a user's real history automatically. Record seed, fixture hash, source/record counts, build, OS/architecture, CPU/storage context, configuration limits, path direct/relay/unknown, sample count, warmup and concurrency. Preserve baseline and candidate artifacts for the same workloads.

## Cost and retention budgets

These are proposed acceptance budgets, not claims of measured cost:

- Normal instrumentation relative to identical recorder-disabled runs: median wall-time/throughput overhead <=3%, allocation increase <=5%, steady added RSS <=16 MiB per daemon on existing workloads. At least five paired repetitions; publish all samples. Noise crossing a threshold requires investigation and a documented decision, not silently raising a target.
- No disk I/O per successful Event or query from instrumentation; one snapshot per ten seconds, one history sample per minute and bounded periodic logs. No telemetry fsync on normal paths.
- `status --json` local cached-data read: p95 <=100 ms once process startup is excluded, <=256 KiB per instance, and bounded selection across at most 64 instances. Report CLI end-to-end latency separately.
- A diagnostics failure cannot block a domain operation; only bounded in-memory recorder critical sections are allowed.
- Retained telemetry ceiling per instance: 24 MiB logs +16 MiB history +256 KiB snapshot, plus one bounded temporary replacement. Global stale-instance retention follows the rules above; live-instance total is reported, not hidden by destructive pruning.

Add `--observe off` to long-running commands for controlled baseline measurements; default is on. Disabled instrumentation must still allow ordinary fatal stderr errors, and status reports disabled rather than a healthy empty collector. Never silently disable collection after an error.

## Implementation sequence and verification

Implement on a focused branch/worktree under `~/dev/.worktrees/ohya/runtime-observability`. Do not push without instruction. Make tested coherent commits. No explanatory code comments; contracts belong in docs. Match existing error handling and typed option patterns. Before changing a listed path, compare with `33df75d`; reconcile drift and preserve unrelated edits.

### 1. Freeze schemas and implement the recorder

Scope: new `internal/observe`, schema fixtures under its tests. Define metric catalogue, snapshot/log versions, safe event codes, fake monotonic/wall clocks, histogram aggregation, reset/window semantics and resource capability representation. Test bounded cardinality, concurrency, exact counter outcomes and no negative deltas across resets.

Verify: `go test -tags sqlite_fts5 -race ./internal/observe` exits 0; JSON golden fixtures decode and unknown fields are tolerated. Include zero/low-sample quantiles, missing samples and backward wall-clock jumps.

### 2. Instrument ingestion, storage, queries and sync

Scope: option injection and narrowly placed measurements in the paths listed above, plus new tests alongside existing uploader/store/query tests. Record both successful and failed outcomes, including failures before a defer is installed. Ensure no observer call changes acknowledgment/checkpoint ordering. Maintain existing public query/annotation result schemas.

Verify: `go test -tags sqlite_fts5 ./internal/watch ./internal/uploader ./internal/store ./internal/mcpserver ./internal/hubsync` exits 0. Extend `TestLostAcknowledgment`, timeout/restart and parse-stop tests to assert attempted bytes can increase while newly committed events do not duplicate. Add commit-failure and checkpoint-failure measurements. Race-test the recorder and concurrent query paths.

### 3. Publish local state and expose status/metrics

Scope: `internal/observe`, new CLI command files, command registration, saved-state resolution reuse, CLI JSON tests and help. Implement file safety, snapshot publisher, bounded history and discovery/instance selection. Do not create new persistent domain state or scan transcript databases.

Verify: `go test -tags sqlite_fts5 ./internal/observe ./cmd/ohya` exits 0. Add child-process tests for healthy, disabled, stale, killed, PID-reused/unknown, duplicate-role, corrupt and unsupported-version snapshots. Test cancellation, directory symlinks, disk-full writers through injected faults, and ambiguous local/remote topology. Golden output proves null/unavailable differs from zero.

### 4. Deliver logs and safe diagnostic exports

Scope: recorder logger/exporter, CLI logs/diagnose, `internal/setup/service.go` and service lifecycle tests. Own log paths through setup, preserving edited integrations. Add startup stderr policy on macOS; do not change service restart policy. Include uninstall/upgrade preservation behavior for telemetry: uninstall preserves existing diagnostic files, future setup can safely prune stale telemetry under retention.

Verify: package and CLI tests above plus `go test -tags sqlite_fts5 ./internal/setup`. Assert rotation under concurrent readers, truncation, follow cancellation, output permissions, no stdout pollution, and partial export behavior. Seed secrets into transcript text, SQL literals, annotation values, source paths, errors and pairing state; assert none occur in exported logs or diagnostics. Native launchd and systemd smoke runs must prove startup errors and application events are discoverable without a restart loop. Compilation alone is insufficient.

### 5. Add authenticated remote aggregates

Scope: hubsync framing/client/endpoint, `serve` recorder wiring, joined CLI resolution, authorization model and protocol documentation. Explicit incompatible-version behavior is mandatory. No remote logs, raw paths, profile control or new public listener.

Verify: `go test -tags sqlite_fts5 ./internal/hubsync ./internal/uploader ./cmd/ohya` and `make tla` exit 0. Test authorized, revoked-before-request, revoked-during-request, malformed frames, response bounds, rate/concurrency limits, timeout, mixed versions and upload continuity. Remote diagnostics must succeed with no remote shell access and cannot advance any Source cursor.

### 6. Establish an agent evaluation and overhead baseline

Scope: synthetic workload harness, benchmark artifacts in existing ignored performance locations, docs/performance.md, docs/reference.md, README.md, architecture/ADR and agent operating instructions. Add optional storage analysis and bounded launch-time profiling only after basic overhead passes; keep them independently reviewable.

Verify: `make bench` plus at least five paired existing-workload runs with observation on/off; record medians, tails, allocations, RSS, log/disk write volume and timing methodology. Run synthetic live append, bulk import, selective/wide query, offline replay, hub restart, contention, parse stop and oversized-record scenarios. Repeat direct/relay paths when available; unsupported relay coverage stays an explicit gap.

Final gate: gofmt changed Go files; `make check` and `make tla` exit 0. Run native macOS/Linux smoke tests. Update command help and docs in the same increments. Do not label later milestones complete because the local CLI works.

## Agent acceptance scenarios

An agent starting with only `ohya --help` must complete the following using documented commands and report evidence/sample times:

1. Identify this client's mode and the actual hub; distinguish an inactive local DB from remote storage.
2. Explain whether complete observed records are caught up, blocked, or unknown and show the age of that conclusion.
3. Distinguish an idle process from a stalled current operation and distinguish recent connection success from durable event progress.
4. Explain a five-second query timeout with admission/reader/execution/result-stage evidence, without exporting query text.
5. Compare attempted upload throughput, durable inserted-event throughput and acknowledged byte progress during retry/offline replay.
6. Find a stopped Source reason and distinguish partial JSONL tails from malformed complete records.
7. Compare RSS, heap, CPU and WAL growth over a real window and identify resets/missing samples.
8. Recover recent startup/application errors on macOS and Linux, with no transcript content or credential disclosure.
9. Export a bounded useful bundle even when the hub is offline or instrumentation storage is unwritable.
10. Collect baseline/candidate measurements with the same fixture and limits and state what remains unmeasured.

The integration tests use synthetic expected outcomes to check these answers; do not require an LLM to judge correctness or run ingestion.

## Risks, stop conditions and deferred decisions

Stop and document a revision if accurate measurement requires moving a commit/acknowledgment boundary, disabling durability, changing authorization policy, adding an unbounded in-memory map, or copying the whole production database for a default command. Do not claim exact pending bytes across incomplete scans or cross-host end-to-end latency from unrelated clocks.

If OS resource access is unavailable, publish a capability gap and implement a supported platform probe later; do not substitute Go heap for RSS. If Tailcat cannot provide path/RTT safely, leave it unknown. If protocol version migration would disrupt a deployed mixed fleet, stage the upgrade explicitly before remote aggregate delivery; local observability still ships independently.

No remote attach profiler, metrics HTTP server, transcript-derived query classification, public diagnostic upload, arbitrary SQL logging, or telemetry-in-SQL virtual tables is authorized by this spec. Revisit only after the baseline demonstrates a concrete need.

## Definition of done

- All six milestones have separate evidence and documented capabilities; unfinished optional profiling/storage work is clearly identified.
- A joined agent can inspect its local uploader and remote hub without SSH or ad hoc SQL scans.
- Metrics describe explicit boundaries, reset semantics, sample coverage and unknown values.
- Logs and diagnostic artifacts are private, bounded, sanitized, discoverable and fail independently of ingestion.
- Existing immutable transcript, idempotence, durable acknowledgment and revocation invariants remain intact.
- Existing checks, native service verification, formal checks and measured overhead budgets pass, with artifacts linked from docs/performance.md.
