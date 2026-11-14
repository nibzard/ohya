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
  - Codex: verified 2026-11-13 against installed codex-cli 0.166.0 (2,488 files, 3.0 GB under `~/.codex/sessions/`, written by versions 0.104–0.167; envelope unchanged). Format, record kinds, identity fields and fixtures are documented in `testdata/codex/README.md`. Points that shape ingestion: the native session id is `session_meta.payload.id` (equals the first file-name UUID) and **one thread spans several rollout files** — resumed/paginated pages are named `rollout-…-<thread-id>_<resume-id>.jsonl` (second UUID never appears in content), set `history_mode: "paginated"`, and re-enumerate prior records after a rollback — 4 of the 7 corpus pairs carry `history_base {thread_id, end_ordinal_exclusive, end_byte_offset}` and overlap the original's ordinal tail (pair: original ordinals 1–984, page 979–1019, but with different content at every shared ordinal); the other 3 re-enumerate from ordinal 1 with no `history_base` — so grouping must key on the thread id across files, never byte offsets, and ordinal collisions need care (see the T044 finding below); `ordinal` exists only from 0.147.0. `event_msg` mirrors `response_item` text (`user_message`/`agent_message`) — parse `response_item` as the transcript or dedupe. Usage: each `token_usage_record.usage` is per `response_id` (not cumulative); `turn_token_usage`/`thread_token_usage` are running totals — sum `usage` only. `session_meta.git` (`branch`, `commit_hash`, `repository_url`) is a direct input for the sessions.repo derivation (T029). Subagent rollouts open with their own meta (`source.subagent.thread_spawn.parent_thread_id`) and then embed the parent's `session_meta` as line 2. `CODEX_HOME` redirects everything (verified: 0.166.0 created `<dir>/tmp` and read `auth.json` there); no compressed session files exist (all plain JSONL). The old `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog`) layout is gone in 0.166.0: schema-numbered DBs sit directly under the Codex home, and the earlier "metadata only" claim fails for `thread_history_1.sqlite`, whose `thread_items.item_json` holds full message/command bodies (projection cache over 511 of 2,488 threads, with byte offsets back into the rollouts) — ingest rollout files only. `session_index.jsonl` is thread-name metadata; `history.jsonl` duplicates user prompts. Wild files occasionally lack the trailing newline or contain malformed complete lines (both covered by fixtures).
  - Pi agent: verified 2026-11-13 (T012) against installed 0.84.2 (already present globally as `@earendil-works/pi-coding-agent`) and latest 1.5.2 from a scratch npm prefix. The writeup claim "project-local `.pi/`" is wrong for sessions: sessions are global, one append-only JSONL per session, at `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<session-uuid>.jsonl` (agent dir overridable by `PI_CODING_AGENT_DIR`; session dir by `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` > `sessionDir`; `--no-session` is ephemeral). `<encoded-cwd>` = cwd minus one leading `/`, every `/` `\` `:` replaced by `-`, wrapped in `--`…`--` (source: `dist/core/session-manager.js`, identical in both versions); lossy (`/a/b` and `/a-b` collide), so never decode it — the `session` header's `cwd` is authoritative. File name = session-start `toISOString()` with `:`/`.` → `-`, then `_` + session UUID (time-ordered, `01a1…`). Session format v3: line 0 is a `session` header (`version`, `id`, `timestamp`, `cwd`); every other entry has `type`, `id` (8-hex), `parentId` (tree, `null` = root), ISO `timestamp`. Kinds seen: `model_change`, `thinking_level_change`, `message` (roles `system`/`user`/`assistant`/`toolResult`). Assistant messages carry `provider`, `model`, `api`, `stopReason`, `responseId`, `rawStopReason`, and one usage+cost object per response (`input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`) — per-response, not repeated across records, so token totals sum straight over assistant messages (no Claude-Code-style dedupe); 1.5.2 adds `durationMs` and `thinkingLevel` and persists a `system` entry (`sections`, `toolsAdded`, empty `content`). `toolResult` carries `toolCallId`, `toolName`, `content[]`, `isError`. Entries flush incrementally: a first model call that fails still persists header + `model_change` + `thinking_level_change` + user + assistant with `stopReason: "error"`, `errorMessage`, zero usage. Headless runs in an untrusted cwd hang on a trust prompt — pass `-a`/`--approve`; runs from the pre-trusted `/home/agent` did not need it. The agent dir also holds `auth.json` (credentials), `settings.json`, `models.json` (provider catalog, keys referenced by env-var name), `trust.json`, `bin/` (`rg`, `fd`), and `~/.pi/skills/`. Real sample sessions and the captured layout (0.84.2 and 1.5.2) are preserved outside the repo at `/home/agent/.looper/ohya-3d80aa08/runtime/task-b55552405a41f18c/work-00-session-15667963bc0501b8/pi-sample-session/` for authoring the fixtures — the T037 fixtures in `testdata/pi/` derive from them; `.pi/` may still exist inside projects for config/packages, but no session writes there were observed.
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


- Codex parser choices (2026-11-13, internal/codex, T011): the record at
  offset 0 names the thread (`session_meta.payload.id`); a parse past offset 0
  needs the committed first record's native ID (`store.SourceState`), so a
  source whose offset-0 record is missing from the hub cannot parse
  (`codex.ErrNoNative`, reported, never guessed). Text comes from
  `response_item` only; `event_msg/user_message` sets `Prompt` (first_prompt)
  because Codex also sends AGENTS.md and `<environment_context>` blocks as
  `user` messages. Replays dedupe on `(session, ordinal)` plus content in the
  write path (migrations 003 and 005); `session_meta` never takes an ordinal,
  because a page's opening meta carries the ordinal one below its first
  record — an ordinal the original file already owns.
  Verified 2026-11-14 (T044, whole corpus): `cache_write_input_tokens` is 0
  in all 310,936 occurrences, so its subset relation stays unverified and it
  stays unmapped; no pre-0.147 resumed page exists (all 7 paginated pairs are
  0.157.1+ and every page record carries an ordinal). Also disproven: a real
  page never byte-replays the original — at all 140 colliding ordinals the
  two files hold different records, because the page re-enumerates after a
  rollback (`history_base.end_ordinal_exclusive` names the durable prefix;
  the aborted turn's records sit in the original only). End-to-end ingest of
  the 7 pairs: 140 suppressed "duplicates" that pair unrelated records and
  11 per-response usage records lost from token totals (3 threads). Facts
  and numbers live in `testdata/codex/README.md`; map cache-write to cache
  creation only after a non-zero sample appears.
  Resolved 2026-11-14 (T051): the dedup is keyed by content (migration 005)
  — a byte-identical copy of a stored original is a duplicate; a differing
  record at the same (session, ordinal) is another original, counted with
  its usage. Rollback-aware winner selection through `history_base` was
  rejected: it must demote stored originals whenever the original file is
  ingested before its page (the write path never updates stored rows), and
  it has no signal for the 3 pages that re-enumerate from ordinal 1 with no
  `history_base`. Migration 003's header still describes the disproven
  byte-replay assumption and must not be edited (applied-migration checksums
  are verified); migration 005 carries the correcting note.

- Pi parser choices (2026-11-14, internal/pi, T037): the `session` header at
  offset 0 names the session — same header-first rule as Codex
  (`pi.ErrNoNative`, FirstNativeID from `store.SourceState`), because no
  later entry repeats the session id. The usage-row key is the assistant
  `responseId`, falling back to the entry id (`Record.EntryID`, parser
  output version 2); an error response reports zeros under its entry id —
  exactly the verified per-response sum. Every user message marks a prompt
  (Pi persists no injected context as user messages; the 1.5.2 `system`
  entry is the preamble and contributes no search text). A toolCall block
  contributes `name + compact sorted-key JSON of arguments` (Claude Code
  tool_use convention). The entry-level ISO timestamp sets time_ms; the
  message-level Unix-ms mirror is not parsed.
  Verified 2026-11-14 (T038) against the 74 real files under
  `~/.pi/agent/sessions` (524 usage records: 517 zai via
  openai-completions, 7 anthropic via anthropic-messages):
  - `cacheWrite` is non-zero in 7 anthropic-messages records and strictly
    additive: `input + cacheRead + cacheWrite + output = totalTokens` in
    every verified record, and the sum without cacheWrite fails in all 7.
    Mapped to the cache-creation count since parser output version 2.
  - Non-zero usage without a `responseId` is the common case, not an edge:
    openai-completions records never carry one (503 successful responses),
    and 4 of 7 anthropic-messages records (the older 2026-04-20
    claude-opus-4-6 runs) lack it — 507 of 524 records would have silently
    counted nothing without the entry-id fallback (entry ids are unique per
    file, verified across the corpus). Errors still report all-zero usage.
  Open:
  - `reasoning` still has no non-zero sample — absent from every
    real-corpus usage object, 0 in the captured openai-responses records —
    so its subset relation (hypothesized: part of output) stays unverified
    and unmapped.
  - `cacheWrite1h` (the 1-hour cache write, on 3 claude-opus-4-8 records)
    is 0 everywhere; whether it is part of `totalTokens` or of `cacheWrite`
    is unverified, so it stays unmapped — recheck both open items if a
    non-zero sample appears.

- FTS5 verification gotcha (verified 2026-11-13, internal/store/open_test.go):
  `SELECT count(*)` on an external-content FTS5 table reads the content table, not
  the index. Assert index consistency with the `integrity-check` command and MATCH
  enumeration. Relevant wherever the write path maintains `events_fts` (T006+).
  Also (2026-11-14, internal/events tests): a bound MATCH parameter takes the
  bare query — `'"phrase words"'` as a parameter is a syntax error, because the
  single quotes are SQL string delimiters, not FTS syntax; pass `"phrase words"`
  and keep the single-quote form only for inline literals.

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
  paths. That includes the expected value: t.TempDir() on macOS sits behind
  /var → /private/var, so a literal temp-dir string never matches (T052,
  fixed 2026-11-14 by computing the expectation through filepath.EvalSymlinks);
  reproduce the condition on Linux by pointing TMPDIR through a symlink (keep
  that TMPDIR short — a long one trips the 108-byte unix-socket path limit in
  the serve e2e tests). Lost state is reported, never silently restarted: an
  unreadable
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
  record → store.Event adapters live in internal/events (moved out of
  cmd/ohya in T017 so hub-side batch normalization shares them; T011 added
  the Codex adapter and the `events.Parse` agent dispatch; usage field
  names, contentForm=="string" prompt test). internal/store keeps a
  mirrored copy in ingest_test.go — an in-package test cannot import
  internal/events without an import cycle — so change both together. The
  crash window between the Ingest
  commit and watch.Acknowledge closes by replay on restart (slice test kills
  a helper process there; replay validates stored bytes and inserts nothing).
  Ingestion batches are bounded at 8 MiB so importing a large history file
  stays predictable in memory; a batch never splits a record — one read may
  extend past the 8 MiB bound to the end of a single larger record, never
  past the configured record size limit (T042, ADR 0004). A stopped source
  prints its diagnostic once per stop offset per run, then stays quiet until
  the offset moves; each diagnostic names its remedy (`--max-record-bytes`
  for a size stop, file repair otherwise). Fresh sources from replacement or
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
  - Serving is wired since T034 (`ohya serve`, see the wiring facts below).

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

- Forced-relay validation facts (2026-11-14, cmd/tailcat-spike + ADR 0002,
  T032; raw artifacts with the T032 run):
  - tsnet's derphttp upgrades the DERP connection to TLS unconditionally
    (no loopback exception); against a plain-HTTP control plane the client
    never even probes /derp, so isolated peers with no direct path never
    connect (observed: 2-minute dial timeout, zero relay traffic). The
    vendor debug switch `TS_DEBUG_USE_DERP_HTTP=1` forces plain-HTTP DERP
    and is the only way to exercise the relay on an HTTP plane — cross-
    machine relay in production rides on the serve TLS gap.
  - The T014 claim that peers "found each other through the embedded DERP"
    is disproven: T014's single-host nodes show no /derp traffic at all;
    discovery ran through the control-plane map exchange, then direct.
  - The spike harness now records the peer path on both sides
    (tailcat.Node.PeerPath: relay name + direct endpoint from
    ipnstate.PeerStatus; empty CurAddr = relay-only). Key conversion is
    key.NodePublic(who.Node.Key) against Status.Peer — note the field is
    Peer, not Peers, in v1.104.4.
  - Reproduction topology: two user-defined docker bridges with the nodes
    on separate networks and Headscale dual-attached (network alias
    `headscale`, so server_url resolves per network); docker's nftables
    forward rules drop all cross-bridge traffic (counters prove it).
    Same-day direct reference: both node containers on one bridge.
    Host-process runs cannot resolve the DERP hostname (server_url's host
    is baked into the DERP map), so keep validation runs in containers.
  - Numbers: relay-only p50 260 µs @1 KiB / 1,244 µs @64 KiB / 14,215 µs
    @1 MiB (69.5 MiB/s) vs direct 124 / 676 / 3,561 µs (251.3 MiB/s) —
    1.8–2.1× at kilobyte sizes, ~4× at 1 MiB; warm dial ~4 s either way.
    busybox `nc -u -z` is not a UDP reachability check (always "open").

- Remote query/annotate path choices (2026-11-13, internal/hubsync +
  internal/mcpserver + internal/store, T036; ADR 0003):
  - Query ('Q') and annotate ('N') frames ride the sync connection; the
    endpoint serves them through hubsync.RemoteEngine, implemented by the
    mcpserver engine, so bounds and read-only policy live in one place.
    The import direction hubsync → mcpserver is deliberate: the sync
    protocol carries the remote MCP surface, and mcpserver stays free of
    sync concerns (no cycle; registry already imported hubsync).
  - Writer derivation is structural: the annotate frame has no writer
    field at all, so nothing can be spoofed.
  - Deterministic annotate rejections needed sentinels for truthful wire
    codes: mcpserver.ErrAnnotateRejected and store.ErrAnnotationRejected.
    Their text equals the historic error prefix ("annotate tool",
    "store: annotate"), so every error message stayed byte-identical while
    errors.Is now classifies CodeRejected vs CodeInternal at the endpoint.
  - The query result wire bound is 4 MiB (16x the engine's 256 KiB default
    output); an engine configured past it answers CodeInternal with the
    bound named — pinned by test. lpad does not exist in the bundled
    SQLite; use hex(zeroblob(n)) to generate wide deterministic rows.
  - RemoteClient.exchange sets conn deadlines per exchange and relies on
    the next exchange resetting them (uploader pattern); no deadline is
    left dangling between exchanges.
  - Deployment wiring landed (T034): serve attaches the engine as the
    RemoteEngine, and the stdio bridge is `hubsync.RemoteSession`
    (internal/hubsync/session.go) behind `mcpserver`'s `Backend`
    interface. On a non-protocol error the session drops the connection,
    reconnects, and retries once; a CodeAuth answer maps to
    ErrRemoteUnauthorized instead of looping with a refused credential.

- Device uploader choices (2026-11-13, internal/uploader, T019):
  - The uploader reuses internal/watch as its checkpoint store: the cursors
    are hub acknowledgments. A state directory shared with local `ohya
    ingest` would mix two acknowledgment sources and skip bytes, so the
    join command (T034) uses a dedicated one (`--state`, default ~/.ohya/join).
  - A pass with nothing pending makes no exchange, so a hub that restarted
    from a restored backup went unnoticed on an idle connection (found by
    test). Each pass now checks the open connection with a 1 ms read
    deadline plus bufio Peek: timeout = open, EOF/reset = reconnect and
    re-probe every source. A hub host that dies without FIN/RST is still
    noticed only at the next exchange or by TCP keepalive (unverified for
    the tsnet netstack).
  - The endpoint writes CodeBusy and closes before reading the hello, so
    the device's first write can fail with a reset; the uploader then tries
    to read the hub's error frame before reporting a plain disconnect.
  - A parse stop is not resent until the file grows (in-memory mark per
    process); a restart resends the stopped bytes once.

- Serve/join wiring facts (2026-11-14, internal/control + cmd/ohya, T034;
  ADR 0002 records the control-plane decision):
  - Embedded headscale v0.29.7 goes through internal/control only, the way
    tailcat isolates tsnet. Facts: `types.LoadConfig(path, true)` is
    viper-global, so one process holds one plane; config paths must be
    absolute (headscale joins relative paths against the config directory,
    doubling them); `app.Serve` installs its own SIGTERM/SIGINT handlers,
    so the plane stops with the process (verified by the e2e clean exits).
    State surface used: GetUserByName/CreateUser,
    CreatePreAuthKey(&types.UserID, reusable=false, ephemeral=false,
    &exp, nil) → *types.PreAuthKeyNew, GetPreAuthKey (spent?), ResolveNode
    (hub enrolled?).
  - The join code is `invitation:preauthkey` — each half single-use within
    the invitation's lifetime. `devices invite` opens the control plane
    before the registry, so a missing plane issues nothing. The plane's
    SQLite (glebarez driver, busy timeout 10 s) tolerates a concurrent
    `devices invite` process while serve runs.
  - Serve derives `-control-url` from `-control-listen` and refuses a
    wildcard listen without an explicit URL (it cannot guess the address
    devices use). The hub node mints its own single-use key only while
    `ResolveNode("ohya-hub")` finds no node; restarts with existing node
    state consume none.
  - Join enrolls with the preauth half, then pairs over the tailnet (node
    dial → 'P' frame → 'C' credential). `ohya mcp --remote` reuses the
    join state directory; `--remote` is boolean and `--hub-addr` (same
    flag and default ohya-hub.tail.ohya:9027 as join) carries the address.
  - e2e (cmd/ohya/serve_e2e_test.go, fork-child pattern): serve ready in
    ~1 s, the 16-record fixture lands within ~10–20 s of join, the first
    remote dial costs ~10 s (tsnet path discovery). Skips on darwin CI —
    macOS runtime behavior of the pinned library is unverified (T046);
    Linux is the development and CI platform (T043).
  - darwin check-job facts (2026-11-14, T057, internal/control): headscale
    serves its gRPC API on a unix socket at <dir>/headscale.sock, and
    darwin's sun_path holds 104 bytes (Linux 108) — t.TempDir() on the
    macOS runner (/var/folders/<xx>/<per-user id>/T/) pushes
    TestControlStartServeAndMint's socket to ~100–105 bytes, so Serve died
    at "setting up gRPC socket: bind: invalid argument". The test now hosts
    the plane under a flat os.MkdirTemp (socket ≤ ~97 bytes on every
    supported platform); reproduce the old failure on Linux by pointing
    TMPDIR at a ~90-char path. Same test also reserved the STUN port via a
    TCP bind, but the STUN listener binds UDP and a failed bind is fatal
    inside headscale (log.Fatal exits the test binary) — it now reserves a
    UDP port, like freeUDPPort in the serve e2e test.

- Parser output version and re-derivation choices (2026-11-13,
  internal/store + internal/events, T025): `events.parser_version` versions
  what a parser derives from one record, deliberately separate from the
  schema version (agentsview's `dataVersion` lesson). Rules: bump the
  parser's `OutputVersion` in the same commit as the behavior change;
  version 1 is the pre-marker output; a build refuses rows stamped above
  its own parser (upgrade first, then re-derive). `store.ReDerive` walks an
  agent's sources in `(source_id, source_offset)` order and each source's
  records in offset order, so re-derivation needs no arrival-order
  accidentals; the deriver binding (`events.Derivers`) re-parses one stored
  record with `SourceScope = source ID` (synthetic native IDs stay stable)
  and `FirstNativeID` fed by the preceding record, which is what Codex needs
  past offset 0. Open:
  - Duplicate reassignment switches from arrival order to walk order: a
    hub that ingested a resumed page before the original file changes
    which byte-identical copy is the original after a rebuild (only the
    row id in `duplicate_of` moves). Distinct records that share an
    ordinal stay separate originals in every order (T051).
  - `events_fts` has insert/delete triggers but no update trigger
    (ingestion never updates rows); re-derivation relies on the
    external-content `rebuild` command in the same transaction to restore
    index consistency — verify with `integrity-check`, never `count(*)`.

- Performance measurement choices (2026-11-14, internal/fixgen +
  internal/events + internal/mcpserver + docs/performance.md, T039):
  - `internal/fixgen` generates deterministic synthetic Claude Code corpora
    (seeded PCG; same seed, same bytes) shaped by the verified format notes:
    ~3.2 KB mean record, 2 MiB main sessions plus subagent files, skewed
    word distribution with rare marker terms so FTS benchmarks have both
    wide and selective queries. `cmd/genfixture` writes it to disk for
    runs of the real binary.
  - Per-agent corpora (T048): `GenerateCodex`/`GeneratePi` mirror the
    verified rollout and session shapes (subagent rollouts open with their
    own meta and embed the parent's; Pi uses the 1.5.2 shape with the
    system entry). All three generators share one text distribution
    (`textGen`), so parse benchmarks compare envelopes, not text volume —
    and the Claude Code bytes are pinned by the earlier measurements:
    refactor without changing the rng call order and verify byte-identity
    against a pre-change snapshot (done for the textGen extraction).
    Measured parse rates: Claude Code 127–132, Codex 173–178, Pi 156–159
    MB/s — same order, JSON decode dominating, so per-agent write-path
    benchmarks stay unnecessary (the path behind the parser is shared and
    fsync-bound). `genfixture -agent codex|pi` writes the per-agent trees
    the CLI discovers. The generators must keep the verified usage
    identities (Codex cached within input; Pi parts summing to totalTokens)
    — pinned by TestCodexCorpusParsesCompletely and TestPiCorpusParsesCompletely
    counting HasUsage against the records that carry usage.
  - The write path is fsync-bound, not CPU-bound: with synchronous FULL,
    the import rate swings 4–20 MB/s with disk contention on the shared
    reference VM, while parse-only stays 126–130 MB/s and replay validation
    ~1 GB/s. CPU profile: 67% of samples in CGO (SQLite inserts/commit),
    8–15% encoding/json; no hot ohya Go function — no Go-side optimization
    is justified (the levers are durability and batch size, both deliberate).
  - Database multiplier is structural, ~2.4x input bytes (raw JSON + text +
    metadata + FTS index); measured 351.6 MB for a 138.7 MiB corpus.
    Peak import RSS ~28 MB (streamed 8 MiB batches); no-op poll over 461
    sources 0.02 s; one live append pass 0.13 s; wide FTS MATCH ~0.5–1 ms
    per 1,000 matching events, everything else flat in volume.
  - The mcpserver query benchmark shares one seeded hub across benchmarks
    via sync.Once and closes it in TestMain; a per-benchmark cleanup would
    close the DB while later benchmarks still use it, and the temp dir must
    be removed by that TestMain (an os.MkdirTemp leak there once filled the
    disk on this machine).
  - Measured limits live in docs/performance.md, not docs/operations.md:
    the operations guide is T040's parked work (install/backup/recovery,
    blocked on macOS verification) and creating the same path here would
    collide when that work integrates; link performance.md from it then.
  - Regression guards run in make check with order-of-magnitude ceilings
    (ingest 24 MiB < 30 s, queries < 2 s) — they catch superlinear
    regressions, not machine drift; precise tracking is benchstat work,
    not automated.

- Memory verification gate choices (2026-11-14, cmd/memory-e2e, T041):
  - Claude Code 2.1.280 headless: `claude -p <prompt> --mcp-config <file>
    --strict-mcp-config --allowed-tools mcp__ohya__sql,... --output-format
    stream-json --verbose` runs with CLAUDE_CONFIG_DIR (plus HOME) redirected
    into the gate's output directory and authenticates from the ambient
    ANTHROPIC_* env alone — no credential files are read. The final stream
    event `{"type":"result","subtype":"success","result":...}` carries the
    answer; assistant `tool_use` blocks carry the tool calls. Give the child
    stdin /dev/null or it waits 3 s for piped input.
  - Codex 0.169 headless: OPENAI_API_KEY is no longer picked up implicitly
    (`preferred_auth_method` is ignored); run `printenv OPENAI_API_KEY |
    codex login --with-api-key` once inside the isolated CODEX_HOME. The
    config.toml naming the ohya server must sit INSIDE CODEX_HOME — a config
    written next to it is silently never read, and the model then answers
    that the sql tool is unavailable (a misleading "Untrusted project-local
    configuration" warning can point the wrong way; the router error is
    "unknown MCP server 'ohya'"). `codex mcp list` under the isolated
    CODEX_HOME proves registration before any agent run. `codex exec --trust`
    is passed defensively (codex gates MCP servers behind project trust in
    untrusted directories) and writes only inside CODEX_HOME — verified
    nothing lands in the real ~/.codex. `codex exec --ephemeral --json -o
    <file>` emits `item.completed` events (mcp_tool_call with server+tool,
    agent_message) plus `turn.completed`; websocket-fallback error items
    appear even on successful runs and are benign.
  - Do not redirect HOME for a mise-shimmed `codex`: the shim busy-spins
    resolving its install when HOME is foreign (verified: shim at 110% CPU,
    zero output; the direct install binary under the mise tree works). The
    gate keeps the real HOME for codex and relies on CODEX_HOME alone for
    isolation; claude gets a scratch HOME plus an env allowlist
    (PATH/HOME/TERM/LANG/CLAUDE_* + ANTHROPIC_* passthrough).
  - The gate's own citation check resolves cited event IDs through a second
    `ohya mcp` session (the same sql surface), so "resolvable evidence" is
    proven against the tool contract, not by opening the database directly.

- Benchstat tracking choices (2026-11-14, Makefile + .github/workflows/ci.yml, T047):
  - benchstat is a pinned tool install
    (golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e),
    deliberately not a go.mod dependency — nothing in the project imports
    it. x/perf has no tagged releases, so the pin is a pseudo-version;
    bump it consciously.
  - `make bench-save` writes raw runs under perf/ (gitignored, like
    profiles); CI uploads each run as the `bench-<sha>` artifact and the
    baseline reaches PRs through the actions-cache (`bench-baseline-<sha>`
    keys, saved only on main pushes — caches are immutable per key, hence
    the sha in the key and the prefix restore).
  - The CI bench job reports and never fails on a delta: GitHub's shared
    macOS runners are not the reference machine, and their fsync-bound
    rows swing with disk contention. Only same-hardware comparisons count;
    CI deltas are a prompt to reproduce with bench-save on both commits.
  - bench-save captures stdout with `>` (not `tee`): a pipeline would hide
    a failed `go test` behind tee's exit 0.
  - BenchmarkIngestAppend must serve any b.N the framework asks for
    (fixed 2026-11-14, T053): its 16 MiB corpus holds only ~470 eight-record
    append windows, and on fast storage (the CI runner) the default 1 s
    benchtime drives b.N past that cap — the old b.Fatalf made the whole
    bench job fail (go test exit 1, make exit 2, output lost inside
    perf/bench.txt). The benchmark now wraps the window index and re-primes
    a fresh database (prefix committed, timer stopped) at each lap start.
    Same trap for any future benchmark over a bounded corpus: never
    b.Fatalf on exhaustion; re-prime or regenerate instead. The bench job's
    artifact upload is now `if: always()` so a failed run still ships its
    partial perf/bench.txt for diagnosis.
