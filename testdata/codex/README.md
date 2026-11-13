# Codex session fixtures

Synthetic, hand-authored fixtures for the OpenAI Codex CLI session format. All
values are invented. The files contain no real transcripts.

Verified against codex-cli 0.166.0 on 2026-11-13 (the corpus also contains
files written by 0.104 through 0.167; the envelope is unchanged). The corpus
held 2,488 files under `~/.codex/sessions/` (3.0 GB), from 2026-02-20 onward.

## Verified format

### File layout

- `~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl` — one rollout
  file. The date directories come from the local session-start time. Every
  file in the corpus matches this pattern; there are no other files under
  `sessions/`.
- Resumed or paginated threads add a second UUID:
  `rollout-<timestamp>-<thread-id>_<resume-id>.jsonl`. The second UUID never
  appears inside the file. See [Native session
  identity](#native-session-identity).
- No compressed session files exist anywhere under `~/.codex`: all 2,488
  session files are plain JSONL (the only `.gz` files are plugin assets).
- `CODEX_HOME` overrides the root. Verified with codex-cli 0.166.0: with
  `CODEX_HOME=<dir>` set, Codex created `<dir>/tmp/` and read `auth.json` from
  there, and never touched `~/.codex`. Ingestion must honor `CODEX_HOME`.
- Other content under `~/.codex` is not a session source: `history.jsonl`
  (user prompt history, duplicates session text), `session_index.jsonl`
  (thread id, name, updated time), `attachments/`, `log/`, and several SQLite
  databases (below).

### Framing

Append-only JSONL. One record per line. The envelope is
`{"timestamp", "type", "payload"}`; records from newer versions add an
`ordinal` (a per-thread counter, see below). Timestamps are ISO 8601 UTC with
milliseconds, for example `2026-11-13T09:00:01.000Z`. Files almost always end
with `\n`; one file in a 150-file sample did not (a crash mid-write).
Malformed complete lines also occur (two in the same sample). Both are
reproduced in fixtures below.

### Record kinds

Top-level `type` values observed:

- `session_meta` — always the first line. The payload carries `id`,
  `timestamp`, `cwd`, `originator` (`codex-tui`, `codex_exec`,
  `codex_cli_rs`, `symphony-orchestrator`), `cli_version`, `source` (`cli`,
  `exec`, `vscode`, or a `subagent` object), `model_provider`, and often
  `base_instructions`, `git` (`commit_hash`, `branch`, `repository_url`),
  `history_mode` (`legacy` or `paginated`), `runtime_workspace_roots`, and
  account ids.
- `response_item` — the model transcript. Payload types: `message` (roles
  `developer`, `user`, `assistant`; `content` is an array of
  `input_text`/`output_text` blocks), `reasoning` (often only
  `encrypted_content`), `function_call` (`name`, JSON string `arguments`),
  `function_call_output`, `custom_tool_call` (`name`, string `input`),
  `custom_tool_call_output`, `web_search_call`, `agent_message`, and rare
  tool-search variants. Assistant messages carry `status`, `phase`
  (`commentary` or `final`), and an `msg_…` id.
- `event_msg` — UI events that mirror the transcript: `user_message` and
  `agent_message` repeat text that also appears in `response_item` records,
  so a parser must pick one stream (prefer `response_item`) or dedupe.
  Others carry detail the transcript lacks: `token_count` (rate limits),
  `exec_command_end` (aggregated output, exit code), `patch_apply_end`
  (file changes), `item_completed`, `sub_agent_activity`, `task_started`,
  `task_complete`, `agent_reasoning`, `context_compacted`,
  `turn_aborted`, `thread_rolled_back`, `web_search_end`,
  `mcp_tool_call_end`, `thread_settings_applied`, `image_generation_end`,
  `thread_goal_updated`.
- `turn_context` — per-turn settings: `turn_id`, `root_turn_id`, `cwd`,
  `approval_policy`, `sandbox_policy`, `model`, `effort`, `summary`.
- `token_usage_record` — usage per API response (below).
- `world_state` — a large harness snapshot (instructions, skill and tool
  hashes, environment). No conversation turns.
- `compacted` — context compaction boundary with a `replacement_history`
  array and window ids.
- `inter_agent_communication_metadata` — collaboration metadata only.

### Native session identity

- `session_meta.payload.id` is the thread id and equals the first UUID in
  the file name (checked on all sampled files). It is the native session id.
- One thread spans multiple files. When a thread resumes or pages, Codex
  writes a new file whose `session_meta.payload.id` is still the original
  thread id; `history_base` records `{thread_id, end_ordinal_exclusive,
  end_byte_offset}` and `history_mode` is `paginated`. Verified pair in the
  corpus: the original file holds ordinals 0–984 and its page 978–1019 —
  the page replays some tail records. Deduplicate across files by
  `(thread id, ordinal)`, never by byte offset alone.
- `ordinal` orders records within a thread, including across pages. Older
  files (before about 0.15x) carry no `ordinal`.
- `session_meta.payload.session_id` is the root thread and can differ from
  `id` when the file is itself a resume. Subagent records' usage carries
  `thread_id` (own) and `session_id` (root).
- Subagent rollouts open with their own `session_meta` (with
  `source.subagent.thread_spawn.parent_thread_id`, `depth`, `agent_path`,
  `agent_nickname`, and `parent_thread_id`/`forked_from_id`) followed by the
  parent's `session_meta` as the second line. Two `session_meta` records in
  one file can therefore be legitimate.

### Usage fields

Each `token_usage_record` covers one API response (`response_id`):
`usage` is per response and not cumulative. `turn_token_usage` and
`thread_token_usage` on the same record are running totals for the turn and
the thread. To total a session, sum `usage` over records; never sum the
cumulative fields twice. Keys: `input_tokens`, `cached_input_tokens`,
`cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens`,
`total_tokens`.

### SQLite databases under CODEX_HOME

The older layout `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog`) does
not exist on 0.166.0. Its successors are schema-numbered files directly under
the Codex home, and the "metadata only" claim does not hold for all of them:

