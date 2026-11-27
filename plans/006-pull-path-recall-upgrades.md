# Plan 006 — Pull-path recall upgrades (response to nicknisi/sessions)

Written 2026-11-27 against main `18eab6f` (v0.8.4, schema 011).
Provenance: competitive study `/notes/2026-11-27-sessions-vs-ohya.md`
(nicknisi/sessions verified 2026-11-27). Every item improves the pull path
only, per the owner decision recorded in NOTES.md 2026-11-26.

## Goal

Close the four concrete gaps the sessions study exposed, without widening
the tool surface or the delivery posture:

1. Agents get digest-shaped and drift-aware answers from recipes, not by
   composing SQL per question.
2. Egress and annotation apply one content policy: secrets today; injection
   phrasing and invisible Unicode added.
3. Full-text search matches morphological variants and CJK text.
4. OpenCode becomes a source.

## Non-goals (explicit)

- No SessionStart hooks or any auto-injection. Pull-only stands.
- No embeddings, no model calls anywhere.
- No new MCP tools. Everything arrives as `sql` recipes, schema, or source
  coverage. `annotate` semantics do not change for clean input.
- No distilled/mined memory store.

## Workstreams

### R1 — Digest and handoff recipes (docs + skill; no Go changes)

Files: `DB.md` (§ Recall recipes, line ~584), `internal/setup/skill.go`
(`SkillContent()`), `docs/reference.md` if it mirrors recipes.

- R1a — session digest recipe: one bounded result set per session — each
  `kind = 'user'` event with the final `kind = 'assistant'` reply of its
  exchange, ordered by `time_ms`, with event IDs for drill-in. Use window
  functions over `events` joined to `events_settled`; cap middle exchanges
  (keep both ends). Verify `kind` values against
  `internal/claudecode/parser.go:22` and the other parsers before writing
  the SQL (`user`, `assistant`, `attachment`, `system` seen there).
- R1b — handoff recipe: for a chosen session, first/last user prompt, final
  assistant reply, `sessions.branch`, and the event ID range — the prompt an
  agent or the menu-bar app can paste.
- R1c — drift steps in the skill's handoff guidance: after the SQL recipe,
  instruct read-only git — commits touching the session's files during
  `[started_at_ms, last_event_at_ms + 2h]` (did the work land), and
  `git status`/`git log` since (what changed). No Go code runs git; the
  agent does, exactly as `sessions continue` briefs it.
- R1d (optional follow-up) — `HandoffPromptBuilder`
  (`macos/OhyaMenuBar/Sources/OhyaMenuBar/`) gains the landed-or-not
  clause by running the same read-only git itself.

Tests: none (documentation and skill text). Acceptance: recipes run
verbatim against a fixture hub through `ohya sql` in a memory-e2e smoke;
skill diff reviewed for token weight (skill must stay small).

### C1 — Content policy at egress and annotation (small Go)

Files: `internal/secrets/secrets.go`, `internal/mcpserver/query.go:256`,
`internal/store/annotate.go`, plus tests.

- C1a — add detectors: invisible Unicode limited to bidi controls
  (U+202A–202E, U+2066–U+2069), U+2060, and U+200B, plus prompt-injection
  phrasing (a short anchored pattern set: "ignore previous instructions",
  "disregard all prior", and close variants — start narrow). Do not match
  U+200C, U+200D, or U+FEFF: they are legitimate in emoji ZWJ sequences,
  Persian and Indic scripts, and byte order marks.
- C1b — egress: extend the redaction call site (`redactCells`,
  `internal/mcpserver/query.go:243`) and confirm `cmd/ohya/recall.go`
  reaches the same function. Bidi and zero-width matches are
  marker-replaced like secrets (ADR 0015 style). Injection phrasing is
  different: ohya's own sessions discuss injection, and replacement hides
  those events from the owner's audits. Default to a per-result flag count
  and leave the text served; replace only if C1d shows a clean corpus. The
  choice goes in the ADR (C1e).
- C1c — annotate: reject operations whose payload matches, naming the rule
  in the error. Annotated text is authored and small; rejection beats
  silent stripping.
- C1d — measure false positives on the local corpus before landing the
  injection set (grep counts over settled events; tighten anchors if the
  rate is visible). Secrets behavior is unchanged.

- C1e — ADR: amend ADR 0015 or add a new ADR covering the wider egress
  contract and the `annotate` rejection path.

Tests: unit tests per pattern; egress test with a bidi character and an
injection sentence, on both the `sql` and `recall` surfaces; an emoji ZWJ
sequence and a Persian ZWNJ word pass unchanged; annotate rejection test; regression that clean prose
and prose *about* secrets ("never pipe $GITHUB_TOKEN through curl") pass.

### F1 — FTS stemming (porter) via migration 012

Files: `internal/store/migrations/012_fts_porter.sql`,
`internal/store/migrate.go`, `internal/store/schema_test.go`.

