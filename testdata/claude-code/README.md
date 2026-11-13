# Claude Code session fixtures

Synthetic, hand-authored fixtures for the Claude Code session format. All
values are invented. The files contain no real transcripts.

Verified against Claude Code 2.1.280 on 2026-11-13. The corpus held 2,646
JSONL files under `~/.claude/projects/` (895 main sessions, about 1.9 GB).

## Verified format

### File layout

- `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl` — one main session
  file per session. The file name is the native session ID.
- `~/.claude/projects/<encoded-cwd>/<session-uuid>/subagents/agent-<hex>.jsonl`
  — subagent transcript. Records carry the parent session ID and
  `isSidechain: true`.
- `~/.claude/projects/<encoded-cwd>/<session-uuid>/subagents/workflows/wf_<id>/agent-<hex>.jsonl`
  — workflow agent transcripts, same shape as subagent files.
- `~/.claude/projects/<encoded-cwd>/<session-uuid>/tool-results/*.txt` —
  spilled tool output. Not JSONL; not a session source.
- `~/.claude/projects/<encoded-cwd>/memory/` — memory files. Not a session
  source.

The directory name encodes the working directory: `/`, `.`, and `_` each
become `-`. The encoding is lossy. Read `cwd` from the records; do not decode
the directory name.

### Framing

Append-only JSONL. One record per line. Every observed file ends with `\n`.
No session ID spans two main files. Malformed complete lines occur in the wild
(at least eight in the corpus). The lines after a malformed line can be valid.

### Record kinds

Message records carry `uuid`, `parentUuid`, `cwd`, `version`, `gitBranch`,
`timestamp`, `userType`, and `sessionId`:

- `user` — a prompt or a tool result. `message.content` is a string for
  prompts, or an array of `tool_result` blocks. Tool results also carry a
  top-level `toolUseResult` object and `sourceToolAssistantUUID`.
- `assistant` — one content block of one API response. See
  [Compound records](#compound-records).
- `attachment` — injected context in `attachment`. Observed types include
  `total_tokens_reminder`, `prompt_snapshot`, `environment`, `date`,
  `instructions`, `model`, `session_context`, `edited_text_file`, and
  `queued_command`.
- `system` — events. `subtype` values include `turn_duration`,
  `stop_hook_summary`, `api_error`, `away_summary`, `scheduled_task_fire`,
  and `compact_boundary`.

Bookkeeping records carry only `sessionId` and their own fields:

- `queue-operation`, `mode`, `permission-mode`, `atis-latch`, `last-prompt`,
  `ai-title`, `cost-state`, `agent-name`.
- The first line of a main file is usually `queue-operation` or `mode`. The
  last line is usually `cost-state`, which holds session totals such as
  `totalCostUSD` and per-model `modelUsage`.

Subagent files add `started`, `result`, `launched`, and `failed`. These carry
`agentId` and a `key`, not `sessionId`.

### Native session identity

- `sessionId` is a UUID and equals the main file name.
- `uuid` and `parentUuid` form a chain per file. The first record has
  `parentUuid: null`. Each record points at the record it answers or follows.
- Subagent records reuse the parent session ID, set `isSidechain: true`, and
  add `agentId`.
- A snake_case `session_id` field also appears on some records. It usually
  equals `sessionId` but not always. Use `sessionId`.

### Compound records

One assistant API response becomes several JSONL records, one per content
block. They share one `message.id` and one `usage` object. `apiBlockIndex`
orders the blocks. `tool_result` user records interleave between the blocks of
one response as tools finish. Block types seen: `thinking` (with
`signature`), `text`, `tool_use`, `server_tool_use`, `mcp_tool_use`, and
`mcp_tool_result`. Locally generated messages use model `<synthetic>`.

### Usage fields

`message.usage` reports one API request. It is not cumulative across the
session. Each record of one response repeats the same per-request values.
`input_tokens` resets per request. `cache_read_input_tokens` grows with the
conversation prefix. To total a session, group records by `message.id`, take
each `usage` once, then sum. Observed keys: `input_tokens`,
`cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`,
`cache_creation`, `service_tier`, `speed`, `iterations`, `inference_geo`,
`output_tokens_details`, and `server_tool_use`.

Timestamps are ISO 8601 UTC with milliseconds, for example
`2026-11-13T09:00:01.000Z`. They are mostly increasing but not strictly
monotonic. Do not sort by timestamp alone.

## Fixtures

Every fixture is synthetic. Each table lists the byte range of each record,
including the trailing `\n`, so tests can assert exact offsets. All records
except the last of `incomplete-tail.jsonl` are `\n`-terminated.

### main-session.jsonl

A main session. It covers one two-response turn: prompt, attachment, a
three-record assistant response (`thinking`, `text`, `tool_use` with one
`message.id` and one repeated `usage`), a tool result with `toolUseResult`,
then a single-record response, and closing bookkeeping records.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 186) | `queue-operation` | First line, as in real files |
| 2 | [186, 269) | `mode` | |
| 3 | [269, 673) | `user` | String prompt; `parentUuid` null |
| 4 | [673, 1086) | `attachment` | `prompt_snapshot` |
| 5 | [1086, 1847) | `assistant` | `apiBlockIndex` 0, `thinking`, `msg_fixture_0001` |
| 6 | [1847, 2565) | `assistant` | `apiBlockIndex` 1, `text`, same ID and usage |
| 7 | [2565, 3347) | `assistant` | `apiBlockIndex` 2, `tool_use` `Bash`, same ID and usage |
| 8 | [3347, 3995) | `user` | `tool_result` array plus `toolUseResult` |
| 9 | [3995, 4410) | `attachment` | `total_tokens_reminder` |
| 10 | [4410, 4848) | `system` | `turn_duration`, level `info` |
| 11 | [4848, 5557) | `assistant` | Second response, `msg_fixture_0002`, new usage |
| 12 | [5557, 5663) | `atis-latch` | |
| 13 | [5663, 5768) | `permission-mode` | `default` |
| 14 | [5768, 5940) | `last-prompt` | Points at line 11 by `leafUuid` |
| 15 | [5940, 6044) | `ai-title` | |
| 16 | [6044, 6528) | `cost-state` | Session totals; last line, 6,528 bytes total |

