# Antigravity (`agy`) transcript fixtures

Synthetic fixtures for `internal/antigravity`. Format and semantics were verified
against installed Antigravity CLI (`agy`) 1.5.14 (40 conversations, 5,235 lines).

## Layout and discovery

- Root directory: `~/.gemini/antigravity-cli/brain` (or `$AGY_DATA_DIR/brain`).
- Transcript file: `<brain>/<conversation-id>/.system_generated/logs/transcript_full.jsonl`.
- Discovery matches only `transcript_full.jsonl` and skips the truncated sibling
  `transcript.jsonl`, `chunks/`, `steps/`, `scratch/`, `.user_uploaded/`, and
  `messages/`.
- Files are append-only JSONL with monotonically increasing `step_index` and
  RFC 3339 UTC `created_at` timestamps.
- Conversation index: `<parent of the brain root>/conversation_summaries.db`,
  table `conversation_summaries(conversation_id PRIMARY KEY, workspace_uris)`,
  where `workspace_uris` is a JSON list of `file://` URIs. ohya reads it with
  `immutable=1` to learn the workspace of a conversation, which becomes the
  `cwd` of a session when its records carry none. The parser itself reads no
  file ([ADR 0018](../../docs/adr/0018-device-observed-source-workspace.md)).

## Normalization rules

- Native session ID: `<conversation-id>` derived from the source path
  (`Options.SourcePath`), falling back to `ohya-synthetic:<SourceScope>` when
  no path pattern matches. The same ID is the `conversation_id` key of the
  conversation index.
- `USER_INPUT` (`role=user`): inner text of `<USER_REQUEST>...</USER_REQUEST>`
  sets both `text` and `prompt` (`sessions.first_prompt`), trimming the trailing
  `<ADDITIONAL_METADATA>` envelope.
- `PLANNER_RESPONSE` (`role=assistant`): indexes `content` and `tool_calls`
  (`name + compact JSON args`); omits `thinking` from search text while keeping
  `raw_json` lossless. `tool_name` is set when exactly one tool call is present.
- `GENERIC` (`role=tool`): indexes `content` and records any `media` MIME types
  in `metadata`.
- `SYSTEM_MESSAGE` (`role=system`): preamble contributes no search text; extracts
  `cwd` from `Command Working Directory:` when present.
- `CHECKPOINT` (`role=system`): indexes compaction summary `content`.
- `ERROR_MESSAGE` (`role=system`): indexes `content` (falling back to `error`).
- Usage: `input_tokens`, `cache_read_tokens`, and `output_tokens` are additive
  (verified across 1,425 non-zero records where `cache_read_tokens > input_tokens`
  in 1,374 cases) and keyed by `step-<step_index>`.
