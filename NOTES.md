# ohya — Notes

<!--
Working notes for ohya — unresolved knowledge useful to future work.
Read at the start of each task; update when useful; prune what is resolved.

How to use this file:
- Read it before starting any task; re-read findings that touch your work.
- Add a dated bullet for information worth carrying into another session.
  One topic per bullet; keep it short.
- Distinguish observed facts from hypotheses. Mark uncertain claims as unverified
  and include a relevant file reference or verification step when useful.
- Remove or rewrite bullets once resolved or disproved — this is a live file,
  not an archive.
- Keep durable decisions in the relevant documentation and actionable tasks in
  the backlog; use this file for unresolved working knowledge.
- Never put secrets, transcripts, or large dumps here; link to sources instead.
-->


- Agent session storage, verified against installed versions on this machine (2026-11-13); these findings shape ingestion:
  - Claude Code: verified 2026-11-13 against installed 2.1.280 (895 main sessions, 2,646 JSONL files). Format, record kinds, identity fields, compound records and usage semantics are documented in `testdata/claude-code/README.md`, with synthetic fixtures. Points that shape ingestion: `sessionId` is the main file name and never spans files; subagent files under `<session-uuid>/subagents/` reuse the parent `sessionId` with `isSidechain: true` plus `agentId`; one API response is several records sharing `message.id` and repeating the same per-request `usage` (not cumulative — dedupe by `message.id` before summing); `cwd`/`gitBranch` live on message records; the project directory name encodes `cwd` lossily (`/ . _` → `-`), so never decode it; timestamps are ISO-8601 UTC ms but not strictly monotonic; malformed complete lines do occur in real files, with valid lines after them.
  - Codex: verified 2026-11-13 against installed codex-cli 0.166.0 (2,488 files, 3.0 GB under `~/.codex/sessions/`, written by versions 0.104–0.167; envelope unchanged). Format, record kinds, identity fields and fixtures are documented in `testdata/codex/README.md`. Points that shape ingestion: the native session id is `session_meta.payload.id` (equals the first file-name UUID) and **one thread spans several rollout files** — resumed/paginated pages are named `rollout-…-<thread-id>_<resume-id>.jsonl` (second UUID never appears in content), set `history_mode: "paginated"`, and re-enumerate prior records after a rollback — 4 of the 7 corpus pairs carry `history_base {thread_id, end_ordinal_exclusive, end_byte_offset}` and overlap the original's ordinal tail (pair: original ordinals 1–984, page 979–1019, but with different content at every shared ordinal); the other 3 re-enumerate from ordinal 1 with no `history_base` — so grouping must key on the thread id across files, never byte offsets, and ordinal collisions need care (see the T044 finding below); `ordinal` exists only from 0.147.0. `event_msg` mirrors `response_item` text (`user_message`/`agent_message`) — parse `response_item` as the transcript or dedupe. Usage: each `token_usage_record.usage` is per `response_id` (not cumulative); `turn_token_usage`/`thread_token_usage` are running totals — sum `usage` only. `session_meta.git` (`branch`, `commit_hash`, `repository_url`) is a direct input for the sessions.repo derivation (T029). Subagent rollouts open with their own meta (`source.subagent.thread_spawn.parent_thread_id`) and then embed the parent's `session_meta` as line 2. `CODEX_HOME` redirects everything (verified: 0.166.0 created `<dir>/tmp` and read `auth.json` there); no compressed session files exist (all plain JSONL). The old `~/.codex/sqlite/codex-dev.db` (`local_thread_catalog`) layout is gone in 0.166.0: schema-numbered DBs sit directly under the Codex home, and the earlier "metadata only" claim fails for `thread_history_1.sqlite`, whose `thread_items.item_json` holds full message/command bodies (projection cache over 511 of 2,488 threads, with byte offsets back into the rollouts) — ingest rollout files only. `session_index.jsonl` is thread-name metadata; `history.jsonl` duplicates user prompts. Wild files occasionally lack the trailing newline or contain malformed complete lines (both covered by fixtures).
  - Pi agent: verified 2026-11-13 (T012) against installed 0.84.2 (already present globally as `@earendil-works/pi-coding-agent`) and latest 1.5.2 from a scratch npm prefix. The writeup claim "project-local `.pi/`" is wrong for sessions: sessions are global, one append-only JSONL per session, at `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<session-uuid>.jsonl` (agent dir overridable by `PI_CODING_AGENT_DIR`; session dir by `--session-dir` > `PI_CODING_AGENT_SESSION_DIR` > `sessionDir`; `--no-session` is ephemeral). `<encoded-cwd>` = cwd minus one leading `/`, every `/` `\` `:` replaced by `-`, wrapped in `--`…`--` (source: `dist/core/session-manager.js`, identical in both versions); lossy (`/a/b` and `/a-b` collide), so never decode it — the `session` header's `cwd` is authoritative. File name = session-start `toISOString()` with `:`/`.` → `-`, then `_` + session UUID (time-ordered, `01a1…`). Session format v3: line 0 is a `session` header (`version`, `id`, `timestamp`, `cwd`); every other entry has `type`, `id` (8-hex), `parentId` (tree, `null` = root), ISO `timestamp`. Kinds seen: `model_change`, `thinking_level_change`, `message` (roles `system`/`user`/`assistant`/`toolResult`). Assistant messages carry `provider`, `model`, `api`, `stopReason`, `responseId`, `rawStopReason`, and one usage+cost object per response (`input`, `output`, `cacheRead`, `cacheWrite`, `reasoning`, `totalTokens`) — per-response, not repeated across records, so token totals sum straight over assistant messages (no Claude-Code-style dedupe); 1.5.2 adds `durationMs` and `thinkingLevel` and persists a `system` entry (`sections`, `toolsAdded`, empty `content`). `toolResult` carries `toolCallId`, `toolName`, `content[]`, `isError`. Entries flush incrementally: a first model call that fails still persists header + `model_change` + `thinking_level_change` + user + assistant with `stopReason: "error"`, `errorMessage`, zero usage. Headless runs in an untrusted cwd hang on a trust prompt — pass `-a`/`--approve`; runs from the pre-trusted `/home/agent` did not need it. The agent dir also holds `auth.json` (credentials), `settings.json`, `models.json` (provider catalog, keys referenced by env-var name), `trust.json`, `bin/` (`rg`, `fd`), and `~/.pi/skills/`. Real sample sessions and the captured layout (0.84.2 and 1.5.2) are preserved outside the repo at `/home/agent/.looper/ohya-3d80aa08/runtime/task-b55552405a41f18c/work-00-session-15667963bc0501b8/pi-sample-session/` for authoring the fixtures — the T037 fixtures in `testdata/pi/` derive from them; `.pi/` may still exist inside projects for config/packages, but no session writes there were observed.
  - Antigravity CLI (`agy`): verified 2026-11-22 against installed 1.5.14 (40 conversations, 5,235 lines, 22.9 MiB under `~/.gemini/antigravity-cli/brain/`). Format and synthetic fixtures are documented in `testdata/antigravity/README.md`. Points that shape ingestion and setup: transcripts are true append-only JSONL at `<brain>/<conversation-uuid>/.system_generated/logs/transcript_full.jsonl` (live-verified same inode + byte-stable prefix + strictly monotone `step_index`, 0 malformed lines); discovery must skip the truncated sibling `transcript.jsonl` and the 100 KiB byte-sliced `chunks/` mirrors (`steps/`, `scratch/`, `.user_uploaded/`, `messages/`). Native session ID is `<conversation-uuid>` from `SourcePath` (wired through `store.SourceState.Path` and `store.DeriveInput.SourcePath` so local ingest, remote `hubsync`, `doctor`, and `rederive` all agree without schema or wire changes). `USER_INPUT` wraps prompts in `<USER_REQUEST>...</USER_REQUEST>` plus `<ADDITIONAL_METADATA>`; `PLANNER_RESPONSE` carries `model`, `content`, `tool_calls` (`name`, `args`), `thinking` (omitted from FTS text, preserved in `raw_json`), and additive per-step tokens (`input_tokens`, `cache_read_tokens`, `output_tokens` — verified `cache_read_tokens > input_tokens` in 1,374 of 1,425 non-zero records); 100% of `GENERIC` steps in the corpus are tool results (`role=tool`); `SYSTEM_MESSAGE` contributes no search text. Global MCP config lives at `~/.gemini/config/mcp_config.json` (`mcpServers.ohya` with `command` + `args`, overridable via `AGY_CONFIG_DIR`; `agy` initializes this file as 0 bytes on first run, so `setup.ApplyJSONEntry` and preflight treat empty/whitespace-only JSON configs as `{}`); `ohya setup` registers client `"agy"` when `agy` is on `PATH`, while `sessions.agent` is `"antigravity"` (root flag `--antigravity-root`, env override `AGY_DATA_DIR`).
  - OpenCode (phase 2): storage moved to SQLite in v2 (verified 2026-11-13 against 2.0.12) - `~/.local/share/opencode/opencode.db` with WAL (tables `session`, `message`, `part`, `event`, and so on); the old one-JSON-per-message layout no longer applies, so it needs a poll-based SQLite source, not a byte cursor.
  - Claude Desktop regular chats: server-side, not file-watchable; potentially pollable via unofficial authenticated API (see Claude Explorer / rpeck/claude-explorer) or official export. Would need a new poll-based source type — deferred.
  - ChatGPT desktop: no reliable local transcripts; official export only — deferred.
  - Cowork local sessions may write JSONL under `~/Library/Application Support/Claude/local-agent-mode-sessions/` — absent on current install; check if usage starts.

- ohya does not use Tailcat (verified 2026-11-16). ADR 0002 calls the embedded `tailscale.com/tsnet` node plus an in-process Headscale "Tailcat". The real Tailcat is `github.com/tailscale/tailcat` (BSD-3, v0.x, no API or wire-format stability promise). It needs no control plane: the server prints a `tc…` address with its WireGuard key, a pre-shared key, and DERP region data, and the client dials it. Rendezvous and fallback relay use Tailscale's public DERP map (`https://tailcat.dev/derpmap.json`, no account) or a self-hosted `derper`. The tsnet plus Headscale design is why a hub needs a reachable control URL and a trusted TLS certificate. Spike results (2026-11-16, `/tmp/tcspike`, library `Server.Listen` on the VM, CLI client on the MacBook with Tailscale quit): v0.7.0 builds and its tests pass against our `tailscale.com` v1.104.4 pin; the spike binary is 30 MB; a saved node key, pre-shared key, and region keep one 233-character address across restarts (`Server.Start` does not write back a generated key). Same LAN: direct path found automatically; one 32 MiB upload ran at 12.4 Mbit/s. Phone hotspot: no direct path in 30 s through the VM's double NAT (Proxmox, then UniFi); relay only, 3.1 to 3.3 Mbit/s, 65 to 265 ms. The MacBook's history is 4.46 GB of raw records, so a first upload over the relay takes about 3 hours. gzip shrinks a 20,000-record sample only 1.9 times. Tailcat v0.7.0 hardcodes a random UDP listen port and builds with `ts_omit_portmapper`, so a router port forward or UPnP cannot help without an upstream change. Sources: https://github.com/tailscale/tailcat, https://pkg.go.dev/github.com/tailscale/tailcat.

- Codex `archived_sessions/` ingestion gap (verified 2026-11-15): `~/.codex/archived_sessions` is a sibling of `sessions/` and holds rollouts moved out of it (6 plain-JSONL files, 1.5 MB, Feb–Sep 2026, none duplicated under `sessions/`; the directory was absent again on 2026-11-16 while `sessions/` remained). Resolved 2026-11-16 (T081): ohya now watches `$CODEX_HOME/archived_sessions` as a second default codex root on both ingest and join (one watcher per agent covers both roots, so a file moving between them keeps its checkpoint by file identity), and a watched source whose path stops being statable prints one diagnostic per disappearance on both paths instead of stopping silently. Watching all of `~/.codex` stays wrong: it would ingest `session_index.jsonl`, `history.jsonl`, and vendor JSONL.