- F1a — verify the bundled SQLite exposes the porter wrapper: create
  `fts5(x, tokenize='porter unicode61')` under the `sqlite_fts5` build tag
  in a test before writing the migration (mattn/go-sqlite3 v1.14.53).
- F1b — migration: drop and recreate `events_fts` with
  `tokenize = 'porter unicode61'` (external-content table), then run
  `INSERT INTO events_fts(events_fts) VALUES('rebuild')`. The insert and
  delete triggers on `events` stay untouched; SQLite resolves them at run
  time.
- F1c — measure migration wall time on the 17 GB hub recovery copy
  (procedure in the hub-deployment notes); record it in
  `docs/install.md`/release notes if it exceeds one minute.
- F1d — docs: re-verify the MATCH paragraphs in `DB.md` (hyphen and
  phrase behavior, around lines 476–497) under the new tokenizer.
- F1e — tests: stemming pair (`refactor` matches `refactoring`) in
  schema/migration tests; `migrate_released_test.go` extended so released
  databases upgrade.
- F1f — rerun the ohya arm of `cmd/recall-bench` before/after; record
  correct-count and tool-output deltas in NOTES.md.

Note: porter is a schema-time tokenizer choice; the MCP authorizer's
`fts3_tokenizer` block (`internal/mcpserver/query.go:406`) is unaffected.

### F2 — CJK search (spike, then gated implementation)

Files: new `internal/store/` tests first; implementation only after the
decision in F2b.

- F2a — spike: build a mixed English/CJK fixture corpus; measure recall
  and index growth for two candidates — an FTS5 `trigram` side table
  (substring and CJK, but queries under three characters match nothing)
  and a two-gram side table like sessions' (matches two-character words;
  roughly 1.7x FTS growth on a mixed corpus per their README).
- F2b — decision record: pick one, or pick neither, with the measurements.
  Record it as a short ADR (`docs/adr/00xx-cjk-search.md`) because it adds
  a second index and a query recipe.
- F2c (only if adopted) — migration 013 for the side table plus triggers,
  a merge recipe in DB.md and the skill, and bench numbers in NOTES.md.

### O1 — OpenCode source (spike, then source)

Files: new `internal/opencode/`, `cmd/ohya/ingest.go`, BACKLOG item 6.

- O1a — spike (BACKLOG item 6): open `~/.local/share/opencode/opencode.db`
  read-only; enumerate the v2 schema across installed versions; decide the
  poll cursor — a per-session watermark (message/part rowid or monotonic
  id) encoded in `sources.cursor_bytes` or a documented metadata field.
  Write the decision into BACKLOG/NOTES before implementation.
- O1b — source: poll-based reader mirroring `internal/pi` structure
  (discover, parse to normalized events, transactional cursor advance);
  idempotent replays; tolerate schema drift by failing loudly per table.
- O1c — fixtures: synthetic OpenCode SQLite fixtures via `cmd/genfixture`;
  parser tests plus a fuzz pass on part/message JSON.
- O1d — wiring: ingest discovery for a DB-path source, `--exclude`
  respected, doctor checks, setup detection when the binary is present.

Tests: poll idempotency (restart mid-poll), database replacement, crash
between read and cursor commit, end-to-end memory-e2e arm.

### D1 — Documentation

- D1a — README compare section: a dated `sessions` subsection beside the
  Atuin threads one, claims limited to the verified study.
- D1b — prune the NOTES.md competitive bullet when items land; keep the
  study note as the archive.

## Sequencing and size

| Order | Item | Size | Gate |
| --- | --- | --- | --- |
| 1 | O1a | S | none (spike; start first) |
| 2 | R1a–R1c | S | none |
| 3 | C1 | S | C1d measurement |
| 4 | F1 | S–M | F1a availability test |
| 5 | F2a/b | S | F1 landed |
| 6 | F2c | M | F2b decision |
| 7 | O1b–O1d | L | O1a design accepted |
| any | R1d, D1 | S | none |

R1 and C1 deliver user-visible value this week. O1 is the largest item and
the only one that touches source discovery; it can proceed in parallel
after its spike. No workstream touches sync or authorization, so no TLA+
model changes are needed; state that in each PR description.

## Task filing

On adoption, file as T154 (R1), T155 (C1), T156 (F1), T157 (F2a+b),
T158 (O1), keeping D1 inside the nearest task.

## Verification

- `gofmt` on changed Go files; `make check` per AGENTS.md.
- Swift-only changes (R1d): `xcrun swift test`, `xcrun swift build`,
  `go run ./cmd/commentcheck`.
- F1/F2: recall-bench ohya-arm rerun plus deterministic fixture tests.
- O1: memory-e2e with an OpenCode fixture; measured first-poll time on a
  real `opencode.db` if one is available on this machine.
