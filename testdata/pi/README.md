# Pi agent session fixtures

Synthetic, hand-authored fixtures for the Pi agent (`@earendil-works/pi-coding-agent`)
session format. All values are invented. The files contain no real
transcripts.

Verified on 2026-11-13 against installed Pi 0.84.2 and Pi 1.5.2 (run from a
scratch npm prefix), using real sessions captured outside the repository: a
successful run and two failed first model calls (provider 401, deprecated
model 404) on 0.84.2, and a successful run on 1.5.2. Usage semantics were
re-verified on 2026-11-14 against the 74 real session files under
`~/.pi/agent/sessions` (524 assistant usage records: 517 zai via
`openai-completions`, 7 anthropic via `anthropic-messages`). The captured
files and the on-disk layout snapshot are the T012 evidence; these fixtures
reproduce their shapes with invented values.

## Verified format

### File layout

- Sessions are **global**, one append-only JSONL file per session, at
  `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<session-uuid>.jsonl`. The
  third-party writeup claim of project-local `.pi/` session storage is wrong;
  `.pi/` inside a project may hold config or packages, but no session writes
  there were observed.
- `<encoded-cwd>` = the session cwd minus one leading `/`, with every `/`,
  `\` and `:` replaced by `-`, wrapped as `--…--` (source:
  `dist/core/session-manager.js`, identical in 0.84.2 and 1.5.2). The
  encoding is lossy — `/a/b` and `/a-b` collide — so never decode it. The
  session header's `cwd` is authoritative.
- The file name is the session-start `toISOString()` with `:` and `.`
  replaced by `-`, then `_` and the session UUID (time-ordered, `01a1…`).
- The agent dir is overridable by `PI_CODING_AGENT_DIR`; the session dir by
  `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` > the `sessionDir` setting.
  `--no-session` is ephemeral and writes nothing. `PI_CODING_AGENT_SESSION_DIR`
  replaces the whole per-cwd session directory, so sessions then sit directly
  in it, without the `--encoded-cwd--` level.
- Other files in the agent dir (`auth.json` with credentials, `settings.json`,
  `models.json`, `trust.json`, `bin/`) are never session sources. Ingest
  `*.jsonl` under the sessions root only.

### Framing

Append-only JSONL, one entry per line, `\n`-terminated. Entries flush
incrementally during a run: a first model call that fails still persists the
header, `model_change`, `thinking_level_change`, the user message and an
assistant message with `stopReason: "error"` and zero usage — partial files
from failed runs are normal, not corruption. Session format v3.

### Record kinds

Line 0 is the `session` header: `{"type":"session","version":3,"id","timestamp","cwd"}`.
Every other entry carries `type`, `id` (8-hex entry id), `parentId` (entry
tree; `null` = root) and an ISO 8601 `timestamp`. The entry `timestamp` is
the parse time; the message-level `timestamp` (Unix milliseconds) mirrors it
and is not parsed.

- `session` — the header. `id` is the native session id and equals the UUID
  in the file name; `cwd` is the working directory. No other entry names the
  session, so a parse past offset 0 needs the header's id from elsewhere
  (the committed first record).
- `model_change` — `provider`, `modelId`.
- `thinking_level_change` — `thinkingLevel`.
- `message` — the transcript, under a `message` object with a `role`:
  - `system` (1.5.2 only): empty string `content`, `sections`
    (preamble/tools/rules/docs/skills — the harness preamble), `toolsAdded`
    (tool name/description/parameters list), `timestamp`. Not conversation.
  - `user`: `content` array of `text` blocks (typed input; Pi persists no
    injected context as user messages — the preamble is the `system` entry).
  - `assistant`: `content` array of `text` blocks (with an optional
    `textSignature` — a signature, not content) and `toolCall` blocks
    (`id`, `name`, `arguments` as a JSON object). Also `provider`, `model`,
    `api` (for example `openai-responses`), one usage object, `stopReason`
    (`toolUse`, `stop`, `error`), `rawStopReason`, `responseId`, and on
    error `errorMessage`; 1.5.2 adds `durationMs` and `thinkingLevel`.
  - `toolResult`: `toolCallId`, `toolName`, `content` array of `text`
    blocks, `isError`, and (1.5.2) `durationMs`.

### Native session identity

The `session` header at offset 0 names the session; every later entry of the
file belongs to it. One session never spans files (unlike Codex threads), and
no entry repeats the session id. The `<encoded-cwd>` directory name is not
identity — it is lossy.

### Usage fields

Each assistant message carries exactly one usage object for its API response
(`input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`,
plus a `cost` object): per response, never repeated across records, so token
totals sum directly over assistant messages — no Claude-Code-style
`message.id` dedupe. In every verified record,
`input + cacheRead + cacheWrite + output = totalTokens`, so `input`
**excludes** both cache reads and cache writes (unlike Codex, where
`input_tokens` includes the cached part) and the counts map without
subtraction. `cacheWrite` is non-zero in 7 of the 524 verified real records
(all `anthropic-messages`), and in each of them the sum without `cacheWrite`
fails, so cache writes are an additive part of `totalTokens` — they map to
the cache-creation count. `reasoning` has no non-zero sample (0 in the
captured `openai-responses` records, absent from every real-corpus usage
object) and `cacheWrite1h` (present on 3 `claude-opus-4-8` records, the
1-hour cache write) is 0 everywhere, so both subset relations stay
unverified and both stay unmapped. A failed model call reports all-zero
usage with no `responseId`.

`responseId` presence depends on the API: `openai-responses` responses carry
one (`resp_…`), `anthropic-messages` responses carry one on newer runs
(`msg_…`, 3 of 7 records; the older 2026-04-20 `claude-opus-4-6` runs carry
none), and `openai-completions` records never do — all 517 zai records,
including 503 successful responses with non-zero usage. Because each entry
carries a unique 8-hex `id` and one assistant entry is one response, the
usage-row key is the `responseId`, falling back to the entry id.

## Fixtures

Every fixture is synthetic. Each table lists the byte range of each record,
including the trailing `\n`, so tests can assert exact offsets.

### main-session.jsonl

A successful session in the 1.5.2 shape: header, model and thinking-level
changes, the `system` preamble entry (trimmed), the user prompt, an assistant
toolCall response, its toolResult, and the final assistant text response with
cache reads. The 0.84.2 shape is the same file without the `system` entry and
the `durationMs`/`thinkingLevel` fields, which the parser tolerates either
way. Session id `00000000-0000-7000-8000-0000000000b1`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 143) | `session` | cwd `/home/dev/pi-project` |
| 2 | [143, 283) | `model_change` | `openai` / `gpt-5.4-nano` |
| 3 | [283, 415) | `thinking_level_change` | `off` |
| 4 | [415, 869) | `message`/system | `sections`, `toolsAdded`; no search text |
| 5 | [869, 1194) | `message`/user | The prompt |
| 6 | [1194, 1893) | `message`/assistant | `bash` toolCall, `resp_fixture_0001`, usage 1840/24/0 = 1864 |
| 7 | [1893, 2239) | `message`/toolResult | `main.py\nnotes.txt\n` |
| 8 | [2239, 2953) | `message`/assistant | Final text, `resp_fixture_0002`, usage 345/41/1536 = 1922 |

### error-session.jsonl

A failed first model call, the 0.84.2 zai-401 shape: the file is created and
flushed anyway; the assistant message has empty content, `stopReason`
`error`, an `errorMessage`, all-zero usage and no `responseId`. Session id
`00000000-0000-7000-8000-0000000000b2`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 143) | `session` | |
| 2 | [143, 275) | `model_change` | `zai` / `glm-5.3` |
| 3 | [275, 410) | `thinking_level_change` | `medium` |
| 4 | [410, 713) | `message`/user | The prompt |
| 5 | [713, 1205) | `message`/assistant | Error, zero usage, no response id |

### anthropic-session.jsonl

A successful session in the verified `anthropic-messages` shape
(`claude-opus-4-8`, invented values): header, model and thinking-level
changes, the user prompt, an assistant toolCall response whose usage carries
a non-zero `cacheWrite` and **no `responseId`** (the older `claude-opus-4-6`
shape observed on 2026-04-20), its toolResult, and a final assistant text
response with cache reads, cache writes, `cacheWrite1h` and a `responseId`
(the 2026-06-18 shape). Session id `00000000-0000-7000-8000-0000000000b5`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 145) | `session` | cwd `/home/dev/pi-anthropic` |
| 2 | [145, 291) | `model_change` | `anthropic` / `claude-opus-4-8` |
| 3 | [291, 423) | `thinking_level_change` | `off` |
| 4 | [423, 713) | `message`/user | The prompt |
| 5 | [713, 1327) | `message`/assistant | `read` toolCall, entry id `0a1b2c04`, no responseId, usage 3/0/5429/186 = 5618 |
| 6 | [1327, 1645) | `message`/toolResult | `ohya is one memory…\n` |
| 7 | [1645, 2341) | `message`/assistant | Final text, `msg_fixture_0002`, usage 2/5618/7143/53 = 12816, `cacheWrite1h` 0 |

### incomplete-tail.jsonl

A crash mid-write. The first three entries are complete. The fourth has no
trailing `\n` and its JSON is cut inside a text string. A parser must retain
bytes from offset 502 for the next poll and stop the cursor at 502. The file
is 679 bytes and does not end with `\n`.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 143) | `session` | |
| 2 | [143, 283) | `model_change` | |
| 3 | [283, 502) | `message`/user | |
| 4 | [502, 679) | partial | No trailing `\n`; JSON cut mid-string |

### malformed-line.jsonl

A complete but invalid entry mid-file: line 3 ends with `\n` but its JSON
breaks inside a text string (an unescaped quote). Lines before and after are
valid. A source must stop at offset 283 with a diagnostic and must not skip
bytes.

| Line | Bytes | Kind | Notes |
|---|---|---|---|
| 1 | [0, 143) | `session` | |
| 2 | [143, 283) | `model_change` | |
| 3 | [283, 667) | invalid | `\n`-terminated, invalid JSON |
| 4 | [667, 935) | `message`/toolResult | Valid; unreached without recovery |

## Limits

These fixtures cover the observed shapes, not every field. Real records
carry more optional fields (larger `sections`, image blocks in user content,
other providers and APIs); parsers must tolerate unknown fields and unknown
entry kinds, and store unknown valid records with kind `unknown`. Headless
runs in an untrusted working directory hang on a trust prompt without
`-a`/`--approve` — an operational note, not a format property.
