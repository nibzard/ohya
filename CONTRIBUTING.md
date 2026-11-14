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
The Linux job exists because the macOS job cannot see Linux-only breakage
(CGO, file identity in `internal/watch`, path handling); the Ubuntu image
ships gcc and make, so CGO needs no setup. `make tla` runs only on the macOS
job, because TLC is platform-independent Java and a second run adds no
coverage. The benchmark job reports and never fails on a regression. It
uploads its raw run even when a benchmark fails, because `make bench-save`
redirects the test output into that file.
Migrations are versioned SQL files under `internal/store/migrations/NNN_name.sql`;
the runner applies them in order and records checksums in `schema_migrations`.
Never rewrite or delete an applied migration — add a new file instead, and keep
connection pragmas in `internal/store` code rather than migration SQL (see DB.md
for the upgrade policy). TLA+ models cover sync, authorization, and device
transport ownership. Run `make tla` to check them with TLC (Java 11 or later).
The pinned TLC jar is downloaded and checksum-verified automatically.
See tla/README.md for the checked properties and assumptions. Use synthetic,
non-sensitive fixtures in `testdata/`.
Fuzz targets run their seed corpus in `make check`. For deep fuzzing, run one
target at a time, for example
`go test -tags sqlite_fts5 -run '^$' -fuzz=FuzzReadFrame ./internal/hubsync`.
The targets are `FuzzParse` in `internal/claudecode`, `internal/codex`, and
`internal/pi`; `FuzzSingleStatement` in `internal/mcpserver`; and
`FuzzReadFrame` and `FuzzReadAuthFrame` in `internal/hubsync`.
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

The gate (`cmd/memory-e2e`) works as follows:

- The Claude Code fixture holds the explicit decision, and the Codex
  fixture holds the original decision and its later correction. Each
  client must therefore read a decision that the other agent recorded.
  Every marker must match exactly one seeded event, and the stale and
  corrected markers never appear in each other's events.
- The current-context case supplies a release codename in the prompt. The
  codename appears nowhere in the seeded history.
- An answer cites evidence as `evidence: <event-id>`. The last citation
  wins, because models sometimes repeat the instruction first. The gate
  resolves the cited ID through its own session on the `sql` tool.
- Claude Code gets an environment allowlist (`PATH` and `ANTHROPIC_*`), a
  scratch `HOME`, and an isolated `CLAUDE_CONFIG_DIR`.
- Codex keeps the operator's `HOME`, because a mise shim launcher
  busy-spins when `HOME` is redirected (verified 2026-11-14).
  `CODEX_HOME` still isolates every file that Codex reads or writes.
- Codex 0.169 does not read `OPENAI_API_KEY` by itself. The gate pipes the
  key to `codex login --with-api-key` and never logs it. The login
  rewrites `config.toml`, so the gate then verifies that the `ohya` MCP
  server is still registered.
- Codex runs with `--trust` for the scratch case directory, because it
  gates project MCP servers behind project trust. The trust record stays
  inside the isolated `CODEX_HOME` (verified 2026-11-14).
- Codex emits WebSocket fallback error items even on successful runs. The
  trace parser records each error item and keeps reading. `checkCase`
  currently counts every recorded problem as a case failure; see the open
  item in NOTES.md.

## Command implementation

- `cmd/ohya` uses only the standard `flag` package. Each command level has a
  `commandSpec` (usage line, description, arguments, flags, examples) and
  one shared template. The flag package's own output is discarded: the
  template renders into a buffer, and the caller sends it to stdout for a
  help request or drops it for a usage error. Flags render with two
  dashes, and a default is shown only when it differs from the zero value
  of its type; string defaults are quoted.
- Local ingestion reads at most 8 MiB per batch. A record larger than the
  batch bound extends the read to its end, up to the record size limit, so
  a batch never splits a record. Ingestion never acknowledges inside
  `ingestSource`; the caller acknowledges the committed cursor afterwards.
- `ohya devices invite` provisions the coordinator user and preauth key
  before the registry invitation, so it never issues half a join code. The
  hub's own admission key is valid for 10 minutes. A joined device's tailnet
  hostname is `dev-` plus the first 12 characters of its Device ID.
- The `cmd/ohya` tests run the test binary as a child process. A first
  argument of `child-run` runs the full CLI with the production exit codes.
  `child-serve`, `child-join`, and `child-mcp` run those commands for the
  end-to-end sync test. With `OHYA_MCP_TEST_DB` set, the binary serves
  `ohya mcp`, so the test framework writes nothing to the protocol stream.

## Code comments

Never write explanatory comments in code. Use clear names and simple
structure. Keep contracts and design rationale in documentation:
`ARCHITECTURE.md`, `DB.md`, the ADRs under `docs/adr/`, the format notes
under `testdata/`, and `tla/README.md`. Preserve required machine
directives, legal notices, and immutable historical files. Preserve
comment text when it is input data, including test fixtures.

`make check` runs `go run ./cmd/commentcheck`, which parses every Go file in
the repository with the standard Go parser and fails on each comment that is
not an approved directive. It covers test files and files for other platforms. It skips
`testdata`, `vendor`, and directories whose names start with `.` or `_`,
as the Go tool does. Add `-inventory` to list every comment with its
package and classification instead of failing.

Approved exceptions:

| Path | Comment | Reason |
|---|---|---|
| `internal/store/migrate.go` | `//go:embed migrations/*.sql` | Embeds the migration files in the binary |
| `internal/store/migrations/*.sql` | SQL comments | Applied migrations are immutable, and their checksums cover the comment bytes |
| `testdata/` | Comment text in fixtures | Fixtures are parser input |
| `internal/mcpserver/*_test.go` | SQL comments inside string literals | Test input for the statement scanner |

To approve a new directive, add its exact prefix to `approvedDirectives` in
`cmd/commentcheck/main.go` and add a row to this table with the reason.
