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


- Agent session storage, verified against installed versions on this machine (2026-11-13); these findings shape ingestion:
  - Claude Code: verified 2026-11-13 against installed 2.1.280 (895 main sessions, 2,646 JSONL files). Format, record kinds, identity fields, compound records and usage semantics are documented in `testdata/claude-code/README.md`, with synthetic fixtures. Points that shape ingestion: `sessionId` is the main file name and never spans files; subagent files under `<session-uuid>/subagents/` reuse the parent `sessionId` with `isSidechain: true` plus `agentId`; one API response is several records sharing `message.id` and repeating the same per-request `usage` (not cumulative — dedupe by `message.id` before summing); `cwd`/`gitBranch` live on message records; the project directory name encodes `cwd` lossily (`/ . _` → `-`), so never decode it; timestamps are ISO-8601 UTC ms but not strictly monotonic; malformed complete lines do occur in real files, with valid lines after them.
  - Codex: verified 2026-11-13 against installed codex-cli 0.166.0 (2,488 files, 3.0 GB under `~/.codex/sessions/`, written by versions 0.104–0.167; envelope unchanged). Format, record kinds, identity fields and fixtures are documented in `testdata/codex/README.md`. Points that shape ingestion: the native session id is `session_meta.payload.id` (equals the first file-name UUID) and **one thread spans several rollout files** — resumed/paginated pages are named `rollout-…-<thread-id>_<resume-id>.jsonl` (second UUID never appears in content), set `history_mode: "paginated"`, and re-enumerate prior records after a rollback — 4 of the 7 corpus pairs carry `history_base {thread_id, end_ordinal_exclusive, end_byte_offset}` and overlap the original's ordinal tail (pair: original ordinals 1–984, page 979–1019, but with different content at every shared ordinal); the other 3 re-enumerate from ordinal 1 with no `history_base` — so grouping must key on the thread id across files, never byte offsets, and ordinal collisions need care (see the T044 finding below); `ordinal` exists only from 0.147.0. `event_msg` mirrors `response_item` text (`user_message`/`agent_message`) — parse `response_item` as the transcript or dedupe. Usage: each `token_usage_record.usage` is per `response_id` (not cumulative); `turn_token_usage`/`thread_token_usage` are running totals — sum `usage` only. `session_meta.git` (`branch`, `commit_hash`, `repository_url`) is a direct input for the sessions.repo derivation (T029). Subagent rollouts open with their own meta (`source.subagent.thread_spawn.parent_thread_id`) and then embed the parent's `session_meta` as line 2. `CODEX_HOME` redirects everything (verified: 0.166.0 created `<dir>/tmp` and read `auth.json` there); no compressed session files exist (all plain JSONL). The old `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog`) layout is gone in 0.166.0: schema-numbered DBs sit directly under the Codex home, and the earlier "metadata only" claim fails for `thread_history_1.sqlite`, whose `thread_items.item_json` holds full message/command bodies (projection cache over 511 of 2,488 threads, with byte offsets back into the rollouts) — ingest rollout files only. `session_index.jsonl` is thread-name metadata; `history.jsonl` duplicates user prompts. Wild files occasionally lack the trailing newline or contain malformed complete lines (both covered by fixtures).
  - Pi agent: verified 2026-11-13 (T012) against installed 0.84.2 (already present globally as `@earendil-works/pi-coding-agent`) and latest 1.5.2 from a scratch npm prefix. The writeup claim "project-local `.pi/`" is wrong for sessions: sessions are global, one append-only JSONL per session, at `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<session-uuid>.jsonl` (agent dir overridable by `PI_CODING_AGENT_DIR`; session dir by `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` > `sessionDir`; `--no-session` is ephemeral). `<encoded-cwd>` = cwd minus one leading `/`, every `/` `\` `:` replaced by `-`, wrapped in `--`…`--` (source: `dist/core/session-manager.js`, identical in both versions); lossy (`/a/b` and `/a-b` collide), so never decode it — the `session` header's `cwd` is authoritative. File name = session-start `toISOString()` with `:`/`.` → `-`, then `_` + session UUID (time-ordered, `01a1…`). Session format v3: line 0 is a `session` header (`version`, `id`, `timestamp`, `cwd`); every other entry has `type`, `id` (8-hex), `parentId` (tree, `null` = root), ISO `timestamp`. Kinds seen: `model_change`, `thinking_level_change`, `message` (roles `system`/`user`/`assistant`/`toolResult`). Assistant messages carry `provider`, `model`, `api`, `stopReason`, `responseId`, `rawStopReason`, and one usage+cost object per response (`input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`) — per-response, not repeated across records, so token totals sum straight over assistant messages (no Claude-Code-style dedupe); 1.5.2 adds `durationMs` and `thinkingLevel` and persists a `system` entry (`sections`, `toolsAdded`, empty `content`). `toolResult` carries `toolCallId`, `toolName`, `content[]`, `isError`. Entries flush incrementally: a first model call that fails still persists header + `model_change` + `thinking_level_change` + user + assistant with `stopReason: "error"`, `errorMessage`, zero usage. Headless runs in an untrusted cwd hang on a trust prompt — pass `-a`/`--approve`; runs from the pre-trusted `/home/agent` did not need it. The agent dir also holds `auth.json` (credentials), `settings.json`, `models.json` (provider catalog, keys referenced by env-var name), `trust.json`, `bin/` (`rg`, `fd`), and `~/.pi/skills/`. Real sample sessions and the captured layout (0.84.2 and 1.5.2) are preserved outside the repo at `/home/agent/.looper/ohya-3d80aa08/runtime/task-b55552405a41f18c/work-00-session-15667963bc0501b8/pi-sample-session/` for authoring the fixtures — the T037 fixtures in `testdata/pi/` derive from them; `.pi/` may still exist inside projects for config/packages, but no session writes there were observed.
  - OpenCode (phase 2): storage moved to SQLite in v2 (verified 2026-11-13 against 2.0.12) - `~/.local/share/opencode/opencode.db` with WAL (tables `session`, `message`, `part`, `event`, and so on); the old one-JSON-per-message layout no longer applies, so it needs a poll-based SQLite source, not a byte cursor.
  - Claude Desktop regular chats: server-side, not file-watchable; potentially pollable via unofficial authenticated API (see Claude Explorer / rpeck/claude-explorer) or official export. Would need a new poll-based source type — deferred.
  - ChatGPT desktop: no reliable local transcripts; official export only — deferred.
  - Cowork local sessions may write JSONL under `~/Library/Application Support/Claude/local-agent-mode-sessions/` — absent on current install; check if usage starts.

- Parser format facts cross-checked against agentsview (2026-11-13, commit
  b262670; full study at /notes/agentsview-ingestion-study-2026-11-13.md):
  - Claude Code: session ID is the filename stem; a differing in-file
    `sessionId` marks a parent/continuation session. Consecutive assistant
    records sharing one `message.id` are streaming chunks of one message and
    must merge. Subagent transcripts live under
    `<project-dir>/<session>/subagents/agent-<id>.jsonl`. Tool results can
    reference a side file ("Full output saved to: ..."). Recorded `cwd` beats
    the encoded directory name for project identity; worktrees make cwd-only
    identity brittle, so keep `git` evidence from records when present.
  - Codex: agentsview claimed rollouts older than about one week are
    zstd-compressed to `.jsonl.zst` in place; this machine's corpus
    contradicts it (2,488 files back to February 2026, all plain JSONL,
    verified 2026-11-13), so treat the claim as unconfirmed. The ingest
    walker only picks up `.jsonl` files, so a `.zst` would be skipped
    silently; if one ever appears, decide skip-with-diagnostic versus a
    streaming decompressing reader then. `session_index.jsonl` is a
    shared, rewritten index — never treat it as an append-only source.
    Forked/subagent rollouts replay the parent's whole history with
    re-stamped timestamps; gate on turn-ID membership or usage double-counts.
    `event_msg.token_count` usage attaches to the last usage-less assistant
    message in the current turn (never drop tokens silently); distinguish
    "zero tokens" from "field absent" with presence flags.
  - Timestamps can disagree with file order in Claude transcripts (queued
    prompts, hooks); record both and sort deterministically.

- Cursor/checkpoint design inputs from agentsview (2026-11-13, study §1):
  gate appends on inode/device + size + mtime + ctime ("a write restores mtime
  but cannot restore ctime"); verify the committed prefix with a SHA-256
  digest of its last 128 KiB; keep a resumable hash state so appends hash only
  the suffix. Advance the cursor only through the last complete valid JSON
  line; if an append contains zero complete records, keep the cursor and write
  no skip-state, because writers can restore the old mtime. Split small
  stat-gate metadata from larger resume blobs. These belong in device-local
  state files (per ARCHITECTURE.md), not the hub schema.

- Version parser output separately from schema (agentsview "dataVersion",
  179 as of the study): every parser behavior change re-derives rows. Because
  ohya preserves raw records, re-derivation can run inside the hub from stored
  raw JSON, without source-file resync. Plan the version marker and a
  re-derive migration before the first parser ships.

- Malformed complete lines: ADR 0004 accepts stopping at the offending
  offset without skipping bytes. Repair the source file, or raise
  `--max-record-bytes` for a size stop. A degraded `ingest --once` pass
  exits 3 and reports each stopped source with its remedy.

- Multi-machine ingestion may not need networking first: agentsview supports
  out-of-band transport (rsync/git/shared mount) into machine-labeled source
  roots, one ingesting instance, dedup by native session ID. A "copied roots"
  source mode could deliver shared history before the Tailcat spike de-risks
  pairing and relays. Consider the ordering in BACKLOG.md item 4.

- MCP memory patterns for `sql`/`annotate` (2026-11-13, study §6): exclude
  sessions active in the last 10 minutes by default so agents do not retrieve
  their own live session; document revision-bound pagination in the tool
  description; treat transcript text returned to agents as hostile
  (deterministic injection-bait labels, secret redaction over decoded JSON);
  fail closed on unreviewed derived values.

- FTS/storage lessons for the hub (2026-11-13, study §4): store
  content-bearing text once — duplicated tool-result summaries cost agentsview
  about 40% of file size; drop FTS triggers and derived indexes during bulk
  import and rebuild after ("any row trigger makes SQLite stage each multi-row
  INSERT through a temporary table"); keep a one-transaction `rebuild` path
  for `events_fts` from day one.

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
  `sessions.repo` derivation is now defined and implemented (2026-11-14, DB.md
  "Repository derivation": normalized repository URL from the session's own
  records; Codex `session_meta.git.repository_url`, others NULL; re-derivation
  recomputes it; codex parser output version 2). Tool aggregates remain
  deferred pending a query need.

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
  - TLC 1.8.0 jars from different builds differ in embedded build metadata
    only (identical file listing; the 2026-11-15 upstream rebuild moved only
    the Build-TimeStamp). The exact bytes are vendored at `tla/vendor/` and
    checksum-pinned (ADR 0007), so build drift between copies no longer
    matters.
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
  - Live TLS smoke (2026-11-14, after T054, self-signed ECDSA cert,
    SAN 127.0.0.1): `ohya serve --tls-cert/--tls-key` serves coordination
    and health over TLS (curl --cacert 200; untrusted client refused).
    The tsnet client honors Go's system-roots knobs — `SSL_CERT_FILE` with
    the plane's certificate is enough for a device to trust it ("passed
    x509 validation"), no OS trust-store edit needed on Linux. Pairing and
    auth complete over TLS, but the first `ohya-hub.tail.ohya:9027` dial
    then resolved through the system resolver (127.0.0.53) instead of
    MagicDNS and join exited 1; the same-host plain-HTTP join resolves and
    uploads fine. Reproduced twice. Root cause found (2026-11-15): serve's
    own hub node also rejects the self-signed plane cert ("bad certificate"
    handshakes), never enrolls, and the tailnet the device joins therefore
    has no ohya-hub peer — MagicDNS then falls back to the system resolver
    for the missing name. Fix: serve prepends a state-dir trust directory
    (symlink to the plane cert) to `SSL_CERT_DIR` before any TLS use, and
    `Node.Dial` waits for the backend to be Running with a MagicDNS suffix
    and peers before dialing. Live retest: zero bad-certificate errors,
    pairing + 1,307 claude-code + 1,671 codex events synced over TLS;
    `TestServeJoinTLS` pins the path. T065's two-host relay measurement is
    still open.
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
    remote dial costs ~10 s (tsnet path discovery). The core serve/join
    and TLS tests now run on darwin CI. Their state directories use a
    short `/tmp/ohya-e2e-*` path to fit the control socket limit. Native
    macOS execution still needs a new CI run; Linux checks cannot prove it.
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

- CLI contract choices (2026-11-14, cmd/ohya/cli.go, T059): help is stdout
  plus exit 0 (bare command, -h, --help at every level, bare `ohya devices`);
  usage errors are one stderr line pointing at the failing command's --help
  plus exit 2; other failures stay exit 1. The flag package itself prints
  nothing (SetOutput(io.Discard)); its Usage renders the uniform template
  into a per-command buffer, because Parse calls Usage both for help and
  before a parse failure — only a help request flushes the buffer to stdout.
  cliMain(args, stdout, stderr) int is the testable exit-code mapping behind
  main; the process-level test forks through TestMain's `child-run` role.
  Help and error texts spell flags with two dashes; the parser keeps
  accepting one dash.
  T060 done (2026-11-14): `ingest --once` returns the errDegraded sentinel
  (cmd/ohya/ingest.go) when passStats counts any failed or stopped source;
  cliMain maps it to exit 3 with no extra stderr line (diagnostics already
  reported per source), and the summary line appends "; N failed, M stopped"
  only when degraded. Gotchas: writeUsage wraps a spec description at 80
  columns, so a contract phrase that crosses a wrap boundary breaks substring
  tests — put such a sentence on its own "\n" paragraph (wrap() splits on
  newlines); and a chmod-0 unreadable-source fixture needs a skip guard when
  the test process can read through mode 0 (root).
  T061 done (2026-11-14): `ohya version` / `ohya --version` print one
  key=value line (cmd/ohya/version.go); schema comes from
  store.SchemaVersion() (highest embedded migration, no database opened),
  parser keys from each package's OutputVersion. Verified fact: with Go
  1.27.3, ReadBuildInfo's Main.Version in a VCS checkout is a pseudo-version
  (`v0.0.0-20261114181354-e177ac9549f4+dirty`), not "(devel)" — the
  version=devel fallback only fires outside VCS or without a module version,
  and vcs.revision/vcs.modified are absent in those builds (revision and
  dirty print "unknown"). The root alias check (isVersionArg) runs before
  subcommand dispatch, so `ohya ingest --version` stays a usage error.
  T062 done (2026-11-14): --json on ingest --once, devices list/invite/
  revoke, rederive, version prints one ADR 0005 envelope
  (cmd/ohya/jsonout.go; schema_version 1). Choices worth keeping:
  - A degraded --once pass is a completed pass: data carries counts and
    stops, error stays null, exit stays 3. Failure envelopes (data null,
    error {code,message,hint}) are only for systemic failures; usage errors
    never produce an envelope (T059's one-line stderr contract owns them).
  - Error codes classify by sentinel (errors.Is), never by prose: two
    sentinels were added for that (registry.ErrAlreadyRevoked,
    control.ErrNotProvisioned); site-known classes (database_open_failed,
    missing_agent_root, registry_open_failed) wrap with the local
    classifiedError, whose Error() stays the underlying message so text
    output is byte-identical.
  - The encoder calls SetEscapeHTML(false): messages carry paths and flag
    spellings with < > & (the hint "--<agent>-root"), and escaped output
    made goldens unreadable.
  - Stop remedies are stable codes in JSON (increase_max_record_bytes |
    repair_source_file) while stderr keeps ADR 0004's prose.
  - Golden tests scrub run-varying values by key ("code", "expires_at",
    duration_ms) and replace tempdir prefixes; json.RawMessage data decodes
    as the literal "null" bytes (never a nil slice) — compare with
    string(data) == "null", not data == nil.
  - Forcing stale rows for a rederive golden needs a parser above version 1:
    events.parser_version has CHECK (>= 1), so the Claude Code parser
    (version 1) cannot go stale; the Pi fixture (version 2) stamped down to
    1 works.

- Doctor choices (2026-11-14, cmd/ohya/doctor.go + internal/store/doctor.go,
  T063): doctor answers what the sql tool cannot — state tied to this binary
  or outside the database. Facts future work must keep:
  - FTS5 integrity-check gotchas on the bundled SQLite 3.54.1 (all verified
    by probe): the command is a write statement, so mode=ro AND
    query_only both refuse it ("attempt to write a readonly database");
    doctor runs it on a private in-memory snapshot taken through the
    driver's Backup API (store.FTSIndexConsistent) — the hub file stays
    read-only. Without a rank argument
    (`INSERT INTO ft(ft, rank) VALUES('integrity-check', N)`) an
    external-content table gets only the structural check and every
    index-vs-content drift passes; with N>0 the checksum comparison runs —
    but it demands a %_docsize row for every content-table row, and the ohya
    schema deliberately leaves NULL/empty-text events unindexed, so a
    healthy hub fails. FTSIndexConsistent therefore drops the maintenance
    trigger and deletes text-less events rows on the snapshot first; the
    comparison then covers exactly the rows the write path indexes. Note
    `SELECT rowid`/`count(*)` on external content read the content table,
    never the index, so no SELECT-only substitute exists.
  - Single-connection pools: a `:memory:` snapshot DB must SetMaxOpenConns(1)
    and run every statement through one checked-out *sql.Conn — ExecContext
    on the pool while the Conn is held blocks forever waiting for a free
    connection (found by test hang).
  - Stop detection is read-only re-decision: parse limit+1 bytes at each
    pending checkpoint cursor (a record within the bound is fully visible —
    JSON bytes ≤ limit plus one terminator); report only a StopError AT the
    cursor (the checkpoint's state, matching where ingest acknowledged);
    FirstNative comes from store.ReadOnlySourceState when cursor > 0 so
    Codex past offset 0 parses. A stop deeper in pending bytes surfaces
    after the next ingest pass advances the cursor onto it.
  - Doctor never creates state: device.Load (new, read-only) instead of
    LoadOrCreate; watch.InspectSources reads checkpoints without Poll.
    Missing identity/database = warn (ingest creates them), unusable ones =
    error. The coordination probe (only when credential.json is present)
    counts any HTTP response as answered — the control plane 404s on /.
    Error findings ride in the JSON data with exit 1 (findings are the
    report, the exit code is the verdict), mirroring degraded ingest.

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

- Memory gate, benign Codex error items, decided 2026-11-15 (T067): Codex
  0.169 emits an `error` item even on successful runs when it falls back
  from the WebSocket transport to HTTPS (observed message: "Falling back
  from WebSockets to HTTPS transport. unexpected status 401 …" — the wss
  endpoint rejects API-key auth; the same episode also emits top-level
  `{"type":"error","message":"Reconnecting… N/5 …"}` events, which the
  trace parser never read). `parseCodexTrace` routes an item whose message
  carries that verified prefix into report-only notes (`runResult.notes`,
  printed under the case verdict); every other error item stays a case
  problem, so an unknown Codex error still fails the gate. Extend
  `benignCodexErrorItem` only with a verified benign sample.

- Freshness harness (2026-11-15, T068, cmd/ohya/freshness_test.go): measures
  the north-star metric — append on the device to visibility through the
  remote sql tool — over the real child-process stack (serve, join, remote
  mcp observer on one host, local tailnet path). Contract kept: unique
  `ohyafresh-<run>-<seq>` markers polled on the direct `events` table (never
  `events_settled`); appender and observer share one process and one
  monotonic clock; fixed-rate appends never wait for visibility, with a
  random phase offset plus jitter against the upload interval
  (`OHYA_FRESH_SEED` reproduces); timed-out samples stay in the distribution
  at drain lower bounds, split into overdue-but-visible and
  outstanding-at-drain, followed by a bounded health wait that reports
  late deliveries and any records that never landed. Env knobs
  (`OHYA_FRESH_SAMPLES/APPEND_EVERY/POLL_EVERY/TIMEOUT/UPLOAD_EVERY/SEED`)
  make the same test the T070 workload-matrix vehicle. Smoke result on the
  default configuration (2 s upload interval, 100 samples, 200 ms appends,
  100 ms polling, 30 s budget, reference VM, Go 1.27.3): lag p50 1.178 s,
  p95 2.043 s, p99 2.154 s, max 2.247 s; 223 polls, 0 errors, query p50
  1.4 ms; 0 timeouts, 0 outstanding; hub 116 events; whole test 44 s. The
  distribution sits just under the upload interval — the interval, not the
  write path, dominates the budget. Gotchas: the observer warms its first
  exchange before the first append (a cold first dial costs seconds and
  would poison sample one; warm cost 2 ms because the shared device node
  was already up), and the marker query pages by `id > last` so the 500-row
  tool bound never truncates a backlog drain.

- Benchmark corpus fixtures (2026-11-15, T069, internal/fixgen/mix.go):
  `fixgen.ImportMix`/`fixgen.QueryMix` + `genfixture -agent mix` emit the
  two named fixtures; stats and commands live in docs/performance.md
  ("Benchmark corpus fixtures"). Facts worth keeping:
  - Observed corpus re-measured 2026-11-15: claude-code 1,042 main files
    (1.29 GiB, mean 1.27 MiB) plus 1,280 subagent files (0.59 GiB, mean
    480 KiB); codex 2,495 rollouts (2.93 GiB, mean 1.17 MiB); pi 74 files
    (2.1 MiB). The 2026-11-13 NOTES numbers (895/2,646) counted differently;
    the mix anchors use the fresh file-level counts.
  - Codex thread count (1,600 at scale 1) is derived from bytes, not
    observed: the generator omits resumed pages, so threads = observed
    bytes / (main + mean two quarter-size subagent rollouts). Real threads
    ≈ 511 spanning ~4.9 files each; the fixture keeps bytes and file sizes,
    not the page fan-out.
  - Records map 1:1 to events in all three formats (structural: every
    adapter makes one event per parsed record), so `fixgen.MeasureMix`
    counts events through the raw parsers — importing internal/events from
    fixgen would create a test import cycle (events' in-package bench tests
    import fixgen), and fixgen now imports only the parser packages.
  - The query fixture answers the perf plan's "one million events without
    a 3 GB input": compact records (text ÷8, session bytes ÷4) land at
    765–889 B mean — the JSON envelope (usage objects, per-block metadata)
    is the floor — giving 969,004 events in 746.4 MiB (4.4x per event).
    Full-record 1M events would be ~3.5 GB.
  - Determinism is pinned by SHA-256 hash tests (mix + all three single
    agents, `TestAgentCorpusStableHash`); the single-agent hashes were
    captured before the Compact knob landed, so they also prove Compact
    leaves default (div=1) bytes identical — repin hashes and the
    docs/performance.md tables together if generator output ever changes.
  - `TestNamedFixtureStats` regenerates both fixtures at full scale only
    under `OHYA_MIX_VERIFY=1` (about 1 GiB RAM, 22 s); the default check
    stays small. genfixture's plain build needs no sqlite_fts5 tag.

- Shared device transport (2026-11-15, ADR 0006): `join` and remote MCP
  must use `tailcat.NewSharedDevice`. Independent nodes with the same state
  stall uploads. The shared adapter owns the node under `owner.lock` and
  publishes an authenticated loopback proxy in owner-only `proxy.json`.
  Never delete the lock file. Stop older binaries before upgrading: they
  do not acquire it. Keep device state on a local filesystem. The regression
  test covers live appends with two MCP clients and transfer after owner death.

- Workload matrix baselines (2026-11-15, T070, docs/performance.md "Workload
  matrix baselines"): all four plan workloads measured on the direct
  local-tailnet path with 1,000-sample doubled runs; raw logs under
  `perf/t070/` (gitignored). Headlines: steady freshness p50 1.06-1.08 s /
  p99 2.06-2.07 s (the 2 s upload pass spends the budget); a 1.5 MB/s
  backlog on a second source doubles it (p99 4.2-4.4 s); three devices
  draining 570 MB barely move it (p99 2.5-2.7 s) while loaded wide FTS p95
  is 110-111 ms at 168k events; initial import 137 MiB = 24.6-447 s
  (fsync-bound swing, CPU constant 10-15 s); FTS-wide p95 at 969k events
  is 193 ms (fails the proposed 100 ms budget; compact fixture matches ~1/8
  of events, a full-size 1M hub lands ~1.5 s); hub idle RSS 65-66 MiB; peak
  WAL 27-32 MiB under sustained mixed read/write — well above the 4-6 MiB
  import-only figure, plan headroom accordingly. Guardrail verdict table in
  the doc.
- Hub-restart recovery regression, found by the T070 matrix (2026-11-15,
  follow-up key `hub-restart-reconnect`), fixed same day (T075): when the hub
  process stopped and restarted, a device whose sync connection was
  established never re-uploaded (dials timed out at the 60 s deadline for
  3+ minutes, 5/5). Diagnosis: after the coordination plane restarts inside
  the hub process, the long-lived device node's control connections sit dead
  in CLOSE-WAIT and its control client never re-polls, so the cached peer
  endpoints never pick up the restarted hub; tailscale.com v1.104.4 does not
  recover by itself. Fix (internal/transport/tailcat, ADR 0006): the shared
  device's owner treats "dial failed while the coordination plane still
  answers an HTTP request" as stale node state — it closes the node, starts
  the same durable identity again, republishes the proxy descriptor, and
  retries once; the direct dial also carries its own 20 s deadline so the
  rebuild fits inside the uploader's 60 s dial budget. A dial failure with an
  unreachable plane keeps the node (ordinary downtime backoff).
  `TestHubRestartReconnect` (two stop/restart cycles, established join)
  recovers in ~31 s per cycle; a post-fix workload-4 run recovers in 36.2 s
  with the full 191-source fixture and stays idempotent; unit tests pin the
  rebuild decision. Open: the
  intermittent hub-side `panic: ipnlocal: watchdog timeout` (tailscale.com
  v1.104.4, 1 of 3 manual restarts during T070) was not reproduced across 10
  post-fix stop/restart cycles (watch the serve stderr in the restart tests if it
  returns; the panic is an ipnlocal deadlock probe, i.e. some watched lock
  hung >30 s).
- T070 harness facts worth keeping: the sql tool returns JSON numbers, which
  decode as float64 — `fmt.Sprint` renders ids ≥ 1e6 as `1.012834e+06`, so
  any harness that parses ids from tool output must `CAST(id AS TEXT)` in
  the query (the bug poisoned every later poll and looked like a hub
  outage). A fresh device node's first dial to the hub costs ~20-25 s
  (tsnet path discovery), so the multi-device workload needs
  `OHYA_FRESH_PAIR_LEAD` before appends start. `fixgen` corpora size at
  sessions × ~3 MiB (main 2 MiB + two quarter-size subagents), so
  `OHYA_FRESH_DEVICE_MIB=192` yields ~285 MB per extra device.
- Ingestion stage attribution (2026-11-15, T071): numbers and method in
  docs/performance.md "Ingestion stage attribution"; raw evidence under
  `perf/t071/` (gitignored). Durable findings:
  - 65–86% of import wall is durable-sync wait (WAL, `synchronous=FULL`):
    `PRAGMA synchronous=OFF` in a throwaway DB collapses a 320 MiB import
    from 47–92 s to 12.7–13.4 s. Commit-only wait dominates; checkpoint
    share 10–17%; FTS trigger maintenance 24–36% at 1x and 14% at 10x
    (measured with the trigger dropped, so the number includes the sync
    volume it induces; the index is 6.6–8.5% of DB size). The rate band is
    flat 32→320 MiB in every agent format — nothing superlinear.
  - Register commits are a separate, measurable stage: one fsync per source
    (43 ms each at 439 sources, 19 s total; 0.04 s with syncs off). The
    harness counts register and first batch as two commits (two
    transactions).
  - One commit is NOT one fsync: strace counted ~1.4 WAL fsyncs per commit
    (each auto-checkpoint adds ~2 WAL + 1 main-db sync; 3,704 commits +
    2×731 checkpoints = 5,166 predicted, 5,154 measured at 10x). State
    durability (`internal/durablefile`: temp file + fsync + rename +
    directory fsync per acknowledged source) costs ~2 fsyncs per file —
    75.5 s, 19% of the 10x end-to-end wall. Device-side cursor durability,
    not hub cost; future import-speed work must weigh it.
  - Discovery is noise: a fully-acknowledged `ingest --once` pass costs
    0.02 s over 191 files, 0.09 s over 1,852.
  - Harness gotchas: `go test -bench '^BenchmarkIngestCorpus/32MiB$'` also
    runs ...Codex/Pi/Mix — the first path element matches as a prefix
    (verified 2026-11-15, Go 1.27.3); use the full sibling name in the
    filter or expect the set. Single 10x diagnostic triples can be voided
    by shared-disk swings (one claude 320 MiB triple went negative on
    checkpoint share); keep 1x repeats for coherent decomposition and name
    which 10x triple carries the numbers.
- tla2tools 1.8.0 release asset replaced upstream (2026-11-15): GitHub now
  serves a rebuild of the same tag. Both jars have an identical file
  listing and builder metadata (Apache Ant 1.10.12, Eclipse Adoptium
  17.0.21+9, Built-By runner); only the embedded Build-TimeStamp differs
  (old 2026-11-06T22:23:57Z, new 2026-11-15T00:21:46Z). The old sha256 pin
  in tla/run-tlc.sh therefore failed every fresh download. Repinned to
  7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f after
  the comparison and a green run of every model. Resolved 2026-11-15 (T077):
  the jar is now vendored at `tla/vendor/tla2tools-1.8.0.jar` and
  `tla/run-tlc.sh` verifies its checksum on every run without downloading
  (ADR 0007); no immutable artifact host exists for tla2tools (Maven Central
  has none, and GitHub asset URLs die on re-upload). Upstream rebuilding the
  tag again can no longer break a fresh run or CI.
- Perf budgets, Phase 1 selection, CI placement (2026-11-15, T072,
  docs/performance.md "Budgets, thresholds, and the Phase 1 selection"):
  freshness p95<2 s and idle RSS<150 MB kept (decision: if freshness needs
  margin, change the 2 s upload interval, never the target); time-to-memory
  and 1M-event query thresholds replaced (quiet-disk wall + unconditional
  CPU per 3 GiB; scoped per-pattern query budgets plus a 1.6 ms/1,000-match
  wide-BM25 rate); peak-memory/disk-growth/loaded-query budgets recorded
  with allowed variation. NO Phase 1 candidate selected — after
  re-scoping, no workload misses (wide-BM25 cost is per-match scoring;
  contended import misses are tenant fsync load, and register/checkpoint
  amortization recovers only ~a quarter) — so T073 closes empty per its
  contract; batch sizing is first in line if the thin 8% quiet-disk
  time-to-memory margin erodes. CI: the freshness smoke rides `make check`
  on check-linux (re-measured 50.4 s, p95 2.062 s at 100 samples — above
  2 s on a phase draw, why the smoke asserts no threshold); statistical
  runs stay off CI; no new job (the bench job is darwin, where the harness
  skips, and report-only); `make check` now passes `-timeout 20m` to go
  test because the cmd/ohya child-process suite measured 229 s reference
  and a 2–4x slower shared runner reaches the 10-minute per-binary default.
- T073 closed empty (2026-11-15): T072 selected no Phase 1 candidate, so per
  its own contract T073 changed no code and no experiment ran. Re-verified at
  the integrated HEAD: docs/performance.md "Phase 1 selection: none" (section
  landed in 61a14f6) records that after the threshold re-scoping no workload
  misses its target or budget and none of the plan's seven candidates meets
  its trigger — a candidate must dominate a workload that misses; make check
  green on the unchanged tree. A new miss reopens selection only through a
  fresh task with new evidence; batch sizing stays first in line. T074
  (Phase 2 accept-or-discard) therefore finds no T073 change to evaluate.
- T074 done (2026-11-15): Phase 2 closed with nothing to accept or discard
  (T072 selected no candidate; T073 changed no code). The matrix was still
  repeated at the integrated HEAD — same machine, fixtures, knobs, fresh
  seeds 2001–2006; raw logs in perf/t074/ (gitignored) and the task artifact
  dir; numbers and budget table in docs/performance.md "Phase 2 acceptance".
  Every workload landed inside the T070 spread and every T072 budget held;
  make check green. Notables worth carrying: steady p95 straddles 2 s again
  (1.971/2.014 s vs T070's 1.945/1.999 s) — the 2 s upload interval consumes
  the budget and a 1,000-sample draw moves p95 by more than the 1–55 ms
  margin, so do not read one side of the line as a regression; mixed
  read/write WAL peak read 33–65 MiB against the 27–32 MiB T070 basis (one
  run 1 MiB over the 64 MiB ceiling, inside the ±50% variation) — the
  high-water mark races auto-checkpoints against reader snapshots, so the
  T070 basis was a low draw; W4 hub-restart recovery 36.2 s in both runs
  (post-T075) with idempotent final counts. No successor candidate; the T072
  reopen triggers stand, and all four workloads must be repeated on the
  relay path after T065.
- Ingest-floor guard widened (2026-11-15, T078): TestIngestPerformanceFloor's
  wall ceiling moved 30 s → 120 s; the allowance and its anchors are in
  docs/performance.md "Regression guards". Anchors: one parallel-load
  failure at 31.2 s (full `go test ./...`, sibling SQLite packages on the
  shared reference disk) versus 1.24 s isolated; the worst tenant rate
  0.31 MiB/s makes the 24 MiB corpus about 77 s, so 120 s covers measured
  contention with margin while per-record durability (about 290 s at 6,794
  events) and superlinear regressions still fail loudly. CI serialization
  (`-p 1`) rejected: tenant load fails a serialized run too, and it slows
  every check. Reopen condition: a loaded run above 120 s → raise the
  ceiling with that evidence, like any other allowed variation.

- Pi fixture cost totals, FMA determinism, fixed 2026-11-15: the macOS CI
  check (first arm64 exposure of the fixgen hash pins) failed
  TestAgentCorpusStableHash and TestGenerateMixIsDeterministic while
  check-linux passed. Root cause, proven bit-for-bit: Go contracts
  `float64(in)*2e-7 + float64(out)*3e-7 + float64(cacheRead)*2e-8` into
  fused multiply-adds on arm64 (one rounding, leftmost multiply kept
  separate) while amd64 rounds each operation; json's shortest float
  formatting then emits different bytes. The pi usage total now uses an
  explicit `math.FMA` chain shaped exactly like the arm64 contraction —
  `math.FMA(cacheRead, 2e-8, math.FMA(out, 3e-7, in*2e-7))` — which the
  Go spec defines as the correctly-rounded fused result on every
  platform. The fixed bytes reproduce the darwin output verbatim
  (per-file ddaaa1bb…/121c5267…/8dc6a83f…); the mix and pi pins moved to
  the darwin-observed corpus hashes, claude-code and codex pins are
  unchanged, and only `total` cost fields differ from the old linux
  fixtures (a few characters in some records; corpus shape and event
  counts unchanged).

- Installer proposal (2026-11-15): `docs/installer-plan.md` records the proposed
  integrated installer: user services, agent MCP registration, verification,
  repair, and later Homebrew support. No installer exists yet. User requires
  one-command working setup, not a binary-only default. Native baselines, Apple
  signing, and Linux bootstrap
  signature policy remain open. Pi's current quickstart recommends a managed
  installer with pinned dependencies; do not describe it as npm-only.

- Installer client research (2026-11-15): current upstream Pi documents native
  MCP commands, but installed `pi mcp --help` prints general help with no MCP
  subcommands. Detect capabilities and test versions; the proposal includes an
  owned extension for supported older Pi. Claude user scope and Codex MCP add
  syntax were checked against installed help. Sources: installer plan.
