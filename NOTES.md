# ohya — Notes

- Verify agent session storage findings before phase 1 parser spikes (researched 2026-11-13, partly from third-party writeups — recheck against installed versions):
  - Claude Code: `~/.claude/projects/**/*.jsonl` — confirmed locally, append-only JSONL.
  - Codex: `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` — confirmed locally; also `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog` = metadata only, no message bodies). Check `CODEX_HOME` overrides and possible compressed session files.
  - Pi agent: project-local `.pi/`, one JSONL per session (per writeups; not installed locally yet — verify format when spiking).
  - OpenCode (phase 2): `~/.local/share/opencode/storage/`, one JSON file per message/part — enumerated storage, not append-only; breaks the byte-cursor model.
  - Claude Desktop regular chats: server-side, not file-watchable; potentially pollable via unofficial authenticated API (see Claude Explorer / rpeck/claude-explorer) or official export. Would need a new poll-based source type — deferred.
  - ChatGPT desktop: no reliable local transcripts; official export only — deferred.
  - Cowork local sessions may write JSONL under `~/Library/Application Support/Claude/local-agent-mode-sessions/` — absent on current install; check if usage starts.
