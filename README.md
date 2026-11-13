# ohya

**One memory for all your coding agents. Everywhere.**

ohya is a shared, searchable memory layer for AI coding agents. It captures
agent sessions automatically, synchronizes them across devices, and exposes
the entire history to any connected agent through read-only SQL and bounded annotation MCP tools.
One Go binary, one SQLite database, no cloud account.

Status: pre-MVP. Local Claude Code ingestion runs end to end: `ohya ingest`
imports existing session history once, then polls incrementally into one
SQLite WAL database through the transactional write path. `ohya mcp` serves
the read-only `sql` tool and the bounded atomic `annotate` tool over stdio.
Device sync is not implemented yet. See [MANIFESTO.md](MANIFESTO.md) for
engineering principles, [PRODUCT.md](PRODUCT.md) for the product vision,
[ARCHITECTURE.md](ARCHITECTURE.md) for system design, and [DB.md](DB.md)
for the database plan.

## Use

```sh
make build
./bin/ohya ingest              # import history, then poll until stopped
./bin/ohya ingest --once       # one pass, then exit
./bin/ohya mcp                 # serve the read-only sql MCP tool on stdio
```

`ohya ingest` watches `~/.claude/projects` (override with `--claude-root`)
and stores everything in `~/.ohya/hub.db` (override with `--db`; the state
directory defaults to the database directory and can be set with `--state`).
The state directory holds `device.json`, the stable device identity, and
`sources/claude-code.json`, the acknowledged byte checkpoint of every
watched file.

The first pass imports existing session files; later polls read only the
bytes past each checkpoint. All writes commit through one transactional
write path, so a crash between commit and checkpoint replays safely on
restart and inserts nothing twice. A malformed record stops its source at
that offset with a diagnostic and never skips bytes; other sources continue.
A replaced or truncated file starts a new source and re-imports history —
the run reports it instead of staying silent. Hardlinked copies inside the
watched tree share one file identity and are watched once; the checkpoint
follows the surviving link when others disappear.

`ohya mcp` serves the read-only `sql` tool to any MCP client over stdio
(same `--db` and `--state` flags). The tool runs exactly one statement per
call — `SELECT` and read-only `PRAGMA` only — on a restricted read-only
connection guarded by a SQLite authorizer. Writes, schema changes,
`ATTACH`, extension loading, and state-changing pragmas are denied; queries
run under a 5 second deadline, return at most 500 rows and 256 KiB of row
JSON, and report truncation explicitly instead of silently. The tool
description publishes the schema, SQL examples, the limits, and this
device's writer ID. To connect an agent, register the command, for example
for Claude Code:

```json
{
  "mcpServers": {
    "ohya": {
      "command": "/absolute/path/to/bin/ohya",
      "args": ["mcp"]
    }
  }
}
```

The `annotate` tool lets an agent persist its own interpretations — tags,
classifications, security-review findings — as JSON values on existing
events, without modifying any original transcript record. Each operation
upserts or deletes one annotation, identified by `(event_id, namespace, key,
writer_id)`. The hub assigns the writer from the device identity; a caller
cannot choose it. A batch of at most 100 operations commits atomically or
not at all, values are valid JSON of at most 64 KiB stored compacted, and
the result reports `applied` and `unchanged` counts. Identical retries
change nothing, not even timestamps. Agents query annotations through the
`sql` tool like any other table.

## Scope

### MVP (phase 1)

- Ingest Claude Code, Codex, and Pi agent session logs (macOS, polling) into
  one hub-owned SQLite WAL database with FTS5/BM25 search.
- Read-only `sql(query)` and bounded `annotate(operations)` MCP tools.
  Agents can persist tags, classifications and security-review findings without
  modifying original transcript records.
- Isolated Tailcat transport adapter for encrypted device-to-hub sync. The
  feasibility spike is complete ([ADR 0002](docs/adr/0002-tailcat-transport.md));
  sync itself is not implemented yet.

### Phase 2

- OpenCode as an ingestion source, pending a parser spike for its
  per-message JSON storage (enumerated files, not append-only JSONL).

### Explicitly deferred

Built-in classification, embeddings, vector search, filesystem notifications, extra
platforms, and any third-party AI processing. See MANIFESTO.md (YAGNI).

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for build requirements and checks,
[BACKLOG.md](BACKLOG.md) for implementation order, and the
[dependency decisions](docs/adr/0001-dependencies.md) for tradeoffs. The
Tailcat transport spike lives in `cmd/tailcat-spike`; its measurements and
the selection decision are in
[ADR 0002](docs/adr/0002-tailcat-transport.md). The sync cursor and
acknowledgment protocol and the authorization, pairing, and revocation
rules are model-checked with TLA+/TLC in `tla/`
(`make tla`; see [tla/README.md](tla/README.md)).

## License

MIT — see [LICENSE](LICENSE).