### subagent-session.jsonl

A subagent transcript. All message records set `isSidechain: true` and
`agentId`, and reuse the parent `sessionId` (`00000000-0000-4000-8000-0000000000a1`
from `main-session.jsonl`). It covers `started`, a two-record response, a
`tool_result` with a `Read`-style `toolUseResult`, and `result`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 139) | `started` | Subagent bookkeeping |
| 2 | [139, 555) | `user` | String prompt; `parentUuid` null |
| 3 | [555, 1343) | `assistant` | `tool_use` `Read`, `msg_fixture_0003` |
| 4 | [1343, 2067) | `user` | `tool_result` plus `toolUseResult` |
| 5 | [2067, 2789) | `assistant` | `text`, same ID and usage as line 3 |
| 6 | [2789, 2956) | `result` | Subagent bookkeeping; 2,956 bytes total |

### incomplete-tail.jsonl

A crash mid-write. The first three records are complete. The fourth record
has no trailing `\n` and its JSON string is cut. A parser must retain bytes
from offset 1,545 for the next poll and stop the cursor at 1,545. The file is
1,997 bytes and does not end with `\n`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 414) | `user` | Complete |
| 2 | [414, 1109) | `assistant` | Complete |
| 3 | [1109, 1545) | `user` | Complete; cursor stops here |
| 4 | [1545, 1997) | partial | No trailing `\n`; invalid JSON |

### malformed-line.jsonl

A complete but invalid record mid-file, as observed in real data. Line 3 ends
with `\n` but its JSON breaks inside `message.content` (an unescaped quote).
Lines before and after are valid. A source must stop at offset 1,094 with a
diagnostic and must not skip bytes.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 405) | `user` | Complete |
| 2 | [405, 1094) | `assistant` | Complete |
| 3 | [1094, 1507) | invalid | `\n`-terminated, invalid JSON |
| 4 | [1507, 1932) | `user` | Valid; unreached without recovery |

## Limits

These fixtures cover the common paths, not every field. Real records carry
more optional fields (`effort`, `requestId`, `isMeta`, `isCompactSummary`,
`session_id`, hook summaries, and others). Parsers must tolerate unknown
fields and unknown record kinds, and store unknown valid records with kind
`unknown`.
