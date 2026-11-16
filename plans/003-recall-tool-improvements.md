# Recall tool improvements

Status: proposed. Priority P2. Planned against `d237c54`, 2026-11-16.
Depends on plan 002 for measurement. No implementation is included.

## Outcome

Agents on both Claude Code and Codex use ohya when a task depends on
earlier work, get answers within the query deadline, and receive results
that fit their context. Each change is measured with `ohya usage` before
and after.

## Evidence

From `NOTES.md` (2026-11-16):

- 8 `sql()` calls in 4 sessions since 2026-11-13. The tools were available
  in 26 sessions. No Codex call was found.
- 1 of 8 calls hit the 5 s deadline. It joined `events` to `sessions` on
  `cwd`. Another took 4 s.
- Results reached 48 KB (session lists with full `first_prompt`).
- No query read `raw_json`. Agents used `text` previews and `sessions`.
- Agents get only the MCP server instruction (`MemoryInstructions` in
  `internal/mcpserver/server.go`). Global agent instructions do not
  mention ohya.

Too few calls exist to rank causes. The work below starts with the cheap
correctness check, then waits for the plan 002 baseline.

## Work items

### R1. Verify the Codex call path (do first)

Zero Codex calls can mean that Codex does not see or cannot call the tool,
or that ohya's usage scan misses the Codex record format.

1. Run a Codex session with the installed integration. Confirm that the
   `ohya` MCP server starts and that `sql` is callable.
2. Capture the Codex transcript records for one MCP call. Add them as a
   parser fixture.
3. If the call path is broken, fix the integration in `internal/setup`.
   If the format was missed, fix the scan in `ohya usage` (plan 002).

### R2. Remove the observed deadline friction

1. Reproduce the timed-out query pattern (`events` joined to `sessions`
   filtered on `cwd`) on a copy of the hub or a synthetic fixture of the
   same size.
2. Read `EXPLAIN QUERY PLAN`. Decide between an index, a narrower
   recall view, or documented query recipes. Prefer recipes and views;
   an index costs storage on every hub, so it needs measured benefit.
3. Measure p95 latency before and after with plan 002 usage records.

### R3. Keep results within the agent's context

1. Use plan 002 data for `result_bytes` and truncation.
2. If large results are common, change the tool description and recipes
   to select previews (`substr`) and bounded columns. Change default
   limits only with evidence. Keep the response schema stable.

### R4. Improve guidance on when to recall

1. Write concise recall recipes into the MCP instructions and tool
   descriptions: find the project's sessions, search decisions with FTS,
   read a session in order, cite event IDs.
2. Measure the call rate per session with tools available, before and
   after, for each agent.
3. Do not add a new MCP tool or automatic recall without evidence.

### R5. Recall evaluation set

1. Build a small, deterministic evaluation set from real recall questions
   and their known answers (event IDs) on synthetic or consented data.
2. Use it to check that R2–R4 and any later storage or index change
   (plan 004, O6) do not lower recall quality.

## Order

1. R1 immediately (cheap, independent).
2. Plan 002 baseline: at least 2 weeks of usage records.
3. R2 and R3, guided by the baseline.
4. R4, measured against the baseline.
5. R5 before plan 004 changes anything that affects search.

## Non-goals

- No embeddings and no LLM-based recall (see `brainstorm.md`, O7).
- No change to the read-only and bounded nature of `sql()`.