- `thread_history_1.sqlite` — `thread_items`, `thread_turns`,
  `thread_realtime_items`. `item_json` holds full message and command bodies
  (`userMessage` rows average 2.2 KB, maximum 450 KB). It is a projection
  cache, not the source of truth: 511 threads were projected versus 2,488
  rollout files, and `thread_history_projection_state.next_rollout_byte_offset`
  points back into the rollout files. Ingest rollout files, never this cache.
- `state_5.sqlite` — thread metadata (read receipts, attachments, dynamic
  tools); its `rollout_migration_*` tables were empty on this install.
- `logs_2.sqlite`, `goals_1.sqlite`, `memories_1.sqlite`, `queue_1.sqlite` —
  app logs and feature state, not transcripts.

## Fixtures

Every fixture is synthetic. Each table lists the byte range of each record,
including the trailing `\n`, so tests can assert exact offsets. All records
except the last of `incomplete-tail.jsonl` are `\n`-terminated.

### main-session.jsonl

A main session: meta with `git`, a developer message, a user prompt mirrored
by an `event_msg`, `turn_context`, a reasoning record, a shell
`function_call` pair with its `exec_command_end`, an assistant message
mirrored by `agent_message`, two `token_usage_record`s in one turn (the
second shows per-response `usage` versus growing `turn_token_usage`), and
`task_complete`. The thread id is
`00000000-0000-7000-8000-0000000000c1`, ordinals 0–13.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 422) | `session_meta` | `git.branch` `main`, `originator` `codex-tui` |
| 2 | [422, 718) | `response_item/message` | Role `developer`, permissions |
| 3 | [718, 939) | `response_item/message` | Role `user`, the prompt |
| 4 | [939, 1141) | `event_msg/user_message` | Mirrors line 3 text |
| 5 | [1141, 1478) | `turn_context` | Model `gpt-5-codex` |
| 6 | [1478, 1678) | `response_item/reasoning` | `encrypted_content` |
| 7 | [1678, 1947) | `response_item/function_call` | `shell`, JSON arguments |
| 8 | [1947, 2190) | `response_item/function_call_output` | Answers `call_fixture_0001` |
| 9 | [2190, 2542) | `event_msg/exec_command_end` | Exit code 0 |
| 10 | [2542, 2890) | `response_item/message` | Role `assistant`, phase `commentary` |
| 11 | [2890, 3137) | `event_msg/agent_message` | Mirrors line 10 text |
| 12 | [3137, 3848) | `token_usage_record` | `resp_fixture_0001`, first response |
| 13 | [3848, 4561) | `token_usage_record` | `resp_fixture_0002`; `usage` resets, turn totals grow |
| 14 | [4561, 4780) | `event_msg/task_complete` | Last line; 4,780 bytes total |

