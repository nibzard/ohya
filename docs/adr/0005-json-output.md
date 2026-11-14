# 0005: JSON output for the reporting commands

Status: accepted, 2026-11-14 (T062).

## Context

Every command that reports facts prints prose. An agent or script that needs
the numbers behind `ohya: imported 16 events from 1 sources`, the devices
list, or the version line must parse that prose, and every consumer
re-implements the parsing. T059 made the text uniform and T060 made the exit
codes a contract; machine-readable output is the missing third piece of the
same surface.

## Decision

The commands that report facts take a `--json` flag. Text stays the default;
nothing about the text output changes.

- Covered commands: `ingest --once`, `devices list`, `devices invite`,
  `devices revoke`, `rederive`, `version`.
- Not covered, deliberately: `serve`, `mcp`, `join`, and the polling `ingest`
  are long-running or stream-oriented; a single trailing object helps no
  consumer of a process that never exits. No output-format registry and no
  streaming variant exist until a user needs one (YAGNI).

### The envelope

With `--json`, a command prints exactly one JSON object on stdout:

```json
{"schema_version":1,"command":"ohya devices list","data":{...},"error":null}
```

- `schema_version` is the envelope contract's version. It changes only on a
  breaking change — a field renamed or removed, or a meaning changed. Adding
  a field to `data`, a new error code, or a new covered command does not bump
  it. The version starts at 1.
- `command` is the command's own name as its `--help` prints it, for example
  `ohya devices list`.
- `data` carries the command's report on success and is `null` on failure.
- `error` is `null` on success; on failure it is
  `{"code":"<stable snake_case>","message":"...","hint":"..."}`. `message` is
  the same text the text output's stderr line carries. `hint` names the
  remedy when one exists and is `""` otherwise.

Diagnostics stay on stderr. The JSON never carries transcript content: the
report holds counts, IDs, states, timestamps, offsets and remedies only.
The join code from `devices invite` appears in `data` and nowhere else — a
failure envelope's `message` and `hint` never contain a code, because a
failed invite mints no code.

Exit codes are unchanged (T059, T060): 0 success, 1 failure, 2 usage error,
3 degraded `ingest --once`. A degraded pass (exit 3) is a completed pass:
`data` carries the counts including `failed` and `stopped`, and `error`
stays `null`. Usage errors (exit 2) keep the T059 text contract — one stderr
line plus a `--help` pointer, nothing on stdout — because they report an
unparseable command line before any command semantics exist; `--help`
remains their remedy.

### Data fields per command

Timestamps are RFC 3339 UTC strings.

- `ingest --once`: `events`, `sources`, `failed`, `stopped`, and `stops`, one
  entry per stopped source: `source_id`, `path`, `offset`, and `remedy`.
  `remedy` is a stable code, not prose: `increase_max_record_bytes` (a size
  stop, ADR 0004) or `repair_source_file` (every other stop).
- `devices list`: `devices` and `invitations`. A device carries `id`,
  `state`, `paired_at`, and `revoked_at` (present only on a revoked device).
  An invitation carries `id`, `state`, `used_by` (present only when used),
  `created_at`, `expires_at`, and the derived `expired` flag. Empty
  collections are `[]`, never `null`.
- `devices invite`: `code` (the full join code — the output the operator
  asked for) and `expires_at`.
- `devices revoke`: `device_id`.
- `rederive`: `events`, `sessions`, `duration_ms`.
- `version`: the T061 keys unchanged — `version`, `revision`, `dirty`, `go`,
  `schema`, `parser.claude`, `parser.codex`, `parser.pi` — so one vocabulary
  spans the text line and the JSON object.

### Error codes

Codes are stable snake_case identifiers; consumers may branch on them.
Every failure maps to exactly one code.

| Code | Meaning | Hint |
|---|---|---|
| `internal_error` | Any failure no rule below classifies | — |
| `database_open_failed` | The hub database could not be opened (`ingest`, `rederive`) | check the `--db` path and its permissions |
| `state_recovery_failed` | The state directory's identity or checkpoint files exist but cannot be used (`ingest`) | restore them from a backup |
| `missing_agent_root` | A session directory passed with `--<agent>-root` does not exist (`ingest`) | pass the flag or create the directory |
| `coordination_plane_absent` | `devices invite` found no coordination plane to bridge to | run `ohya serve` once |
| `registry_open_failed` | The device registry could not be opened (`devices` subcommands) | check the `--state` directory |
| `device_unknown` | `devices revoke` named a device that is not paired | run `ohya devices list` |
| `device_already_revoked` | `devices revoke` named an already-revoked identity | revocation is final; re-admission needs a new identity |

Classification is by sentinel (`errors.Is`), never by matching message text:
`watch.ErrStateLost` and `device.ErrUnusable` map to `state_recovery_failed`,
`control.ErrNotProvisioned` to `coordination_plane_absent`,
`registry.ErrDeviceUnknown` and `registry.ErrAlreadyRevoked` to their codes.
Failures whose class only the failing call site knows are wrapped there.

## Consequences

Agents and scripts read one JSON object instead of parsing prose, and the
text output stays the default for humans. `schema_version` is a promise of
stability, so additive changes must not bump it — a golden test per command
pins the field names, and one shared struct decodes every command's output.
The exit-code contract does the heavy lifting for control flow; the envelope
carries the facts. Two sentinels were added where classification would
otherwise rest on prose (`registry.ErrAlreadyRevoked`,
`control.ErrNotProvisioned`); both preserve their messages' existing text.
Covering the long-running commands stays deliberately undone: a trailing
object after hours of serving serves no consumer, and inventing one now
would add contract surface without a user.
