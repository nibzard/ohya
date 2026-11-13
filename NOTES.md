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

- macOS distribution still needs native builds on both supported architectures and
  signing/notarization verification; the SQLite driver requires a C toolchain.
