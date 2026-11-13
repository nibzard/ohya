# ohya
**One memory for all your coding agents. Everywhere.**

*User-owned history. One binary, one authoritative database, two tools.*

**Product vision, not shipped functionality.** See README.md for MVP scope.
Shared-history queries require a reachable hub. Offline capture relies on source
files remaining available until uploaded.

## What is ohya?

**ohya is a shared, searchable memory layer for AI coding agents.**

Claude Code, OpenAI Codex, and other coding agents create valuable context every day: conversations, decisions, debugging sessions, code changes, tool executions, and unfinished work. But that knowledge gets trapped in individual sessions, tools, and devices.

*Phase 1: Claude Code and Codex. Phase 2: Pi agent and OpenCode.*

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

**2. Sync instantly.** Embedded Tailscale Tailcat networking securely connects devices through the planned transport adapter. Pairing and relay requirements must be validated before release. New events are pushed as they're written, with offline catch-up and durable synchronization.

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

**Minimal surface area.** One binary. One database. Two MCP tools. Deterministic ingestion and agent-side reasoning.

---

### The vision

**ohya turns fragmented coding sessions into one continuous, portable memory.**

Start in Claude Code on your laptop. Continue in Codex on your desktop. Return days later with another agent. Everything you worked on is already there, available through SQL, without having to explain it again.

**ohya — pick up where any agent left off.**