### subagent-session.jsonl

A subagent rollout. Line 1 is the subagent's own meta with
`source.subagent.thread_spawn` pointing at the main fixture's thread; line 2
embeds the parent's `session_meta`, as observed in real files. The usage
record sets `thread_id` to the subagent and `session_id` to the root thread.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 565) | `session_meta` | Subagent `Finch`, depth 1, parent `…0c1` |
| 2 | [565, 987) | `session_meta` | Parent's meta, embedded |
| 3 | [987, 1234) | `response_item/message` | Role `user`, the task |
| 4 | [1234, 1555) | `response_item/message` | Role `assistant`, phase `final` |
| 5 | [1555, 2362) | `token_usage_record` | `thread_id` `…0d1`, `session_id` `…0c1`; 2,362 bytes total |

### resumed-page.jsonl

A continuation page for `main-session.jsonl`'s thread: the same
`payload.id` (`…0c1`), `history_mode` `paginated`, and `history_base` with
`end_ordinal_exclusive` 12 and `end_byte_offset` 3,848 — the byte offset
where `main-session.jsonl`'s ordinal 12 starts. Lines 2 and 3 replay
ordinals 12 and 13 from the original file, then new records continue at
ordinal 14. A parser must group both files into one native session and
deduplicate by `(thread id, ordinal)`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 568) | `session_meta` | Same id as main; `history_base` present |
| 2 | [568, 1281) | `token_usage_record` | Replay of main line 13 (ordinal 12) |
| 3 | [1281, 1500) | `event_msg/task_complete` | Replay of main line 14 (ordinal 13) |
| 4 | [1500, 1726) | `response_item/message` | Role `user`, new prompt (ordinal 14) |
| 5 | [1726, 2048) | `response_item/message` | Role `assistant` (ordinal 15); 2,048 bytes total |

### incomplete-tail.jsonl

A crash mid-write. The first three records are complete. The fourth record
has no trailing `\n` and its JSON string is cut. A parser must retain bytes
from offset 797 for the next poll and stop the cursor at 797. The file is
942 bytes and does not end with `\n`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 288) | `session_meta` | `originator` `codex_exec`, `source` `exec` |
| 2 | [288, 503) | `response_item/message` | Role `user` |
| 3 | [503, 797) | `response_item/message` | Role `assistant`; cursor stops here |
| 4 | [797, 942) | partial | No trailing `\n`; JSON cut mid-string |

### malformed-line.jsonl

A complete but invalid record mid-file, as observed in real data. Line 3
ends with `\n` but its JSON breaks inside `payload.message` (an unescaped
quote). Lines before and after are valid. A source must stop at offset 502
with a diagnostic and must not skip bytes.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 286) | `session_meta` | |
| 2 | [286, 502) | `response_item/message` | Role `user` |
| 3 | [502, 681) | invalid | `\n`-terminated, invalid JSON |
| 4 | [681, 1007) | `response_item/message` | Valid; unreached without recovery |

## Limits

These fixtures cover the common paths, not every field. Real records carry
more optional fields (`creator_user_id`, `context_window`, `dynamic_tools`,
`runtime_workspace_roots`, `internal_chat_message_metadata_passthrough`,
`world_state` snapshots, and others). Parsers must tolerate unknown fields
and unknown record kinds, and store unknown valid records with kind
`unknown`. Files written by versions before 0.147.0 carry no `ordinal` and
rely on file order alone.
