# 0016: Agent-marked purge scope

Status: accepted, 2026-11-20 (T148).

## Context

A user will tell their coding agent "clean up the data" or "find the
garbage". The agent is the right tool for the search: it can read the hub,
judge which sessions are worthless, and explain why. But the agent is also
the least trustworthy writer the hub has. Transcript text and
model-generated queries are untrusted input
([ADR 0015](0015-egress-secret-redaction.md) egress is one consequence), and
once agents rotate their own transcript files the hub is the only durable
copy of history. An agent holding a delete capability — including one
steered by a prompt injected through a stored transcript — could destroy
records, including the evidence of its own actions.

[ADR 0013](0013-owner-scoped-session-purge.md) made deletion an owner
action (`ohya purge`, dry-run default) but left the owner without the
agent's eyes: the owner must type selectors, while the agent is the one
that can see which sessions are garbage.

## Decision

**The cleanup loop is FIND → MARK → EXECUTE, and only the owner
executes.** No new MCP tool is added; the flow closes over the two
existing tools plus one new purge selector.

1. **FIND — read-only SQL.** The `sql` tool description and DB.md
   ("Garbage audit") publish one-statement recipes: sources that stopped
   syncing, sessions whose working directory resolved to no repository,
   the largest sources and their oversized records, empty or
   single-record sessions, replay copies inside one session, and the
   model distribution. Every recipe keeps the deadline-safe shapes of the
   recall recipes: per-source and per-session drill-downs instead of
   hub-wide scans over unindexed columns (`duplicate_of`, record length).
2. **MARK — the existing bounded `annotate` tool.** Agents mark the
   deciding event of each garbage session under the reserved namespace
   `tags`, key `garbage`, with a value stating the reason and evidence
   (`{"reason": …, "evidence": <event id>}`). The value is free; the
   namespace and key are the contract that `--marked` selects. Marks are
   persistent, writer-attributed, and capped by the existing annotate
   limits (100 operations, 1 MiB per request, 64 KiB per value), so the
   owner has an audit trail of who marked what and why — an agent cannot
   mark anonymously and cannot flood the hub.
3. **EXECUTE — `ohya purge --marked <namespace.key>`.** The selector
   plans (dry run first, as always) the sessions whose events carry that
   mark from any writer, then applies the ADR 0013 eviction: whole
   sessions, whole sources, tombstones. It combines with the other
   selectors by AND. The value splits at the first dot, so a namespace
   used with `--marked` gets no dot (`tags.garbage`, never `a.b.c`).

The agent's deliverable is the two commands, printed for the owner — the
same contract as every other remedy ohya prints (`ohya sources reallow`,
`ohya rederive`).

### Why no purge MCP tool

A `purge(query)` tool, however well bound, hands the destructive
capability to the least trusted principal and inverts ADR 0013's stance
that retention is owner-decided. The marking convention gives the agent
full expressive power over scope — it decides exactly which sessions it
nominates — while the owner keeps the trigger. Marks also compose with
the dry-run plan: the owner sees names, counts, and bytes before anything
dies.

### Semantics and edges

- Marks from every writer count: two agents agreeing on a session need no
  coordination, and a disagreement is visible in the review query (each
  row names its writer).
- A mark on a synthetic session selects nothing: synthetic sessions are
  not selectable (ADR 0013) and follow their source, so the recipe
  excludes the `ohya-synthetic:` prefix and the plan refuses unselected
  real sessions sharing a source.
- Marks die with their events: the per-source purge deletes
  `event_annotations` of the evicted events, so a rerun of the same
  `--marked` selector finds nothing. The tombstone's `scope` column
  (`marked=tags.garbage`) is the durable record of what selected the
  purge.
- Unmarking is an annotate `delete` — the agent can withdraw a nomination
  before the owner acts.

### Model checking

None added. `--marked` changes which sessions a purge plan selects, not
the source lifecycle, the write path, or the tombstone contract; the
ADR 0013 models (Sync.tla, Auth.tla) already cover purge and reallow as
owner actions and stay valid for any selector.

## Consequences

The read-only `sql` boundary stays absolute: an agent can nominate
garbage but never erase history, so a prompt-injected agent cannot destroy
evidence of itself. The owner gains an agent-curated retention path whose
audit trail is the annotation table itself — no new table, no classifier,
no new tool. The cost is one selector on `ohya purge` and a documented
convention that `tags/garbage` is reserved; any agent that writes that key
participates in the audit.
