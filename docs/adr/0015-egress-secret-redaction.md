# 0015: Egress secret redaction at the sql tool boundary

Status: accepted, 2026-11-19 (T142).

## Context

Agent transcripts contain credentials: exported tokens in shell commands,
`.env` content pasted into prompts, private keys printed by a debug step.
ohya keeps raw records byte for byte by design — the lossless invariant is
what makes re-derivation, retry validation, and audit possible — and every
paired device's agents read the same hub through the `sql` tool. Until now
the only barrier between a stored credential and every agent context was
one sentence in the server instructions ("never reveal credentials"), which
is instruction-only and unverifiable.

Atuin redacts secrets at capture (competitive study 2026-11-19,
`/notes/2026-11-19-atuin-threads-vs-ohya.md`). That placement is not
available to ohya: redacting at ingest would destroy the raw record, and
the raw record is the product. The correct boundary for ohya is egress.

## Decision

**One fixed, deterministic pattern table in `internal/secrets` runs over
every string cell of a `sql` result inside the mcpserver Engine, before
the result is returned.** Matches are replaced by kind-labeled markers
(`[redacted:github-token]`), and the result carries one additive
`redactions` count. The local stdio tool and the remote hubsync query and
annotate frames are all served by the same Engine, so one pass covers both
paths; `ohya search` and `ohya show`, which query through the Engine,
inherit it unchanged.

`ohya secrets scan` gives the operator the same visibility over stored
records: it runs the same table over every event's `raw_json` and prints
counts, pattern ids, event ids, and session ids — never matched text.

### The pattern table

Fixed shapes, no classifier, no model, no new dependency:

| Pattern id | Shape |
|---|---|
| `aws-access-key-id` | `AKIA`/`ASIA` + 16 upper-case alphanumerics |
| `github-token` | `ghp_` + 36 alphanumerics, `github_pat_` + 30 |
| `anthropic-api-key` | `sk-ant-` + 16 key characters |
| `openai-api-key` | `sk-` + 20 key characters |
| `slack-token` | `xox[abposr]-` + 20 token characters |
| `google-api-key` | `AIza` + exactly 35 key characters |
| `private-key-block` | a full `-----BEGIN … PRIVATE KEY-----` … `-----END …-----` block |
| `env-assignment` | `UPPER_SNAKE` secret-suffixed name (`API_KEY`, `ACCESS_KEY`, `SECRET_KEY`, `PRIVATE_KEY`, `TOKEN`, `SECRET`, `PASSWORD`, `PASSPHRASE`, `PASSWD`) `=` a value of at least 8 characters; the variable name is kept, only the value is replaced |

The table is ordered and applied sequentially: token patterns run before
the env pattern, so `ANTHROPIC_API_KEY=sk-ant-…` keeps the precise
`anthropic-api-key` marker while the name stays readable. The env value
charset excludes `[`, so a value that is already a marker is never
re-matched; redaction is idempotent (pinned by table tests and a fuzz
target that also pins determinism). Near-miss negatives are pinned so
false positives cannot drift in: `MAX_TOKENS=4096`, `OLDPWD=/home`,
`PUBLIC_KEY=…`, a `BEGIN PRIVATE KEY` header without its `END`, short or
lower-case token shapes, and plain SQL text. An anchor prefilter skips
strings that contain none of the literals every pattern requires, keeping
secret-free results nearly free to redact.

### Wire compatibility

`Result.redactions` is `omitempty`, so a result with no redactions is
byte-identical to the previous shape on the hubsync wire; the hubsync
round-trip test carries a non-zero count to pin the decode/encode path.

## Consequences

- **Best-effort risk reduction, never a guarantee.** A fixed table finds
  only the shapes it knows; novel, malformed, or truncated secrets can
  still pass (a `substr()` cut, a base64-encoded BLOB cell, a
  unicode-escaped token). The tool description and the scan output say so,
  and results stay untrusted input either way.
- **The lossless invariant holds.** Redaction happens after the rows are
  read, on the result only. Stored records, derived text, FTS, and
  annotations are untouched.
- **Removing a stored secret is a separate owner action** (`ohya purge`);
  redaction changes what leaves the hub, not what the hub keeps.
- **Over-redaction is possible and accepted.** A random base64 run can
  collide with the `AIza` or `AKIA` shape; a non-secret assignment like a
  template value can match the env pattern. Losing a non-secret value from
  a result is cheap; shipping a secret is not.
- The scan reads `raw_json` only: the derived `text` column is a subset of
  the record's bytes for real data, and every event row — including replay
  duplicates, whose `text` is NULL — carries `raw_json`.
- No configuration flag: the pass is always on for every engine. A
  disable switch would be one more way for a deployment to silently lose
  the boundary, and no use case has been shown (YAGNI).

## Rejected alternatives

- **Redact at capture or ingest** (Atuin's placement): destroys the raw
  record, breaks re-derivation and retry validation, contradicts the
  product's lossless stance.
- **A classifier or LLM pass**: contradicts the no-AI-ingestion stance,
  adds nondeterminism and cost, and cannot be proven on the hot path.
- **A redaction column or derived view**: duplicates knowledge of the
  pattern table into the schema and still needs an egress pass to be
  trustworthy.
- **Denying queries that match secrets in SQLite**: the match must happen
  on decoded text, after it leaves SQLite; inside SQLite it would need a
  UDF and re-implement the table in SQL.
