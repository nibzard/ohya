# ohya

Shared, searchable coding-agent history in a user-owned SQLite database.

**Status: planning; no executable or installation workflow yet.** Start with a
local vertical slice before adding networking. The initial target is macOS and
Claude Code; Codex follows once the ingestion contract works against fixtures.

## MVP scope

One Go binary imports local JSONL transcripts, preserves original records,
normalizes searchable text deterministically, and exposes one bounded read-only
MCP tool: `sql(query)`. Use polling initially. No classifier, embeddings, plugin
system, local replica database, or durable outgoing spool in the MVP.

The eventual hub owns the authoritative transcript database. Devices also keep
small durable identity and checkpoint files. Queries require a reachable hub;
offline uploads can replay only while their original transcript files survive.
This is user-owned storage, not a guarantee of offline access to shared history.

## Implementation order

1. Local Claude Code fixtures → parser → SQLite/FTS → bounded SQL MCP.
2. Durable checkpoints and failure tests: restart, retries, partial lines,
   malformed records, replacement and truncation.
3. Codex fixtures and parser using the same ingestion contract.
4. Small executable TLA+/TLC sync model, transport feasibility spike, and
   two-device synchronization. Validate Tailcat identity persistence, pairing,
   reconnects and relay requirements before committing to deployment promises.
5. Authorization model, pairing/revocation, service installation and recovery docs.
   Remote access must remain disabled until authorization is implemented.

Each increment needs observable acceptance tests before the next expands scope.
The local slice is done when a fixture is searchable over MCP, replay adds no
rows, and a restart resumes without losing complete records. Sync additionally
requires tests for lost acknowledgments, interrupted transactions and reconnection.
Authorization requires forged-device and revoked-credential tests.

## Plans

- [Product scope](PRODUCT.md)
- [Architecture and protocol contracts](ARCHITECTURE.md)
- [Database semantics](DB.md) and [executable schema](ohya_schema.sql)
- [Engineering principles](MANIFESTO.md)

No Go module exists yet, so Go build, test and vet commands are not applicable.
