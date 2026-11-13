# ohya — Notes

<!--
Working notes for ohya — unresolved knowledge useful to future work.
Read at the start of each task; update when useful; prune what is resolved.

How to use this file:
- Read it before starting any task; re-read findings that touch your work.
- Add a dated bullet for information worth carrying into another session.
  One topic per bullet; keep it short.
- Distinguish observed facts from hypotheses. Mark uncertain claims as unverified
  and include a relevant file reference or verification step when useful.
- Remove or rewrite bullets once resolved or disproved — this is a live file,
  not an archive.
- Keep durable decisions in the relevant documentation and actionable tasks in
  the backlog; use this file for unresolved working knowledge.
- Never put secrets, transcripts, or large dumps here; link to sources instead.
-->


- Verify agent session storage findings before phase 1 parser spikes (researched 2026-11-13, partly from third-party writeups — recheck against installed versions):
  - Claude Code: verified 2026-11-13 against installed 2.1.280 (895 main sessions, 2,646 JSONL files). Format, record kinds, identity fields, compound records and usage semantics are documented in `testdata/claude-code/README.md`, with synthetic fixtures. Points that shape ingestion: `sessionId` is the main file name and never spans files; subagent files under `<session-uuid>/subagents/` reuse the parent `sessionId` with `isSidechain: true` plus `agentId`; one API response is several records sharing `message.id` and repeating the same per-request `usage` (not cumulative — dedupe by `message.id` before summing); `cwd`/`gitBranch` live on message records; the project directory name encodes `cwd` lossily (`/ . _` → `-`), so never decode it; timestamps are ISO-8601 UTC ms but not strictly monotonic; malformed complete lines do occur in real files, with valid lines after them.
  - Codex: verified 2026-11-13 against installed codex-cli 0.166.0 (2,488 files, 3.0 GB under `~/.codex/sessions/`, written by versions 0.104–0.167; envelope unchanged). Format, record kinds, identity fields and fixtures are documented in `testdata/codex/README.md`. Points that shape ingestion: the native session id is `session_meta.payload.id` (equals the first file-name UUID) and **one thread spans several rollout files** — resumed/paginated pages are named `rollout-…-<thread-id>_<resume-id>.jsonl` (second UUID never appears in content), carry `history_mode: "paginated"` plus `history_base {thread_id, end_ordinal_exclusive, end_byte_offset}`, and replay tail records (corpus pair: original ordinals 0–984, page 978–1019), so dedupe must key on `(thread id, ordinal)` across files, not byte offsets; `ordinal` exists only from 0.147.0. `event_msg` mirrors `response_item` text (`user_message`/`agent_message`) — parse `response_item` as the transcript or dedupe. Usage: each `token_usage_record.usage` is per `response_id` (not cumulative); `turn_token_usage`/`thread_token_usage` are running totals — sum `usage` only. `session_meta.git` (`branch`, `commit_hash`, `repository_url`) is a direct input for the sessions.repo derivation (T029). Subagent rollouts open with their own meta (`source.subagent.thread_spawn.parent_thread_id`) and then embed the parent's `session_meta` as line 2. `CODEX_HOME` redirects everything (verified: 0.166.0 created `<dir>/tmp` and read `auth.json` there); no compressed session files exist (all plain JSONL). The old `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog`) layout is gone in 0.166.0: schema-numbered DBs sit directly under the Codex home, and the earlier "metadata only" claim fails for `thread_history_1.sqlite`, whose `thread_items.item_json` holds full message/command bodies (projection cache over 511 of 2,488 threads, with byte offsets back into the rollouts) — ingest rollout files only. `session_index.jsonl` is thread-name metadata; `history.jsonl` duplicates user prompts. Wild files occasionally lack the trailing newline or contain malformed complete lines (both covered by fixtures).
  - Pi agent: verified 2026-11-13 (T012) against installed 0.84.2 (already present globally as `@earendil-works/pi-coding-agent`) and latest 1.5.2 from a scratch npm prefix. The writeup claim "project-local `.pi/`" is wrong for sessions: sessions are global, one append-only JSONL per session, at `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<session-uuid>.jsonl` (agent dir overridable by `PI_CODING_AGENT_DIR`; session dir by `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` > `sessionDir`; `--no-session` is ephemeral). `<encoded-cwd>` = cwd minus one leading `/`, every `/` `\` `:` replaced by `-`, wrapped in `--`…`--` (source: `dist/core/session-manager.js`, identical in both versions); lossy (`/a/b` and `/a-b` collide), so never decode it — the `session` header's `cwd` is authoritative. File name = session-start `toISOString()` with `:`/`.` → `-`, then `_` + session UUID (time-ordered, `01a1…`). Session format v3: line 0 is a `session` header (`version`, `id`, `timestamp`, `cwd`); every other entry has `type`, `id` (8-hex), `parentId` (tree, `null` = root), ISO `timestamp`. Kinds seen: `model_change`, `thinking_level_change`, `message` (roles `system`/`user`/`assistant`/`toolResult`). Assistant messages carry `provider`, `model`, `api`, `stopReason`, `responseId`, `rawStopReason`, and one usage+cost object per response (`input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`) — per-response, not repeated across records, so token totals sum straight over assistant messages (no Claude-Code-style dedupe); 1.5.2 adds `durationMs` and `thinkingLevel` and persists a `system` entry (`sections`, `toolsAdded`, empty `content`). `toolResult` carries `toolCallId`, `toolName`, `content[]`, `isError`. Entries flush incrementally: a first model call that fails still persists header + `model_change` + `thinking_level_change` + user + assistant with `stopReason: "error"`, `errorMessage`, zero usage. Headless runs in an untrusted cwd hang on a trust prompt — pass `-a`/`--approve`; runs from the pre-trusted `/home/agent` did not need it. The agent dir also holds `auth.json` (credentials), `settings.json`, `models.json` (provider catalog, keys referenced by env-var name), `trust.json`, `bin/` (`rg`, `fd`), and `~/.pi/skills/`. Real sample sessions and the captured layout (0.84.2 and 1.5.2) are preserved outside the repo at `/home/agent/.looper/ohya-3d80aa08/runtime/task-b55552405a41f18c/work-00-session-15667963bc0501b8/pi-sample-session/` for the T013 fixtures; `.pi/` may still exist inside projects for config/packages, but no session writes there were observed.
  - OpenCode (phase 2): `~/.local/share/opencode/storage/`, one JSON file per message/part — enumerated storage, not append-only; breaks the byte-cursor model.
  - Claude Desktop regular chats: server-side, not file-watchable; potentially pollable via unofficial authenticated API (see Claude Explorer / rpeck/claude-explorer) or official export. Would need a new poll-based source type — deferred.
  - ChatGPT desktop: no reliable local transcripts; official export only — deferred.
  - Cowork local sessions may write JSONL under `~/Library/Application Support/Claude/local-agent-mode-sessions/` — absent on current install; check if usage starts.

- macOS distribution still needs native builds on both supported architectures and
  signing/notarization verification; the SQLite driver requires a C toolchain.

- Claude Code parser normalization choices (2026-11-13, internal/claudecode; relevant to T006
  aggregates): searchable text per record concatenates block text in source order, and a
  tool_use block contributes `name + compact sorted-key JSON of input` (so commands and file
  paths are searchable); bookkeeping records contribute no text. Role comes from message.role,
  falling back to the record type for user/assistant; tool_name is set only for exactly one
  tool block. Only the `timestamp` field sets time_ms (cost-state `startTime` is ignored).
  Records missing sessionId get native ID `ohya-synthetic:<source-scope>` — subagent
  `started`/`result` records carry no sessionId, so they land in that synthetic session by
  design. The record size limit counts JSON bytes excluding the line terminator and also stops
  an incomplete tail already above the limit. A valid-JSON non-object line (array, null) is
  malformed and stops the source, like invalid JSON.


- FTS5 verification gotcha (verified 2026-11-13, internal/store/open_test.go):
  `SELECT count(*)` on an external-content FTS5 table reads the content table, not
  the index. Assert index consistency with the `integrity-check` command and MATCH
  enumeration. Relevant wherever the write path maintains `events_fts` (T006+).

- Source watcher choices (2026-11-13, internal/watch, T005): change detection
  applies three signals in order — platform identity (dev/ino on Unix), size
  against the acknowledged cursor, then the checkpoint fingerprint (SHA-256 over
  a 4 KiB window ending at the cursor). Identity catches replacement, size
  catches truncation, and the fingerprint catches same-inode rewrites, including
  truncate-and-regrow between polls. Accepted edges: a cursor-0 source can bind
  to a reused inode after its file is deleted (harmless — nothing was ingested);
  a device-number change across reboots (tmpfs) re-imports under new source IDs,
  which is retry-safe but not semantic dedup. State keys resolve symlinks in
  parent directories (macOS /tmp → /private/tmp) so one file keeps one key
  whether or not it currently exists; tests must compare against normalized
  paths. Lost state is reported, never silently restarted: an unreadable
  device.json or sources/<agent>.json is a recovery error.
- Hardlinks and the watcher (2026-11-13, internal/watch, T028): two live paths
  can share one platform identity (hardlinking backup tool inside the watched
  tree; reachable through `ohya ingest`, which passes every discovered *.jsonl
  to Poll). Poll watches an incarnation once — the first path in path order
  wins, duplicate candidates are refused — and the rename/move rebuild refuses
  a move whose target key another source claims (staying entry or competing
  move), so an acknowledged checkpoint is never overwritten; a refused source
  stays missing and keeps its checkpoint, and a returning path is reported as a
  replacement, not a silent "new file". A checkpoint file from a pre-fix build
  may already hold two sources on one identity: loadState tolerates it (ErrStateLost
  would brick upgrades for no recovery gain) and the poll guards keep both.

- Write-path open question (2026-11-13, internal/store, T006):
  `sessions.repo` stays NULL — no rule derives a repository path from observed
  record fields yet (git root discovery from `cwd` is unspecified, and the
  Claude Code directory encoding is lossy). Define the derivation before any
  query relies on `repo`. Token aggregate policy is settled in DB.md; tool
  aggregates remain deferred pending a query need.

- Local slice wiring (2026-11-13, cmd/ohya, T007): the production
  claudecode.Record → store.Event adapter lives in internal/events (moved
  out of cmd/ohya in T017 so hub-side batch normalization shares it; usage
  field names, contentForm=="string" prompt test). internal/store keeps a
  mirrored copy in ingest_test.go — an in-package test cannot import
  internal/events without an import cycle — so change both together. The
  crash window between the Ingest
  commit and watch.Acknowledge closes by replay on restart (slice test kills
  a helper process there; replay validates stored bytes and inserts nothing).
  Ingestion batches are bounded at 8 MiB so importing a large history file
  stays predictable in memory; a batch never splits a record. A stopped
  (malformed) source prints its diagnostic once per stop offset per run,
  then stays quiet until the offset moves; fresh sources from replacement or
  truncation are reported because they re-import history, new files are not
  (a first import of thousands of files must not print a line per file).

- SQL tool boundaries (2026-11-13, internal/mcpserver, T008) — driver facts
  the read-only policy relies on, all verified by probe against
  mattn/go-sqlite3 v1.14.53:
  - The driver's `QueryContext` executes EVERY statement in the input and
    returns the last statement's rows (sqlite3.go `query` loops the prepare
    tail). One-statement-per-call is therefore enforced by our own scanner
    (`statement.go`) before execution; the authorizer alone would not stop
    `SELECT 1; <anything read-only-but-second>`.
  - This build's authorizer op codes differ from the classic sqlite3.h
    numbering (SELECT=21, READ=20, PRAGMA=19, FUNCTION=31, SAVEPOINT=32);
    always compare against the driver constants, never hardcoded values.
    SQLITE_RECURSIVE (=33 here) is not exported by the driver at all —
    `sqliteOpRecursive` hardcodes it, and the recursive-CTE test case pins
    it; re-verify on any driver bump.
  - FTS5 MATCH internally reads `PRAGMA data_version`, so a pragma
    allow-list must include it or all FTS queries die with "authorization
    denied". MATCH also fires FUNCTION op with arg2="match".
  - Authorizer denials surface with three different SQLite messages:
    "not authorized" (most ops), "access to table.column is prohibited"
    (SQLITE_READ), "authorization denied" (VACUUM). queryError maps all
    three to ErrDenied.
  - `CREATE TRIGGER ... BEGIN ...; ... END` trips the one-statement scanner
    (body semicolons). Harmless: statement blocks only exist in denied DDL.
  - MCP SDK v1.8.0 e2e stdio tests must use the TestMain fork-server trick
    (env var → run server instead of tests, like cmd/ohya/mcp_test.go):
    a test binary serving on stdout interleaves framework output ("PASS")
    with protocol frames. `mcp.NewInMemoryTransports()` covers the
    protocol-level tests without processes.

- Annotate tool choices (2026-11-13, internal/mcpserver + internal/store, T009):
  the SDK's inferred input schema already rejects unknown fields
  (struct inference sets additionalProperties: false) and runs BEFORE the
  tool handler, so a spoofed `writer_id` dies in schema validation; the
  handler re-decodes `req.Params.Arguments` (a json.RawMessage on
  CallToolParamsRaw) with DisallowUnknownFields for the cases the schema
  cannot express (upsert/delete value rule, charset, duplicates) and for
  better error text. jsonschema-go maps `json.RawMessage` to an array of
  integers — never put it in the schema-facing In type; keep a parallel
  `Value any` shape for the schema and raw types for decoding. Values are
  stored compacted, so "identical retry" means equal compact JSON (number
  spelling like 1e2 still counts as different text). Batch counts: applied
  = insert/replacement/delete-that-removed; unchanged = identical upsert
  or absent delete; an empty batch is accepted with zeros. `updated_at_ms`
  is clamped up to `created_at_ms` on update so a backwards hub clock step
  cannot trip the table CHECK and permanently wedge an annotation.

- TLA+/TLC tooling facts (2026-11-13, tla/, T015; relevant to T016's model):
  - TLC 1.8.0 from the GitHub release asset
    (github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar,
    sha256 e1f11b75…cdbcd7f) differs from the tla2tools-1.8.0.jar kept by
    other projects on this machine — pin the release URL, not a local copy.
  - cfg files accept only scalar sets/numbers/strings/booleans as constant
    values; records, tuples, and per-element atoms are rejected. That is why
    Sync.tla models "each source is one device's uploader" structurally
    instead of an owner function (device identity/ownership is T016 anyway).
  - TLA+ has no negative numbers and TLC rejects cross-type equality, so
    no-message sentinels are type-consistent: `<<>>` for the batch channel,
    `MaxLen + 1` (above every cursor) for the ack channel.
  - TLC needs `-deadlock`: the drained system's quiescent state is a
    legitimate fixed point. Its coverage column shows `0` distinct states
    for reset actions (DropBatch, crashes) — expected, their post-states
    are always co-reachable via benign paths; the actions still fire.
  - The reachability bound that keeps the state space small is real
    protocol structure, not a modeling gap: with one in-flight batch of at
    most MaxBatch records, the hub cursor can never lead the acknowledged
    cursor by more than that batch plus an undelivered ack.
  - Liveness with message loss needs the loss/crash actions disabled
    (ReliableNet constant): infinite-loss behaviors violate no fairness
    assumption, so eventual-sync cannot hold without the stated
    eventual-reconnection assumption. Liveness checking is the cost driver
    (12k states, ~13 s, ~1.1 GB) versus seconds for 54k safety states.

- Authorization model choices (2026-11-13, tla/Auth.tla, T016):
  - Immutability properties ("ownership cannot move") are invisible in a
    state invariant without history: a move overwrites its own trace. The
    model carries monotone history variables (`srcEverOwned`, `revokedEver`,
    `annRowSes`) and checks "at most one owner ever" as cardinality bounds;
    six one-line mutations each tripped a distinct invariant. One mutation
    survived by design (an active device consuming a second invitation
    breaks no stated property); the guard blocks it anyway.
  - "A revoked device cannot establish a new authorized session" forces
    revocation to be final per device identity: if a revoked identity could
    consume a fresh invitation, the property would be false. Re-admission
    therefore means a new device.json identity. Orphaned sources stay
    locked (no rebind) — the conservative reading of the sync contract.
  - Invitation expiry is a nondeterministic action, not a clock: the
    protocol fact is that consumption must race expiry and can lose; TTL
    length is configuration. Safety-only model — pairing liveness is device
    reconnection, owned by Sync.tla's eventual-reconnection assumption.
  - String sets work as cfg CONSTANT values ({"d1","d2"}), so the registry
    can be indexed by device atoms directly — no structural workaround
    needed, unlike per-element model values.
  - Auth.cfg 46k states ~1 s; AuthWide.cfg (3 devices, 2 invitations —
    pairing scarcity) 127k states ~3 s; all invariants hold.

- Hub sync endpoint choices (2026-11-13, internal/hubsync, T017):
  - The endpoint is transport-agnostic (serves any net.Listener) and
    authenticates through an Authenticator interface; the wire device ID is
    only ever compared against the authenticated identity, never trusted.
    T018's registry + Tailcat WhoIs will implement the interface.
  - Ownership is enforced at the single write path, not the endpoint:
    store.Batch carries the submitting DeviceID and Ingest rejects a foreign
    device (ErrSourceOwnership), so local ingestion and sync share one rule.
    Rejection classes are sentinels (ErrBatchRejected, ErrSourceOwnership,
    ErrSourceNotRegistered) — the endpoint maps them to protocol error codes
    and retries everything else with capped backoff.
  - A record the hub parser refuses mid-batch commits the valid prefix and
    acknowledges with a stop offset+reason, mirroring local source-stop
    semantics; the rest of that batch is dropped (the device repairs and
    resends). Skipping past the stop is a gap rejection.
  - Records are parsed per record (JSON + one synthetic terminator) with
    SourceScope = wire source ID — identical normalization to local
    ingestion, pinned by a test that compares the full events/sessions rows
    of both paths over the same fixture bytes. A record whose JSON contains
    a line terminator fails the "exactly one record at its framed
    boundaries" cross-check and is rejected.
  - SQLite trigger gotcha: trigger bodies reject bound parameters and
    RAISE(ABORT, …) fires inside the write-path transaction — the endpoint
    tests use one to break a commit mid-transaction and prove rollback
    leaves the cursor unchanged.
  - The endpoint has no CLI yet: serving needs a transport config and the
    production control plane (T033→T034); the credential-registry
    authenticator now exists (T018). The uploader is T019.

- Device pairing choices (2026-11-13, internal/registry + internal/hubsync + internal/device, T018):
  - The registry is its own SQLite database (registry.db in the owner-only
    state directory), not tables in hub.db — the sql MCP tool's connection
    can never reach it, so no EngineOptions.DenyTables entry is needed. Its
    schema is pinned by PRAGMA user_version (single version, refuse newer);
    the store migration runner is deliberately not reused for a two-table
    protected store.
  - The registry DSN needs _txlock=immediate: two processes racing to
    consume one invitation must serialize at BEGIN. With the default
    deferred BEGIN, both run the SELECT, then both try to upgrade to a
    write lock and one dies with database-is-locked instead of losing
    cleanly with ErrInvitationUsed (pinned by TestConsumeRaceSingleUse
    through two handles).
  - Per-request check placement matters: Authorize must run AFTER the frame
    is read, immediately before dispatch. Checking before ReadFrame
    authorizes the idle wait, not the request — a revocation landing while
    the connection sits between frames was then missed for the next frame
    (found by test; the stale check passed, the frame was processed, the
    probe got an ack after the device was revoked).
  - Revocation teardown is poll-based: revocations can be committed by any
    process sharing registry.db (the ohya devices CLI), so the endpoint
    learns them by polling the revoked set (default 1 s) and then closes
    the device's connections and cancels their per-connection contexts;
    in-flight requests die through ctx-aware SQL and the retry loop.
  - Wire: first frame of a connection is 'P' (pair) or 'H' (hello); the
    hub mints the credential at Consume and delivers it once ('C'). The
    auth frames are read with the exact-read frameReader directly off the
    conn — no bufio — so the endpoint's later bufio.Reader starts exactly
    at the next frame. AuthTimeout is enforced with a conn read deadline
    around Authenticate.
  - Secrets (invitation and credential) are 128-bit crypto/rand hex,
    stored only as SHA-256 digests; "do not implement new cryptography"
    reads as stdlib primitives only. Revoked identities can never consume
    a fresh invitation (registry state must be "absent"), so re-admission
    is a new device.json — Auth.tla's RevocationIrreversible.

- Tailcat/tsnet driver facts (2026-11-13, internal/transport, T014; measured
  in ADR 0002, reproduced by cmd/tailcat-spike against local Headscale
  v0.29.7):
  - `tsnet.Server.Up(ctx)` surfaces a rejected pairing key in milliseconds
    ("authkey already used"); a bare `Dial` would sit in NeedsLogin until
    the context deadline. The adapter therefore calls Up before dialing so
    authorization failures fail fast — keep that when the sync layer grows.
  - A tsnet listener's Accept is not context-aware; close the listener to
    unblock shutdown.
  - Ephemeral registration makes the coordination server expire the node at
    disconnect, orphaning the local state (next start needs a new pairing
    key). ohya nodes register durably.
  - WhoIs on an accepted connection returns the peer's login and FQDN —
    the transport-level device identity for the authorization model.
  - `tailscale.com` v1.104.4 hard-requires `go >= 1.27.3` in go.mod;
    GOTOOLCHAIN=auto downloads the toolchain. Headscale v0.29.7 CLI wants
    numeric user IDs (`preauthkeys create -u 1`), and its `server_url`
    must be reachable from every node process.
