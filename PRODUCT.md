# ohya
**One memory for all your coding agents. Everywhere.**

*User-owned history. One binary, one authoritative database, two tools.*

**Product vision, not shipped functionality.** See README.md for MVP scope.
Shared-history queries require a reachable hub. Offline capture relies on source
files remaining available until uploaded.

## What is ohya?

**ohya is a shared, searchable memory layer for AI coding agents.**

Claude Code, OpenAI Codex, and other coding agents create valuable context every day: conversations, decisions, debugging sessions, code changes, tool executions, and unfinished work. But that knowledge gets trapped in individual sessions, tools, and devices.

*Phase 1: Claude Code, Codex, and Pi agent. Phase 2: OpenCode.*

ohya captures those sessions automatically, synchronizes them across devices, and makes the entire history available to any connected AI agent through read-only SQL and bounded annotation MCP tools.

No copy-pasting context. No manual memory management. No expensive AI indexing pipeline.

**Your agents already know how to reason. ohya gives them access to everything you've worked on.**

## How it works

```text
  MacBook             Desktop             Server
+------------+     +------------+     +------------+
| Claude Code|     | Claude Code|     | Codex      |
| Codex      |     | Codex      |     | Claude Code|
+-----+------+     +-----+------+     +-----+------+
      |                  |                  |
      +------------------+------------------+
                         |
               Embedded Tailcat
             Encrypted P2P Sync
                         |
                         v
                +-----------------+
                |      ohya       |
                |-----------------|
                | SQLite          |
                | Sessions/Events |
                | FTS5 + BM25     |
                | Metadata        |
                +--------+--------+
                         |
                   Two MCP Tools
                   sql / annotate
                         |
                         v
                +-----------------+
                |   ANY AI AGENT  |
                |                 |
                | Search, query,  |
                | reason, resume  |
                +-----------------+
```

**1. Capture automatically.** A lightweight Go process watches Claude Code and Codex session logs, incrementally extracting messages, tool calls, timestamps, files, and metadata. Existing history is imported automatically.

**2. Sync instantly.** Embedded Tailcat networking securely connects devices through a relay, with a direct path when one exists. The hub needs no reachable address, no certificate, and no open port. New events are pushed as they're written, with offline catch-up and durable synchronization.

**3. Store and index.** Everything lives in one SQLite database. Native FTS5 and BM25 provide fast, ranked full-text search without embeddings, vector databases, or external services.

**4. Let agents do the thinking.** Every connected agent gets `sql(query)` for reading and `annotate(operations)` for writing annotations. Agents write their own SQL, discover relevant history, join data, search transcripts, and retrieve exactly what they need.

## SQL Mode for agent memory

Inspired by Code Mode, ohya moves computation to the data instead of flooding an LLM's context window.

Rather than providing dozens of specialized MCP tools, ohya exposes one read-only SQL interface, with execution and output limits.

An agent can ask:

- "What was I working on yesterday in Claude Code?"
- "Find the debugging session where we fixed that authentication issue."
- "What decisions did we make about this database architecture?"
- "Which files did I touch across my last five sessions?"
- "What's unfinished in this repository, and where should I resume?"

The model generates SQL, retrieves the evidence, and reasons over the results. The same memory works with any model capable of using MCP.

## Rich metadata, almost free

ohya extracts structured information directly from session logs:

| Session metadata | Activity metadata |
|---|---|
| Agent, model, device | Messages and tool calls |
| Repository and branch | Files referenced or edited |
| Start and last activity | Commands and test results |
| Token usage, when available | Errors and debugging activity |
| Session and subagent IDs | Timestamps and Git operations |

No LLM is required for ingestion or search.

Agents can classify every record, attach tags and persist security-review findings.
Annotations stay separate from original transcripts, with writer identity and
timestamps. Built-in classification is deferred; intelligence stays in the agent.

The original transcripts remain the source of truth.

## Install once. Connect everywhere.

The intended experience is three commands:

```bash
ohya init                 # Create your private hub
ohya join <pairing-code>  # Connect another device
ohya mcp install          # Connect coding agents
```

*Illustrative product CLI.*

One compiled Go binary handles ingestion, synchronization, SQLite storage, and MCP. A local stdio MCP bridge connects coding agents to the remote hub through Tailcat.

No Docker. No PostgreSQL. No cloud account. No vector infrastructure. No background LLM required.

## Product principles

**Local-first by design.** Your session history stays in your own SQLite database, with encrypted device communication and explicitly authorized access.

**Fast by default.** Incremental ingestion, near-real-time synchronization, indexed search, and minimal overhead.

**Intelligence belongs to the agent.** ohya stores facts and makes them queryable; connected models interpret, organize, and apply them.

**Pull, never push.** ohya installs no hooks and injects nothing into an agent session. The user decides when and how an agent uses the memory.

**Minimal surface area.** One binary. One database. Two MCP tools. Deterministic ingestion and agent-side reasoning.

## Competitive position (2026-11-19)

The nearest shipped neighbor is Atuin "threads"
([v18.24.0](https://github.com/atuinsh/atuin/releases/tag/v18.24.0),
2026-10-15): file-tailing capture of the same Claude Code, Codex, and Pi
session logs, SQLite FTS5/BM25 search, and MCP tools for agent
self-recall. That convergence validates the demand and the architecture.
This position is dated and issue-linked; re-verify against the competitor
before quoting it later.

ohya commits to four differences:

1. **Operator-owned shared hub, no third-party server.** Devices pair with
   one-time invitations and sync to a hub the operator runs; nothing
   outside that hub ever holds the transcripts. Atuin threads are
   single-user, and its live teams beta is hosted-hub-only, uploading
   shared threads in plaintext to the hosted hub.
2. **Lossless raw records.** Every original JSONL record is stored byte
   for byte. Memory survives agent-side transcript rotation, and parser
   upgrades re-derive normalized rows from the stored records instead of
   asking devices to resend. Capture pipelines that clip and redact at
   capture cannot answer what was lost.
3. **Bounded arbitrary SQL instead of fixed keyword tools.** Agents already
   speak SQL; ohya spends its surface budget on one bounded read-only
   query tool rather than a growing list of retrieval endpoints. Fixed
   keyword tools hit a ceiling their own users report —
   [atuinsh/atuin#4589](https://github.com/atuinsh/atuin/issues/4589) asks
   for boolean, date, regular-expression, and model filters that one
   `WHERE` clause already expresses.
4. **Checked sync and authorization.** Acknowledgment, ordering, and
   revocation invariants are model-checked with TLA+/TLC before they ship.
   Silent loss and a surviving revoked device are the failures a memory
   product cannot walk back.

---

### The vision

**ohya turns fragmented coding sessions into one continuous, portable memory.**

Start in Claude Code on your laptop. Continue in Codex on your desktop. Return days later with another agent. Everything you worked on is already there, available through SQL, without having to explain it again.

**ohya — pick up where any agent left off.**
