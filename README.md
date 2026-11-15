<div align="center">

<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/logo-dark.svg">
    <img alt="ohya" src="docs/assets/brand/logo-light.svg" width="320">
  </picture>
</h1>

**One memory for all your coding agents. Everywhere.**

Claude Code, Codex, and Pi sessions from every machine in one searchable
SQLite database, served back to any agent over MCP.
One Go binary. No cloud account. No embeddings.

[![CI](https://github.com/nibzard/ohya/actions/workflows/ci.yml/badge.svg)](https://github.com/nibzard/ohya/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.27.3+](https://img.shields.io/badge/go-1.27.3%2B-00ADD8.svg)](go.mod)
![Status: pre-release](https://img.shields.io/badge/status-pre--release-orange.svg)

[Quick start](#quick-start) ·
[How it works](#how-it-works) ·
[Reference](docs/reference.md) ·
[Architecture](ARCHITECTURE.md) ·
[Contributing](CONTRIBUTING.md)

</div>

> [!IMPORTANT]
> ohya is pre-release. No release assets are published yet, so build from
> source. Ingestion, MCP tools, and device sync run end to end. macOS
> runtime verification and distribution are still open.

## Why ohya

Your agents produce decisions, debugging sessions, and unfinished work every
day. That context stays in one tool on one machine, until it is lost.

- **Shared:** Codex can read what Claude Code did on your other laptop
  yesterday.
- **Local-first:** your transcripts stay on hardware that you own. Devices
  sync over an encrypted tailnet to a hub that you run.
- **Plain SQL:** agents query with `SELECT` and FTS5/BM25. There is no
  ingestion model, no vector store, and no AI pipeline.
- **Lossless:** every original JSONL record is kept byte for byte. Parser
  upgrades re-derive rows from the stored records.
- **Durable:** Claude Code deletes transcripts after 30 days by default
  (`cleanupPeriodDays`). ohya keeps every record that it has ingested.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/architecture-dark.svg">
  <img alt="Devices tail agent transcripts and upload them over an encrypted tailnet to the ohya hub, which stores them in SQLite with FTS5 and serves the sql and annotate MCP tools to any agent." src="docs/assets/architecture-light.svg">
</picture>

1. **Capture:** `ohya ingest` (on the hub) or `ohya join` (on a device)
   tails `~/.claude`, `~/.codex`, and `~/.pi` session files.
2. **Sync:** batches go to the hub over an embedded Tailcat tailnet. The hub
   deduplicates records and acknowledges a cursor only after a durable
   commit. Offline devices replay when they reconnect.
3. **Store:** one SQLite WAL database keeps the raw records and the
   normalized sessions and events, with an FTS5 index.
4. **Recall:** any MCP client calls `sql` for bounded read-only queries and
   `annotate` to attach its own tags and findings.

<p align="center">
  <img alt="Terminal: ohya ingest imports 51 events from 5 sources, ohya doctor reports ok checks, and the sql tool returns sessions per agent." src="docs/assets/terminal.svg" width="860">
</p>

## Quick start

Requirements: Go 1.27.3 or later, CGO, and a C compiler (Xcode Command Line
Tools on macOS, `build-essential` on Debian or Ubuntu).

```sh
git clone https://github.com/nibzard/ohya && cd ohya
make build
./bin/ohya setup --dry-run   # show the changes setup will make
./bin/ohya setup             # ingestion service + MCP entries for detected agents
./bin/ohya doctor            # check the result
```

Setup registers ohya with Claude Code, Codex, and Pi, and it starts a user
service that imports your history and then follows new records. Start a new
agent session and ask it about earlier work.

<details>
<summary>Manual setup (no service, no client changes)</summary>

```sh
./bin/ohya ingest --once     # import existing history, then exit
./bin/ohya ingest            # or: keep polling until stopped
```

Register the MCP server in your agent, for example in Claude Code:

```json
{
  "mcpServers": {
    "ohya": { "command": "/absolute/path/to/bin/ohya", "args": ["mcp"] }
  }
}
```

</details>

### Share memory across machines

```sh
# On the hub
./bin/ohya serve --tls-cert cert.pem --tls-key key.pem --control-url https://hub.example:9417
./bin/ohya devices invite            # prints a one-time join code

# On each device: paste the code without echo, keep it out of shell history
read -rs OHYA_JOIN_CODE && export OHYA_JOIN_CODE
./bin/ohya setup --join-code-env OHYA_JOIN_CODE --control https://hub.example:9417
```

Joined agents use `ohya mcp --remote` and query the shared history on the
hub. Revoke a device at any time with `ohya devices revoke ID`. See
[serve and join](docs/reference.md#commands) for addresses, TLS, and state
directories.

## What agents can ask

The `sql` tool publishes the schema and examples to the agent when it
connects. A typical recall query:

```sql
SELECT e.id, s.agent, s.cwd, substr(e.text, 1, 300) AS excerpt
FROM events_fts JOIN events_settled e ON e.id = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH 'authentication AND refresh'
ORDER BY events_fts.rank LIMIT 10;
```

| Tool | Does | Bounds |
| --- | --- | --- |
| `sql(query)` | One `SELECT` or read-only `PRAGMA` on a read-only connection | 5 s, 500 rows, 256 KiB, explicit truncation |
| `annotate(operations)` | Upserts or deletes JSON annotations on events; originals stay unchanged | 100 operations, 64 KiB per value, atomic |

`events_settled` omits sessions that were active in the last 10 minutes, so
an agent does not read its own live transcript.

## Commands

| Command | Purpose |
| --- | --- |
| `ohya setup` / `uninstall` | Configure or remove the service and agent integrations; memory stays |
| `ohya ingest [--once]` | Import local session history, then poll for new records |
| `ohya mcp [--remote]` | Serve `sql` and `annotate` on stdio, locally or through a joined device |
| `ohya serve` | Run the hub: coordination plane, DERP relay, and sync endpoint |
| `ohya join CODE` | Pair this device with a hub and upload its sessions |
| `ohya devices invite\|list\|revoke` | Manage paired devices |
| `ohya rederive` | Rebuild normalized rows after a parser upgrade |
| `ohya doctor` | Read-only health checks |
| `ohya version` | Build, parser, and schema versions |

Every command takes `--help`. Reporting commands take `--json`. No command
prompts for input. The [reference](docs/reference.md) describes the flags,
exit codes, and failure behavior.

## Security model

- Transcript content and SQL are untrusted. The SQL authorizer denies writes,
  `ATTACH`, extensions, and state-changing pragmas.
- Devices pair with one-time, expiring invitations. Revocation is final and
  is checked on every request.
- The hub sets the annotation writer from the authenticated device. A caller
  cannot set it.
- Sync and authorization state machines are model-checked with TLA+/TLC
  ([`tla/`](tla/README.md)).

## Status and roadmap

- **Works now:** Claude Code, Codex, and Pi ingestion; `sql` and `annotate`;
  pairing, sync, and revocation; remote MCP; integrated setup.
- **Open:** macOS runtime verification; release archives and a Homebrew tap.
- **Next:** an OpenCode source (OpenCode stores sessions in SQLite).
- **Not planned:** embeddings, vector search, built-in AI classification,
  third-party AI processing.

See [BACKLOG.md](BACKLOG.md) for the full plan and [PRODUCT.md](PRODUCT.md)
for the product vision.

## Documentation

| Document | Contents |
| --- | --- |
| [Reference](docs/reference.md) | Commands, flags, MCP tools, setup, and the code map |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System design and boundaries |
| [DB.md](DB.md) | Schema and migration policy |
| [Sync protocol](docs/sync-protocol.md) | Batches, cursors, and acknowledgments |
| [Performance](docs/performance.md) | Measured limits and benchmarks |
| [Release operations](docs/release-operations.md) | Release, backup, and recovery |
| [ADRs](docs/adr/) | Architecture decisions |
| [MANIFESTO.md](MANIFESTO.md) | Engineering principles |
| [BRAND.md](BRAND.md) | Logo, icon, colors, and usage |
| [TRADEMARKS.md](TRADEMARKS.md) | Use of the ohya name and marks |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) and [MANIFESTO.md](MANIFESTO.md),
then run:

```sh
make check   # gofmt, tests, and vet with the sqlite_fts5 build tag
```

## License

The code and documentation are MIT. See [LICENSE](LICENSE).

The ohya logo, wordmark, and icons in [`docs/assets/brand/`](docs/assets/brand/),
and the script that draws them, `scripts/brand-logo.py`, are not covered by
the MIT License. They are copyright Niko, all rights
reserved. See [BRAND.md](BRAND.md).

"ohya"™ and the elephant mark are trademarks of Niko. You can fork the code,
but a distributed fork must use a different name. See the
[trademark policy](TRADEMARKS.md).
