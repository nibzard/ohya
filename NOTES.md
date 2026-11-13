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
  - Codex: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` — confirmed locally; also `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog` = metadata only, no message bodies). Check `CODEX_HOME` overrides and possible compressed session files.
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

- Write-path open question (2026-11-13, internal/store, T006):
  `sessions.repo` stays NULL — no rule derives a repository path from observed
  record fields yet (git root discovery from `cwd` is unspecified, and the
  Claude Code directory encoding is lossy). Define the derivation before any
  query relies on `repo`. Token aggregate policy is settled in DB.md; tool
  aggregates remain deferred pending a query need.

- Local slice wiring (2026-11-13, cmd/ohya, T007): the production
  claudecode.Record → store.Event adapter lives in cmd/ohya/adapter.go
  (usage field names, contentForm=="string" prompt test). internal/store
  keeps a mirrored copy in ingest_test.go to drive the write path from
  fixtures — change both together. The crash window between the Ingest
  commit and watch.Acknowledge closes by replay on restart (slice test kills
  a helper process there; replay validates stored bytes and inserts nothing).
  Ingestion batches are bounded at 8 MiB so importing a large history file
  stays predictable in memory; a batch never splits a record. A stopped
  (malformed) source prints its diagnostic once per stop offset per run,
  then stays quiet until the offset moves; fresh sources from replacement or
  truncation are reported because they re-import history, new files are not
  (a first import of thousands of files must not print a line per file).
