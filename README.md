# ohya

**One memory for all your coding agents. Everywhere.**

ohya is a shared, searchable memory layer for AI coding agents. It captures
agent sessions automatically, synchronizes them across devices, and exposes
the entire history to any connected agent through one read-only SQL MCP tool.
One Go binary, one SQLite database, no cloud account.

Status: pre-MVP, design phase. See [MANIFESTO.md](MANIFESTO.md) for
engineering principles, [PRODUCT.md](PRODUCT.md) for the product vision,
[ARCHITECTURE.md](ARCHITECTURE.md) for system design, and [DB.md](DB.md)
for the database plan.

## Scope

### MVP (phase 1)

- Ingest Claude Code and Codex session logs (macOS, polling) into one
  hub-owned SQLite WAL database with FTS5/BM25 search.
- One read-only `sql(query)` MCP tool for connected agents.
- Isolated Tailcat transport adapter for encrypted device-to-hub sync,
  after a feasibility spike.

### Phase 2

- Pi agent and OpenCode as ingestion sources, pending parser spikes for
  their session-log formats.

### Explicitly deferred

Classification, embeddings, vector search, filesystem notifications, extra
platforms, and any third-party AI processing. See MANIFESTO.md (YAGNI).

## License

MIT — see [LICENSE](LICENSE).
