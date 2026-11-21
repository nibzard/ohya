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

- Covered commands: `ingest --once`, `service status|start|stop|restart`,
  `search`, `show`, `sessions`, `devices list`, `devices invite`,
  `devices revoke`, `purge`, `sources reallow`, `rederive`, `doctor`,
  `usage`, `version` (the observability reports `status`, `metrics`,
  `logs`, and `diagnose` also follow the envelope; see their own docs).
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
  stop, ADR 0004) or `repair_source_file` (every other stop). `excluded`
  (added 2026-11-20) is a per-agent map of session files skipped by
  `--exclude` globs at discovery; it is `{}` when no exclusion is
  configured.
- `devices list`: `devices` and `invitations`. A device carries `id`,
  `state`, `paired_at`, and `revoked_at` (present only on a revoked device).
  An invitation carries `id`, `state`, `used_by` (present only when used),
  `created_at`, `expires_at`, and the derived `expired` flag. Empty
  collections are `[]`, never `null`.
- `devices invite`: `code` (the full join code — the output the operator
  asked for) and `expires_at`.
- `devices revoke`: `device_id`.
- `purge`: `dry_run` (`true` without `--apply`), `scope` (the selector
  text), `sessions` and `sources` (empty arrays, never `null`; a session
  carries `id`, `agent`, `device_id`, `native_id`, `events`; a source
  carries `id`, `agent`, `device_id`, `path`, `cursor_bytes`, `events`,
  `native_id`, and — only with `--apply` — `purged_at`), plus the
  `events` and `bytes` totals. The bytes are the evicted sources'
  committed cursors, the mirrored-transcript measure of ADR 0013.
- `sources reallow`: `source_id` and `cursor_bytes` (the restored cursor).
- `rederive`: `events`, `sessions`, `duration_ms`.
- `doctor`: `findings`, one entry per check: `check`, `severity`
  (`ok`, `warn`, `error`), `detail`, and `remedy` (the one-line remedy, `""`
  when none is owed). Error findings ride in `data`, not in `error`: the
  findings are the report and the exit code is the verdict, exactly like a
  degraded ingest pass carries its stops through `data`.
- `version`: the T061 keys unchanged — `version`, `revision`, `dirty`, `go`,
  `schema`, `parser.claude`, `parser.codex`, `parser.pi` — so one vocabulary
  spans the text line and the JSON object.
- `service status|start|stop|restart`: `service` (`service` or `hub-service`),
  `action`, `installed`, `edited`, `loaded`, `running`, `changed`, plus
  optional `path` and `detail`.
- `search`: `query`, `matches` (`[]` when empty; each item carries `event_id`,
  `session_id`, `agent`, optional `repo`/`time`/`time_ms`, and redacted,
  sanitized `excerpt`), `truncated`, optional `truncation_reason`, and
  optional `redactions`.
- `show`: `session` (`id`, `agent`, `device_id`, `native_id`, optional
  `repo`/`cwd`/`branch`/`model`/`first_prompt`/`started_at`/`started_at_ms`/
  `last_event_at`/`last_event_at_ms`/`settled`, and `event_count`), `events`
  (`[]` when empty, oldest first; each item carries `id`, optional
  `time`/`time_ms`/`role`/`tool_name`, `kind`, and `excerpt`), `truncated`,
  optional `truncation_reason`, and optional `redactions`.
- `sessions`: `sessions` (`[]` when empty, newest first, excluding synthetic
  `ohya-synthetic:` companions; same session shape as `show.session` with
  `settled` computed against the hub's 10-minute `events_settled` threshold),
  `truncated`, optional `truncation_reason`, and optional `redactions`.

### Error codes

Codes are stable snake_case identifiers; consumers may branch on them.
Every failure maps to exactly one code.

| Code | Meaning | Hint |
|---|---|---|
| `internal_error` | Any failure no rule below classifies | — |
| `database_open_failed` | The hub database could not be opened (`ingest`, `rederive`) | check the `--db` path and its permissions |
| `state_recovery_failed` | The state directory's identity or checkpoint files exist but cannot be used (`ingest`) | restore them from a backup |
| `missing_agent_root` | A session directory passed with `--<agent>-root` does not exist (`ingest`) | pass the flag or create the directory |
| `hub_identity_absent` | `devices invite` found no saved hub identity to compute an address from | run `ohya serve` once to create the hub identity, then retry |
| `registry_open_failed` | The device registry could not be opened (`devices` subcommands) | check the `--state` directory |
| `device_unknown` | `devices revoke` named a device that is not paired | run `ohya devices list` |
| `device_already_revoked` | `devices revoke` named an already-revoked identity | revocation is final; re-admission needs a new identity |
| `service_not_configured` | `service start|stop|restart` found no owned setup receipt | run `ohya setup` (or `ohya setup --hub` for the hub service) first |
| `service_unit_missing` | the setup receipt exists, but its unit file is missing on disk | run `ohya setup` to recreate the service unit |
| `service_control_failed` | the user service manager failed a `service` action | check your user service manager and run `ohya doctor` |
| `not_paired` | a `--remote` recall command found no pairing credential or hub address | run `ohya join` first |
| `unauthorized` | the hub refused the device credential during remote recall | device was revoked or state belongs to a different hub |
| `fts_syntax_error` | `search` failed to parse an FTS5 query (for example unquoted punctuation) | wrap punctuation-bearing terms in double quotes |
| `session_not_found` | `show` named an unknown session ID | find session ids with `ohya search` or `ohya sessions` |
| `query_denied` / `query_failed` | the read-only recall query was denied or failed | — |

Classification is by sentinel (`errors.Is`), never by matching message text:
`watch.ErrStateLost` and `device.ErrUnusable` map to `state_recovery_failed`,
`registry.ErrDeviceUnknown` and `registry.ErrAlreadyRevoked` to their codes.
Failures whose class only the failing call site knows are wrapped there;
`hub_identity_absent` wraps `address.ErrNoIdentity` at the invite call site.

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

## Amendment (2026-11-16): transport code follows the address transport

[ADR 0011](0011-tailcat-address-transport.md) replaced the embedded
coordination plane with the Tailcat address transport. The table and the
sentinel list above are updated: `coordination_plane_absent` and
`control.ErrNotProvisioned` left with the plane (T123), and
`devices invite` fails with `hub_identity_absent` (wrapping
`address.ErrNoIdentity`, T122) when the state directory holds no hub
identity. No other code changed.
