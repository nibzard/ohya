# ohya

**One memory for all your coding agents. Everywhere.**

ohya is a shared, searchable memory layer for AI coding agents. It captures
agent sessions automatically, synchronizes them across devices, and exposes
the entire history to any connected agent through read-only SQL and bounded annotation MCP tools.
One Go binary, one SQLite database, no cloud account.

Status: pre-MVP, design phase. See [MANIFESTO.md](MANIFESTO.md) for
engineering principles, [PRODUCT.md](PRODUCT.md) for the product vision,
[ARCHITECTURE.md](ARCHITECTURE.md) for system design, and [DB.md](DB.md)
for the database plan.

## Scope

### MVP (phase 1)

- Ingest Claude Code, Codex, and Pi agent session logs (macOS, polling) into
  one hub-owned SQLite WAL database with FTS5/BM25 search.
- Read-only `sql(query)` and bounded `annotate(operations)` MCP tools.
  Agents can persist tags, classifications and security-review findings without
  modifying original transcript records.
- Isolated Tailcat transport adapter for encrypted device-to-hub sync,
  after a feasibility spike.

### Phase 2

- OpenCode as an ingestion source, pending a parser spike for its
  per-message JSON storage (enumerated files, not append-only JSONL).

### Explicitly deferred

Built-in classification, embeddings, vector search, filesystem notifications, extra
platforms, and any third-party AI processing. See MANIFESTO.md (YAGNI).

## License

MIT — see [LICENSE](LICENSE).
