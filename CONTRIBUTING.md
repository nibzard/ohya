# Contributing

Read MANIFESTO.md and AGENTS.md first. Go 1.27.3 or later, CGO and a C
compiler are required (on macOS, install Xcode Command Line Tools; on
Debian or Ubuntu, `sudo apt install build-essential`). The Go
minimum comes from the pinned Tailcat transport dependency
([ADR 0002](docs/adr/0002-tailcat-transport.md)); with the default
GOTOOLCHAIN=auto the toolchain is fetched as needed. SQLite is bundled in
the driver. From the repository root:

```sh
make check  # gofmt, tests and vet, with FTS5 enabled
make build  # bin/ohya
./bin/ohya
```

`make bench` runs the ingestion and retrieval benchmarks over
deterministic synthetic corpora for each agent format (raise the volume
with `OHYA_BENCH_MIB`);
the measured limits and how to reproduce them are documented in
[docs/performance.md](docs/performance.md). Two performance regression
tests run as part of `make check`. To compare commits instead of single
runs, `make bench-save` writes a repeated run (default 10 per benchmark)
and `make bench-diff BASE=<old> NEW=<new>` prints the benchstat table
between two saved runs; CI stores every run as an artifact and posts the
diff against the main-branch baseline — see "Tracking across commits" in
docs/performance.md.

For individual Go commands use `-tags sqlite_fts5`, including tests and vet;
without it the SQLite build lacks FTS5. CI runs these checks on macOS and
on Linux (`ubuntu-latest`), the platform development executes on; the
benchmark job runs on macOS only so its numbers always compare against the
same runner hardware.
Migrations are versioned SQL files under `internal/store/migrations/NNN_name.sql`;
the runner applies them in order and records checksums in `schema_migrations`.
Never rewrite or delete an applied migration — add a new file instead, and keep
connection pragmas in `internal/store` code rather than migration SQL (see DB.md
for the upgrade policy). The TLA+ models for the sync and authorization
protocols live in `tla/`; run `make tla` to check them with TLC (Java 11 or
later; the pinned TLC jar is downloaded and checksum-verified automatically).
See tla/README.md for the checked properties and assumptions. Use synthetic,
non-sensitive fixtures in `testdata/`.
Do not commit session transcripts or credentials. See BACKLOG.md for work order.

`make memory-e2e` is the memory verification gate for release claims. It
seeds an isolated hub database with synthetic recorded decisions through the
real ingest path, then a real Claude Code and a real Codex client — launched
headlessly with `CLAUDE_CONFIG_DIR` and `CODEX_HOME` redirected under
`memory-e2e/` — must retrieve the evidence through the `sql` MCP tool and
cite event IDs that resolve in the hub; a later-correction case must return
the corrected value, and a current-context case must be answered without
querying memory at all. The gate never reads `~/.claude` or `~/.codex`; the
clients authenticate from ambient environment variables (`ANTHROPIC_*`, and
`OPENAI_API_KEY` for a one-time `codex login` inside the isolated home), and
all traces stay under `memory-e2e/` (gitignored). It is not part of
`make check`: it needs both CLIs on `PATH`, network access, and credentials.