- Competitive clock — Atuin threads (verified 2026-11-19): Atuin shipped
  "threads" (v18.24.0, 2026-10-15; renamed from "AI sessions" in the same
  cycle) — daemon file-tailing capture of Claude Code/Codex/Pi/OpenCode and 5+
  more harnesses from the same JSONL sources ohya watches, encrypted record
  store + derived SQLite FTS5/BM25 projection (threads.db), per-user E2EE sync
  through hosted or self-hosted relay, `atuin mcp` list/read/search tools for
  agent self-recall, TUI resume/fork across harnesses; lossy capture (secrets
  redacted, 64 KiB tool-I/O clip). It is single-user; the live "Threads for
  teams" beta is hosted-hub-only and uploads shared threads in plaintext
  inside TLS. Open flanks: no delete/retention (#4434), no per-machine sync
  opt-out (#4660), one home per harness (#4687), keyword-only search (#4589).
  ohya's wedge stays: operator-owned shared hub with no third-party server,
  lossless raw records, bounded arbitrary SQL + annotate, TLA+-verified
  sync/auth. Full study: /notes/2026-11-19-atuin-threads-vs-ohya.md.
  Published 2026-11-19 (T143): dated comparison sections in README.md and
  PRODUCT.md; recall recipes (date-bounded on time_ms, resume from the
  repo's last settled session, sessions.model, per-device) in the sql tool
  description and DB.md "Recall recipes" (every published SQL block runs
  verbatim through the engine; strftime '%s' must be written '%%s' in the
  ToolDescription source because the whole description is one Fprintf
  format string); empty-root flags framed as per-machine opt-out in
  docs/reference.md.
  Since 2026-11-20 (T151) that verification is permanent:
  TestPublishedRecipesRunVerbatim in internal/mcpserver extracts every SQL
  block from ToolDescription and the MCP guide resources
  (`ohya://guides/garbage-audit`, `ohya://guides/analytics-and-review`) and
  runs each verbatim through the engine over a hub seeded with the published
  literals (repo github.com/owner/name, model claude-sonnet-4-5, device
  laptop, session claude:dev1:s1, event ids 42/100/101/102). The block
  counts are pinned (10 in ToolDescription, 8 in garbage-audit, 4 in
  analytics-and-review since 2026-11-23), so adding or removing an example
  is a conscious test update. Upfront MCP prompt size (`MemoryInstructions` +
  `ToolDescription` + `AnnotateDescription`) is also bounded in
  TestServerServesSQLTool (7,374 bytes / <= 8,000 byte budget, down from
  15,431 bytes before moving rare playbooks to MCP resources).
  A syntax error or a denied statement inside a block fails at execution;
  an opener that stops matching (for example a block rewritten to start
  with DELETE) trips the count pin instead.

- Hub measurements 2026-11-24 (a read-only pass over the live hub; aggregates
  only; the fixes are in the next two bullets): 7,031 real sessions, 2.88M
  events, 16.8 GiB on disk for 10.74 GB ingested (1.68x), 233 repositories,
  two devices.
  - Adoption over 30 days: `sql` ran 163 times in 40 sessions; Claude Code
    had the tool in 357 sessions and called it in 37; `annotate` has zero
    rows; of the 163 calls 22 hit the deadline and 26 returned an error.
  - The CLI leaves no usage record: `ohya sql` builds the same engine with
    no observer (cmd/ohya/recall.go `openRecallBackend`), so `ohya usage`
    counts MCP calls only and the 40 sessions are a lower bound. A
    transcript search finds `ohya sql|search|show|sessions` mentions in
    shell commands of at most 31 more sessions. Decision 2026-11-25: keep
    this gap. A usage directory per CLI call does not work, because
    `CleanStaleInstances` keeps only the last 8 stopped instances per role.
    The two options were transcript evidence of shell calls in
    `ohya usage`, or one shared append file for CLI records. Revisit when a
    decision needs CLI counts. `--remote` CLI calls are already recorded
    on the hub.
  - Old MCP builds after an upgrade (2026-11-25): with v0.7.1 installed,
    both `ohya mcp` processes of open Claude Code sessions still ran the
    deleted v0.7.0 binary. MCP recall still cut `task-…` names and printed
    the old deadline text, and `ohya status` showed them as running. The
    `mcp_build` check of `ohya doctor` (`e1a6415`) now warns about them.
    The check first read `<state>/observe` only, so a joined device, whose
    MCP servers publish to `<state>/join/observe`, reported nothing. It
    now reads the observe directory of the state directory the doctor
    examines. With v0.8.0 installed (2026-11-25, 22:45Z), one such
    process remains: PID 555004, build `60eb331` of v0.7.0. `ohya setup`
    reported it as an `mcp-builds` warning at the end of the upgrade, and
    `ohya doctor` reports it as `mcp_build`. `/mcp` in that session
    renews it.
  - The check compared the build revision only (fixed 2026-11-25). A
    development build with uncommitted changes was not reported when its
    revision was the one the doctor compared with. The check now compares
    the revision and the uncommitted flag. Two uncommitted builds of one
    revision cannot be told apart, so the check reports them and says so.
    Tests: `TestCheckMCPBuildsComparesUncommittedBuilds` and
    `TestDoctorComparesUncommittedBuilds`. Each of 12 mutation checks
    failed a test. Limit: a test binary carries no build stamp, so the
    line of `buildIdentity` that reads the stamp has no test. It is the
    same comparison as `startTelemetry`, which writes the stamp into the
    snapshots.
  - An untracked file in the root makes each development build of the
    tree uncommitted. The stray `erl_crash.dump` of 2026-11-22 (Erlang
    28.4.1 of mise did not start) was moved out of the repository on
    2026-11-25. That Erlang starts without a crash now.
    Setup reports them since 2026-11-25, as docs/installer-plan.md asks.
    The result `mcp-builds` has the status `warning`: it keeps the exit
    code 0 and names the remedy, because setup never stops a coding
    session. Setup reads the processes before it starts its own memory
    process and from the observe directory of the state that it
    configures. Tests: `TestSetupReportsMCPServersOfAnotherBuild` and
    `TestSetupPrintsMCPServersOfAnotherBuildAfterARun`. Each of 9
    mutation checks failed a test.
  - `sessions.repo` is NULL for 2,888 of 7,031 real sessions (41%): 1,071
    Claude Code looper-worktree sessions that started before 2026-11-16
    (paths first observed later, worktrees since deleted), 1,461 Codex
    sessions outside a git tree, and all Antigravity sessions (62 at that
    hour, 73 later the same day; the parser records no cwd). Every
    looper-worktree session since 2026-11-18 resolves.
  - `SELECT count(*) FROM events` scans `sqlite_autoindex_events_1`: 3.1 s
    warm, over the 5 s deadline on a cold cache. `sum(event_count)` on
    `sessions` is instant.
  - `ohya secrets scan` (17:12, v0.7.0) took 12 min 24 s and reported
    8,997 matches in 5,813 events and 1,925 sessions. A rescan at 21:24
    with a build of `41cc624` (3,197 s under load) reported 5,185 matches
    in 2,613 events and 702 sessions. Per pattern, before and after:
    openai-api-key 4,714 and 505, env-assignment 3,627 and 3,760,
    anthropic-api-key 106 and 194, private-key-block 147 and 149,
    aws-access-key-id 237 and 445, google-api-key 92 and 29,
    github-token 46 and 31, slack-token 28 and 72. Four patterns grew
    because of one new session, the Claude Code session that did this
    work (543 matches in 85 events: test fixtures and doc examples).
    1,224 sessions left the list (2,899 matches, all `sk-` inside
    identifiers); 382 sessions kept the same events (2,199 and 2,179
    matches); 319 sessions kept fewer matches (3,899 and 2,463).
  - Secrets triage list (2026-11-24, built from the rescan; one line per
    session; counts, shapes, and variable names, never a value; kept
    outside the repository): 427 sessions hold 2,493 plausible matches
    and 2,692 are dropped as placeholders, dates, path segments, or
    low-entropy values. Priority 1 (a known key shape or a private key
    block of 600 characters or more): 61 sessions (claude-code 13,
    codex 48). Priority 2 (another token shape or a small key block): 38
    (antigravity 1, claude-code 5, codex 32). Priority 3 (environment
    assignments only): 328 (claude-code 21, codex 304, pi 3). The first
    estimate after the 17:12 scan (about 250 values in 197 sessions) was
    too low. The owner reviews the list: rotate first, then purge.
  - Query failures: of 163 MCP `sql` calls, 22 hit the 5 s deadline and 26
    returned an error. Agents use `LIKE` in 71 calls (44%); `LIKE` appears
    in 12 of the 23 slowest calls and a wide `events` scan in 13. No prior
    decision exists on a substring index (FTS5 trigram) or a deadline hint.
  - Antigravity scoping: 73 of 73 sessions have no cwd, repo, or branch.
    The parser reads cwd only from a `Command Working Directory:` line in a
    system message. A count on 2026-11-24 finds the text in 10 of 8,600
    Antigravity events: `planner_response`, `generic`, and `checkpoint`
    records of one conversation, and no system message. An earlier note
    here said that no record carries the line. Fixed on `main` for the
    next schema release (ADR 0018; see "Antigravity workspace" below):
    the sessions get a `cwd` when the hub and each device run a build
    with schema 9.
  - `ohya doctor --deep` passed on this hub on 2026-11-24: 19 findings,
    all `ok`; `events_fts` is consistent with `events`. The run took
    24 min 45 s (about 10 minutes for the copy, 15 for the check) with a
    local build of `3bf3d50`. The installed v0.7.0 holds the code before
    that fix, and a regression test shows that this code does not finish a
    copy while another connection commits (see "Migration recovery copy
    cost" below). v0.7.0 was not run with `--deep` on the live hub. Before
    the fix, the help and four documents said that the check copies the
    database into memory; it copies to a temporary file under TMPDIR and
    needs free disk a little more than the database.
  - `ohya purge` takes one `--session` per run, so a list of 427 sessions
    needs 427 runs. Decision 2026-11-24: keep that friction for a
    destructive command. `xargs -n 1` runs a list.
  - `sources.file_identity` was dead: migration 001 declared it, the sql
    tool description and `observe.ReadCatalogue` listed it, and no code
    wrote it (0 non-NULL rows on the hub). Migration 009 drops it
    (2026-11-25), and both lists no longer name it. See "Schema 9
    upgrade path" below.

- Hub fixes 2026-11-24 (plan approved by the owner: correctness, error
  text of the pull path, and documents; no hooks and no injection into a
  session, see PRODUCT.md "Pull, never push"). The commits were made on a
  stale base and sit on top of `origin/main` since the merge `f62cdc5`
  (see "Integration 2026-11-24"). The first two groups need no schema
  change and are released in v0.7.1; the Antigravity workspace is schema 9
  and is on `main` only.
  - Redaction (`41cc624`): the six prefix patterns start with a left
    boundary that stays in the output, so `task-`, `desk-`, `disk-`,
    `ask-`, and `risk-` identifiers stay whole. Accepted risk, recorded in
    the amendment of ADR 0015: a key glued to a letter, digit, hyphen, or
    underscore is no longer redacted.
  - `doctor --deep` (`0107da4`, `771aea1`, `7baf6df`): one space rule
    shared with the recovery copy, a guard before the snapshot, an error
    of its own for an inconsistent index, and "could not run" with the
    cause for each other failure. The help and four documents no longer
    say that the check copies the database into memory.
  - Documents of record (`c11ef2b`): the usage doc says that CLI calls
    write no usage record, the secret scan section has the recipe from
    scan to purge, and README and PRODUCT.md state the pull-only rule.
  - Query deadlines (`6b503a6`, `254d0f4`, `368d5d8`): the trace of
    functions and columns is always on, and a deadline error ends with one
    hint (`LIKE` over `events`, `count` over `events`, or a narrow query).
    `like` is in the function catalogue, so `ohya usage` reports it. No
    trigram index was added: watch the share of deadline errors in
    `ohya usage` over 30 days first (baseline: 22 of 163 calls).
  - Test helper: `oldHub.kick` in `internal/uploader` closed nothing, so
    the "after reconnect" assertions of the old hub test held without a
    reconnect (fixed in `2812eb5`).
  - Concurrent open (`2ff0478`, added outside the plan): two processes
    that open the same database at the same time apply each migration
    once, and `store.Open` repeats its attempt on a busy error. This is
    the fix of the `TestHubSetupTwoStepFlow` failure (see its entry
    below, under "TestTimeoutRetries").
  - The released-schema lists in DB.md name v0.7.1 since `5fcd091`.

- v0.7.1 release (2026-11-24, 22:40:31Z; the tag points to `08b3538` on
  the branch `release/v0.7.1`, which starts at `5f2d015` with eleven
  cherry-picks): candidate run 45499259636 passed all eight jobs on one
  run; the macOS arm64 job queued for about eight minutes for lack of
  capacity. Before the tag: four checksums, four `candidate.*.txt` files
  (revision `08b3538`, `dirty=false`), and the native library reports
  were verified, and the downloaded `install.sh` differed from the commit
  only in the pinned version. Six assets, as for v0.7.0. The released
  installer upgraded the hub from v0.7.0 in 14 s: `backup: not needed`
  (schema 8 on both sides), `service` and `hub-service` running, four
  clients unchanged, `ohya doctor` 20 ok (exit 0) after 144 s without
  `--deep`, and `ohya status` on v0.7.1. The MacBook synced again at
  22:57:48Z, 13 minutes after the hub restart. Each `--hub` run issues a
  join code that expires after 15 minutes; the installer output went to a
  private file, which was deleted after a redacted read. Not done: the
  DMG was not opened on a Mac.
  - The dry run of an upgrade must use the extracted binary of the new
    release. The installed v0.7.0 answered `ohya install --version
    v0.7.1 --hub --dry-run --json` with "installer release identity
    v0.7.0 differs from requested v0.7.1".
  - Two full checks (`GOFLAGS=-count=1 make check`) of `main` at
    `5fcd091` each failed one timing test, and the release commit
    `08b3538` passed its own two checks before the tag. The first run
    failed `TestTransportReporting` (`protocol_compatible = <nil>; want
    true`, 536 s) while an 18 GB copy and the test run loaded the disk.
    The second failed `TestIdleKeepalivePreventsReconnectProbeStorm` (4
    extra round trips) under the load of the parallel suite and passed
    `cmd/ohya` in 374 s. Each test passed 60 times alone. The keepalive
    test has a known cause and a fix on `main`; see its entry below,
    under "TestTimeoutRetries".

- Integration 2026-11-24 (merge `f62cdc5`): the 17 commits of this day
  were made on `f115b69`, nine commits behind `origin/main` (the v0.7.0
  release work, fetched 2026-11-23 23:05). The cause: no `git fetch` and
  no `git status -sb` ran before the work began, and `git status` alone
  does not show a branch that is behind. Rule: run both at the start of
  a session in this worktree.
  - The remote `9d86d67` probes a source that the hub knows and skips
    its registration. `2812eb5` sent the `sourceWorkspace` frame after
    that registration, so a known source never sent its workspace; after
    an upgrade, no existing source would. Three tests of `2812eb5`
    failed on the merged tree. The frame now follows the probe of a
    known source too, and one new test covers it.
  - `KeepaliveInterval` of the remote and `SourceWorkspace` are both in
    `uploader.Options`. `Uploader.offer` holds the capability offer for
    the connection and for the keepalive.
  - `docs/release-operations.md` holds the v0.7.0 entry (`5f2d015`). A
    bullet in the measurements above said it had none, and is removed.
  - Checks on the merged tree: `GOFLAGS=-count=1 make check` passed
    twice (`cmd/ohya` in 258 s and in 251 s), and the tests of
    `internal/uploader` and `internal/hubsync` passed with the race
    detector. `make tla` was not rerun: neither side changed `tla/`. The
    Swift app changes of the remote were not built here: this VM has no
    Xcode.
  - `docs/sync-protocol.md`, ARCHITECTURE.md, and `tla/README.md` describe
    the keepalive (`9d86d67`, 3 minutes) and the probe before the
    registration (2026-11-25). The hub closes a connection that sends no
    frame for 5 minutes, and an idle uploader sends one `caps` frame in
    3 minutes. A source with a checkpoint is probed first and registered
    only when the hub answers `CodeUnknownSource`.

- Antigravity workspace (2026-11-24, ADR 0018; `c220c9e`, `ed9b421`,
  `d66bfc6`, `2812eb5`, `5c62866`): Antigravity transcripts carry no
  working directory, so no session of that agent had a `cwd` or a `repo`
  (73 of 73: 68 on the hub machine, 5 on the MacBook). Rule: the `cwd` of
  a session is its first record `cwd`, else the workspace of a source that
  holds its events. The device reads the workspace of a conversation from
  `conversation_summaries.db` next to the brain root and sends it in the
  `sourceWorkspace` frame (marker `F`, capability bit 2). The hub stores
  the first path of a source write-once in `sources.workspace_path`.
  - The index file is not documented by Antigravity. ohya opens it with
    `immutable=1`, which reads the main file only and not the write-ahead
    log: a conversation that Antigravity has not checkpointed is missed
    once and found by the next lookup. Checked with a temporary database:
    no `-wal` or `-shm` file appears and the file bytes stay unchanged.
    For contrast: a plain read-only open of the real index by an
    exploration agent created both files in the Antigravity directory at
    18:08 on 2026-11-24. Both were gone at 21:00, after the index itself
    changed at 19:46.
  - The parser is unchanged and `OutputVersion` stays 1, so no re-derive
    is forced.
  - Model: `SetSourceWorkspace` in Auth.tla. The final model has
    18,286,389 states (Auth, 3 min 34 s) and 51,982,533 states (AuthWide,
    11 min 17 s), 3.9 times the counts before the action and under the
    30-minute bound. Two of three mutations are caught; the third
    survives by design (tla/README.md).
  - Limits: the first workspace of a source is final, and a later change
    in the index is ignored. The upgrade order is hub first: an old hub
    advertises no bit 2, and a new device then sends no frame and logs
    one line per process.
  - Open until the release: schema 8 to 9 costs one full recovery copy
    (about 17 GiB) at the next `ohya setup`, so check the free space
    first. Measured on 2026-11-24 with a current build: the copy of
    16.91 GiB took 11 min 21 s while a test run loaded the disk (26 to
    71 MiB/s). Budget about 15 minutes with ingestion stopped and about
    20 GB of free space; the kept recovery point holds 18 GB afterwards.
    The installed v0.7.1 read the migrated copy, so processes that were
    started before the upgrade keep their read access. Then run
    `SELECT count(*), count(cwd), count(repo) FROM
    sessions WHERE agent = 'antigravity'`. 25 of the 68 conversation
    directories on the hub machine are gone, so some sessions get a `cwd`
    and no `repo`, which is correct. The MacBook fills its 5 sessions
    when its ohya is upgraded.
  - Decision of the owner (2026-11-24): migration 009 is not released,
    so two deferred schema items go into it at no extra recovery copy —
    the removal of `sources.file_identity` and the index
    `events(source_id, parser_version)` of the doctor note ("Doctor cost on
    a 6.6 GB hub"). Done 2026-11-25; see "Schema 9 upgrade path" below.

- Schema 9 upgrade path (2026-11-25; decision of the owner for v0.8.0):
  - Migration 009 now also drops `sources.file_identity` and builds the
    index `events_source_parser_version` on `events(source_id,
    parser_version)`. The `parser_versions` check of `ohya doctor` reads
    it through a grouped subquery.
  - Measured on a read-only copy of this hub (9,415 sources, 2,892,749
    events, 18.15 GB). Without the index, the old query took 3 min 52 s
    and the new one 4 min 52 s on a cold cache: `parser_version` is the
    last column of `events`, after the large text columns, so each scan
    reads the overflow pages of every row. Checked on a synthetic table
    of 20 KB rows: a scan of the last column read 966 MiB from disk and
    a count of the rows read 196 MiB. With the index, the new query
    took 0.18 s and the old one 1.09 s. The migration (`store.Open`)
    took 3 min 41 s and grew the file by 117 MiB.
  - Tests: the query and its plan in `internal/store/doctor_test.go`,
    and the columns of `sources` and the plan after each released
    upgrade in `migrate_released_test.go`.
  - `ohya setup` applies the pending migrations after the recovery point
    and before it changes a service file (ADR 0008, "Recovery points"),
    and reports `schema: updated` or `schema: failed` with the cause.
    A `Type=simple` service is active before its first open of the
    database ends, the hub probe of setup allows 30 s, and a service
    whose open finds the database busy now waits for the write lock (it
    gave up after one busy timeout of 5 s before 2026-11-25). A migration
    inside a starting service therefore outlasts the probe on this hub.
    That failure of the old flow follows from the code and from the
    held-transaction check in the next bullet; it was not reproduced with
    a real migration.
  - Checked with a held write transaction and a pending 009: a second
    writer open failed after 5.0 s with `store: migration
    009_source_workspace: begin: database is locked`. A read-only open
    and its query worked at once. So before the change a new `ohya mcp`
    process that started during the migration of a hub failed, and that
    agent had no memory tools until its MCP server restarted. Processes
    that are already open keep their access. Fixed on 2026-11-25: the
    open of `ohya mcp` and of both services now waits for the lock (see
    "Store transactions and the write lock" below). An agent session that
    starts during the migration still has no memory tools for that time.
  - A batch that the running hub service commits during such a migration
    now waits for the lock and gives up after about 15 s with no
    acknowledgment; before the change it was refused after 302 ms. The hub
    service of v0.7.1 gives up at once. See "Store transactions and the
    write lock" below.
  - Tests in `internal/setup`: the migration runs before the first
    service command; setup stops before the service file when the
    migration fails; a database without a schema stays with the service.

- Store transactions and the write lock (2026-11-25; measured; decided by
  the owner the same day):
  - Before this change each write transaction of the store started with a
    deferred `BeginTx` and read before it wrote, so the first write
    statement failed at once when another connection held the write lock.
    SQLite skips the busy handler for that change. A held lock failed a
    hub batch after 302 ms with three attempts and no acknowledgment; the
    device kept its checkpoint, so no event was lost.
  - Live journal before the change: `ohya.service` logged `database is
    locked` 1,508 times from 2026-11-16 to 2026-11-25: 12 on 2026-11-16,
    2 on 2026-11-18 and 1,494 from 07:44 to 08:41 on 2026-11-20 (two
    sources of looper worktrees, up to 58 a minute). `ohya-serve.service`
    logged none. The journal does not show which process held the write
    lock in that hour.
  - Change: `writerDSN` has `_txlock=immediate`, as the registry DSN has
    since 2026-11-13, and the read-only `PlanPurge` reads through a reader
    pool. A write transaction now waits up to the busy timeout of 5 s. A
    hub batch under a longer lock gives up after three attempts in about
    15 s, within the device request timeout of 60 s. The hub service of
    v0.7.1 gives up at once during the first upgrade with this build,
    because it runs the old code.
  - Limit: a context that ends does not shorten the wait. The default busy
    callback of SQLite 3.54.1 does not check the interrupt flag (main.c,
    `sqliteDefaultBusyCallback`), so a canceled `BeginTx` returns with
    `database is locked` after one busy timeout. A service that stops
    while it waits for the lock can take 5 s longer.
  - Tests: `TestWriteTransactionWaitsForTheWriteLock` and
    `TestPlanPurgeNeedsNoWriteLock` (`internal/store/open_test.go`), and
    `TestBatchWaitsForAnotherWriter` (`internal/hubsync`). Each fails on
    a deferred DSN or on a plan through the writer.
  - The time of the `internal/store` tests swings with the load of this
    machine (10 s to 49 s for one package) and does not change with the
    transaction mode.
  - Open wait (2026-11-25): `store.WaitForLock(report)` makes `store.Open`
    repeat its attempt until its context ends and call `report` after a
    busy attempt, at most once per busy timeout. `ohya mcp`, `ohya serve`
    and the ingest service open with it. One-shot commands and `ohya
    setup` stay bounded, and their busy error now names the lock and the
    wait.
  - `ohya mcp` opens the database in the background
    (`mcpserver.Opening`). The handshake and the tool list need no
    database. A tool call that arrives before the open ends waits up to
    its deadline and then returns "the hub database is still opening". An
    open error stops the server, and the process exits with that error.
  - Measured: an up-to-date database needs no write lock to open. A
    bounded open under a held `BEGIN IMMEDIATE` succeeded at once, and
    only a pending migration made it wait. The command tests therefore
    hold the database with a connection in `locking_mode=EXCLUSIVE`, which
    blocks every statement of another connection. That lock ends only when
    the connection closes; a rollback does not release it.
  - Limit: a process that stops while its open waits for the lock exits up
    to one busy timeout later, because SQLite does not shorten the wait.
    Claude Code stops a stdio server with SIGINT, then SIGTERM after
    100 ms, then SIGKILL after 400 ms more, so `ohya mcp` is killed in
    that case. That is safe: the open holds no transaction, and a
    migration is atomic.
  - Tests: `TestOpenWaitsForTheLockOnlyWhenAsked`,
    `TestRetryBusyWaitsUntilTheContextEnds` and
    `TestAtMostEveryReportsOncePerInterval` (`internal/store`);
    `TestOpening*` and `TestServerAnswersWhileTheDatabaseOpens`
    (`internal/mcpserver`); `TestMCPStdioServesWhileTheHubDatabaseIsLocked`,
    `TestMCPStdioExitsWithTheOpenError` and
    `TestHubServicesWaitForALockedHubDatabase` (`cmd/ohya/hub_lock_test.go`).
    Each fails when its part of the change is reverted.
  - Research note: /notes/2026-11-24-concurrent-open-migration-race.md.

- A stray build was deleted by mistake (2026-11-24, 22:23): a scratch
  cleanup ran `rm -f` with relative paths while the shell stood in the
  repository root, and removed `ohya` there (50,636,568 bytes, built on
  2026-11-23 at 13:13). Git ignores that path, so `git status` never
  showed it. No service, MCP configuration, or running process used that
  path: all of them run `/home/agent/.local/bin/ohya` (v0.7.0). No copy
  of the file exists. `make build` writes a build of the current tree to
  `bin/ohya`; that build has schema 9, so do not run a write command of
  it against the live hub before the release. Rule: delete with absolute
  paths, after `ls -l` of each target.

- Competitive clock — claude-mem (verified 2026-11-24): thedotmack/claude-mem
  v16.1.2 (Apache-2.0, 119,454 stars, 395,623 npm downloads in 30 days,
  15 releases in 12 days) is the category leader on agent memory, with the
  opposite design. Hooks inside 20+ agents send each tool call to a local
  worker; a second model (default `haiku` through the Claude Agent SDK)
  writes observations and a session summary; up to 10,000 characters are
  injected at SessionStart; 19 to 26 fixed MCP tools (about 14 KB of schema
  by source estimate); SQLite FTS5 plus a Chroma sidecar. Capture is
  at-most-once by owner decision (one 250 ms live handoff, RAM buffer,
  5-minute expiry; `docs/architecture/observer-lifecycle.md`: "An outage can
  leave a gap in generated memory") because an LLM reducer cannot be replayed.
  Past sessions are not imported; tool fields are cut to 16,000 characters
  before the model sees them and `tool_uses` rows to 64 KiB. Cloud Sync is
  opt-in, plaintext to sync.cmem.ai, one shared token, derived rows only;
  TeamBrain is hosted and paid. Defaults: telemetry on, auto-update on,
  redaction off, local API open on loopback. No formal verification. Open
  flanks: #4159 (warmup pings burn billed calls), #4969 (an update broke
  hooks), #4841 (no origin machine on synced memories), plan trackers
  #3602-#3610. Where it leads: zero-effort push recall, harness breadth, a
  web viewer. ohya's wedge stays: lossless raw records, full history import,
  deterministic replay, no model cost, bounded SQL, operator hub, checked
  sync. Full study: /notes/2026-11-24-claude-mem-vs-ohya.md. No follow-up
  task filed; the study lists candidate actions (recall-bench arm, dated
  comparison sections, a decision on a deterministic session-start index).

- Device-side exclude globs landed 2026-11-20 (T147): one repeatable
  `--exclude GLOB` flag on ingest, join, and setup; one enforcement point
  (discoverSessionFiles in cmd/ohya/ingest.go — an excluded path is never
  registered, read, or sent); visible accounting (pass summary clause,
  `excluded` map in ingest --once --json, `discovery_excluded{agent}`
  gauge rendered by `ohya status`, startup line naming the globs, join
  logs each agent's skip count on change). Facts worth keeping:
  - Matching is filepath.Match (path.Match semantics) against the
    absolute path or its symlink-resolved form; `*` never crosses `/`, so
    a deep tree needs a glob per level — deliberately no `**` (operator
    decision 2026-11-19).
  - Flag-time validation calls filepath.Match(glob, "") — Go's matchChunk
    keeps validating a chunk after a mismatch, so `ok[` is caught, but a
    malformed post-star chunk only surfaces at runtime when an earlier
    chunk matches; discovery then fails the pass loudly.
  - An exclusion arriving after registration: the watcher keeps reporting
    the source Missing (its checkpoint entry survives); ingest and the
    uploader print one exclusion-specific line instead of the
    "disappeared" wording by recognizing the missing path in the pass's
    excluded-path set (both path forms stored, because watch state keys
    resolve parent symlinks — macOS t.TempDir is /var → /private/var).
    watch_sources{state=missing} still counts it; the excluded gauge
    reconciles.
  - setup stores the globs newline-joined in preferences.Flags["exclude"]
    (map[string]string forces one string; Set splits on \n, so restore and
    a pasted multi-line value behave the same); an empty value means no
    excludes — `--exclude ''` clears the list, the same empty-means-none
    convention as the root flags, and Set("") must be a no-op clear or
    every setup rerun over a saved `""` dies with "--exclude glob is
    empty" (found by the setup e2e suites). Service args carry one
    `--exclude GLOB` pair each.
  - Deliberately not built: guardian-class defaults (evidence-gated task
    against the 2,507-file Codex corpus if ever), regex/doublestar,
    per-source toggles, hub-side push-down.

- Competitive clock (observed 2026-11-15, openai/codex main): Codex is landing first-party memory (`memories/write`; `memories_1.sqlite` now present in `~/.codex`), a SQLite-backed `message-board` + message-board-mcp crate, and `external-agent-migration` importing Claude Code sessions. Cross-agent, local-first, shared remain ohya's wedge; factor into launch sequencing. Their session search still greps raw JSONL (ripgrep with JSON-escaped terms, search.rs) plus a bolted-on SQLite listing index — converging on ohya's design from both sides.

- Parser format facts cross-checked against agentsview (2026-11-13, commit
  b262670; full study at /notes/agentsview-ingestion-study-2026-11-13.md):
  - Claude Code: session ID is the filename stem; a differing in-file
    `sessionId` marks a parent/continuation session. Consecutive assistant
    records sharing one `message.id` are streaming chunks of one message and
    must merge. Subagent transcripts live under
    `<project-dir>/<session>/subagents/agent-<id>.jsonl`. Tool results can
    reference a side file ("Full output saved to: ..."). Recorded `cwd` beats
    the encoded directory name for project identity; worktrees make cwd-only
    identity brittle, so keep `git` evidence from records when present.
  - Codex: agentsview claimed rollouts older than about one week are
    zstd-compressed to `.jsonl.zst` in place; this machine's corpus
    contradicts it (2,488 files back to February 2026, all plain JSONL,
    verified 2026-11-13), so treat the claim as unconfirmed. The ingest
    walker only picks up `.jsonl` files, so a `.zst` would be skipped
    silently; if one ever appears, decide skip-with-diagnostic versus a
    streaming decompressing reader then. `session_index.jsonl` is a
    shared, rewritten index — never treat it as an append-only source.
    Forked/subagent rollouts replay the parent's whole history with
    re-stamped timestamps; gate on turn-ID membership or usage double-counts.
    `event_msg.token_count` usage attaches to the last usage-less assistant
    message in the current turn (never drop tokens silently); distinguish
    "zero tokens" from "field absent" with presence flags.
  - Timestamps can disagree with file order in Claude transcripts (queued
    prompts, hooks); record both and sort deterministically.

- Cursor/checkpoint design inputs from agentsview (2026-11-13, study §1):
  gate appends on inode/device + size + mtime + ctime ("a write restores mtime
  but cannot restore ctime"); verify the committed prefix with a SHA-256
  digest of its last 128 KiB; keep a resumable hash state so appends hash only
  the suffix. Advance the cursor only through the last complete valid JSON
  line; if an append contains zero complete records, keep the cursor and write
  no skip-state, because writers can restore the old mtime. Split small
  stat-gate metadata from larger resume blobs. These belong in device-local
  state files (per ARCHITECTURE.md), not the hub schema.

- Version parser output separately from schema (agentsview "dataVersion",
  179 as of the study): every parser behavior change re-derives rows. Because
  ohya preserves raw records, re-derivation can run inside the hub from stored
  raw JSON, without source-file resync. Plan the version marker and a
  re-derive migration before the first parser ships.

- Malformed complete lines: ADR 0004 accepts stopping at the offending
  offset without skipping bytes. Repair the source file, or raise
  `--max-record-bytes` for a size stop. A degraded `ingest --once` pass
  exits 3 and reports each stopped source with its remedy.

- Multi-machine ingestion may not need networking first: agentsview supports
  out-of-band transport (rsync/git/shared mount) into machine-labeled source
  roots, one ingesting instance, dedup by native session ID. A "copied roots"
  source mode could deliver shared history before the Tailcat spike de-risks
  pairing and relays. Consider the ordering in BACKLOG.md item 4.

- MCP memory patterns for `sql`/`annotate` (2026-11-13, study §6): exclude
  sessions active in the last 10 minutes by default so agents do not retrieve
  their own live session; document revision-bound pagination in the tool
  description; treat transcript text returned to agents as hostile
  (deterministic injection-bait labels, secret redaction over decoded JSON);
  fail closed on unreviewed derived values.

- FTS/storage lessons for the hub (2026-11-13, study §4): store
  content-bearing text once — duplicated tool-result summaries cost agentsview
  about 40% of file size; drop FTS triggers and derived indexes during bulk
  import and rebuild after ("any row trigger makes SQLite stage each multi-row
  INSERT through a temporary table"); keep a one-transaction `rebuild` path
  for `events_fts` from day one.

- macOS distribution still needs native builds on both supported architectures and
  signing/notarization verification; the SQLite driver requires a C toolchain.

- Claude Code parser normalization choices (2026-11-13, internal/claudecode; relevant to T006
  aggregates): searchable text per record concatenates block text in source order, and a
  tool_use block contributes `name + compact sorted-key JSON of input` (so commands and file
  paths are searchable); bookkeeping records contribute no text, and only `user` and
  `assistant` records read `message` (2026-11-25, see the request-log bullet at the end of
  this file). Role comes from message.role,
  falling back to the record type for user/assistant; tool_name is set only for exactly one
  tool block. Only the `timestamp` field sets time_ms (cost-state `startTime` is ignored).
  Records missing sessionId get native ID `ohya-synthetic:<source-scope>` — subagent
  `started`/`result` records carry no sessionId, so they land in that synthetic session by
  design. The record size limit counts JSON bytes excluding the line terminator and also stops
  an incomplete tail already above the limit. A valid-JSON non-object line (array, null) is
  malformed and stops the source, like invalid JSON.


- Codex parser choices (2026-11-13, internal/codex, T011): the record at
  offset 0 names the thread (`session_meta.payload.id`); a parse past offset 0
  needs the committed first record's native ID (`store.SourceState`), so a
  source whose offset-0 record is missing from the hub cannot parse
  (`codex.ErrNoNative`, reported, never guessed). Text comes from
  `response_item` only; `event_msg/user_message` sets `Prompt` (first_prompt)
  because Codex also sends AGENTS.md and `<environment_context>` blocks as
  `user` messages. Replays dedupe on `(session, ordinal)` plus content in the
  write path (migrations 003 and 005); `session_meta` never takes an ordinal,
  because a page's opening meta carries the ordinal one below its first
  record — an ordinal the original file already owns.
  Verified 2026-11-14 (T044, whole corpus): `cache_write_input_tokens` is 0
  in all 310,936 occurrences, so its subset relation stays unverified and it
  stays unmapped; no pre-0.147 resumed page exists (all 7 paginated pairs are
  0.157.1+ and every page record carries an ordinal). Also disproven: a real
  page never byte-replays the original — at all 140 colliding ordinals the
  two files hold different records, because the page re-enumerates after a
  rollback (`history_base.end_ordinal_exclusive` names the durable prefix;
  the aborted turn's records sit in the original only). End-to-end ingest of
  the 7 pairs: 140 suppressed "duplicates" that pair unrelated records and
  11 per-response usage records lost from token totals (3 threads). Facts
  and numbers live in `testdata/codex/README.md`; map cache-write to cache
  creation only after a non-zero sample appears.
  Resolved 2026-11-14 (T051): the dedup is keyed by content (migration 005)
  — a byte-identical copy of a stored original is a duplicate; a differing
  record at the same (session, ordinal) is another original, counted with
  its usage. Rollback-aware winner selection through `history_base` was
  rejected: it must demote stored originals whenever the original file is
  ingested before its page (the write path never updates stored rows), and
  it has no signal for the 3 pages that re-enumerate from ordinal 1 with no
  `history_base`. Migration 003's header still describes the disproven
  byte-replay assumption and must not be edited (applied-migration checksums
  are verified); migration 005 carries the correcting note.

- Pi parser choices (2026-11-14, internal/pi, T037): the `session` header at
  offset 0 names the session — same header-first rule as Codex
  (`pi.ErrNoNative`, FirstNativeID from `store.SourceState`), because no
  later entry repeats the session id. The usage-row key is the assistant
  `responseId`, falling back to the entry id (`Record.EntryID`, parser
  output version 2); an error response reports zeros under its entry id —
  exactly the verified per-response sum. Every user message marks a prompt
  (Pi persists no injected context as user messages; the 1.5.2 `system`
  entry is the preamble and contributes no search text). A toolCall block
  contributes `name + compact sorted-key JSON of arguments` (Claude Code
  tool_use convention). The entry-level ISO timestamp sets time_ms; the
  message-level Unix-ms mirror is not parsed.
  Verified 2026-11-14 (T038) against the 74 real files under
  `~/.pi/agent/sessions` (524 usage records: 517 zai via
  openai-completions, 7 anthropic via anthropic-messages):
  - `cacheWrite` is non-zero in 7 anthropic-messages records and strictly
    additive: `input + cacheRead + cacheWrite + output = totalTokens` in
    every verified record, and the sum without cacheWrite fails in all 7.
    Mapped to the cache-creation count since parser output version 2.
  - Non-zero usage without a `responseId` is the common case, not an edge:
    openai-completions records never carry one (503 successful responses),
    and 4 of 7 anthropic-messages records (the older 2026-04-20
    claude-opus-4-6 runs) lack it — 507 of 524 records would have silently
    counted nothing without the entry-id fallback (entry ids are unique per
    file, verified across the corpus). Errors still report all-zero usage.
  Open:
  - `reasoning` still has no non-zero sample — absent from every
    real-corpus usage object, 0 in the captured openai-responses records —
    so its subset relation (hypothesized: part of output) stays unverified
    and unmapped.
  - `cacheWrite1h` (the 1-hour cache write, on 3 claude-opus-4-8 records)
    is 0 everywhere; whether it is part of `totalTokens` or of `cacheWrite`
    is unverified, so it stays unmapped — recheck both open items if a
    non-zero sample appears.

- FTS5 verification gotcha (verified 2026-11-13, internal/store/open_test.go):
  `SELECT count(*)` on an external-content FTS5 table reads the content table, not
  the index. Assert index consistency with the `integrity-check` command and MATCH
  enumeration. Relevant wherever the write path maintains `events_fts` (T006+).
  Also (2026-11-14, internal/events tests): a bound MATCH parameter takes the
  bare query — `'"phrase words"'` as a parameter is a syntax error, because the
  single quotes are SQL string delimiters, not FTS syntax; pass `"phrase words"`
  and keep the single-quote form only for inline literals.
  Also (2026-11-20, T153): a bare hyphenated MATCH term parses as an FTS5
  column filter (`auth-refresh` → `no such column: refresh`, because
  `events_fts` has only the `text` column; other bare punctuation, for
  example `/`, → `fts5: syntax error`); double quotes turn it into a phrase
  over the split tokens. The rule is published in search --help,
  docs/reference.md, DB.md's search example, and the sql tool description's
  MATCH guidance, and the search error path appends the hint
  (`ftsQuotingHint` in cmd/ohya/recall.go fires on `fts5: syntax error` or
  on `no such column:` whose name occurs in the query text, so a genuine
  schema-drift column error stays unhinted).

- Source watcher choices (2026-11-13, internal/watch, T005): change detection
  applies three signals in order — platform identity (dev/ino on Unix), size
  against the acknowledged cursor, then the checkpoint fingerprint (SHA-256 over
  a 4 KiB window ending at the cursor). Identity catches replacement, size
  catches truncation, and the fingerprint catches same-inode rewrites, including
  truncate-and-regrow between polls. Accepted edges: a cursor-0 source can bind
  to a reused inode after its file is deleted (harmless — nothing was ingested);
  a device-number change across reboots (tmpfs) re-imports under new source IDs,
  which is retry-safe but not semantic dedup. State keys resolve symlinks in
  parent directories (macOS /tmp → /private/tmp) so one file keeps one key
  whether or not it currently exists; tests must compare against normalized
  paths. That includes the expected value: t.TempDir() on macOS sits behind
  /var → /private/var, so a literal temp-dir string never matches (T052,
  fixed 2026-11-14 by computing the expectation through filepath.EvalSymlinks);
  reproduce the condition on Linux by pointing TMPDIR through a symlink (keep
  that TMPDIR short — a long one trips the 108-byte unix-socket path limit in
  the serve e2e tests). Lost state is reported, never silently restarted: an
  unreadable
  device.json or sources/<agent>.json is a recovery error.
  Any code matching walked discovery paths against watch checkpoint keys must
  compare path forms: reportJoinBacklog missed this (macOS CI red,
  2026-11-20, T152) and counted every checkpointed source as fresh backlog
  because byPath held parent-resolved keys while walkSessionFiles returns raw
  paths; it now looks candidates up through excludePathForms (the T147
  both-forms helper), and TestReportJoinBacklogResolvesSymlinkedRoots pins the
  condition on Linux through a symlinked root.
- Hardlinks and the watcher (2026-11-13, internal/watch, T028): two live paths
  can share one platform identity (hardlinking backup tool inside the watched
  tree; reachable through `ohya ingest`, which passes every discovered *.jsonl
  to Poll). Poll watches an incarnation once — the first path in path order
  wins, duplicate candidates are refused — and the rename/move rebuild refuses
  a move whose target key another source claims (staying entry or competing
  move), so an acknowledged checkpoint is never overwritten; a refused source
  stays missing and keeps its checkpoint, and a returning path is reported as a
  replacement, not a silent "new file". A checkpoint file from a pre-fix build
  may already hold two sources on one identity: loadState tolerates it (ErrStateLost
  would brick upgrades for no recovery gain) and the poll guards keep both.

- Write-path open question (2026-11-13, internal/store, T006):
  `sessions.repo` derivation is now defined and implemented (2026-11-14, DB.md
  "Repository derivation": normalized repository URL from the session's own
  records; Codex `session_meta.git.repository_url`, others NULL; re-derivation
  recomputes it; codex parser output version 2). Tool aggregates remain
  deferred pending a query need. 2026-11-25: 274 historical sessions have a
  NULL `repo` although a sibling session has one; see the bullet "Historical
  sessions keep a NULL `repo`" at the end of this file (open decision).

- Local slice wiring (2026-11-13, cmd/ohya, T007): the production
  record → store.Event adapters live in internal/events (moved out of
  cmd/ohya in T017 so hub-side batch normalization shares them; T011 added
  the Codex adapter and the `events.Parse` agent dispatch; usage field
  names, contentForm=="string" prompt test). internal/store keeps a
  mirrored copy in ingest_test.go — an in-package test cannot import
  internal/events without an import cycle — so change both together. The
  crash window between the Ingest
  commit and watch.Acknowledge closes by replay on restart (slice test kills
  a helper process there; replay validates stored bytes and inserts nothing).
  Ingestion batches are bounded at 8 MiB so importing a large history file
  stays predictable in memory; a batch never splits a record — one read may
  extend past the 8 MiB bound to the end of a single larger record, never
  past the configured record size limit (T042, ADR 0004). A stopped source
  prints its diagnostic once per stop offset per run, then stays quiet until
  the offset moves; each diagnostic names its remedy (`--max-record-bytes`
  for a size stop, file repair otherwise). Fresh sources from replacement or
  truncation are reported because they re-import history, new files are not
  (a first import of thousands of files must not print a line per file).

- SQL tool boundaries (2026-11-13, internal/mcpserver, T008) — driver facts
  the read-only policy relies on, all verified by probe against
  mattn/go-sqlite3 v1.14.53:
  - The driver's `QueryContext` executes EVERY statement in the input and
    returns the last statement's rows (sqlite3.go `query` loops the prepare
    tail). One-statement-per-call is therefore enforced by our own scanner
    (`statement.go`) before execution; the authorizer alone would not stop
    `SELECT 1; <anything read-only-but-second>`.
  - This build's authorizer op codes differ from the classic sqlite3.h
    numbering (SELECT=21, READ=20, PRAGMA=19, FUNCTION=31, SAVEPOINT=32);
    always compare against the driver constants, never hardcoded values.
    SQLITE_RECURSIVE (=33 here) is not exported by the driver at all —
    `sqliteOpRecursive` hardcodes it, and the recursive-CTE test case pins
    it; re-verify on any driver bump.
  - FTS5 MATCH internally reads `PRAGMA data_version`, so a pragma
    allow-list must include it or all FTS queries die with "authorization
    denied". MATCH also fires FUNCTION op with arg2="match".
  - Authorizer denials surface with three different SQLite messages:
    "not authorized" (most ops), "access to table.column is prohibited"
    (SQLITE_READ), "authorization denied" (VACUUM). queryError maps all
    three to ErrDenied.
  - `CREATE TRIGGER ... BEGIN ...; ... END` trips the one-statement scanner
    (body semicolons). Harmless: statement blocks only exist in denied DDL.
  - MCP SDK v1.8.0 e2e stdio tests must use the TestMain fork-server trick
    (env var → run server instead of tests, like cmd/ohya/mcp_test.go):
    a test binary serving on stdout interleaves framework output ("PASS")
    with protocol frames. `mcp.NewInMemoryTransports()` covers the
    protocol-level tests without processes.

- Annotate tool choices (2026-11-13, internal/mcpserver + internal/store, T009):
  the SDK's inferred input schema already rejects unknown fields
  (struct inference sets additionalProperties: false) and runs BEFORE the
  tool handler, so a spoofed `writer_id` dies in schema validation; the
  handler re-decodes `req.Params.Arguments` (a json.RawMessage on
  CallToolParamsRaw) with DisallowUnknownFields for the cases the schema
  cannot express (upsert/delete value rule, charset, duplicates) and for
  better error text. jsonschema-go maps `json.RawMessage` to an array of
  integers — never put it in the schema-facing In type; keep a parallel
  `Value any` shape for the schema and raw types for decoding. Values are
  stored compacted, so "identical retry" means equal compact JSON (number
  spelling like 1e2 still counts as different text). Batch counts: applied
  = insert/replacement/delete-that-removed; unchanged = identical upsert
  or absent delete; an empty batch is accepted with zeros. `updated_at_ms`
  is clamped up to `created_at_ms` on update so a backwards hub clock step
  cannot trip the table CHECK and permanently wedge an annotation.

- TLA+/TLC tooling facts (2026-11-13, tla/, T015; relevant to T016's model):
  - TLC 1.8.0 jars from different builds differ in embedded build metadata
    only (identical file listing; the 2026-11-15 upstream rebuild moved only
    the Build-TimeStamp). The exact bytes are vendored at `tla/vendor/` and
    checksum-pinned (ADR 0007), so build drift between copies no longer
    matters.
  - cfg files accept only scalar sets/numbers/strings/booleans as constant
    values; records, tuples, and per-element atoms are rejected. That is why
    Sync.tla models "each source is one device's uploader" structurally
    instead of an owner function (device identity/ownership is T016 anyway).
  - TLA+ has no negative numbers and TLC rejects cross-type equality, so
    no-message sentinels are type-consistent: `<<>>` for the batch channel,
    `MaxLen + 1` (above every cursor) for the ack channel.
  - TLC needs `-deadlock`: the drained system's quiescent state is a
    legitimate fixed point. Its coverage column shows `0` distinct states
    for reset actions (DropBatch, crashes) — expected, their post-states
    are always co-reachable via benign paths; the actions still fire.
  - The reachability bound that keeps the state space small is real
    protocol structure, not a modeling gap: with one in-flight batch of at
    most MaxBatch records, the hub cursor can never lead the acknowledged
    cursor by more than that batch plus an undelivered ack.
  - Liveness with message loss needs the loss/crash actions disabled
    (ReliableNet constant): infinite-loss behaviors violate no fairness
    assumption, so eventual-sync cannot hold without the stated
    eventual-reconnection assumption. Liveness checking is the cost driver
    (12k states, ~13 s, ~1.1 GB) versus seconds for 54k safety states.

- Authorization model choices (2026-11-13, tla/Auth.tla, T016):
  - Immutability properties ("ownership cannot move") are invisible in a
    state invariant without history: a move overwrites its own trace. The
    model carries monotone history variables (`srcEverOwned`, `revokedEver`,
    `annRowSes`) and checks "at most one owner ever" as cardinality bounds;
    six one-line mutations each tripped a distinct invariant. One mutation
    survived by design (an active device consuming a second invitation
    breaks no stated property); the guard blocks it anyway.
  - "A revoked device cannot establish a new authorized session" forces
    revocation to be final per device identity: if a revoked identity could
    consume a fresh invitation, the property would be false. Re-admission
    therefore means a new device.json identity. Orphaned sources stay
    locked (no rebind) — the conservative reading of the sync contract.
  - Invitation expiry is a nondeterministic action, not a clock: the
    protocol fact is that consumption must race expiry and can lose; TTL
    length is configuration. Safety-only model — pairing liveness is device
    reconnection, owned by Sync.tla's eventual-reconnection assumption.
  - String sets work as cfg CONSTANT values ({"d1","d2"}), so the registry
    can be indexed by device atoms directly — no structural workaround
    needed, unlike per-element model values.
  - Auth.cfg 46k states ~1 s; AuthWide.cfg (3 devices, 2 invitations —
    pairing scarcity) 127k states ~3 s; all invariants hold.

- Hub sync endpoint choices (2026-11-13, internal/hubsync, T017):
  - The endpoint is transport-agnostic (serves any net.Listener) and
    authenticates through an Authenticator interface; the wire device ID is
    only ever compared against the authenticated identity, never trusted.
    T018's registry + Tailcat WhoIs will implement the interface.
  - Ownership is enforced at the single write path, not the endpoint:
    store.Batch carries the submitting DeviceID and Ingest rejects a foreign
    device (ErrSourceOwnership), so local ingestion and sync share one rule.
    Rejection classes are sentinels (ErrBatchRejected, ErrSourceOwnership,
    ErrSourceNotRegistered) — the endpoint maps them to protocol error codes
    and retries everything else with capped backoff.
  - A record the hub parser refuses mid-batch commits the valid prefix and
    acknowledges with a stop offset+reason, mirroring local source-stop
    semantics; the rest of that batch is dropped (the device repairs and
    resends). Skipping past the stop is a gap rejection.
  - Records are parsed per record (JSON + one synthetic terminator) with
    SourceScope = wire source ID — identical normalization to local
    ingestion, pinned by a test that compares the full events/sessions rows
    of both paths over the same fixture bytes. A record whose JSON contains
    a line terminator fails the "exactly one record at its framed
    boundaries" cross-check and is rejected.
  - SQLite trigger gotcha: trigger bodies reject bound parameters and
    RAISE(ABORT, …) fires inside the write-path transaction — the endpoint
    tests use one to break a commit mid-transaction and prove rollback
    leaves the cursor unchanged.
  - Serving is wired since T034 (`ohya serve`, see the wiring facts below).

- Device pairing choices (2026-11-13, internal/registry + internal/hubsync + internal/device, T018):
  - The registry is its own SQLite database (registry.db in the owner-only
    state directory), not tables in hub.db — the sql MCP tool's connection
    can never reach it, so no EngineOptions.DenyTables entry is needed. Its
    schema is pinned by PRAGMA user_version (single version, refuse newer);
    the store migration runner is deliberately not reused for a two-table
    protected store.
  - The registry DSN needs _txlock=immediate: two processes racing to
    consume one invitation must serialize at BEGIN. With the default
    deferred BEGIN, both run the SELECT, then both try to upgrade to a
    write lock and one dies with database-is-locked instead of losing
    cleanly with ErrInvitationUsed (pinned by TestConsumeRaceSingleUse
    through two handles).
  - Per-request check placement matters: Authorize must run AFTER the frame
    is read, immediately before dispatch. Checking before ReadFrame
    authorizes the idle wait, not the request — a revocation landing while
    the connection sits between frames was then missed for the next frame
    (found by test; the stale check passed, the frame was processed, the
    probe got an ack after the device was revoked).
  - Revocation teardown is poll-based: revocations can be committed by any
    process sharing registry.db (the ohya devices CLI), so the endpoint
    learns them by polling the revoked set (default 1 s) and then closes
    the device's connections and cancels their per-connection contexts;
    in-flight requests die through ctx-aware SQL and the retry loop.
  - Wire: first frame of a connection is 'P' (pair) or 'H' (hello); the
    hub mints the credential at Consume and delivers it once ('C'). The
    auth frames are read with the exact-read frameReader directly off the
    conn — no bufio — so the endpoint's later bufio.Reader starts exactly
    at the next frame. AuthTimeout is enforced with a conn read deadline
    around Authenticate.
  - Secrets (invitation and credential) are 128-bit crypto/rand hex,
    stored only as SHA-256 digests; "do not implement new cryptography"
    reads as stdlib primitives only. Revoked identities can never consume
    a fresh invitation (registry state must be "absent"), so re-admission
    is a new device.json — Auth.tla's RevocationIrreversible.

- Remote query/annotate path choices (2026-11-13, internal/hubsync +
  internal/mcpserver + internal/store, T036; ADR 0003):
  - Query ('Q') and annotate ('N') frames ride the sync connection; the
    endpoint serves them through hubsync.RemoteEngine, implemented by the
    mcpserver engine, so bounds and read-only policy live in one place.
    The import direction hubsync → mcpserver is deliberate: the sync
    protocol carries the remote MCP surface, and mcpserver stays free of
    sync concerns (no cycle; registry already imported hubsync).
  - Writer derivation is structural: the annotate frame has no writer
    field at all, so nothing can be spoofed.
  - Deterministic annotate rejections needed sentinels for truthful wire
    codes: mcpserver.ErrAnnotateRejected and store.ErrAnnotationRejected.
    Their text equals the historic error prefix ("annotate tool",
    "store: annotate"), so every error message stayed byte-identical while
    errors.Is now classifies CodeRejected vs CodeInternal at the endpoint.
  - The query result wire bound is 4 MiB (16x the engine's 256 KiB default
    output); an engine configured past it answers CodeInternal with the
    bound named — pinned by test. lpad does not exist in the bundled
    SQLite; use hex(zeroblob(n)) to generate wide deterministic rows.
  - RemoteClient.exchange sets conn deadlines per exchange and relies on
    the next exchange resetting them (uploader pattern); no deadline is
    left dangling between exchanges.
  - Deployment wiring landed (T034): serve attaches the engine as the
    RemoteEngine, and the stdio bridge is `hubsync.RemoteSession`
    (internal/hubsync/session.go) behind `mcpserver`'s `Backend`
    interface. On a non-protocol error the session drops the connection,
    reconnects, and retries once; a CodeAuth answer maps to
    ErrRemoteUnauthorized instead of looping with a refused credential.

- Device uploader choices (2026-11-13, internal/uploader, T019):
  - The uploader reuses internal/watch as its checkpoint store: the cursors
    are hub acknowledgments. A state directory shared with local `ohya
    ingest` would mix two acknowledgment sources and skip bytes, so the
    join command (T034) uses a dedicated one (`--state`, default ~/.ohya/join).
  - A pass with nothing pending makes no exchange, so a hub that restarted
    from a restored backup went unnoticed on an idle connection (found by
    test). Each pass now checks the open connection with a 1 ms read
    deadline plus bufio Peek: timeout = open, EOF/reset = reconnect and
    re-probe every source. A hub host that dies without FIN/RST is still
    noticed only at the next exchange or by TCP keepalive (unverified for
    the in-process netstack).
  - The endpoint writes CodeBusy and closes before reading the hello, so
    the device's first write can fail with a reset; the uploader then tries
    to read the hub's error frame before reporting a plain disconnect.
  - A parse stop is not resent until the file grows (in-memory mark per
    process); a restart resends the stopped bytes once.

- Parser output version and re-derivation choices (2026-11-13,
  internal/store + internal/events, T025): `events.parser_version` versions
  what a parser derives from one record, deliberately separate from the
  schema version (agentsview's `dataVersion` lesson). Rules: bump the
  parser's `OutputVersion` in the same commit as the behavior change;
  version 1 is the pre-marker output; a build refuses rows stamped above
  its own parser (upgrade first, then re-derive). `store.ReDerive` walks an
  agent's sources in `(source_id, source_offset)` order and each source's
  records in offset order, so re-derivation needs no arrival-order
  accidentals; the deriver binding (`events.Derivers`) re-parses one stored
  record with `SourceScope = source ID` (synthetic native IDs stay stable)
  and `FirstNativeID` fed by the preceding record, which is what Codex needs
  past offset 0. Open:
  - Duplicate reassignment switches from arrival order to walk order: a
    hub that ingested a resumed page before the original file changes
    which byte-identical copy is the original after a rebuild (only the
    row id in `duplicate_of` moves). Distinct records that share an
    ordinal stay separate originals in every order (T051).
  - `events_fts` has insert/delete triggers but no update trigger
    (ingestion never updates rows); re-derivation relies on the
    external-content `rebuild` command in the same transaction to restore
    index consistency — verify with `integrity-check`, never `count(*)`.

- Performance measurement choices (2026-11-14, internal/fixgen +
  internal/events + internal/mcpserver + docs/performance.md, T039):
  - `internal/fixgen` generates deterministic synthetic Claude Code corpora
    (seeded PCG; same seed, same bytes) shaped by the verified format notes:
    ~3.2 KB mean record, 2 MiB main sessions plus subagent files, skewed
    word distribution with rare marker terms so FTS benchmarks have both
    wide and selective queries. `cmd/genfixture` writes it to disk for
    runs of the real binary.
  - Per-agent corpora (T048): `GenerateCodex`/`GeneratePi` mirror the
    verified rollout and session shapes (subagent rollouts open with their
    own meta and embed the parent's; Pi uses the 1.5.2 shape with the
    system entry). All three generators share one text distribution
    (`textGen`), so parse benchmarks compare envelopes, not text volume —
    and the Claude Code bytes are pinned by the earlier measurements:
    refactor without changing the rng call order and verify byte-identity
    against a pre-change snapshot (done for the textGen extraction).
    Measured parse rates: Claude Code 127–132, Codex 173–178, Pi 156–159
    MB/s — same order, JSON decode dominating, so per-agent write-path
    benchmarks stay unnecessary (the path behind the parser is shared and
    fsync-bound). `genfixture -agent codex|pi` writes the per-agent trees
    the CLI discovers. The generators must keep the verified usage
    identities (Codex cached within input; Pi parts summing to totalTokens)
    — pinned by TestCodexCorpusParsesCompletely and TestPiCorpusParsesCompletely
    counting HasUsage against the records that carry usage.
  - The write path is fsync-bound, not CPU-bound: with synchronous FULL,
    the import rate swings 4–20 MB/s with disk contention on the shared
    reference VM, while parse-only stays 126–130 MB/s and replay validation
    ~1 GB/s. CPU profile: 67% of samples in CGO (SQLite inserts/commit),
    8–15% encoding/json; no hot ohya Go function — no Go-side optimization
    is justified (the levers are durability and batch size, both deliberate).
  - Database multiplier is structural, ~2.4x input bytes (raw JSON + text +
    metadata + FTS index); measured 351.6 MB for a 138.7 MiB corpus.
    Peak import RSS ~28 MB (streamed 8 MiB batches); no-op poll over 461
    sources 0.02 s; one live append pass 0.13 s; wide FTS MATCH ~0.5–1 ms
    per 1,000 matching events, everything else flat in volume.
  - The mcpserver query benchmark shares one seeded hub across benchmarks
    via sync.Once and closes it in TestMain; a per-benchmark cleanup would
    close the DB while later benchmarks still use it, and the temp dir must
    be removed by that TestMain (an os.MkdirTemp leak there once filled the
    disk on this machine).
  - Measured limits live in docs/performance.md, not docs/operations.md:
    the operations guide is T040's parked work (install/backup/recovery,
    blocked on macOS verification) and creating the same path here would
    collide when that work integrates; link performance.md from it then.
  - Regression guards run in make check with order-of-magnitude ceilings
    (ingest 24 MiB < 30 s, queries < 2 s) — they catch superlinear
    regressions, not machine drift; precise tracking is benchstat work,
    not automated.

- Memory verification gate choices (2026-11-14, cmd/memory-e2e, T041):
  - Claude Code 2.1.280 headless: `claude -p <prompt> --mcp-config <file>
    --strict-mcp-config --allowed-tools mcp__ohya__sql,... --output-format
    stream-json --verbose` runs with CLAUDE_CONFIG_DIR (plus HOME) redirected
    into the gate's output directory and authenticates from the ambient
    ANTHROPIC_* env alone — no credential files are read. The final stream
    event `{"type":"result","subtype":"success","result":...}` carries the
    answer; assistant `tool_use` blocks carry the tool calls. Give the child
    stdin /dev/null or it waits 3 s for piped input.
  - Codex 0.169 headless: OPENAI_API_KEY is no longer picked up implicitly
    (`preferred_auth_method` is ignored); run `printenv OPENAI_API_KEY |
    codex login --with-api-key` once inside the isolated CODEX_HOME. The
    config.toml naming the ohya server must sit INSIDE CODEX_HOME — a config
    written next to it is silently never read, and the model then answers
    that the sql tool is unavailable (a misleading "Untrusted project-local
    configuration" warning can point the wrong way; the router error is
    "unknown MCP server 'ohya'"). `codex mcp list` under the isolated
    CODEX_HOME proves registration before any agent run. `codex exec --trust`
    is passed defensively (codex gates MCP servers behind project trust in
    untrusted directories) and writes only inside CODEX_HOME — verified
    nothing lands in the real ~/.codex. `codex exec --ephemeral --json -o
    <file>` emits `item.completed` events (mcp_tool_call with server+tool,
    agent_message) plus `turn.completed`; websocket-fallback error items
    appear even on successful runs and are benign.
  - Do not redirect HOME for a mise-shimmed `codex`: the shim busy-spins
    resolving its install when HOME is foreign (verified: shim at 110% CPU,
    zero output; the direct install binary under the mise tree works). The
    gate keeps the real HOME for codex and relies on CODEX_HOME alone for
    isolation; claude gets a scratch HOME plus an env allowlist
    (PATH/HOME/TERM/LANG/CLAUDE_* + ANTHROPIC_* passthrough).
  - The gate's own citation check resolves cited event IDs through a second
    `ohya mcp` session (the same sql surface), so "resolvable evidence" is
    proven against the tool contract, not by opening the database directly.

- CLI contract choices (2026-11-14, cmd/ohya/cli.go, T059): help is stdout
  plus exit 0 (bare command, -h, --help at every level, bare `ohya devices`);
  usage errors are one stderr line pointing at the failing command's --help
  plus exit 2; other failures stay exit 1. The flag package itself prints
  nothing (SetOutput(io.Discard)); its Usage renders the uniform template
  into a per-command buffer, because Parse calls Usage both for help and
  before a parse failure — only a help request flushes the buffer to stdout.
  cliMain(args, stdout, stderr) int is the testable exit-code mapping behind
  main; the process-level test forks through TestMain's `child-run` role.
  Help and error texts spell flags with two dashes; the parser keeps
  accepting one dash.
  T128 done (2026-11-17): flags may appear on either side of a positional
  argument. `commandFlagSet.parse` permutes flag arguments (with their
  values) before positionals before flag.Parse, because the standard flag
  package stops at the first positional. Gotchas pinned in tests: a
  non-bool flag that ends the argument list without its value must NOT be
  permuted in front of positionals — flag.Parse would silently bind the
  first positional as the value (permuteFlags returns it and parse feeds it
  to fs.Parse alone for the authentic "flag needs an argument" error); a
  value is taken from the immediately following argument even when it looks
  like a flag (flag package semantics); `--` ends the scan and keeps the
  rest positional; a `--state X` after the positional now works, so
  "revoke needs exactly one device ID" only fires when the ID count is
  really wrong.
  T060 done (2026-11-14): `ingest --once` returns the errDegraded sentinel
  (cmd/ohya/ingest.go) when passStats counts any failed or stopped source;
  cliMain maps it to exit 3 with no extra stderr line (diagnostics already
  reported per source), and the summary line appends "; N failed, M stopped"
  only when degraded. Gotchas: writeUsage wraps a spec description at 80
  columns, so a contract phrase that crosses a wrap boundary breaks substring
  tests — put such a sentence on its own "\n" paragraph (wrap() splits on
  newlines); and a chmod-0 unreadable-source fixture needs a skip guard when
  the test process can read through mode 0 (root).
  T061 done (2026-11-14): `ohya version` / `ohya --version` print one
  key=value line (cmd/ohya/version.go); schema comes from
  store.SchemaVersion() (highest embedded migration, no database opened),
  parser keys from each package's OutputVersion. Verified fact: with Go
  1.27.3, ReadBuildInfo's Main.Version in a VCS checkout is a pseudo-version
  (`v0.0.0-20261114181354-e177ac9549f4+dirty`), not "(devel)" — the
  version=devel fallback only fires outside VCS or without a module version,
  and vcs.revision/vcs.modified are absent in those builds (revision and
  dirty print "unknown"). The root alias check (isVersionArg) runs before
  subcommand dispatch, so `ohya ingest --version` stays a usage error.
  T139 done (2026-11-20): the build identity moved to
  `internal/buildinfo.Identity()`; `ohya version`, install/status/telemetry,
  and the MCP initialize serverInfo.version all read it, and release builds
  inject the tag with `-X github.com/nibzard/ohya/internal/buildinfo.ReleaseVersion`
  (package.sh updated; the old `main.releaseVersion` symbol is gone — release
  tooling that sets it must move too). A dev build's serverInfo.version is the
  pseudo-version (embeds commit + dirty), a release build's is the tag.
  T062 done (2026-11-14): --json on ingest --once, devices list/invite/
  revoke, rederive, version prints one ADR 0005 envelope
  (cmd/ohya/jsonout.go; schema_version 1). Choices worth keeping:
  - A degraded --once pass is a completed pass: data carries counts and
    stops, error stays null, exit stays 3. Failure envelopes (data null,
    error {code,message,hint}) are only for systemic failures; usage errors
    never produce an envelope (T059's one-line stderr contract owns them).
  - Error codes classify by sentinel (errors.Is), never by prose: two
    sentinels were added for that (registry.ErrAlreadyRevoked,
    control.ErrNotProvisioned); site-known classes (database_open_failed,
    missing_agent_root, registry_open_failed) wrap with the local
    classifiedError, whose Error() stays the underlying message so text
    output is byte-identical.
  - The encoder calls SetEscapeHTML(false): messages carry paths and flag
    spellings with < > & (the hint "--<agent>-root"), and escaped output
    made goldens unreadable.
  - Stop remedies are stable codes in JSON (increase_max_record_bytes |
    repair_source_file) while stderr keeps ADR 0004's prose.
  - Golden tests scrub run-varying values by key ("code", "expires_at",
    duration_ms) and replace tempdir prefixes; json.RawMessage data decodes
    as the literal "null" bytes (never a nil slice) — compare with
    string(data) == "null", not data == nil.
  - Forcing stale rows for a rederive golden needs a parser above version 1:
    events.parser_version has CHECK (>= 1), so the Claude Code parser
    (version 1) cannot go stale; the Pi fixture (version 2) stamped down to
    1 works.

- Doctor choices (2026-11-14, cmd/ohya/doctor.go + internal/store/doctor.go,
  T063): doctor answers what the sql tool cannot — state tied to this binary
  or outside the database. Facts future work must keep:
  - FTS5 integrity-check gotchas on the bundled SQLite 3.54.1 (all verified
    by probe): the command is a write statement, so mode=ro AND
    query_only both refuse it ("attempt to write a readonly database");
    doctor runs it on a private in-memory snapshot taken through the
    driver's Backup API (store.FTSIndexConsistent) — the hub file stays
    read-only. Without a rank argument
    (`INSERT INTO ft(ft, rank) VALUES('integrity-check', N)`) an
    external-content table gets only the structural check and every
    index-vs-content drift passes; with N>0 the checksum comparison runs —
    but it demands a %_docsize row for every content-table row, and the ohya
    schema deliberately leaves NULL/empty-text events unindexed, so a
    healthy hub fails. FTSIndexConsistent therefore drops the maintenance
    trigger and deletes text-less events rows on the snapshot first; the
    comparison then covers exactly the rows the write path indexes. Note
    `SELECT rowid`/`count(*)` on external content read the content table,
    never the index, so no SELECT-only substitute exists.
  - Single-connection pools: a `:memory:` snapshot DB must SetMaxOpenConns(1)
    and run every statement through one checked-out *sql.Conn — ExecContext
    on the pool while the Conn is held blocks forever waiting for a free
    connection (found by test hang).
  - Stop detection is read-only re-decision: parse limit+1 bytes at each
    pending checkpoint cursor (a record within the bound is fully visible —
    JSON bytes ≤ limit plus one terminator); report only a StopError AT the
    cursor (the checkpoint's state, matching where ingest acknowledged);
    FirstNative comes from store.ReadOnlySourceState when cursor > 0 so
    Codex past offset 0 parses. A stop deeper in pending bytes surfaces
    after the next ingest pass advances the cursor onto it.
  - Doctor never creates state: device.Load (new, read-only) instead of
    LoadOrCreate; watch.InspectSources reads checkpoints without Poll.
    Missing identity/database = warn (ingest creates them), unusable ones =
    error. The join-state check (T123) verifies credential.json plus the
    saved hub address and never prints the address, which is a secret.
    Error findings ride in the JSON data with exit 1 (findings are the
    report, the exit code is the verdict), mirroring degraded ingest.

- Benchstat tracking choices (2026-11-14, Makefile + .github/workflows/ci.yml, T047):
  - benchstat is a pinned tool install
    (golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e),
    deliberately not a go.mod dependency — nothing in the project imports
    it. x/perf has no tagged releases, so the pin is a pseudo-version;
    bump it consciously.
  - `make bench-save` writes raw runs under perf/ (gitignored, like
    profiles); CI uploads each run as the `bench-<sha>` artifact and the
    baseline reaches PRs through the actions-cache (`bench-baseline-<sha>`
    keys, saved only on main pushes — caches are immutable per key, hence
    the sha in the key and the prefix restore).
  - The CI bench job reports and never fails on a delta: GitHub's shared
    macOS runners are not the reference machine, and their fsync-bound
    rows swing with disk contention. Only same-hardware comparisons count;
    CI deltas are a prompt to reproduce with bench-save on both commits.
  - bench-save captures stdout with `>` (not `tee`): a pipeline would hide
    a failed `go test` behind tee's exit 0.
  - BenchmarkIngestAppend must serve any b.N the framework asks for
    (fixed 2026-11-14, T053): its 16 MiB corpus holds only ~470 eight-record
    append windows, and on fast storage (the CI runner) the default 1 s
    benchtime drives b.N past that cap — the old b.Fatalf made the whole
    bench job fail (go test exit 1, make exit 2, output lost inside
    perf/bench.txt). The benchmark now wraps the window index and re-primes
    a fresh database (prefix committed, timer stopped) at each lap start.
    Same trap for any future benchmark over a bounded corpus: never
    b.Fatalf on exhaustion; re-prime or regenerate instead. The bench job's
    artifact upload is now `if: always()` so a failed run still ships its
    partial perf/bench.txt for diagnosis.

- Memory gate, benign Codex error items, decided 2026-11-15 (T067): Codex
  0.169 emits an `error` item even on successful runs when it falls back
  from the WebSocket transport to HTTPS (observed message: "Falling back
  from WebSockets to HTTPS transport. unexpected status 401 …" — the wss
  endpoint rejects API-key auth; the same episode also emits top-level
  `{"type":"error","message":"Reconnecting… N/5 …"}` events, which the
  trace parser never read). `parseCodexTrace` routes an item whose message
  carries that verified prefix into report-only notes (`runResult.notes`,
  printed under the case verdict); every other error item stays a case
  problem, so an unknown Codex error still fails the gate. Extend
  `benignCodexErrorItem` only with a verified benign sample.

- Freshness harness (2026-11-15, T068, cmd/ohya/freshness_test.go): measures
  the north-star metric — append on the device to visibility through the
  remote sql tool — over the real child-process stack (serve, join, remote
  mcp observer on one host, local tailnet path). Contract kept: unique
  `ohyafresh-<run>-<seq>` markers polled on the direct `events` table (never
  `events_settled`); appender and observer share one process and one
  monotonic clock; fixed-rate appends never wait for visibility, with a
  random phase offset plus jitter against the upload interval
  (`OHYA_FRESH_SEED` reproduces); timed-out samples stay in the distribution
  at drain lower bounds, split into overdue-but-visible and
  outstanding-at-drain, followed by a bounded health wait that reports
  late deliveries and any records that never landed. Env knobs
  (`OHYA_FRESH_SAMPLES/APPEND_EVERY/POLL_EVERY/TIMEOUT/UPLOAD_EVERY/SEED`)
  make the same test the T070 workload-matrix vehicle. Smoke result on the
  default configuration (2 s upload interval, 100 samples, 200 ms appends,
  100 ms polling, 30 s budget, reference VM, Go 1.27.3): lag p50 1.178 s,
  p95 2.043 s, p99 2.154 s, max 2.247 s; 223 polls, 0 errors, query p50
  1.4 ms; 0 timeouts, 0 outstanding; hub 116 events; whole test 44 s. The
  distribution sits just under the upload interval — the interval, not the
  write path, dominates the budget. Gotchas: the observer warms its first
  exchange before the first append (a cold first dial costs seconds and
  would poison sample one; warm cost 2 ms because the transport was
  already up), and the marker query pages by `id > last` so the 500-row
  tool bound never truncates a backlog drain.

- Benchmark corpus fixtures (2026-11-15, T069, internal/fixgen/mix.go):
  `fixgen.ImportMix`/`fixgen.QueryMix` + `genfixture -agent mix` emit the
  two named fixtures; stats and commands live in docs/performance.md
  ("Benchmark corpus fixtures"). Facts worth keeping:
  - Observed corpus re-measured 2026-11-15: claude-code 1,042 main files
    (1.29 GiB, mean 1.27 MiB) plus 1,280 subagent files (0.59 GiB, mean
    480 KiB); codex 2,495 rollouts (2.93 GiB, mean 1.17 MiB); pi 74 files
    (2.1 MiB). The 2026-11-13 NOTES numbers (895/2,646) counted differently;
    the mix anchors use the fresh file-level counts.
  - Codex thread count (1,600 at scale 1) is derived from bytes, not
    observed: the generator omits resumed pages, so threads = observed
    bytes / (main + mean two quarter-size subagent rollouts). Real threads
    ≈ 511 spanning ~4.9 files each; the fixture keeps bytes and file sizes,
    not the page fan-out.
  - Records map 1:1 to events in all three formats (structural: every
    adapter makes one event per parsed record), so `fixgen.MeasureMix`
    counts events through the raw parsers — importing internal/events from
    fixgen would create a test import cycle (events' in-package bench tests
    import fixgen), and fixgen now imports only the parser packages.
  - The query fixture answers the perf plan's "one million events without
    a 3 GB input": compact records (text ÷8, session bytes ÷4) land at
    765–889 B mean — the JSON envelope (usage objects, per-block metadata)
    is the floor — giving 969,004 events in 746.4 MiB (4.4x per event).
    Full-record 1M events would be ~3.5 GB.
  - Determinism is pinned by SHA-256 hash tests (mix + all three single
    agents, `TestAgentCorpusStableHash`); the single-agent hashes were
    captured before the Compact knob landed, so they also prove Compact
    leaves default (div=1) bytes identical — repin hashes and the
    docs/performance.md tables together if generator output ever changes.
  - `TestNamedFixtureStats` regenerates both fixtures at full scale only
    under `OHYA_MIX_VERIFY=1` (about 1 GiB RAM, 22 s); the default check
    stays small. genfixture's plain build needs no sqlite_fts5 tag.

- Workload matrix baselines (2026-11-15, T070, docs/performance.md "Workload
  matrix baselines"): all four plan workloads measured on the direct
  local-tailnet path with 1,000-sample doubled runs; raw logs under
  `perf/t070/` (gitignored). Headlines: steady freshness p50 1.06-1.08 s /
  p99 2.06-2.07 s (the 2 s upload pass spends the budget); a 1.5 MB/s
  backlog on a second source doubles it (p99 4.2-4.4 s); three devices
  draining 570 MB barely move it (p99 2.5-2.7 s) while loaded wide FTS p95
  is 110-111 ms at 168k events; initial import 137 MiB = 24.6-447 s
  (fsync-bound swing, CPU constant 10-15 s); FTS-wide p95 at 969k events
  is 193 ms (fails the proposed 100 ms budget; compact fixture matches ~1/8
  of events, a full-size 1M hub lands ~1.5 s); hub idle RSS 65-66 MiB; peak
  WAL 27-32 MiB under sustained mixed read/write — well above the 4-6 MiB
  import-only figure, plan headroom accordingly. Guardrail verdict table in
  the doc.
- T070 harness facts worth keeping: the sql tool returns JSON numbers, which
  decode as float64 — `fmt.Sprint` renders ids ≥ 1e6 as `1.012834e+06`, so
  any harness that parses ids from tool output must `CAST(id AS TEXT)` in
  the query (the bug poisoned every later poll and looked like a hub
  outage). A fresh device transport's first dial to the hub costs seconds
  (path discovery), so the multi-device workload needs
  `OHYA_FRESH_PAIR_LEAD` before appends start. `fixgen` corpora size at
  sessions × ~3 MiB (main 2 MiB + two quarter-size subagents), so
  `OHYA_FRESH_DEVICE_MIB=192` yields ~285 MB per extra device.
  The T075 hub-restart regression this matrix found lived in the deleted
  coordination stack; performance.md "Workload 4" keeps the history, and
  the address transport's own restart handling is recorded under T122
  below.
- Ingestion stage attribution (2026-11-15, T071): numbers and method in
  docs/performance.md "Ingestion stage attribution"; raw evidence under
  `perf/t071/` (gitignored). Durable findings:
  - 65–86% of import wall is durable-sync wait (WAL, `synchronous=FULL`):
    `PRAGMA synchronous=OFF` in a throwaway DB collapses a 320 MiB import
    from 47–92 s to 12.7–13.4 s. Commit-only wait dominates; checkpoint
    share 10–17%; FTS trigger maintenance 24–36% at 1x and 14% at 10x
    (measured with the trigger dropped, so the number includes the sync
    volume it induces; the index is 6.6–8.5% of DB size). The rate band is
    flat 32→320 MiB in every agent format — nothing superlinear.
  - Register commits are a separate, measurable stage: one fsync per source
    (43 ms each at 439 sources, 19 s total; 0.04 s with syncs off). The
    harness counts register and first batch as two commits (two
    transactions).
  - One commit is NOT one fsync: strace counted ~1.4 WAL fsyncs per commit
    (each auto-checkpoint adds ~2 WAL + 1 main-db sync; 3,704 commits +
    2×731 checkpoints = 5,166 predicted, 5,154 measured at 10x). State
    durability (`internal/durablefile`: temp file + fsync + rename +
    directory fsync per acknowledged source) costs ~2 fsyncs per file —
    75.5 s, 19% of the 10x end-to-end wall. Device-side cursor durability,
    not hub cost; future import-speed work must weigh it.
  - Discovery is noise: a fully-acknowledged `ingest --once` pass costs
    0.02 s over 191 files, 0.09 s over 1,852.
  - Harness gotchas: `go test -bench '^BenchmarkIngestCorpus/32MiB$'` also
    runs ...Codex/Pi/Mix — the first path element matches as a prefix
    (verified 2026-11-15, Go 1.27.3); use the full sibling name in the
    filter or expect the set. Single 10x diagnostic triples can be voided
    by shared-disk swings (one claude 320 MiB triple went negative on
    checkpoint share); keep 1x repeats for coherent decomposition and name
    which 10x triple carries the numbers.
- tla2tools 1.8.0 release asset replaced upstream (2026-11-15): GitHub now
  serves a rebuild of the same tag. Both jars have an identical file
  listing and builder metadata (Apache Ant 1.10.12, Eclipse Adoptium
  17.0.21+9, Built-By runner); only the embedded Build-TimeStamp differs
  (old 2026-11-06T22:23:57Z, new 2026-11-15T00:21:46Z). The old sha256 pin
  in tla/run-tlc.sh therefore failed every fresh download. Repinned to
  7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f after
  the comparison and a green run of every model. Resolved 2026-11-15 (T077):
  the jar is now vendored at `tla/vendor/tla2tools-1.8.0.jar` and
  `tla/run-tlc.sh` verifies its checksum on every run without downloading
  (ADR 0007); no immutable artifact host exists for tla2tools (Maven Central
  has none, and GitHub asset URLs die on re-upload). Upstream rebuilding the
  tag again can no longer break a fresh run or CI.
- Perf budgets, Phase 1 selection, CI placement (2026-11-15, T072,
  docs/performance.md "Budgets, thresholds, and the Phase 1 selection"):
  freshness p95<2 s and idle RSS<150 MB kept (decision: if freshness needs
  margin, change the 2 s upload interval, never the target); time-to-memory
  and 1M-event query thresholds replaced (quiet-disk wall + unconditional
  CPU per 3 GiB; scoped per-pattern query budgets plus a 1.6 ms/1,000-match
  wide-BM25 rate); peak-memory/disk-growth/loaded-query budgets recorded
  with allowed variation. NO Phase 1 candidate selected — after
  re-scoping, no workload misses (wide-BM25 cost is per-match scoring;
  contended import misses are tenant fsync load, and register/checkpoint
  amortization recovers only ~a quarter) — so T073 closes empty per its
  contract; batch sizing is first in line if the thin 8% quiet-disk
  time-to-memory margin erodes. CI: the freshness smoke rides `make check`
  on check-linux (re-measured 50.4 s, p95 2.062 s at 100 samples — above
  2 s on a phase draw, why the smoke asserts no threshold); statistical
  runs stay off CI; no new job (the bench job is darwin, where the harness
  skips, and report-only); `make check` now passes `-timeout 20m` to go
  test because the cmd/ohya child-process suite measured 229 s reference
  and a 2–4x slower shared runner reaches the 10-minute per-binary default.
- T073 closed empty (2026-11-15): T072 selected no Phase 1 candidate, so per
  its own contract T073 changed no code and no experiment ran. Re-verified at
  the integrated HEAD: docs/performance.md "Phase 1 selection: none" (section
  landed in 61a14f6) records that after the threshold re-scoping no workload
  misses its target or budget and none of the plan's seven candidates meets
  its trigger — a candidate must dominate a workload that misses; make check
  green on the unchanged tree. A new miss reopens selection only through a
  fresh task with new evidence; batch sizing stays first in line. T074
  (Phase 2 accept-or-discard) therefore finds no T073 change to evaluate.
- T074 done (2026-11-15): Phase 2 closed with nothing to accept or discard
  (T072 selected no candidate; T073 changed no code). The matrix was still
  repeated at the integrated HEAD — same machine, fixtures, knobs, fresh
  seeds 2001–2006; raw logs in perf/t074/ (gitignored) and the task artifact
  dir; numbers and budget table in docs/performance.md "Phase 2 acceptance".
  Every workload landed inside the T070 spread and every T072 budget held;
  make check green. Notables worth carrying: steady p95 straddles 2 s again
  (1.971/2.014 s vs T070's 1.945/1.999 s) — the 2 s upload interval consumes
  the budget and a 1,000-sample draw moves p95 by more than the 1–55 ms
  margin, so do not read one side of the line as a regression; mixed
  read/write WAL peak read 33–65 MiB against the 27–32 MiB T070 basis (one
  run 1 MiB over the 64 MiB ceiling, inside the ±50% variation) — the
  high-water mark races auto-checkpoints against reader snapshots, so the
  T070 basis was a low draw; W4 hub-restart recovery 36.2 s in both runs
  (post-T075) with idempotent final counts. No successor candidate; the T072
  reopen triggers stand, and all four workloads must be repeated on the
  relay path after T065.
- Ingest-floor guard widened (2026-11-15, T078): TestIngestPerformanceFloor's
  wall ceiling moved 30 s → 120 s; the allowance and its anchors are in
  docs/performance.md "Regression guards". Anchors: one parallel-load
  failure at 31.2 s (full `go test ./...`, sibling SQLite packages on the
  shared reference disk) versus 1.24 s isolated; the worst tenant rate
  0.31 MiB/s makes the 24 MiB corpus about 77 s, so 120 s covers measured
  contention with margin while per-record durability (about 290 s at 6,794
  events) and superlinear regressions still fail loudly. CI serialization
  (`-p 1`) rejected: tenant load fails a serialized run too, and it slows
  every check. Reopen condition: a loaded run above 120 s → raise the
  ceiling with that evidence, like any other allowed variation.

- Pi fixture cost totals, FMA determinism, fixed 2026-11-15: the macOS CI
  check (first arm64 exposure of the fixgen hash pins) failed
  TestAgentCorpusStableHash and TestGenerateMixIsDeterministic while
  check-linux passed. Root cause, proven bit-for-bit: Go contracts
  `float64(in)*2e-7 + float64(out)*3e-7 + float64(cacheRead)*2e-8` into
  fused multiply-adds on arm64 (one rounding, leftmost multiply kept
  separate) while amd64 rounds each operation; json's shortest float
  formatting then emits different bytes. The pi usage total now uses an
  explicit `math.FMA` chain shaped exactly like the arm64 contraction —
  `math.FMA(cacheRead, 2e-8, math.FMA(out, 3e-7, in*2e-7))` — which the
  Go spec defines as the correctly-rounded fused result on every
  platform. The fixed bytes reproduce the darwin output verbatim
  (per-file ddaaa1bb…/121c5267…/8dc6a83f…); the mix and pi pins moved to
  the darwin-observed corpus hashes, claude-code and codex pins are
  unchanged, and only `total` cost fields differ from the old linux
  fixtures (a few characters in some records; corpus shape and event
  counts unchanged).

- Installer proposal (2026-11-15): `docs/installer-plan.md` records the proposed
  integrated installer: user services, agent MCP registration, verification,
  repair, and later Homebrew support. Implementation is in progress. User requires
  one-command working setup, not a binary-only default. Native baselines, Apple
  signing, and Linux bootstrap
  signature policy remain open. Pi's current quickstart recommends a managed
  installer with pinned dependencies; do not describe it as npm-only.

- Installer client research (2026-11-15): current upstream Pi documents native
  MCP commands, but installed `pi mcp --help` prints general help with no MCP
  subcommands. Detect capabilities and test versions; the proposal includes an
  owned extension for supported older Pi. Claude user scope and Codex MCP add
  syntax were checked against installed help. Sources: installer plan.

- Integrated installer implementation (2026-11-15): `install.sh` and
  `internal/setup` implement verified bootstrap installation, owned JSON/TOML
  configuration, local and remote setup, user service templates, PATH blocks,
  SQLite recovery points, removal, and real MCP checks. No assets are published.
  Native release validation remains unfinished; local policy preflight is implemented. The real remote setup test
  proves pairing and reruns against a hub without changing user services.
  Verified Claude override path: `CLAUDE_CONFIG_DIR/.claude.json`.

- Pi compatibility verified (2026-11-15): the installed 0.84.2 extension loader
  loaded the owned ohya extension and completed SQL plus Unicode annotation
  round trips against a built ohya binary. No model or provider was used.
  Native MCP stays preferred; compatibility fallback is gated to this verified
  version. The bridge uses the existing Go MCP SDK and stdin for request data.

- Setup health checks (2026-11-15): loaded and running service states are
  distinct; readiness requires a process and two successful status checks.
  Doctor inspects owned integrations read-only and follows saved remote setup
  state. Remote upload checkpoints are checked for readability, not reparsed
  without the hub's Source state. Native service-manager validation remains
  a release gate.

- Native Linux installer smoke (2026-11-15): a real systemd user service
  imported 16 synthetic events, acknowledged the full Source cursor, passed
  doctor, and uninstalled cleanly. The first run exposed literal quotes in
  WorkingDirectory; the fix has a systemd-analyze regression test. This host
  does not prove the proposed Ubuntu 22.04 or macOS baselines. Release candidate
  scripts and a manual native matrix are prepared; nothing is published.
- Recovery limit: an automatic SQLite recovery point is not a coordinated
  checkpoint/device backup. Restore matching stopped state as a set; do not
  pair an older hub database with newer acknowledgments. See the operations
  guide before a writable restore.

- Clean installer candidate verification (2026-11-15): source `1c79cf3`,
  version `v0.0.0-installer-candidate.2`, reports `dirty=false`. HTTPS install,
  real systemd ingestion, full Source acknowledgment, doctor, and clean service
  removal pass. Actual standalone version upgrade and binary removal preserve
  memory and recovery copies with paths containing spaces. Preferences and
  exact private client backups persist. Failed service changes restore the
  prior owned service. macOS and Homebrew jobs remain unrun; no publication.

- README images: GitHub serves SVGs in `<img>`, so `prefers-color-scheme`
  inside an SVG follows the OS, not the GitHub theme. Keep two static files
  (`docs/assets/architecture-{light,dark}.svg`) and select them with
  `<picture>`. `docs/assets/terminal.svg` shows real `ohya ingest --once`,
  `ohya doctor`, and `sql` tool output over the Claude Code, Codex, and Pi
  `main-session.jsonl` fixtures plus `claude-code/subagent-session.jsonl` and
  `pi/anthropic-session.jsonl`; regenerate it when that output changes.

- 2026-11-15, local trial on macOS 27.2.1 arm64 (real data, throwaway DB):
  - Native transport evidence for T046: `TestFreshnessSmoke` and
    `TestHubRestartReconnect` (serve, join, live appends, hub restart) pass
    on this Mac (`go test ./cmd/ohya`, 145 s). CI still skips them on darwin.
    One machine; not yet a CI result.
  - Ingest of the full local history (3,292 files, 4.2 GB): 876,852 events in
    182 s, 130 MB peak RSS, 5.1 GB database (4.1 GB raw_json). The record
    bound is now 32 MiB (ADR 0004 amendment); the largest local record was
    16.8 MiB.
  - Claude Code deletes transcripts after `cleanupPeriodDays` (default 30),
    so continuous ingestion is the only copy of older Claude history.
  - FTS queries on 964k events return in about 0.25 s including process
    start.

- 2026-11-15, v0.1.0 published from `8fb3d2e` after `make check`, `make tla`,
  and the hosted candidate matrix (run 43760301528) passed. Assets: three
  archives, `SHA256SUMS`, and `install.sh`; macOS archives are unsigned
  because this machine has no Developer ID Application certificate. The
  qualified `ohya.rb` formula is in the run artifacts for a future tap.
  `install.sh` printed "release version must start with v and a digit" when a
  repository has no stable release, because GitHub redirects
  `/releases/latest` to `/releases`; closed 2026-11-17 (T084): install.sh
  now detects that redirect, names the no-stable-release condition, and
  points to the release list with `--version`.

- 2026-11-15, v0.1.0 setup failure on a real GUI Mac (27.2.1): `launchctl
  print` includes `resource coalition`/`jetsam coalition` sub-blocks whose
  `state = active` lines clobbered the job-level `state = running` in the
  flat parser, so setup's 12 s readiness wait timed out and doctor reported
  a false "not running" while ingestion ran fine. CI macOS runners show no
  coalition blocks, so runner-green launchctl parsing proves little; verify
  such parsing on a logged-in GUI Mac. Fixed with depth-aware parsing in
  `internal/setup/service.go` plus a dump-shaped regression test; v0.1.1
  (commit dab456e) shipped the fix the same day and was verified through the
  released installer on the same machine.

- Doctor cost on a 6.6 GB hub (2026-11-15): `parser_versions` scans all events
  (1.3 s). An index `events(source_id, parser_version)` with a per-source
  `max` subquery cuts it to 0.02 s (42 MB, 19 s build), measured on a clone.
  Not added alone because each migration now forces a full recovery copy at
  setup; add it with the next schema migration. `doctor --deep` copies the
  whole database to a temporary file under TMPDIR (`store.FTSIndexConsistent`
  through the backup API; a note here said "into memory" until 2026-11-24),
  so it needs free disk a little more than the database and took 24 min 45 s
  on the 18.1 GB hub; copying only the FTS shadow tables is unverified.
  Added by migration 009 on 2026-11-25. On the 18.15 GB hub the query took
  4 min 52 s without the index on a cold cache and 0.18 s with it ("Schema 9
  upgrade path").

- README recall trial (2026-11-15): release-hosted installer URL and its
  `--help` verified by download. Quickstart now separates installed usage
  from source builds and leads with cross-agent handoffs. Immediate handoffs
  need explicit `events` queries: `events_settled` excludes sessions active
  within 10 minutes. Broad recall also needs project scoping to avoid
  attributing another project's decisions to the current repository.

- Runtime observability proposal (2026-11-15): full implementation spec in
  [plans/001-runtime-observability.md](plans/001-runtime-observability.md).
  Defines agent-facing status/metrics/logs/diagnose, precise cursor and latency
  semantics, bounded private telemetry, authenticated remote hub aggregates,
  phased delivery and overhead/failure-path acceptance gates. Milestones 1–4
  delivered 2026-11-16/17 (T090–T093, T096); milestones 5 (T098) and 6
  (T097) are open.

- Migration recovery copy cost on a slow single disk (observed 2026-11-16):
  the workspaces migration's forced full recovery copy (see the 6.6 GB hub
  note above) of a 13.1 GB hub took 8.5 h (00:25–09:01) on this VM's single
  emulated QEMU disk (~10 MB/s write ceiling under load) and starved the
  machine into an unresponsive state that needed a forced reboot: iowait
  70–85% all night, journald in a 94-restart loop (SIGKILL could not remove
  its D-state processes), kernel soft lockups on 5 of 8 CPUs plus RCU stalls.
  Memory (9–11% used) and CPU steal (~0.1%) were fine — disk saturation
  alone. Post-migration hub is 16.3 GB (was 13.1 GB). Full timeline with
  sar/journalctl evidence:
  /notes/2026-11-16-vm-hang-ohya-workspaces-migration.md. Implication: on
  slow disks a full-copy migration is an hours-long denial of service against
  everything sharing the disk; consider size-aware or incremental backup
  (or at minimum a progress log line and idle-time execution).
  Size-awareness landed same day (T100, plan 004 S1): setup checks free
  space on the recovery filesystem (database + WAL + 10% + 64 MiB) before
  copying and refuses with both numbers and a remedy (JSON code
  `insufficient_disk_space`), changes nothing on refusal, reports the
  estimate before the copy plus a stderr line every further 10%, stages the
  copy in a hidden `.recovery-*` directory renamed to `recovery-*` only on
  success (an interrupted copy leaves no partial point; abandoned staging is
  removed on the next run), and the copy deadline scales with size instead
  of the old fixed 2 minutes (2 min minimum, else a 256 KiB/s average floor,
  so the measured 39 min/GB worst case still completes). Cost guidance is in
  docs/release-operations.md "Recovery point cost". Idle-time execution and
  incremental backup remain unimplemented.
  Probable cause found 2026-11-24 (commit `3bf3d50`, ADR 0008): the stepped
  copy held no read transaction between steps, and SQLite restarts a stepped
  copy at page 1 when another connection commits between two steps
  (`sqlite3_backup_step` documentation). The 8.5 h fit many restarts plus
  one last pass after the other writers stopped at 08:42: that is an
  inference, no log shows the restarts. The copy now holds one read
  snapshot from the first step to the last. Measured the same day: 758 MB/s
  raw read on this disk, and about 10 minutes for one copy pass of 18.1 GB
  (inside the `doctor --deep` run), so the "10 MB/s ceiling" was not a
  property of the disk. Two regression tests hold a second connection that
  commits during the copy. Research note:
  /notes/2026-11-24-stepped-copy-restarts.md.

- Ingest idle write amplification (root-caused and fixed 2026-11-16):
  idle ingest wrote ~9 MB/s; 99.9% went to `sources/<agent>.json`
  checkpoint rewrites, not SQLite. Each pass acknowledged ~20 sources that
  never advance (records the agent tore, which stop the source under
  ADR 0004, or an abandoned final line). Each acknowledgment rewrote and
  fsynced the whole 1.5 MB file. The "0.13 MB/s before" baseline was false:
  `ohya.service` was stopped from Nov 15 18:49 to 22:53. Fixes: no-op
  acknowledgments do not save; `Watcher.Flush` saves once per pass (ingest
  `poll`, uploader `Sync`); ingest and the uploader skip re-reading a
  source while its (end, cursor, mtime) matches a pass without progress.
  Remaining limits: a repair that keeps size and mtime needs a restart;
  each pass with progress still rewrites the whole file, which grows with
  the number of sources. If that dominates, move checkpoints into SQLite
  (ADR needed). Measured after deploying 6d464f1 (2026-11-16, 30 s
  windows): ingest disk writes fell from 13.2 MB/s to 0.76 MB/s.
- Ingest per-pass read cost (root-caused and fixed 2026-11-16, plan 004 S2):
  idle ingest read ~13 MB/s, nearly all from the page cache — one 4 KiB
  fingerprint window per source per pass and one `RegisterSource`
  transaction per source per pass. Fixes: `watch.Poll` skips the
  fingerprint read while platform identity, size, and modification time
  match the previous poll (in-memory per Watcher, so a restart re-verifies
  every source once), and the ingestor registers only sources this process
  has not registered at that path (new, replaced, moved, or first pass
  after restart); a write-path `ErrSourceNotRegistered` drops that memory
  so the next pass re-registers a hub row lost to a restored backup.
  Measured with `/proc/<pid>/io`, 60 s steady window, 1,482-source corpus
  at the 2 s interval: rchar fell from 176 MB (2.9 MB/s, one window per
  source per pass) to 330 bytes; storage reads stayed 0. Numbers in
  docs/performance.md "Idle ingestion cost". Remaining limits: an in-place
  rewrite that restores exact size and mtime stays invisible until a
  restart (pre-existing accepted edge, same as the write-amplification
  note above); replacement, truncation, and mtime-moving rewrites are
  still detected exactly (pinned by the existing watcher tests).

- v0.1.2 release (2026-11-16): published from `6ed9fc3`; hosted macOS arm64
  runners can fail to start a job ("not acquired by Runner ... capacity"),
  which cancels it after ~15 min. `gh run rerun --failed` recovered it. The
  hub host had been upgraded by a hand copy, so `install.sh` refused it until
  the receipted v0.1.1 executable was restored; install hub builds through
  `install.sh` to keep the receipt valid.

- Hub size breakdown (measured 2026-11-16, 16.3 GB hub, `dbstat` plus a
  random sample of 40k events): the hub mirrors 9.85 GB of raw transcript
  bytes (sum of `sources.cursor_bytes`; 5.38 GB from this VM, 4.47 GB from
  the MacBook) at 1.66x. `events` is 14.9 GB; the FTS index is 0.83 GB; all
  other indexes total under 0.6 GB; there are no free pages. In an event
  row, `raw_json` holds about 60–73% of the bytes and `text` plus `metadata`
  the rest. By kind, Codex `response_item` holds 43% and Codex `event_msg`
  20%; `event_msg` largely repeats `response_item` content and has no text.
  0.5% of rows (over 64 KiB: Codex `compacted` snapshots of about 113 KB,
  `session_meta` of about 17 KB, large tool results) hold 27% of the bytes.
  Codex `encrypted_content` is 7.5% of `raw_json`. Per-row deflate of
  `raw_json` gives only 2.0x, because rows are small. Raw copies are the
  durable memory: agents delete or rotate their own transcript files. Size
  levers, none chosen: compress `raw_json` at rest (about 5 GB; agents'
  SQL over `raw_json` needs a decompress function; ADR needed), or derive
  `text` on demand (about 4 GB; costs query speed).

- Usage evidence and instrumentation gap (measured 2026-11-16): the MCP
  server logs no queries, latency, rows, or errors; `plans/001` is still a
  proposal. Agent transcripts that ohya ingests already record its own use
  (tool call, result, timestamps). Since 2026-11-13 the hub holds only 8
  `sql()` calls, all from Claude Code in 4 sessions (both devices). The
  tools were available in 26 sessions. No Codex call was found. Results:
  1 of 8 hit the 5 s deadline, 1 took 4 s, the rest 19–942 ms; results
  were up to 48 KB. None of the 8 read `raw_json`; all used `text`
  previews or `sessions`. That is too little to choose a storage design,
  for example how often agents read `raw_json`. Duplication on MacBook
  data, FTS share by kind, and decode cost need offline analysis, not new
  instrumentation. Correction (2026-11-16, T094): "No Codex call was found"
  and "available in 26 sessions" were scan artifacts, not facts — see the
  usage report note below.

- Observation schemas and recorder frozen (2026-11-16, T090, plans/001
  milestone 1 + plans/002 usage record): `internal/observe` holds the fixed
  metric catalogue (families from plan 001's measurement dictionary; label
  values are closed sets with a fallback, so series are bounded by
  `SeriesBound` and junk values fold into the fallback, never a new series),
  fixed duration histogram bounds (0.1 ms … 60 s, +Inf; quantiles are bucket
  estimates and return unavailable below 20 observations), the snapshot
  payload (format 1, also the history.jsonl sample payload), the structured
  log entry (format 1, stable event codes with curated messages — raw error
  text has no field), fake monotonic/wall clocks, resource readings where
  unsupported is `null` + reason (never zero), window deltas that refuse to
  join counters across instances, resets, or missing samples, and the usage
  record with per-field export classes (ADR 0010; classification tests fail
  on a class-less field or a never-collected field name). Freeze facts:
  recorder calls with an unknown metric name, wrong kind, wrong label
  arity, or negative duration are dropped and counted, never panic, never
  touch domain state. Everything is in-memory; publication, files, and the
  engine wiring are milestones 2–4 (T091–T097).

- Runtime paths instrumented (2026-11-16, T091, plans/001 milestone 2):
  watch, store, uploader, hubsync, and the local ingestor report through
  `observe.Observer` (new narrow interface; `observe.Disabled` is the no-op;
  `*Recorder` satisfies it). Facts future work must keep:
  - The catalogue grew by six names (watch_read_bytes,
    watch_checkpoint_saves, watch_checkpoint_bytes_written, ingest_passes,
    ingest_pass_duration, ingest_failures{failure_kind}) — the per-pass I/O
    facts the idle-amplification notes wanted, so idle cost is readable
    from metrics: one fingerprint window per source per pass, zero
    checkpoint saves on a no-op pass, bytes written per save.
  - Nil observers normalize to Disabled at each constructor
    (`watch.Options.Observer`, `uploader.Options.Observer`,
    `hubsync.Options.Observer`, `store.WithObserver` on Open,
    `ingestOptions.observer` in cmd/ohya). Production CLI processes still
    pass nil: recorder construction, publication, and `--observe off` are
    milestone 3 (T093); today the assertions live in the per-package
    observer tests.
  - `IngestResult.Duplicates` counts ordinal-content duplicates the write
    path recognizes (rows still insert with `duplicate_of` set and count in
    `Inserted`); the ON CONFLICT path is unreachable under the stored-records
    cursor invariant and is not asserted.
  - Store `sqlite_outcomes` separates busy/locked/error by driver code and
    deliberately skips domain rejections (ErrBatchRejected family) — a
    refused batch is not a database failure. `upload_batches` has the same
    shape on the device: a transport-lost batch is attempted but neither
    successful nor rejected.
  - `upload_checkpoint_advances` counts only acknowledgments whose deferred
    checkpoint flush succeeded (pending advances survive a failed flush and
    count at the next successful one); `watch_checkpoint_saves` counts real
    state-file writes with their byte volume.
  - Hub-side: connections admitted/rejected/active, per-frame authorization,
    gate wait, normalize (with parse metrics by agent), store transaction,
    ack write; a clean EOF on connection close is not a read failure.
  - Boundaries left open on purpose: `discovery_last_scan_ms`
    and the SQLite DBStats/file-size gauges need the recorder clock and a
    periodic sampler, which belongs to the T093 publisher. The uploader-side
    pending-byte gauges are deferred to remote-status work (T098) — the
    ingestor sets `watch_pending_complete_bytes` (failed sources' backlog)
    and `watch_partial_tail_bytes` (per-source tails cached in
    `ingestor.tails`) as sampled gauges. The mcpserver SQL/Annotation
    families landed in T117 (see below).
  - `internal/observe/observertest` is the shared fake Observer for tests;
    it fails Add calls with unknown metric names or wrong label arity into
    `UnknownAddCalls()`, and every observer test asserts that stays 0 —
    the guard against fat-fingered metric names the recorder would silently
    drop. `observe.SeriesKey` renders keys for assertions.

- SQL and Annotation engine metrics shipped (2026-11-16, T117, plans/001
  milestone 2 completion): `EngineOptions.Observer` (nil normalizes to
  `observe.Disabled` in NewEngine) emits the catalog's sql_* and annotate_*
  series from the engine, so the local stdio path and the remote hubsync
  handleQuery/handleAnnotate path report through the same engine — hubsync
  keeps only its transport-level hub_failures and cannot double-count. Facts
  future work must keep:
  - The engine reuses the T092 queryTrace for phases; a trace is created
    whenever Usage or Observer is set (`Engine.traced`), so the untraced fast
    path stays byte-identical when neither is configured. Production CLI
    processes still pass no observer (recorder wiring is T093, milestone 3).
  - Outcome labels come from the frozen `usageOutcome` classifier, so
    sql_requests{query_class,query_outcome} and the usage record can never
    disagree; annotate requests report under query_class="annotate".
  - `sql_active` is an engine-owned count set under a small mutex after
    admission and released via defer, so the gauge never exceeds
    MaxConcurrency; requests refused at admission count
    `sql_busy_rejections` plus sql_requests{outcome=busy} and never enter the
    gauge. Rows/result bytes/truncations emit only on success — a failed
    request returns nothing, even if iteration materialized rows before the
    error. Phase durations emit only when the phase completed (unset trace
    fields are skipped, never zero-filled).
  - `annotate_duration` wraps the store.Annotate call (validate + transaction
    + commit — the engine cannot see the pure tx span); both engine-side
    (ErrAnnotateRejected) and store-side (store.ErrAnnotationRejected)
    rejections count `annotate_validation_failures`.
  - `annotate_operations{op,op_result}` needed a per-kind split:
    `store.AnnotateResult` gained four `json:"-"` counters
    (Applied/Unchanged × Upsert/Delete) set in the store's apply loop, so the
    MCP JSON reply, the hubsync wire frame, and every wire-roundtrip test
    comparison stay byte-identical (wire decode leaves the per-kind fields
    zero); only direct store/engine call-site literals in tests needed
    updating, and they now pin the per-kind counts.
  - The device side of `ohya mcp --remote` now reports the client-view
    sql_*/annotate_* series from hubsync.RemoteSession itself (T108, see
    below); the joining process holds no Engine, so its metrics are the
    client-observed subset (requests/outcome, total duration, rows,
    truncations, busy rejections, annotate validation failures).

- Usage recording for `sql`/`annotate` shipped (2026-11-16, T092, plan 002):
  the engine wraps its existing authorizer per call, so `SQLITE_READ`
  table.column sets and `SQLITE_FUNCTION` names (arg2) are collected with no
  SQL text. Verified authorizer facts that shaped the code: FTS5 queries
  report reads as `events_fts.events_fts`/`.ROWID`/`.rank` plus the shadow
  `events_fts_config.k/v` — all outside the frozen T090 catalogue, so they
  fold into `other` (the separate `fts` flag is set from the table name);
  `count(*)` fires a read with an empty column name, which is skipped
  (no column identity); an authorizer denial surfaces as SQLITE_AUTH (23)
  and is classified `rejected`, which required `queryError` to wrap the
  driver error with `%w` (not `%v`) so the result code survives. Records go
  to `<state>/observe/<instance>/usage.jsonl` through a bounded async queue
  (256 deep, drop-and-count; 8 MiB per file, two rotated files; 0700/0600;
  no fsync). Per-instance retention is size-bounded by rotation; the 30-day
  and cross-instance pruning belongs to plan 001 milestone 3 instance
  cleanup (T093). The device-side client-outcome record for `ohya mcp
  --remote` landed 2026-11-17 (T108, see below); the `ohya usage` report
  shipped as T094.

- Usage report shipped (2026-11-16, T094, plan 002): `ohya usage`
  (cmd/ohya/usage.go) combines server usage records with transcript
  evidence in the hub and reproduced the manual 2026-11-16 numbers
  exactly (8 claude-code `sql` calls, 4 sessions, 2 devices, 1 timeout,
  max result 48,474 B, 0 `raw_json` reads; ~2.3 s on the 16.3 GB hub).
  Facts future work must keep:
  - Verified transcript evidence shapes. Claude Code: `tool_use` blocks
    named `mcp__ohya__sql`/`mcp__ohya__annotate` with `id` `call_*`,
    paired to `tool_result` blocks by `metadata` JSON (the result record
    embeds the call ID); availability comes from `attachment` records of
    type `prompt_snapshot` or `deferred_tools_delta`. Codex: `event_msg`
    records with `payload.type='item_completed'` and
    `payload.item.type='McpToolCall'`, `payload.item.server='ohya'`
    (fields `tool`, `status`, `arguments.query`, `result.content[].text`,
    `duration`, plus `payload.started_at_ms`/`completed_at_ms`), and the
    older `payload.type='mcp_tool_call_end'` with
    `payload.invocation.server`. `event_msg` records carry no searchable
    text, so FTS cannot find them — Codex needs per-session
    `json_extract` scans.
  - The manual scan's two misses are corrected: its Codex query read
    `$.payload.arguments` instead of `$.payload.item.arguments` (14 real
    Codex `sql` calls exist, 1 hitting the 5 s deadline), and its
    availability count of 26 included one `edited_text_file` false
    positive (the true count is 25: 19 prompt snapshots + 6 deferred
    tool deltas). The plan's warning to verify the Codex format before
    claiming "no Codex use" was exactly right.
  - Discovery must be set-based on a large hub. The claude-code path
    finds candidates through `events_fts MATCH '"mcp ohya sql" OR "mcp
    ohya annotate"'` (unicode61 splits `mcp__ohya__sql` into mcp/ohya/sql)
    and confirms structurally by parsing metadata; one FTS query beats
    every per-session scan. Availability is one `EXISTS (... LIMIT 1)`
    query over `sessions_recent` (~1 s) where a per-session loop cost
    47 s over 1,256 sessions. Codex per-session scans stay (no FTS text).
  - Timeout classification: an error result counts as `timeout` only when
    its text contains "execution deadline exceeded" — and only for calls
    already known failed (`is_error` for Claude Code, `status='failed'`
    for Codex), because a successful result can itself quote the marker.
  - Output contract: deterministic, bounded (deadline default 120 s,
    5,000 sessions / 20,000 calls per agent), partial reports list what
    was skipped and exit 1 only when nothing was collected; no SQL text,
    query text, or transcript excerpts by default (`--local-detail` adds
    the local query text; result text is never printed). The JSON
    envelope follows ADR 0005.

- Codex call path verified live (2026-11-16, T103, plan 003 R1): the
  integration and the scan are both sound — plan R1's "no Codex call" premise
  was the scan bug T094 already fixed. Verified end to end with `ohya setup`'s
  own code path writing `[mcp_servers.ohya]` into an isolated `CODEX_HOME`
  (seeded sandbox hub), `codex mcp list` showing it, and `codex exec`
  (0.169.0) starting the server and completing `sql` calls; `ohya usage` on
  the real hub still reports the 14 Codex calls. New parser fixture
  `testdata/codex/mcp-tool-call.jsonl` pins the transcript records of one MCP
  call (parser test plus `cmd/ohya` `TestUsageCodexMCPFixture`, which ingests
  the fixture and asserts the scan reads it). Facts future work must keep:
  - Codex config layering, verified by live A/B runs (0.169.0): a
    `.codex/config.toml` in any ancestor of the working directory acts as a
    project override that BEATS `$CODEX_HOME/config.toml` when both define
    the same server name (a cwd under `/home/agent` resolved `ohya` to the
    real installed entry and answered from the real hub; the same config from
    `/tmp` answered from the sandbox hub). This is exactly what setup's
    preflight refuses ("defines ohya for this project") — a tempting "fix" to
    skip `$HOME/.codex/config.toml` in that walk was made and reverted; the
    guard is correct because the override really wins. Consequence for
    isolation harnesses (`cmd/memory-e2e`), hardened 2026-11-17 (T129):
    the gate now keeps every codex child cwd outside all `.codex`-bearing
    ancestors — the trace tree when its chain is clean, else a scratch
    directory under the system temporary directory, else a fail-fast error
    naming every shadowing config — and runs `codex login`, `codex mcp
    list`, and `codex exec` from that root (symlinks in the chain resolve
    first, matching the child's physical cwd). Also verified 2026-11-17
    (0.169.0): `codex mcp list` reads only `$CODEX_HOME` — it shows the
    sandbox entry even from a cwd whose ancestor config would win inside
    `codex exec` — so a registration check alone can never detect the
    shadowing.
  - 0.169 evidence shapes confirmed live: the `item_completed` item carries
    `server`, `tool`, `arguments`, `status`, `result.content[].text` plus
    `structuredContent`, `duration`, and payload `started_at_ms`/
    `completed_at_ms` (T094's shapes were right). The model may call MCP tools
    through the `exec` meta-tool (`custom_tool_call` name `exec` running
    `tools.mcp__ohya__sql(...)`); the McpToolCall event then sits between the
    `custom_tool_call` and `custom_tool_call_output` records. 0.169 also
    writes `response_item/additional_tools` (developer-role tool catalog;
    MCP tools appear as `mcp__<server>__<tool>`).
  - The ChatGPT-account auth on this VM hit its usage limit (reset 23:26
    local, 2026-11-16), which fails `codex exec` turns before any tool call;
    API-key auth in the isolated CODEX_HOME worked. Never run `codex login
    --with-api-key` against the real `~/.codex` — it replaces the user's
    `auth.json`.

- Recall evaluation set shipped (2026-11-17, T107, plan 003 R5): synthetic
  sessions in `testdata/recall/` (all three agent formats, markers identify
  known answers) plus `internal/recalleval` — question set, `Ingest` through
  the production parse/write path, and `Run` through the mcpserver engine.
  Runs in `make check`; baseline recall 9/9 at top 10. Facts worth keeping:
  - Answer events are identified by unique `oyr-*` markers resolved at run
    time (`text LIKE`, exactly one event required); numeric event IDs are
    NOT pinned in code — they are deterministic per corpus revision and
    pinned by TestRecallDeterministic instead, so fixture edits do not need
    repinning. Markers never appear in MATCH terms (a question must not pass
    by matching its own marker).
  - The set deliberately includes multi-match questions (failed test vs
    diagnosis, stale bound vs correction) so BM25 ordering is exercised;
    several answers must rank below noisier matches and still land in the
    top 10.
  - The O6 measurement recipe (plan 004, T112's unblock condition) is pinned
    by TestRecallMeasuresUnindexedToolOutputs: deleting the FTS rows of both
    agents' tool outputs (Codex `function_call_output`: kind='response_item',
    tool_name NULL, metadata callId present; Claude Code `tool_result`:
    kind='user' with a tool_result first metadata block) drops recall to
    6/9 — exactly the three questions whose answers live only in tool
    outputs. External-content FTS5 delete works as INSERT ... SELECT with the
    literal first column.
  - The repo-scoped question guards `sessions.repo` derivation end to end:
    a broken derivation empties the scoped result and fails the guard.

- Tailcat address adapter (2026-11-16, T121, ADR 0011;
  internal/transport/address): hub identity (version 1: node private key,
  pre-shared key, DERP region as JSON) lives owner-only in
  `<state>/tailcat-hub/identity.json` through durablefile; since T123 this
  is the only transport adapter (the tsnet stack is deleted). Verified
  library facts that shaped the code:
  - `Server.Start` picks defaults into locals and never writes Key,
    PresharedKey, or Region back — an identity must be generated and
    reloaded by ohya itself, and the selected region of a first start
    without an explicit Region is only observable through
    `ParseAddr(srv.TailcatAddr())`.
  - `ConnInfo.Addr()` zeroes RegionID/RegionCode/RegionName and node
    names before CBOR encoding, and `ParseAddr` restores RegionID as
    index+1 and node names from HostName — so the address string does
    not depend on the region's ID, but a round-tripped region is not
    byte-identical to a configured one. The adapter therefore saves a
    caller-provided Region verbatim and only falls back to the parsed
    region when Tailcat picked one from the default map; a configured
    region with a different RegionID is refused on restart (the
    identity pins it).
  - Hermetic tests: a local DERP relay is `tailscale.com/derp/derpserver`
    behind httptest StartTLS on 127.0.0.1:0 with a node that sets
    `IPv4: "127.0.0.1"`, `IPv6: "none"`, `DERPPort`, and
    `InsecureForTests: true` (derphttp skips cert verification); no STUN
    port and no public relay map are needed. TestMain sets `IN_TS_TEST`
    (magicsock skips portmapper probes) and stubs
    `netcheck.HookStartCaptivePortalDetection` exactly like tailcat's own
    tests, or every engine start waits on gateway probes. A fresh client
    resends its meow ping every second for up to 10 s, so no explicit
    wait for the server's DERP connection is needed; a full hub+device
    exchange test costs ~7 s.
  - `PresharedKey.UnmarshalText` echoes its input in the error, so the
    identity loader decodes through string fields and reports an invalid
    pre-shared key without the underlying text (never leak key bytes
    through error strings); the corrupt-identity regression pins that
    plus "no overwrite on failure".
  - The device dials `Client.DialTCPPort(ctx, 9027)`; one Device is bound
    to one hub address for its process lifetime (a second address is
    refused). Dial caps itself at 20 s when the caller's deadline is
    later, matching the uploader's 60 s reconnect budget. `Drain(ctx)`
    (Server/Client `DrainTCP`) exists for clean shutdown: exiting right
    after Close can lose FINs inside the in-process TCP stack.
    Never-started case (fixed 2026-11-16, T119): tailcat's
    `Client.DrainTCP` derefs `c.lb`, which stays nil until the first
    Ping/dial runs `ensureStarted` (`Client.Close` is the guarded one),
    so draining a never-dialed client — or the fresh client
    `resetClient` swaps in after a failed dial — panicked with exit 2
    on the `mcp --remote` and join exit paths. `Device.Drain` now
    mirrors `Hub.Drain`'s started guard: it drains only a client that
    completed a dial since the last reset (no dial, no TCP endpoints,
    nothing to flush) and returns nil otherwise.
  - go.mod: `github.com/tailscale/tailcat v0.7.0` direct,
    `tailscale.com` still v1.104.4 (no version bumps; tidy moved
    `golang.org/x/sys` v0.49.0 from indirect to direct because tailcat's
    unix file compiles into the imported package).

- ohya1 join codes and the caller switch to the address transport
  (2026-11-16, T122, ADR 0011/0012; cmd/ohya, internal/transport/address,
  internal/device): serve, devices invite, join, and mcp --remote now use
  internal/transport/address. T123 then deleted the tsnet adapter,
  internal/control, and the headscale dependency; doctor's join-state
  check verifies the saved hub address instead of probing a coordination
  URL, and transport path/round-trip reporting waits for T125.
  Facts the switch depends on:
  - The join code is `ohya1.<invitation>.<tailcat-address>`; invitation is
    hex and the address is base64url, so `.` never collides. The address is
    a secret (pre-shared key inside), so no error message, log, or test
    output may include it or the code; the old `<invitation>:<preauth-key>`
    form is rejected with a message that names the new format and
    `ohya devices invite`.
  - `address.LoadHubAddr(stateDir)` recomputes the hub address offline from
    the saved identity through the exported
    `tailcat.DiscoPublicForNode` + `ConnInfo.Addr`; it is byte-identical to
    `Server.TailcatAddr()` because the identity pins the exact region.
    devices invite uses it, so it needs no running transport.
  - Hub restart recovery: `Client.up` short-circuits after the first meow,
    so a long-lived client never re-meows, and a restarted hub forgets every
    peer — handshakes then die against the old direct UDP endpoint and the
    first dial after a restart times out. The adapter now (a) sends one
    meow (`Client.Ping`) before every dial — after the first success the
    library returns immediately, so this costs one relay packet — and (b)
    replaces the tailcat client after a failed dial, because the client key
    carries no identity (ADR 0011); the retry then connects in ~4 s. The
    uploader's backoff and RemoteSession's single retry absorb the failed
    first dial; hub-restart visibility dropped from >2 min to ~4 s.
  - Hello-first rebind probe is `hubsync.RemoteSession.Query(ctx,
    "SELECT 1")` on the saved credential: the hub's CodeAuth error text
    distinguishes revoked ("device is revoked") from a different hub
    ("credential does not belong to a paired device"); on rejection join
    fails with that reason and never consumes the invitation. No protocol
    or TLA+ change (ADR 0012 says so); `make tla` still passes.
  - Hermetic e2e: the local DERP recipe from T121 moved to
    cmd/ohya/serve_e2e_test.go (startLocalDERP + a region JSON file);
    child-serve injects it through OHYA_TEST_DERP_REGION_FILE in TestMain,
    never the public relay map; the same TestMain sets IN_TS_TEST and stubs
    captive portal detection for every child. A SIGTERM to a child in its
    first ~300 ms (still in package init) kills it before its signal
    handler exists — stop test children only after they did observable
    work, or the strict stopChild reports "signal: terminated".

- Headscale stack removal (2026-11-16, T123, ADR 0011 "What is removed"):
  internal/control, internal/transport/tailcat (tsnet adapter, shared-node
  lock, loopback proxy, SSL_CERT_DIR trust dir, netns switch, MagicDNS
  wait), cmd/tailcat-spike, and tla/TransportOwner (model, cfg, runner
  line, README section) are deleted; `make tla` covers Sync and Auth only.
  go.mod shrank to five direct dependencies (headscale plus everything
  only it carried is gone; golang.org/x/net and x/sys moved back to
  indirect). Release binary, linux/amd64, package.sh flags:
  72,148,584 → 49,350,600 bytes (31.6% smaller; macOS size unmeasured —
  no darwin machine in this task; docs/performance.md "Release binary
  size"). doctor lost --control and the coordination probe: the join-state
  check now reads credential.json plus hub-address.json, warns on a
  missing address, and never prints the address (a secret). The
  coordination_plane_absent JSON code died with its only producer.

- go.sum stability under the verification gate (2026-11-16, T123 repair):
  the check setup runs `go mod download all`, which records hashes for the
  whole module graph and grows go.sum far beyond what `go mod tidy`
  writes. A candidate that stages only the tidied go.sum fails the gate
  with "the verification command passed but changed project files". Stage
  a go.sum produced by `go mod download all` (a superset of tidy's) so the
  tree is stable under both commands; do not re-tidy it away.

- Hub setup and the device join command (2026-11-16, T124, ADR 0012;
  cmd/ohya, internal/setup, internal/transport/address, install.sh):
  `ohya setup --hub` adds a second owned service (`ohya-serve.service` /
  `com.nibzard.ohya.serve.plist`, receipt `setup/hub-service.json`) after
  the ingestion service, skips the recovery point for it (one copy per run
  already happened before the ingest restart; the online snapshot absorbs
  the running writer), then waits for the identity the serve process
  writes and probes the hub before issuing the code. Facts worth keeping:
  - Probe classification is two-step on purpose (address.ProbeHub): an
    outbound TCP connect to the relay node the identity pins (DERPPort 0
    means 443) decides ErrRelayUnreachable, and only then a device-side
    dial of the hub address decides ErrHubUnreachable — tailcat's meow
    Ping alone cannot tell an unreachable relay from a hub that never
    connected to it, but the prior TCP check collapses that ambiguity.
  - The join code is printed only to stdout text or the JSON field
    `data.hub.code_secret`; the setup results never embed it, and the
    `hub` component's detail names only the relay region and node key.
  - install.sh `--join` reads /dev/tty with `stty -echo`; the pty test
    feeds the code through `script -qec` with a delayed write (input that
    arrives before stty runs is echoed by the pty) and strips \r before
    asserting. Without a tty or under --json it refuses and names
    --join-code-env.
  - The e2e harness replaces the service manager through a package-level
    `serviceRunnersHook` in cmd/ohya (nil in production): the fake maps
    systemctl/launchctl verbs to real child-serve/child-run ingest
    processes. Doctor's service inspection uses the same hook so it sees
    the children. Ordering trap: `ohya.service` is a substring of
    `ohya-serve.service`, so the serve commands must be matched first.
  - mcp --remote on a joined setup needs `--state <setup>/join`, not the
    setup state root; the setup engine's client args already point there.
  - `make check` ends with `go run ./cmd/commentcheck`, which fails on every
    comment in a .go file (only `//go:embed` is approved) — including the
    doc comments standard Go style asks for on exported symbols. Write no
    comments at all; keep the rationale in docs and NOTES.

- Plan 004 S3 offline storage analyses (measured 2026-11-17, 16.6 GB hub,
  2.69 M events; read-only scripts under `perf/`, raw tables in
  `perf/t102/` and the task artifact dir, both gitignored; no transcript
  content in any output):
  - FTS index share (`perf/ftsshare`, every 20th event id inserted into
    per-category contentless FTS5 tables, `dbstat` after `optimize`;
    the sum of the category tables overestimates the all-docs index by
    only 3%, so shares are trustworthy): tool outputs hold 67.7% of the
    FTS index (22,386 of 64,457 sampled indexed docs; 100.6 of
    137.4 MB sampled text). Codex `response_item` across all roles is
    59.6% alone; Codex `event_msg` contributes zero — all its rows carry
    NULL text, so FTS never indexes them. O6 (index less) has real
    headroom: excluding tool outputs would remove about two thirds of
    the index; the recall cost is unmeasured and still needs plan 003
    R5. Reference cross-check: the sample's optimized index/text ratio
    is 0.351 versus about 0.30 on the hub (0.83 GB over ~2.7 GB indexed
    text — the earlier "5.6 GB of text" figure counts NULL-text rows).
  - Duplication and compression per device (`perf/devratio`; phase A
    every 20th row, phase B 60 sessions per device picked with
    probability proportional to estimated size, zstd-3 =
    klauspost SpeedDefault, dictionaries trained by `zstd --train` on a
    disjoint half of the picked sessions):
    VM (5.5 GB raw): per-row 2.26x, 64 KB blocks 3.01x, 1 MB blocks
    3.76x, per-row with a 110 KB dictionary 2.61x (self-trained also
    2.62x), 1 MB blocks with dictionary 4.43x, session-stream duplicate
    share 30.4%.
    MacBook (4.5 GB raw): per-row 2.20x, 64 KB blocks 2.82x, 1 MB
    blocks 3.28x, per-row with dictionary 2.47x — but 2.78x when
    measured on the dictionary's own training half, so the
    brainstorm's 3.08x per-row dictionary number (trained and measured
    on one sample) is optimistic by roughly 0.3x — session-stream
    duplicates 34.8%. The brainstorm's ~33% duplication therefore holds
    on the MacBook data. Large strings (>=256 B) carry 76.5% (VM) and
    86.7% (MacBook) of raw bytes. Cross-device large-string repeats are
    ~0% (0.0% VM, 0.2% MacBook): the two devices share almost no
    content, so per-device dictionaries lose nothing and content
    addressing (O2) has no cross-device win on this hub. A row-uniform
    sample undercounts duplication (18-20%) versus whole-session
    streams (30-35%) because it breaks intra-session repeats — use
    session streams for duplication claims.
  - klauspost/compress zstd decode cost (`perf/zstdbench`, blocks of
    consecutive raw_json records from the 4 largest sessions, i9-12900HK,
    single goroutine, warmed, klauspost v1.20.0 = the module's pin):
    64 KB block no dictionary 123 us (759 MB/s), with dictionary
    158 us (590 MB/s); 1 MB block no dictionary 1.33 ms (874 MB/s),
    with dictionary 1.72 ms (676 MB/s). Decode runs at 0.6-0.9 GB/s on
    one core; a dictionary costs about 25% of decode speed. O1's
    `ohya_raw(id)` decoding one 1 MB block costs ~1.3-1.7 ms — the
    brainstorm's "about 1 ms" estimate was right — and a 64 KB block
    costs ~0.12-0.16 ms, so block size trades decode latency against
    ratio, not against any throughput wall.
  - `zstd --train` needs a few dozen samples minimum (it fails with
    "Src size is incorrect" on toy corpora), and mattn's dbstat needs
    the extra `sqlite_dbstat` build tag — `perf/README.md` records both
    run commands.

- Transport path and first-upload reporting (2026-11-17, T125, ADR 0012;
  internal/transport/address/path.go, cmd/ohya, internal/registry,
  internal/uploader): doctor, join, devices list, and the uploader now
  report path type, relay region, round trip, and backlog drain
  progress. Facts future work must keep:
  - Device side measures with up to three `tailcat.Client.DiscoPing`
    calls (`Device.PathInfo`): `PingResult.Endpoint != ""` is a direct
    path, `DERPRegionID != 0` is a relay with `DERPRegionCode`,
    `LatencySeconds` is the round trip. Repeated pings upgrade a relay
    answer to direct; never invent a value — unmeasured is
    `unknown`/null.
  - Hub side reads `Server.Status().Peer` — but `PeerStatus.TailscaleIPs`
    is EMPTY in tailcat, so a connection's remote address cannot be
    matched by IP list. Each peer has a deterministic tunnel address:
    `fd7a:115c:a1e0::/48` ULA plus 10 bytes of the node key (tailcat's
    unexported `tcAddrForKey`, mirrored as `address.peerTunnelAddr`; the
    live e2e test fails if upstream ever changes the scheme).
    `CurAddr != ""` is direct; `Relay` holds the DERP region CODE (not
    the name) via magicsock's `derpRegionCodeOfIDLocked`.
  - The sync wire protocol is unchanged (ADR 0012): the hub learns the
    path from its own transport status at authentication time through
    `pathRecorder` (cmd/ohya/hubpath.go wraps the registry
    Authenticator), persisting registry.db v2 (`devices.last_path`,
    `devices.last_sync_at_ms`; ALTER TABLE migration from v1) on connect
    and at most every 30 s. `ohya devices list` prints them; null means
    never synced.
  - Doctor on a paired device dials the hub once: PathInfo then one
    `SELECT 1` through the sync protocol (protocol compatibility is the
    real exchange, not a version string compare). A hub behind a closed
    port holds the probe ~10 s (meow resend loop); the whole probe is
    bounded at 45 s. The JSON `transport` object appears only when
    paired; hub address/credential never appear anywhere.
  - Uploader backlog gauges (`upload_backlog_bytes`,
    `upload_backlog_sent_bytes`, `upload_backlog_rate_bytes`,
    `upload_backlog_eta_ms`): a drain starts when a pass sees pending
    bytes and completes when a later pass sees none (completion is
    detected at the START of the next pass, so tests need one extra
    sync). Rate publishes only with ≥2 ack samples spanning ≥1 s inside
    a 10 s window; before that the log line has no rate. Log lines need
    a ≥1 MiB total, at least one acknowledged byte, and bytes left
    (except the completion line), and print at most every 10 s — small
    appends stay fully silent. ETA uses float math; integer
    `Duration/Duration` truncates.

- Recall deadline friction removed (2026-11-17, T104, plan 003 R2): the
  timed-out production call was `events` JOIN `sessions` ON cwd with
  ORDER BY time_ms DESC LIMIT 5 (verified in the hub transcripts; 5,027 ms
  timeout, 3,991 ms sibling, both 2026-11-15 23:32 local — inside the
  recovery-copy iowait storm). Facts worth keeping:
  - The plan is indexed but streams every matching events row into the
    sort (one table-page read per project event when cold; ~16 ns warm).
    The fix is a query recipe, not a schema change: select the newest IDs
    in a subquery over `events_settled` (SEARCH ev USING COVERING INDEX
    events_session_time), then fetch those IDs by rowid. Published in the
    sql tool description and DB.md "Project recall example";
    TestProjectEventsRecipePlan pins the covering plan, equivalence test
    pins identical rows. Cold on the live hub at the incident cwd: 1,080 ms
    → 218 ms.
  - `perf/recallduration` reproduces and measures the pattern on a compact
    mix fixture (scale 2.8, seed 7: 3.88 M events, largest cwd 493,571
    events; ~25 min build). Usage-record timings (T092 sink): plain join
    p95 8.6 ms warm / 2.4 s cold; recipe 4.0 ms / 100 ms. A
    `posix_fadvise(DONTNEED)` eviction makes cold runs repeatable; cold
    single runs are order-sensitive (later shapes keep some pages).
  - Index rejected with measurements: `events(time_ms DESC, id)` costs
    +70.5 MiB and 82 s build on the 6.7 GiB fixture, answers an active
    project in 75 ms cold, but a no-match/old cwd scans the whole index
    backward — 5,004 ms on 20 of 20 runs (hard timeout). Views cannot fix
    the shape (no parameters; they flatten into the same plan).
  - Driver fact: the engine's authorizer stays registered on pooled reader
    connections, so a raw `PRAGMA page_count` through the readers pool
    after an engine query is denied ("not authorized") — read such pragmas
    through the writer (fix in perf/recallduration).
  - Live-hub before/after confirmation rides on T113 (hub binary predates
    the usage recorder) and the T099 baseline window.

- Device-side client-outcome records for `ohya mcp --remote` (2026-11-17,
  T108, plan 002): hubsync.RemoteSession records one usage record per
  sql/annotate exchange plus the client-view observer metrics, wired from
  runMcpRemote through RemoteSessionOptions{DeviceID, Usage, Observer}
  (join and doctor probes pass zero options and record nothing). Facts
  future work must keep:
  - The device-side record keeps what only the device knows: the MCP
    client name/version bucket (read from the mcpserver origin context the
    tool handler already injects — requestOrigin sets path local, the
    session overrides it to remote), the session hint, its own device
    identity, the client-observed total in phase_ms.total_ms, and rows for
    successful sql calls. Hub-only fields (phase breakdown, result bytes,
    sqlite_code, reads, functions, fts, annotate op counts) stay zero —
    the hub-side engine record owns them; ohya usage reads both through
    the same observe/ tree per --state, and joining across machines stays
    plan 001 milestone 5 (T098).
  - Outcome classification is wire-code based (remoteUsageOutcome): hub
    CodeBusy → busy, CodeRejected/CodeTooLarge/ErrTooLarge/auth refusal →
    rejected, ctx cancel → cancelled, ctx or conn deadline → timeout
    (os.ErrDeadlineExceeded — the conn deadline surfaces as a net error,
    not context.DeadlineExceeded), everything else (dial loss, protocol
    garbage, CodeInternal carrying the real SQLite error) → sql_error. A
    hub-side timeout arrives as CodeRejected (the endpoint folds
    ErrDeadline into it), so the device cannot distinguish it — the
    hub-side record does.
  - The origin Path from mcpserver.requestOrigin is always local (an
    Engine default); RemoteSession ignores it and stamps remote, because
    the session knows it is the client end.
  - Metrics from the session mirror the engine's label shapes exactly
    (sql_requests{query_class,query_outcome}, sql_total_duration,
    sql_rows, sql_truncations{truncation_kind}, sql_busy_rejections,
    annotate_validation_failures) — hub and device snapshots stay
    comparable series-by-series; per-process snapshots cannot
    double-count.
  - Trap fixed alongside: openUsageWriter returns nil *UsageWriter on
    failure, and assigning that to the UsageSink interface field made a
    non-nil interface — the local mcp path and serve then PANICKED on the
    first sql call after "usage records disabled" (nil deref inside
    UsageWriter.Record stamping InstanceID). usageSink() in cmd/ohya/mcp.go
    now converts nil-for-nil; pinned by TestMCPStdioServesWithoutUsageRecords.
  - e2e: TestFreshnessSmoke asserts the real child-mcp --remote stack
    writes one device-side record per poll query into the join state's
    observe/ tree (client "other" — freshness-observer is outside the
    fixed set). waitUsageRecords(n) TRUNCATES to n records — re-read with
    readUsageRecords before counting everything.

- Released-schema upgrade tests own DB.md upgrade-policy item 5 (2026-11-17,
  T083): releases ship databases (v0.1.0/v0.1.1 schema 6, v0.1.2 schema 7),
  so DB.md's old "no released database exists" baseline was false.
  `internal/store/migrate_released_test.go` builds a database at each schema
  in `releasedSchemas` from the embedded migration bytes, seeds rows a
  released write path could have written (FTS-indexed event, ordinal replay
  duplicate, token usage, annotation, and from schema 7 a workspaces row),
  upgrades through `store.Open`, asserts the rows survive, and then ingests
  through the current write path. The same change that publishes a release
  adds it to `releasedSchemas` and DB.md policy item 5;
  `TestReleasedSchemasMatchPublishedTags` enforces that wherever git tags
  are visible — it fails on a published v-tag missing from the list, on a
  wrong schema number (it counts each tag's shipped migrations), and on a
  shipped migration file whose bytes differ from the tree (the immutability
  rule). CI's check-linux job fetches tags at checkout (actions/checkout
  `fetch-tags: true`, T131, 2026-11-17), so the guard runs in CI too — a
  depth-1 tag fetch carries each tag's full tree, which is all ls-tree and
  show read; the macOS check job and the bench job still fetch no tags. The
  deferred events(source_id, parser_version) doctor
  index keeps its deferral (see the 6.6 GB hub note above): add it with the
  next migration, which these tests then exercise as an upgrade from every
  released schema.

- Local observability milestone 3 delivered (2026-11-17, T093,
  plans/001-runtime-observability.md): internal/observe gained the snapshot
  publisher (atomic snapshot.json every 10 s plus startup and Close, one
  history.jsonl sample per minute, 256 KiB/16 MiB caps, truncation fallback
  that keeps counters), instance discovery (64-instance bound with explicit
  truncation), PID liveness probing, and runtime resource sampling.
  CleanStaleInstances runs at startup: at most eight terminated instances
  per role and seven days of files, and a live or unknown PID is never
  deleted. cmd/ohya gained `ohya status` (freshness, process state,
  sources, backlog, findings; exit 3 when anything is stale or degraded)
  and `ohya metrics` (1m/5m/1h/24h windows; counter deltas over monotonic
  elapsed time; quantiles need twenty observations; instances are never
  merged). All four long-running commands take `--observe on|off`; serve
  and mcp also write usage records, ingest and join do not (they never
  serve sql). `--remote` on status reports the capability gap as finding
  remote_status_unsupported; hub-side aggregates are milestone 5 (T098).
  Resolved 2026-11-19 (T132): a SIGTERM during an ingest poll or during
  store.Open is a clean stop — exit 0, no per-source "context canceled"
  diagnostics, no ingest_failures counts. cmd/ohya/ingest.go treats
  ctx.Err() as a clean stop at every cancellation point (store.Open, poll
  error returns, the poll source loop, RegisterSource/Ingest/SourceState
  error paths, workspace observation), mirroring uploader.Run's
  ctx-first check. Verified fact from the fix work: with the pre-fix
  code, poll's RPC errors were already swallowed as per-source failures
  (continuous mode exited 0 but noisy; --once exited 3); the one observed
  exit 1 was the store.Open window after the startup snapshot publishes —
  the only ctx-aware call whose error returns to runIngest as a command
  error. TestStatusChildCleanShutdownPublishesFinalSnapshot now requires
  exit 0 exactly.
  Extended 2026-11-20 (T150): serve and join carry the same contract. The
  ctx-aware startup error returns are node.Listen and store.Open in
  runServe, and pair plus rejoin/verifyCredential in runJoin; each treats
  a canceled context — errors.Is(ctx.Err(), context.Canceled), the signal
  class — as a clean stop (exit 0). DeadlineExceeded is deliberately NOT
  a clean stop: setup embeds runJoin under a 90 s pairing deadline, and a
  bare ctx.Err() gate would report a timed-out or interrupted pair as a
  successful remote binding; setup additionally re-checks pairCtx.Err()
  after a nil runJoin so an interrupted pair is never bound. ingest's
  T132 gate keeps the bare form because runIngest has no deadline caller.
  reportJoinTransport skips its report on a canceled context — pre-fix it
  printed "hub path unknown (… context canceled)" on a SIGTERM during the
  resume window even though join then exited 0. Deterministic tests use a
  pre-canceled context plus a pre-expired deadline (pinning the signal
  class); the join ones dial a synthesized tailcat address with RegionID
  -1 (fresh keys, no embedded region), which fails fast inside the
  DERP-map fetch before any platform networking, so no local relay is
  needed. Fact kept for future transport tests: tailcat's process-wide
  in-memory defaultDERPMapCache (1 h max age) can satisfy the map without
  touching ctx, so a canceled-context test stays outcome-deterministic
  only because the next ctx-aware step (PickBestRegion netcheck, Ping, or
  store.Open's verifyWriter) also fails immediately — assert the clean
  stop, never which gate fired. stopChild in serve_e2e_test.go already
  required exit 0 for a serve child stopped after observable work, so no
  e2e assertion needed tightening; a SIGTERM-during-startup e2e would be
  racy (the pre-NotifyContext window kills with signal status, and the
  local-relay startup window is milliseconds wide).

- Logs and diagnostics delivered (2026-11-17, T096, plans/001 milestone 4):
  internal/observe gained the LogWriter (`events.jsonl`, 8 MiB plus two
  rotated files per instance) and cmd/ohya gained `ohya logs` and
  `ohya diagnose`. Facts future work must keep:
  - Producer contract: Event() never blocks (256-deep queue, drops are
    counted), coalesces warn/error bursts only while an identical entry
    is queued (claim-before-write under one mutex makes the Counts map
    race-free), and rejects unknown event codes — raw error text has no
    field, so no secret can enter through an error. The final summary
    carries real deltas when allowlisted counters moved (observed
    `discovery_scans=17` in the smoke run) and outcome "final" when they
    did not — both branches are intended; TestLogWriterRejectsInvalidCodesAndOversizeLines
    pins the second, the smoke run exercised the first.
  - Setup owns the macOS startup stderr file (state/logs/<label>.stderr.log,
    rotated at 8 MiB to .1; no StandardOutPath — stdout stays unowned)
    while Linux keeps the journal; launchd restart policy (KeepAlive,
    ThrottleInterval 5) is unchanged. Setup also prunes stale observe/
    instances (observe.CleanStaleInstances) and uninstall preserves
    observe/ plus the stderr log. Native launchd smoke remains unrun —
    no macOS machine was available; systemd user-session smoke passed:
    a failing unit surfaces its usage error in the journal and stops at
    StartLimitBurst=5 (no restart loop), a healthy unit's startup,
    shutdown, and final summary are visible through `ohya logs`, and
    `ohya diagnose` exports a 7.7 KB artifact. Units and state were
    torn down through the registered idempotent cleanup.
  - Boundary proven by construction and TestDiagnoseNeverExportsSeededSecrets:
    logs/diagnose read only the already-sanitized observe/ files, so
    secrets seeded in transcript text, SQL literals, annotation values,
    source paths, error blobs, and device.json never appear in output.
  - Hub-side transition events (auth failures, device pairing) are not
    emitted from the hubsync endpoint yet — the EventSink reaches
    uploader, mcp engine, and local ingest only; that plus remote
    aggregates is milestone 5 (T098).

- Pi Durable comparison priorities (2026-11-16):
  1. Measure whether agents retrieve useful answers. Use known questions
     and expected Event IDs. Track latency, result size, and failures
     (`plans/003-recall-tool-improvements.md`).
  2. Protect the existing recovery contract through storage changes.
     Preserve commit-before-acknowledgment ordering and tests for lost
     acknowledgments, interrupted commits, process crashes, and hub restarts.
  3. Improve handoffs with source references. Retrieve decisions, failed
     attempts, and unfinished work. Cite Events, then check the current
     repository before acting.

- v0.2.0 released (2026-11-17) from `3e6adba`: round 9's usage
  instrumentation (`ohya usage`/`status`/`metrics`/`logs`/`diagnose`,
  ADRs 0005/0010) and the Tailcat transport migration (ADRs 0011/0012,
  `ohya setup --hub`, one-line join, no control plane). Candidate
  qualification run 44087678215; the first candidate 44085094843 failed
  on a linux-only syscall in perf/recallduration plus three
  environment-sensitive tests (symlinked temp paths on macOS, mtime
  granularity on runner storage, a fixed 80 ms ticker window). CI macOS
  runners again proved narrower than local linux: run make check on all
  three platforms before cutting a release, not only linux.

- Doctor hub-unit validation (2026-11-17, T134): doctor now parses systemd
  user units itself instead of trusting only setup receipts — the live hub
  ran `ohya-hub.service`, a hand-named unit no receipt covers, whose
  ExecStart still passed the flags T122 removed (--control-listen,
  --stun-listen, --tls-cert, --tls-key, --control-url), while the running
  serve (PID 45514) executed the pre-v0.2.0 binary replaced on disk
  (/proc/45514/exe "(deleted)"); installed v0.2.0 doctor exited 0 over it.
  Facts future work must keep:
  - `setup.ScanServeUnits(dir, bin)` claims a unit only when its ExecStart
    executable resolves (EvalSymlinks) to the RUNNING binary and the first
    argument is `serve` — a dev-build doctor never reports an installed
    deployment, and units for other binaries never fire. The unit NAME is
    irrelevant (ohya-hub.service, ohya-serve.service, anything else).
  - Flag validation shares the real serve flag set through
    newServeFlagSet (cmd/ohya/serve.go), so the check cannot drift from the
    command; a removed flag surfaces as the flag package's own error text
    ("flag provided but not defined: -control-listen").
  - The replaced-executable check reads systemctl MainPID (real unit name
    required) then /proc/<pid>/exe for the " (deleted)" suffix — Linux only;
    darwin hub deployments (launchd plists) are NOT scanned, and the check
    degrades silently where /proc or systemctl is absent. Covering plists is
    unwritten work; no live macOS hub exists as evidence.
  - Remedies differ by unit name: the setup-owned ohya-serve.service gets
    "rerun ohya setup --hub to rewrite"; a hand-named unit additionally gets
    the retire step (systemctl --user disable --now <unit>).
  - The e2e hook stays error-only; the PID query is the overridable
    hubUnitMainPID var (default setup.ServiceMainPID). Tests must stub it —
    the default would invoke real systemctl for injected unit names.

- Live-hub performance tracking (2026-11-17, T140): perf/livehub is the
  recurring tracker for the deployed hub, checked in and tested; on the
  hub VM a cron entry (`*/15 * * * *`) runs `-mode capture` then
  `-mode report` against `~/.ohya`, keeping the standing report at
  `~/.ohya/perf/report.md` (binary at `~/.ohya/perf/bin/livehub`,
  rebuild with `go build -o ~/.ohya/perf/bin/livehub ./perf/livehub`
  after checkout changes). Capture reads `history.jsonl` and
  `usage.jsonl` only — no database opens, no new services; the 15-minute
  cadence beats both retention windows (24 h history samples, eight
  retained terminated instances per role). Rows and comparison rules are
  documented in docs/performance.md "Live-hub trend". First capture
  (655 records, 12 instances, 2026-11-17) already states three findings:
  peak WAL 160.5 MiB beyond the round-8 ceiling plus allowed variation
  (live hub: 2.7 M events, 16 GiB database, continuous ingest — the
  matrix basis was 168 k to 1 M events; watch checkpoint behavior, not a
  matrix failure); sql p95 993 ms with n 9 over the 250 ms screening
  budget (agent-driven mix, zero timeouts); and the standing gap that
  the serve process (PID 45514) predates v0.2.0, publishes no observe
  data, and therefore contributes no hub-side metrics until the operator
  restart (T126). Ingest-side numbers from the v0.2.0 ingest daemon on
  day one: pass p50/p95 76.5/121 ms over 14,330 passes, sqlite txn
  p50/p95 8.05/46.7 ms, publish p50/p95 0.30/0.48 ms — inside the
  T070/T071 context band.

- Revocation teardown race, fixed 2026-11-17: TestRevocationCancelsInFlightRequest
  (internal/registry) failed once under full-suite parallel load because the
  endpoint could still write one error frame between a session's cancel() and
  conn.Close() (closeDevice orders cancel-then-close; a descheduled closer let
  the woken handler reply first). Root cause fixed in internal/hubsync/endpoint.go:
  connSession now guards every protocol write with the session context, so a
  cancelled session sends nothing and teardown owns the connection. If a similar
  "request completed after revocation" flake appears again, check for writes
  bypassing connSession (the Serve-loop busy rejection is pre-session and stays
  raw by design). Same load exposed a second, test-side race:
  TestObserverCountsHubBatchStages asserted hub_ack_write_duration, which the
  server records after the ack bytes are already on the wire, so a preempted
  server goroutine loses the assertion; that one counter now uses
  observertest.Recorder.AwaitDurationCount.

- Agent cleanup loop closed (2026-11-20, T148, ADR 0016): FIND (read-only
  sql recipes) → MARK (annotate namespace tags, key garbage) → EXECUTE
  (`ohya purge --marked <namespace.key>`, owner only). Facts worth keeping:
  - The garbage-audit recipes in the sql tool description and DB.md are
    deadline-safe by shape, verified read-only against the live 16 GB hub
    (2026-11-20): `duplicate_of` and record length carry no index, so a
    hub-wide "find duplicates/oversized" scan would read every stored
    record; the oversized recipe drills per source (`cursor_bytes` ranks
    sources first; the largest live source answers in the drill shape
    instantly) and the replay count per session. The first draft of the
    empty-sessions recipe counted events per session with a correlated
    subquery and exceeded 120 s on the live hub (one covering-index range
    per session, cold and disk-contended) — it now filters on the
    maintained `sessions.event_count` (sessions-table-only, instant; the
    published-recipes seed sets event_count explicitly because direct SQL
    seeding bypasses the aggregate).
  - `--marked` splits its value at the FIRST dot, so a namespace used with
    the selector gets no dot (tags.garbage, never a.b.c); both halves must
    pass the annotation name charset (store.ParseMarkSelector, one rule
    with annotate). The mark subquery in sessionFilter matches any writer,
    and marks die with their events on purge — the tombstone scope
    (`marked=tags.garbage`) is the durable record of what selected it.
  - TestPublishedRecipesRunVerbatim's seed grew (stale/40-day source, a
    no-repo workspace + session, a 70 KiB event, a replay duplicate, a
    stale model, a seeded tags/garbage mark), and the date-bounded find
    marker moved from "strftime('%s','now')" (now also in the
    stale-sources recipe) to "7*86400". Markers must stay unique across
    all 20 blocks.
  - No TLA+ change: --marked selects sessions, it does not touch the
    source lifecycle, write path, or tombstone contract (ADR 0016 states
    the scope).

- Owner-scoped purge shipped (2026-11-19, T144, ADR 0013): `ohya purge`
  (dry-run default, --apply) selects real sessions by --session/--repo/
  --agent/--before, evicts whole sources one transaction each, and
  tombstones every evicted source (migration 008, purged_sources);
  `ohya sources reallow <id>` removes a tombstone and restores the source
  at its purged cursor. Facts future work must keep:
  - The "one source, one session" premise needed a correction: a source
    holds at most one REAL session plus at most its own source-scoped
    synthetic session. Measured on the whole live hub (read-only,
    2026-11-19): 292 claude-code sources mix a real session with their own
    synthetic companion (subagent started/result records carry no
    sessionId); 0 sources hold two real sessions, a foreign synthetic
    session, more than two sessions, or a synthetic session spanning
    sources. Synthetic sessions are therefore never selectable on their
    own (they follow their source), and the invariant is pinned by
    internal/store TestNoSourceHoldsTwoRealSessions.
  - Reallow must restore the source row at the PURGED cursor, never at 0:
    cursor 0 would make the device rewind and replay the whole retained
    file back into the hub (resurrection). With the purged cursor, the
    device's probe reconciles and history resumes from the purge point; a
    below-cursor replay then fails the existing no-stored-record rule.
    tla/Sync.tla pins this: PurgeSource freezes hubCursor; the mutation
    that resets it to 0 is caught (WireAckLeCommit fires first).
  - The endpoint maps the purge refusal (store.ErrSourcePurged) to
    CodeRejected, which the uploader treats as a per-source block carrying
    the hub's message — so the remedy text ("ohya sources reallow <id>")
    reaches the device log without any protocol change. Revocation of the
    device still wins over reallow (Auth.tla, checked).
  - Purge model cost: SyncConcurrent grew 53k → 929k states with the
    purge dimension (MaxPurges=1); Auth.cfg 515k → 4.6M. Liveness cfg sets
    MaxPurges=0 — a purged source legitimately stops syncing, so
    EventualSync needed the "or below a purge mark" escape. One Auth
    mutation survives by design (dropping Upload's srcReg guard changes no
    authorization state; the storage content of the refusal is owned by
    Sync.tla) — documented beside the existing survived-mutation
    precedent.
  - Migration 008 forces one more size-aware recovery copy at the next
    setup run (release-operations "Purge and recovery points"); freed
    pages stay in the file — no VACUUM by design.

- Instrumentation overhead baseline (2026-11-19, T097, plans/001 milestone 6):
  numbers, verdicts, and the scenario table are in docs/performance.md
  "Instrumentation overhead baseline"; raw samples under perf/t097/
  (gitignored). Facts future work must keep:
  - Pairing knobs: `OHYA_BENCH_OBSERVE=off|metrics|on` on the benchmark
    suites (`metrics` = recorder without the usage writer — the split that
    attributes plan 001 vs plan 002 cost), `OHYA_FRESH_OBSERVE=on|off` on
    the freshness stack (child-serve now parses `--observe`; the remote
    usage-record assertion is skipped for off runs), and
    `perf/observeoverhead` for whole-daemon pairs (imports a corpus with
    two stop fixtures, alternates sides, checks identical event counts,
    status p95, idle writes, and telemetry size).
  - Ingest verdict: recorder cost is unmeasurable against the write path —
    allocs +0.03%, median import wall +0.0% over six interleaved daemon
    pairs, steady RSS +0.2 MiB against the 16 MiB budget, idle disk writes
    exactly the snapshot cadence (36 KiB per 75 s), `status --json` CLI
    end-to-end p95 8.6 ms against the 100 ms budget.
  - Query verdict: plan 001 metrics cost ~+10 µs/+1.9 KiB per query
    (+0.3–0.5% on realistic shapes; +20% allocs only against the
    236-alloc FTS microbench rows); the plan 002 usage record adds ~14
    allocs/+2 KiB. Decision recorded in performance.md: keep both (ADR
    0010 scoped per-call usage recording); reopen trigger is a real
    workload showing query overhead. The engine's traced authorizer runs
    whenever Observer OR Usage is set, so observer-only engines still pay
    the usage-shaped trace.
  - Measurement traps proven again: whole-file bench pairing cannot
    resolve fsync-bound rows (the 4 MiB claude row swung +587% from a disk
    window; an interleaved re-probe gave median +4% with ±30% same-side
    swings) — interleave sides or use the daemon harness; freshness
    contention pairs are owned by backlog-drain luck (seed 4203: 14.9 MiB
    outstanding vs 6.9 MiB in the twin moved p50 to 6.5 s while the same
    run's loaded queries were FASTER), so attribute drain state before
    blaming telemetry.
  - Stop-fixture log shape: `source_stopped` fires once per detection —
    twice per stopped source in these runs (first pass plus the
    confirming pass), then silent through the skip-reread rule; stderr
    stays once per stop offset per run. events.jsonl is therefore the
    bounded record of stops, not a per-pass spam source.
  - Hot-path allocation contract pinned by TestRecorderHotPathAllocations
    (internal/observe): a pass-shaped Add/Observe/Set set allocates 4
    times (all from label rendering on labeled calls; unlabeled calls
    allocate nothing); ceiling 6.
  - Gaps stated in the doc: relay-path and macOS pairs unmeasured (no
    second host, no darwin machine); the knobs make both a run away.

- Transport e2e under parallel test load (observed 2026-11-19, T145 repair):
  `TestHubSetupTwoStepFlow` failed 2 of 3 full `go test ./...` runs on this
  VM (never isolated, never in CI) at the hub relay probe — tailcat v0.7.0's
  `Client.ping` wraps its own 10 s deadline, and the setup probe fired the
  moment the identity file appeared, before the child serve finished
  connecting to its relay. `checkHubSetup` now retries `ErrHubUnreachable`
  for 30 s (ADR 0012 consequence); `ErrRelayUnreachable` still fails on the
  first attempt. Same lesson for any new first-write frame after hello: a
  hub that refuses mid-handshake (CodeBusy closes before reading) resets
  the device's write, and only `roundTrip`'s 100 ms error-frame recovery
  read classifies it — the capability negotiation initially missed that
  branch and lost the busy classification (found by `TestBusyHubRetries`
  under load; every new post-hello exchange must mirror the recovery read).
  The busy tests of `internal/mcpserver` probe only after the slow query
  holds its slot (`startSlowQuery`); see "Time budgets of tests on loaded
  runners (2026-11-25)".

- Operator recall shipped (2026-11-19, T141, cmd/ohya/recall.go): `ohya
  search <query> [--repo] [--agent]` and `ohya show <session-id> [--last N]`
  publish the engine's recall to humans. Facts future work must keep:
  - One query path only: both commands run SQL through the mcpserver Engine
    (local: store.OpenReadOnly + hub.Reader + NewEngine(reader, nil, ...),
    remote: hubsync.RemoteSession behind the same two-method interface);
    bounds and the read-only policy are inherited, never re-implemented.
    The engine takes inline SQL text only (no bound parameters), so user
    input enters as doubled-quote SQL string literals; a bare apostrophe in
    an FTS query is therefore an FTS5 syntax error (exit 1), while a
    double-quoted phrase with an apostrophe matches — verified against the
    bundled SQLite.
  - Target resolution follows doctor/status, not just the mcp flag: a setup
    state whose setup/connection.json selects remote memory switches the
    default to --remote (join state <setup>/join) unless --db is explicit;
    explicit --remote alone defaults the join state to ~/.ohya/join like
    mcp. Local misses get the ingest remedy; remote unpaired states get the
    join remedy, both before any dial.
  - Presentation choices: search prints the best 20 by BM25 rank as
    two-line hits (event id, session id, agent, repo-or-dash, UTC time,
    indented 200-char excerpt); show prints the session header then the
    newest N events oldest-first with duplicates hidden
    (duplicate_of IS NULL). Excerpts neutralize control bytes (terminal
    escape injection from untrusted transcript text) and empty-text events
    print kind only. search reads `events`, not `events_settled` — the
    10-minute settled guard protects agents from their own live session,
    not an operator inspecting the hub.
  - Verified on the live 16 GB hub (read-only): search "tailcat AND derp"
    194 ms, show of a 450-event session 11 ms — the acceptance ("a human
    finds sessions by keyword and inspects one without sqlite3 or an
    agent") holds on real scale. Remote wiring is pinned in-process by the
    serve e2e (runSearchCommand/runShowCommand against the joined dev
    state).

- Egress secret redaction shipped (2026-11-19, T142, internal/secrets +
  internal/mcpserver + cmd/ohya/secrets.go, ADR 0015): the sql Engine
  passes every string result cell through one fixed pattern table before
  the Result is returned, so stdio, the hubsync remote query frames, and
  search/show all ship `[redacted:<pattern-id>]` markers; `Result.redactions`
  (omitempty) carries one additive count. Facts future work must keep:
  - Pattern-table semantics: patterns apply sequentially in table order,
    tokens before `env-assignment`, so `ANTHROPIC_API_KEY=sk-ant-…` keeps
    the precise `anthropic-api-key` marker and the variable name; the env
    value charset excludes `[` so a marker is never re-matched (redaction
    is idempotent — pinned by table tests and FuzzRedact). The env name is
    UPPER_SNAKE with a secret suffix (API_KEY, ACCESS_KEY, SECRET_KEY,
    PRIVATE_KEY, TOKEN, SECRET, PASSWORD, PASSPHRASE, PASSWD; bare KEY and
    PWD are deliberately absent so `PUBLIC_KEY=` and `OLDPWD=` stay
    readable), value at least 8 chars, `=` with no spaces, and the
    unquoted value charset also stops at quotes, whitespace, `[`, `{`, `)`,
    `,`, `;` and refuses `$` as the first char (an env-var reference is not
    a secret).
  - An anchor prefilter (strings.Contains over the literals every pattern
    requires: AKIA, ASIA, ghp_, github_pat_, sk-, xox, AIza,
    "PRIVATE KEY", =) keeps secret-free results nearly free; correctness
    is pinned by the positive fixtures running through the same path.
  - Count semantics are shared: `secrets.Count` runs the same sequential
    passes, so `ohya secrets scan` counts exactly what the engine would
    redact. The scan reads `raw_json` only (the derived `text` is a subset
    of the record's bytes; duplicates carry NULL text but full raw_json),
    pages by event id (500 per page), opens read-only, and never prints
    matched text — counts, pattern ids, event ids, session ids only.
  - Known accepted false positives: a long random base64 run can collide
    with the AIza and AKIA shapes; over-redaction is chosen over egress of
    a real secret.
  - Fixed (2026-11-24, `41cc624`): the `sk-` pattern had no left boundary,
    so it redacted ordinary identifiers. `sk-[A-Za-z0-9_-]{20,}` matched
    inside `task-…`, `desk-`, `disk-`, `ask-`, and `risk-` slugs, and
    `ohya sql "SELECT 'task-add-device-revocation-regression-tests' AS x"`
    returned `ta[redacted:openai-api-key]`. On the live hub 4,287 of 4,714
    `openai-api-key` hits (91%) and 69 of 92 `AIza` hits sat inside a
    longer token. The six prefix patterns now start with a left boundary
    (`(^|[^A-Za-z0-9_-])`) that `keep` preserves, with regression tests
    and an amendment in ADR 0015. Rescan of the hub: 5,185 matches in 702
    sessions, from 8,997 in 1,925 (see "Hub measurements 2026-11-24").
    Accepted risk: a key glued to a letter, digit, hyphen, or underscore is
    no longer redacted.
  - Triage caution (2026-11-24): a scratch script's "name before the
    value" capture printed four high-entropy neighbours of `AIza` hits.
    Triage code must print counts, lengths, and variable names only, and
    must capture a name only after a real separator (`:`, `=`, a quote).
  - Truncation interplay: redaction runs after row collection, so a
    `substr()`-cut excerpt can cut a secret shape in half and pass it
    through — best-effort by design and stated in the tool description.
  - If the pattern table ever changes, keep the near-miss negatives in
    internal/secrets/secrets_test.go in step so false positives cannot
    drift in; no schema or parser version is involved (the table is
    egress-only code).
  - Measured (2026-11-19, real write path): a 54.8 MiB fixgen claude-code
    corpus (16,948 events, 139 MB database) scans in 3.4 s wall ≈ 3.4 s CPU
    — about 16 MB/s of raw_json, so the deployed 16 GB hub (~10 GB raw)
    extrapolates to roughly 10 minutes of mostly-CPU scan. The same run
    found exactly the 3 seeded secrets and zero false positives across the
    16,947 clean real-shaped events; `ohya search` over the seeded record
    prints `GITHUB_TOKEN=[redacted:github-token]` with every name kept.
  - Operational lesson from the same day: an unbounded `ohya ingest` smoke
    (claude root set, default codex/pi roots left active) ingested the
    machine's real ~3 GB codex corpus into a throwaway database and filled
    the nearly-full VM volume (195 GB at ~97% before the run). Always pass
    `--codex-root "" --pi-root ""` (or every explicit root) for fixture
    ingests, and check free space first; the live hub survived the
    disk-full window unharmed (its service kept importing, 0 failed).

- TestTimeoutRetries was load-flaky under the full suite (2026-11-19,
  T142 verification): its `RequestTimeout: 300ms` sat within reach of real
  round-trip latencies while `go test ./...` runs every package in
  parallel on this VM (98%-full volume; cmd/ohya alone took 589s in the
  failing run). Two failure shapes, same root: (a) a pre-batch round trip
  on the first connection (hello, capability negotiation, registration,
  probe) missed 300ms, so the one-shot `stallAck` fault stayed armed and
  stalled the *recovery* acknowledgment on the second connection;
  (b) a recovery round trip itself missed 300ms. The recorded failure log
  (unreachable -> reconnected -> unreachable on a second local port) is
  shape (a). Fix: `RequestTimeout: 2s` — the injected stall blocks
  indefinitely, so the timeout under test fires at any deadline — plus a
  `batches == 1` pin after the first failed sync so the fault provably
  armed the data-batch acknowledgment and not an earlier round trip.
  Guideline: an injected-fault deadline must be far above machine
  latencies, because the fault only needs the deadline to exist.

- TestHubSetupTwoStepFlow (2026-11-19 to 2026-11-24): it failed in
  full-package runs with `the hub did not answer through its relay …
  context deadline exceeded` after about 35 s, and it passed alone. On
  2026-11-24 it failed in 6 of 10 full runs with 78 GiB free, so a nearly
  full volume (the first suspect, 98% on 2026-11-19) was not the cause.
  The logs of the child processes showed the cause on 2026-11-24. The
  first `ohya setup --hub` starts the ingest child and the hub child on
  one fresh database, and both ran the migrations. The runner read
  `schema_migrations` outside its transaction, so the slower child ran a
  migration that the other had just committed and exited at its open
  (`migration 004_parser_output_version: duplicate column name:
  parser_version`). The hub child never joined its relay, and each probe
  of setup waited 10 s for a ping that nobody answered. A second kind
  needs no second migration: a new connection gets `database is locked`
  at once at its first statement while another process creates the
  database or recovers the write-ahead log. The busy timeout does not
  apply there, because SQLite skips the busy handler when a wait could
  deadlock. In a stress test on the old code, at least 9 of 36 and 5 of
  36 concurrent opens of one fresh database failed in two runs.
  - Fixed (2026-11-24, `2ff0478`): each migration runs in a
    `BEGIN IMMEDIATE` transaction that reads `schema_migrations` again,
    so the second process waits and finds the migration recorded.
    `store.Open` repeats its attempt on a busy error until one busy
    timeout has passed since the attempt that first failed. With the
    fix, 200 concurrent opens passed, the test passed 3 times under the
    load of seven other test packages, and two uncached full runs passed
    (`GOFLAGS=-count=1 make check`: `cmd/ohya` in 233 s and 296 s).
  - On an installation the loser did not stay down: systemd restarts a
    service that exits after 5 s (`Restart=on-failure`), so a first
    install showed a short outage and a journal line.
  - A cached run proves nothing about a flake: `make check` returns the
    results of the last run when no Go file changed. Use
    `GOFLAGS=-count=1` to count flakes.
  - Fixed for services and `ohya mcp` on 2026-11-25 with
    `store.WaitForLock` (see "Store transactions and the write lock"). A
    migration that one process runs for longer than two busy timeouts
    (009 takes 3 min 41 s on this hub; see "Schema 9 upgrade path") no
    longer makes them exit. A one-shot command and `ohya setup` still
    exit with `database is locked` after one busy timeout, now with a
    hint.
  - Still open: `store.OpenReadOnly` has no retry, and macOS was not
    tested. Research note:
    /notes/2026-11-24-concurrent-open-migration-race.md.

- TestIdleKeepalivePreventsReconnectProbeStorm (`9d86d67`; margins
  changed 2026-11-25): it failed in one of two full runs on `5fcd091`
  with `idle syncs triggered 4 extra source round trips (total 14,
  initial 10)` and passed 60 times alone. The four round trips are one
  reconnect: the hello and one probe for each of the three sources. The
  test gave the hub an idle timeout of 140 ms and the uploader a
  keepalive interval of 40 ms. A pass that stalled for more than about
  75 ms delayed the keepalive, the hub closed the connection, and the
  next pass connected again.
  - Reproduced with four `dd` writers that synced each write
    (`bs=1M oflag=dsync`): 6 failures in 30 runs. The same load slowed
    one run from 2.7 s to 7 s, so a stall of that size is ordinary on a
    busy disk. CPU contention alone (24 busy processes, test at nice 10)
    caused no failure in 30 runs.
  - The hub timeout is now 2 s and the idle window 2.5 s: 0 failures in
    24 runs under the same writers and in 12 under CPU contention.
    Without a keepalive in the window (interval 30 s) the test still
    fails with the same four round trips.
  - Guideline, as for TestTimeoutRetries: keep each real-time margin of
    a test far above the stalls of a loaded machine, and reproduce a
    timing failure under load before the margins change.

- TestTransportReporting (2026-11-24): it failed in one full run on
  `5fcd091` with `protocol_compatible = <nil>; want true` after 536 s,
  while an 18 GB copy of the hub and the same test run loaded the disk
  (load average about 10). It passed in the next full run (`cmd/ohya` in
  374 s) and alone. The hub had answered, and the protocol query of the
  doctor then failed with an error that is not a version error, so the
  doctor left `ProtocolCompatible` nil and reported `hub_protocol` as a
  warning with the cause. That is the intended output for a hub that
  does not answer within 45 s. No change: the load was mine.

- Compressed backfill measured (2026-11-20, T146, docs/performance.md
  "Compressed backfill"): ADR 0014's deferred tuning questions closed with
  data. Numbers and facts worth keeping:
  - Real-record wire ratio at level 3 is about 4.9x (systematic 1.08 GB
    session sample of the live hub, read-only), while the fixgen mix
    fixture drains at 5.96x — synthetic text over-compresses, so plan with
    the real number. The 4.46 GB relay first join extrapolates from the
    measured ~3 h plain to ~45–60 min compressed; the shaped-link model is
    validated by its plain extrapolation (3.7 h) landing just above the
    measured baseline.
  - On the LOCAL tailnet path compression also wins 2.7x (27.6 s vs 75.8 s
    for a 137 MiB drain, real child serve+join; the 1,049 MiB 8x corpus
    reproduces it at 4m02s vs 11m01s): the plain drain is bounded by the
    transport (~1.6-1.9 MB/s through the tailcat stack on this host), the
    compressed one by hub commit (~4.4-5 MB/s, the known fsync band).
    Device AND hub CPU are both LOWER compressed (10.3-10.5/66-67 vs
    14.6-19.4/75-77 s/GiB) — zstd costs less than encrypting, parsing, and
    committing six times the bytes.
  - Level stays 3: better(7) buys 6.8% wire, best(11) 11.6% — and the 11.6%
    is an upper bound, because per-SESSION sections see the cross-file
    matches a per-FILE wire batch cannot — for 1.7–7x encode cost.
    Dictionary rejected: 2.3–5.3% additional reduction on real records vs
    the >25% bar (fixture agrees at 1.3–3.3%). Small files (<128 KiB) gain
    26–55% from a dictionary — the steady-state shape — but hold a sliver
    of backfill bytes; that is the reopen condition, recorded with the
    decision.
  - Harnesses: `perf/uploadbench` (-mode relay: shaped-link A/B drain
    through the real in-process endpoint, compressed then plain, fresh hub
    per side; -mode dict: speed-class ratios plus `zstd --train` dictionary
    on a fixture or on a hub DB sample — hubdata gained
    AgentSessions/CountAgentSessions/SampleAgentSessions);
    `OHYA_BENCH_UPLOAD=1 go test ./cmd/ohya -run TestUploadBackfillBench`
    (child serve+join over the real transport; OHYA_BENCH_UPLOAD_SCALE
    grows the corpus; per-side CPU from /proc, wire/payload from the join
    child's final observe snapshot — SIGTERM stop publishes it); codec
    benches joined make bench/bench-save (BenchmarkBatchWritePlain, ...Encode,
    ...Decode in internal/hubsync; x-ratio rides a ReportMetric, so
    benchstat tracks ratios too). Drain rows stay out of CI (fsync- and
    model-bound; the bench job reports, never fails).
  - `OHYA_UPLOAD_COMPRESSION=off` in the joining process removes the zstd
    bit from its capability offer for its lifetime (uploader reads it per
    connection) — the A/B knob and the operational escape hatch,
    documented in docs/sync-protocol.md; pinned by
    TestCompressionDisabledUploadsPlainFrames. Since `2812eb5`
    (2026-11-24) a process that reports a source workspace still offers
    bit 2, and a process that reports none sends no offer.
  - klauspost/compress exposes four speed classes, not the zstd level
    range (EncoderLevelFromZstd: 1–2 fastest, 3–5 default, 6–9 better,
    10+ best) — a "level 5" bench is identical to level 3, so label by
    speed class. And batchDecoder's WithDecodeAllCapLimit caps decode at
    the destination slice CAPACITY: reusing a 1 MiB buffer fails on 1.2 MiB
    sections with "decompressed size exceeds configured limit" — decode
    into a per-stream buffer sized like the production rawLen allocation.

- Release v0.3.0 published (2026-11-20, commit `3fe9f4e`, candidate run
  44814572741): ships schema 8 (`008_purged_sources`), parser versions
  `claude=2 codex=4 pi=3`, ADR 0013/0014/0016, `ohya search`/`show`,
  secret redaction, and reliability/security hardening. Local `make check`,
  `make tla`, all 7 hosted candidate jobs, and local `install.sh` hub
  upgrade passed (`ohya.service` + `ohya-serve.service` active; legacy
  `ohya-hub.service` retired).

- Perf optimization ideas observed on the 16.6 GiB / 5,884-source live hub
  (2026-11-21, `ohya metrics --window 1h --role ingest`:
  `ingest_pass_duration` p50 78.3 ms / p95 216.8 ms, `discovery_duration`
  p50 3.5 ms / p95 22.8 ms, `sqlite_transaction_duration` p50 22.1 ms /
  p95 1740.6 ms):
  - Skip unchanged cold historical subtrees via directory mtimes or
    `fsnotify`/kqueue hints with periodic fallback sweeps to cut idle
    `discoverSessionFiles` + `Poll` CPU near zero.
  - Storage & FTS write tail: execute `plans/004-storage-efficiency.md`
    D1/D2 (cold `raw_json` compression) and O6 (selective FTS5 indexing)
    once the plan 002 usage gate is met.
  - Resolved 2026-11-23: per-pass `dir -> resolvedDir` symlink caching landed
    in `watch.Poll` and `cmd/ohya` exclude/backlog paths, and parallelizing
    the 14 slow transport/setup/doctor tests in `cmd/ohya` plus the 9 DERP
    tests in `internal/transport/address` cut `cmd/ohya` 216.8 s -> 43–59 s
    and `internal/transport/address` 57.1 s -> ~24 s on macOS.

- macOS menu bar app (`macos/OhyaMenuBar`) verified on macOS 27.2 with
  Xcode 27.2 (2026-11-23): `xcrun swift test` passes (5 tests, no build
  warnings), and `scripts/bundle.sh` builds `Ohya.app`. A manual run with
  embedded v0.6.1 against a throwaway `HOME` showed the Status, Recall
  (list and search), and Devices tabs with real data. Open items:
  - The app needs ohya v0.6.x. A v0.1.x binary does not have `status`,
    `sessions`, `usage`, `logs`, `purge`, `service`, or `search`.
  - The agent label "antigravity" shows in lowercase, and Recall has no
    Antigravity filter chip.
  - Not exercised: Pause/Resume (launchd labels are global, so a throwaway
    `HOME` does not isolate them), the logs sheet, diagnostics export,
    purge, invite, and revoke.
  - The popover layout has no automated test.
  - First-run setup (plan 005 M4b) verified 2026-11-23 against an empty
    `HOME`: the "Not set up" view, Use this Mac (an incomplete report,
    because the real service holds the global launchd label), and Join a
    hub with a real code all worked. A throwaway `HOME` does not isolate a
    join: it pairs a real new device with the hub, which then needs
    `ohya devices revoke`. Test Join a hub only against a test hub.
  - Packaging verified 2026-11-23 (`package-app.sh`, ad-hoc): universal
    app and `ohya`, `minos 15.0` on every slice, DMG mount, and first-run
    install from the DMG copied the universal `ohya` into `~/.local/bin`.
    The x86_64 "deprecated for your deployment target (macOS 27.2)"
    warning comes from the host SDK; the binaries still carry minos 15.0.
  - `NSHomeDirectory()` ignores `HOME`, so a throwaway `HOME` does not
    hide `/Users/<user>/.local/bin/ohya` from binary resolution. To test
    the bundled path, point the app's binary override
    (`defaults write com.nibzard.ohya.menubar ohya.binaryOverridePath …`)
    at the bundled copy, then delete the key.

- Upgrade of a pre-Tailcat joined Mac to v0.6.1 (2026-11-23, observed on
  this MacBook). Shipped in v0.7.0: launchd bootout race (d8c875e), shared
  skill directories through symlinks (0a98606), the dead-end hint for a
  paired device without `hub-address.json` (302069f), one counted line
  for sources already missing at startup (aa2c5ed), and backlog progress
  lines without news. Open frictions:
  - An unused invitation stays redeemable after a rejoin with an existing
    credential ("the invitation stays unused"), and no command revokes an
    invitation. A code pasted into a shell or chat stays live until its
    TTL. Consuming or revoking it changes the authorization state machine,
    so it needs a TLA+ update first.
  - `ohya join CODE` puts a pasted code into shell history and `ps`.
    `ohya join --join-code-env NAME`, the installer `--join` prompt, and the
    menu bar app's Join a hub field keep it out (verified 2026-11-23:
    `--join-code-env` exists on join since v0.6.1). README still lists
    `ohya join CODE` as the join command.
  - A joined device needs two commands after an upgrade (`ohya join CODE`
    in the foreground, then `ohya setup`), and running both leaves two
    join processes on one state directory until setup replaces the
    service. The installer `--join` path does both in one step; error
    messages should point to it (now done for the missing-address case).
  - `join` and `ingest` take no lock on their state directory (verified:
    only setup and the installer use `flock`), so two uploaders ran on one
    set of checkpoints for minutes. Hub deduplication probably prevents
    duplicate events, but an older acknowledgment written after a newer
    one is unverified; a single-writer lock per state directory is the
    candidate fix and belongs in the sync TLA+ model's assumptions.

- Inode reuse after deletion on Linux (observed 2026-11-23 in CI, ext4):
  a new session file can receive the inode of a just-deleted, already
  checkpointed source. The watcher then reports the new file as that
  source "replaced (content before the acknowledged cursor changed)" and
  says bytes "were never uploaded and are lost", which is false: the old
  file was deleted and the new file is new. Tests that delete and then
  create files now rename the old files out of the watched pattern
  instead. Unverified in production; a fix would compare more than file
  identity (for example the head bytes) before treating a path as a
  replaced source.

- Recall bench (2026-11-22, `cmd/recall-bench`): comparative measurement of
  raw transcript files versus the ohya sql MCP tool as recall access paths,
  with real headless clients; design and protocol in
  /notes/2026-11-22-recall-bench-design.md. Facts worth keeping:
  - Curating a marker against the production hub echoes it into the
    curator's own session (sql queries, tool results, even writing the
    question file), so every curated marker matches one extra session and
    one extra local file — expected counts must include the curation
    session, and it sits in `events` but not `events_settled` while live,
    hence the separate `expected_settled` field. Canaries planted by
    `recall-bench plant` do not have this problem: codewords are generated
    inside the planter and never pass through the operator's session.
  - Planted sessions flow to the hub through the running `ohya ingest`
    service in seconds; the settle gate (10 minutes idle), not ingestion,
    is the wait before `events_settled` sees them.
  - Headless planting runs must sanitize the environment: inherited
    `CLAUDE_CODE_*` variables change client behavior (pi exited 1 under a
    full passthrough and worked under a minimal env), and transient
    provider failures make one retry loop necessary.
  - Run 1 results (2026-11-22, 72 runs, glm-5.3 on both arms, full study
    in /notes/2026-11-22-recall-bench-run1.md): ohya recalled everything
    raw files could plus the cross-device fact at ~2/3 the cost (input
    -17%, output -39%, cache-read -49%, wall -17%, $5.45 vs $8.08). Two
    losses: counting (grep -c beat SQL 8.4 s vs 29.4 s) and a
    time+agent-filtered pi canary where the model's SQL spiraled to two
    180 s timeouts out of three — recipe candidate: sessions by agent
    and day. The crossdevice raw-arm "successes" cited the curator's own
    transcript, proving the curation-echo caveat is real; next run
    should build that question script-only with digit-masked curation.
  - Run 2 results (2026-11-23, `recall-runs/20261123T0001Z/`, 72 runs,
    glm-5.3 on both arms via `claude-zai` `ANTHROPIC_*` env, ohya at
    `aafefda` with FTS5 `snippet()`, bounded `first_prompt`, agent+window
    and FTS count recipes, and MCP guide resources trimming upfront prompt
    15,431 -> 7,374 bytes): `raw` arm stayed flat ($8.08 -> $8.03,
    $0.231 -> $0.229/correct, 35/36) while `ohya` improved to **36/36
    (0 timeouts)**, total wall 1300 s -> 992 s (-24%; -45% vs raw),
    tool-result bytes 519 KB -> 207 KB (-60%; -74% vs raw), input tokens
    591k -> 520k (-12%), output tokens 47.2k -> 38.1k (-19%), cache-read
    tokens 2.64M -> 1.96M (-26%), API cost $5.45 -> $4.53 (-17%; -44% vs
    raw), and cost per correct answer $0.160 -> $0.126 (-21%). Both Run 1
    losses flipped: `timeboxed` 4/6 -> 6/6 (wall med 83.9 s -> 43.4 s,
    `c4-timeboxed-pi` result bytes 55.5 KB -> 7.1 KB), `aggregate` wall
    med 29.4 s -> 14.3 s (-51%, beating raw 41.6 s), `paraphrase` wall
    med 48.5 s -> 27.8 s (-43%), `negative` input tok med 21.5k -> 8.4k
    (-61%).
  - Atuin `v18.25.1` (`2026-11-20`) head-to-head exercise (2026-11-23,
    `recall-runs/20261123T0001Z/atuin-compare.md`, same 12 questions x 3
    repeats = 36 runs on `glm-5.3`, Atuin fully isolated in `/tmp/atuin-bench/`
    with `auto_sync = false`):
    - Import & footprint: local `claude-code` (3,041 threads, 2m11s), `codex`
      (2,517 threads, 7m35s), `pi` (89 threads, 0.8s) = 5,647 threads in
      9m46s, 3.7 GB on disk (`2.2 GB records.db` + `1.5 GB threads.db`,
      zstd-compressed `content_z`). Requires running `atuin daemon` even for
      read-only MCP queries. No Antigravity (`agy`) harness, no `threads`
      delete/purge subcommand. Upfront MCP schema: 16,879 B (~4,220 tok, 6
      tools) vs ohya's 7,374 B (~1,844 tok). Important correction vs
      2026-11-19 note: `v18.25.1` now stores zstd-compressed `ToolResult`
      payloads in `messages.content_z` and FTS-indexes them (`messages_fts`
      has 1.51M rows), even though `atuin_threads_read` collapses tool runs
      unless `roles` includes `tool`.
    - Shared 11 local questions (33 runs/arm, apples-to-apples): `raw`
      33/33 (1,263 s, 279 calls, 524 KB, $6.05), `atuin` 33/33 (779 s, 127
      calls, 197 KB, 22.9k out tok, $3.79, $0.115/correct), `ohya` 33/33
      (922 s, 117 calls, 182 KB, 35.6k out tok, $4.15, $0.126/correct).
    - All 12 questions incl. remote-only `x1-crossdevice-uploader` (36
      runs/arm): `raw` 35/36 (1 timeout, 1,792 s, $8.03), `atuin` 35/36 (1
      timeout, 1,235 s, 544 KB, $5.25 + unbilled timeout), `ohya` **36/36**
      (0 timeouts, **992 s**, **128 calls**, **207 KB**, **$4.53**,
      **$0.126/correct**). Note: `raw` and `atuin` only passed 2/3 on `x1`
      via the local benchmark-authoring session echo; `ohya` read the actual
      remote MacBook session in 23.4 s / 4 calls.
    - Actionable finding for ohya: on local FTS lookups, ohya used fewer
      tool calls (117 vs 127), fewer result bytes (182 KB vs 197 KB), and
      fewer cache-read tokens (1.79M vs 1.89M), yet emitted +55% more output
      tokens (35.6k vs 22.9k) and 18% more wall time (922 s vs 779 s).
      Trace inspection showed two causes: (1) composing SQL takes more
      output/thinking tokens than a 2-arg search tool, and (2) ohya's FTS
      recipe returns raw `e.time_ms` epoch-ms instead of ISO timestamps
      (`datetime(e.time_ms/1000, 'unixepoch')`) and omits `s.agent`, causing
      extra thinking tokens and follow-up queries on date/agent questions
      (`c4-timeboxed-pi`, `r3-timeboxed-incident`). Meanwhile ohya cleanly
      beat Atuin on `c6-aggregate` (14.3 s / 0.3 KB vs 30.0 s / 2.3 KB),
      `c8-negative` (17.0 s / 2.5 KB vs 31.6 s / 13.3 KB), `c2-paraphrase`
      (11.9 s vs 20.7 s), `c5-correction` (14.9 s vs 18.8 s), and
      cross-device `x1` (23.4 s vs 165.5 s / timeout).
  - Targeted `v0.6.0` verification (`66ccefc`, 2026-11-23,
    `recall-runs/20261123T-v060-targeted/compare.md`, 5 affected questions x
    3 repeats = 15 runs on `glm-5.3`: `r1`, `r2`, `r3`, `c4`, `c7` after
    landing UTC timestamps, `s.agent`, `e.session_id`, `= never LIKE`,
    context-around-hit, and 1-hit-per-session FTS): `ohya` Run 2 -> `v0.6.0`
    held 15/15 correct while cutting total wall 595 s -> **439 s (-26%)**,
    calls 66 -> **55 (-17%)**, input tokens 247.8k -> **199.7k (-19%; 17%
    under Atuin's 240.0k)**, output tokens 25.1k -> **19.6k (-22%)**,
    tool-result bytes 152 KB -> **122 KB (-20%; under Atuin's 129 KB)**, and
    cost $2.39 -> **$1.96 (-18%)**. Largest median gains: `c4-timeboxed-pi`
    43.4 s -> 18.6 s (-57%, 7.1 KB -> 1.5 KB), `c7-multihop` 31.1 s -> 22.6 s
    (-27%, out tok 1,875 -> 966), `r1-needle-incident` 35.2 s -> 26.6 s
    (-24%), `r3-timeboxed-incident` 31.4 s -> 26.7 s (calls 5 -> 3, zero 5 s
    SQL timeouts). The -$0.43 drop closes the local-only cost gap vs Atuin
    `v18.25.1` (implying ~$3.72 vs $3.79 across the 11 shared local
    questions).
  - `v0.6.0` released on 2026-11-23 from `66ccefc` (CI run 45160837836, all 7
    jobs green; `make check` + `make tla` zero errors); local upgrade via
    `sh install.sh` succeeded end to end (`ohya doctor` 20/20 `ok`, all four
    owned skills installed, `ohya-serve.service` restarted).


- CLI output review (2026-11-23, every command run in a pseudo-TTY at
  110–120 columns against the live hub and a fixture database). Shipped in v0.6.1
  (`7332972`, `485ff43`, `c7cc98a`, `e6af32d`): terminal fitting, sessions/search
  layout, doctor remedies on passing checks, nested-style resets, setup
  rows, metrics counters, empty help sections. Still open:
  - `ohya status --state DIR` without `--db` reports the default hub's
    storage (`~/.ohya/hub.db`), not a database in DIR. Decide whether an
    explicit `--state` should imply `DIR/hub.db` (`cmd/ohya/status.go`
    `statusStorageSection`).
  - Long-running commands (`ingest`, `serve`, `join`) still print
    `ohya: …` log lines; the stop diagnostic is one 250-character line
    with an ADR path. Keep them log-shaped for journald, but consider a
    shorter remedy.
  - Help descriptions are still dense prose with internal references
    (`docs/adr/0005`, `path.Match`); a rewrite to short paragraphs is
    open.
  - `show` lists `unknown` and `system` events with empty excerpts, and
    `logs` prints raw metric labels such as
    `ingest_batches{batch_result=committed}=6`.
  - `version` prints one long `key=value` line; it is machine-shaped by
    design, and `--json` exists.
  - The install output (`install.sh` through `ohya setup`) still mixes
    `ohya: …` lines with the new aligned rows.

- Claude Code request-log records were indexed as text (found and fixed
  2026-11-25, internal/claudecode, parser output version 3): Claude Code
  2.1.328 and 2.1.331 write `api-request`, `api-request-blob`,
  `api-request-shape`, `session-owner`, and `tools-used` records into the
  main session file. 2.1.280 also writes `pr-link`, `file-history-snapshot`,
  `file-history-delta`, and `fork-context-ref` (in subagent files, 47 rows on
  the hub). The hub stored all nine types as kind `unknown`.
  An `api-request-blob` holds a copy of one API message in `message`, and the
  parser read `message` for each kind. Thus each blob got a role (and
  `tool_name` for single-tool copies) and the full
  text of its message in `events.text` and `events_fts`. Sample of 12 sessions
  on the Linux device: blobs were about 20% of the rows, 17% of the raw bytes
  (12.1 of 70 MB), and 38% of the indexed text (10.9 of 28.9 MB). Token totals
  and first prompts were correct, because a blob has no `message.id`, `model`,
  or `usage`.
  - Fix: only `user` and `assistant` records read `message`. All nine types
    are kinds now (`testdata/claude-code/README.md`, fixture
    `request-log.jsonl`). A type that is not known yet contributes no text.
  - Operator step, not done: `ohya rederive` on the live hub. It re-parses
    about 839k Claude Code rows (765k on the Linux device, 74k on the
    MacBook) and rebuilds `events_fts` across all agents in one transaction.
    v0.8.0 is installed on the hub since 2026-11-25, 22:45Z, so the step
    can run now.
  - Open: a blob with role `system` (tool additions, injected system text; 20
    of 2,477 blobs in the sample) is in no transcript message. That text is
    now in `raw_json` only. The recall bench (plan 003 R5) must show if a
    search needs it.
  - `internal/fixgen` generates no request-log records. Benchmark corpora lack
    the record types that make about 20% of the rows of a real 2.1.328 file.

- Historical sessions keep a NULL `repo` when their directory is gone at
  observation time (observed 2026-11-25 with the hub `sql` tool; open
  decision, ADR 0009): `sessions.repo` comes from the session's own records,
  or else from the write-once workspace observation of `(device_id, cwd)`
  (`internal/store/workspace.go`). Derivation reads no sibling session. The
  first workspace observations are dated 2026-11-16 on the Linux device and
  2026-11-23 on the MacBook, after most sessions. Thus 274 sessions in 20
  distinct `(device_id, cwd)` directories (211 codex, 58 pi, 5 claude-code;
  `/Users/nikola/dev/steel/steel-pi` spans both `codex` and `pi`) have a NULL
  `repo` although a sibling session in the same device and directory has one.
  A query that scopes by `repo` does not find them.
  - All five affected MacBook directories do not exist now (checked), which
    agrees with their observations. Example on the Linux device:
    `/home/agent/wtk` has 100 sessions with a NULL `repo` (July 2026) and 42
    with `github.com/nibzard/wtk`; its observation (2026-11-16) is "no
    repository". Cause on the Linux device not verified.
  - `/home/agent/oak` is a different case: the first `claude-code` session
    started 2026-11-17 14:48:46 and triggered the write-once "no repository"
    observation two seconds later (`14:48:48`, before a git remote existed).
    The next day (`2026-11-18 07:57:38`), a Codex session recorded
    `github.com/nibzard/oakspec` from `session_meta`, leaving the three later
    `claude-code` sessions (`2026-11-18`–`19`) with NULL `repo`. This shows
    the gap also happens whenever git/remote setup occurs after the first
    Claude Code or Pi session in a directory, not only for deleted checkouts.
  - Option A, keep ADR 0009: a fallback reads a repository that the session
    process did not use, and a directory can hold a different project later.
  - Option B, amend ADR 0009: a session with a NULL `repo` inherits the
    repository of the sibling session nearest in time, on the same device and
    in the same directory. This needs a derivation change, a DB.md update,
    and `ohya rederive`.

- v0.8.0 release (2026-11-25, 22:29:02Z; the tag points to `915df53` on
  `main`, after `make check` with `GOFLAGS=-count=1` and `make tla` passed
  on that commit): candidate run 45709812198 passed all eight jobs on its
  second attempt. Attempt 1 failed the macOS Intel job in `make check` (see
  "Time budgets of tests on loaded runners" below); `gh run rerun 45709812198
  --failed` reran that job and the jobs that depend on it. Before the tag:
  four checksums, four `candidate.*.txt` files (revision `915df53`,
  `dirty=false`, `schema=9`, `parser.claude=3`), and the native library
  reports were verified, and the downloaded `install.sh` differed from the
  commit only in the pinned version. Six assets, as for v0.7.1; the
  published assets were downloaded again and compared byte for byte. The
  menu bar app has no change since v0.7.1; the branch
  `menubar-session-tags` was merged after the tag (`6676d10`).
  - The released installer upgraded the hub from v0.7.1 with exit 0 in
    15 min 42 s: a 17.0 GB recovery point in 11 min 43 s, migration 009 in
    3 min 50 s (`schema: updated`, "database schema 8 is now 9"), `backup:
    saved` under `~/.ohya/setup/backups/recovery-2631208279/`, both
    services running, `hub: ready`, `memory: ready`. The database grew by
    120 MiB. `ohya doctor` exited 0 in under one second: 20 ok checks and
    the `mcp_build` warning. The MacBook synced again 45 s after the hub
    restart.
  - Antigravity backfill on the live hub after the first pass of the new
    ingest service: 74 sessions, 68 with a `cwd`, 40 with a `repo`, and 68
    of 74 Antigravity sources have a workspace.
  - Each `--hub` run issues a join code that expires after 15 minutes; the
    installer output went to a private file, which was deleted after a
    redacted read. Not done: `ohya rederive` on the hub, and the DMG was
    not opened on a Mac.

- Time budgets of tests on loaded runners (2026-11-25): two CI runs of this
  day failed in timing tests.
  - `TestLargeFileUploadsInBoundedBatches` (`internal/uploader`) failed in
    candidate run 45709812198, attempt 1, on macos-15-intel: "Sync:
    uploader: hub connection failed; unacknowledged bytes are sent again
    after reconnecting: hubsync: read frame: read tcp ...: i/o timeout"
    after 11.28 s. Cause: the fixture gives each request 10 s, and the
    test sends two batches of up to 64 MiB to a hub that inserts them into
    SQLite. The round trip took longer than 10 s while the other packages
    of `go test ./...` loaded the runner. The same test takes 2 to 5 s
    here. Reproduced with a throwaway proxy that delays the reply of the
    hub to a batch by 11 s: with the old budget the test failed at
    11.03 s with the same text, with the new budget it passed at 24.23 s.
    Fix: the test uses `DefaultRequestTimeout` (60 s, the production
    value) and a sync budget of two round trips. Open: 60 s was not
    measured against a 64 MiB batch on a slow hub.
  - `TestObserverReportsBusyRejectionAndActiveGauge` failed in CI run
    45619526142 (Linux, `-race`), and `TestQueryBusyFailsFastAndRecovers`
    failed in CI run 45717625926 (macOS). Cause: both tests sent the probe
    `SELECT 1` while the slow query was still on its way to the only slot.
    When the probe held the slot at that moment, the slow query got
    `ErrBusy`, the test dropped that error, and no later probe could be
    rejected. Reproduced on 2026-11-26 under `-race` and 12 CPU hogs: the
    slow query returned "server busy" in each failed run. Fix (2026-11-26):
    `startSlowQuery` waits until the engine counts one active query, then
    each test probes once and expects `ErrBusy`, and the slow query must end
    with `context.Canceled`. 1,800 runs passed under the same load. Research
    note: /notes/2026-11-25-timing-tests-on-loaded-runners.md.

- menubar-session-tags (2026-11-25; `b7e31f1` and `fbce579` by the owner,
  merged in `6676d10` after the v0.8.0 tag, so it is in no release): `ohya
  status --json` reports `topology.device_id`, the hub reports its device
  ID in the diagnostics frame, and the menu bar app labels each recalled
  session as This Mac, Hub, or a device. CI run 45706558153 on the branch
  passed `xcrun swift test`, `make check`, and `make build` on macOS. The
  app was not opened on a Mac during the review. Review items, none a
  blocker:
  - Fixed on 2026-11-26: on a paired state directory `credential.json`
    names the device. `ohya status` reports the credential's ID, the text
    report has a `Device` row and `device=` in the `Remote hub` row, and a
    `device.json` that names another device gives
    `device_identity_mismatch` in status and an error in `ohya doctor`.
    `ohya join` creates `device.json` only to pair, and `ohya mcp --remote`
    takes its writer ID from the credential. An unusable `device.json` is
    an error in both commands.
  - Swift, changed on 2026-11-26 and not built on this Linux VM (CI on
    macOS runs `xcrun swift test`): the tag symbol and each `StatusDot`
    are hidden from VoiceOver, `StatusDot` takes a `Health` value and owns
    its colors, and the "Hub reachable" row uses `row(_:value:dot:)`.
  - The two commit subjects have 57 and 65 characters, and their bodies
    are not wrapped at 72. The commits were not changed.
