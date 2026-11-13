# ohya — Notes

<!--
ABOUTME: Working notes for ohya — frictions, papercuts, open questions, ideas.
ABOUTME: Read at the start of each task; update continuously; prune what is resolved.

How to use this file:
- Read it before starting any task; re-read findings that touch your work.
- Add a dated bullet whenever you hit friction, a papercut, an open question,
  an idea, or an unverified finding. One topic per bullet; keep it short.
- Mark uncertain claims as unverified and name what would confirm them.
- Remove or rewrite bullets once resolved or disproved — this is a live file,
  not an archive.
- Never put secrets, transcripts, or large dumps here; link to sources instead.
-->


- Verify agent session storage findings before phase 1 parser spikes (researched 2026-11-13, partly from third-party writeups — recheck against installed versions):
  - Claude Code: `~/.claude/projects/**/*.jsonl` — confirmed locally, append-only JSONL.
  - Codex: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` — confirmed locally; also `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog` = metadata only, no message bodies). Check `CODEX_HOME` overrides and possible compressed session files.
  - Pi agent: project-local `.pi/`, one JSONL per session (per writeups; not installed locally yet — verify format when spiking).
  - OpenCode (phase 2): `~/.local/share/opencode/storage/`, one JSON file per message/part — enumerated storage, not append-only; breaks the byte-cursor model.
  - Claude Desktop regular chats: server-side, not file-watchable; potentially pollable via unofficial authenticated API (see Claude Explorer / rpeck/claude-explorer) or official export. Would need a new poll-based source type — deferred.
  - ChatGPT desktop: no reliable local transcripts; official export only — deferred.
  - Cowork local sessions may write JSONL under `~/Library/Application Support/Claude/local-agent-mode-sessions/` — absent on current install; check if usage starts.
