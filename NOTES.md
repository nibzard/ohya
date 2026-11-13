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
  - Pi agent: project-local `.pi/`, one JSONL per session (per writeups; not installed locally yet — verify format when spiking).
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
