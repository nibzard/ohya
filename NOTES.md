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


- Agent session storage — verified on this machine 2026-11-13; local corpus exists for parser spikes:
  - Claude Code: `~/.claude/projects/<dir-slug>/<uuid>.jsonl`, append-only JSONL
    with typed records (`type` field; not every line is a message). 2,642
    session files locally, including `-home-agent-ohya` (this repo's own
    sessions — convenient test corpus). Also `~/.claude/history.jsonl`
    (global prompt history).
  - Codex (codex-cli 0.166.0, `CODEX_HOME` unset): `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`,
    2,488 files, no compressed sessions. No `~/.codex/sqlite/` on this
    machine; instead `~/.codex/logs_2.sqlite`, `goals_1.sqlite`,
    `memories_1.sqlite`. The earlier `codex-dev.db` / `local_thread_catalog`
    claim is version-dependent — treat rollout JSONL as the corpus.
  - Pi (pi 0.84.2): sessions are global, not project-local —
    `~/.pi/agent/sessions/<cwd-slug>/<ISO-ts>_<uuid>.jsonl` (74 files),
    version-3 typed JSONL with an `id`/`parentId` event chain. Project-local
    `.pi/` holds config only (extensions, prompts, skills).
  - OpenCode v2.0.12 (phase 2): storage moved to SQLite —
    `~/.local/share/opencode/opencode.db` with WAL (tables `session`,
    `message`, `part`, `event`, and so on). The old one-JSON-per-message
    layout no longer applies; needs a poll-based SQLite source, not a byte
    cursor.
  - Deferred: Claude Desktop regular chats (server-side; unofficial
    authenticated API via Claude Explorer / rpeck/claude-explorer, or
    official export), ChatGPT desktop (export only), Cowork local sessions
    (macOS path `~/Library/Application Support/Claude/local-agent-mode-sessions/`;
    not observable on Linux).

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
  - Codex: rollouts older than about one week are zstd-compressed to
    `.jsonl.zst` in place (none locally yet; plan a streaming decompressing
    reader or an explicit skip with a diagnostic). `session_index.jsonl` is a
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

- Open decision, malformed complete lines: ARCHITECTURE.md stops the source
  at the offending offset; agentsview skips the line, counts it, and surfaces
  the count. Real archives contain junk lines, and a stopped source blocks all
  later events for that session. Collect evidence during the Claude Code
  spike, then confirm or revise the stop-at-offset rule.

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
