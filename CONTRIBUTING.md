# Contributing

Read MANIFESTO.md and AGENTS.md first. Go 1.27.3 or later, CGO and a C
compiler are required (on macOS, install Xcode Command Line Tools). The Go
minimum comes from the pinned Tailcat transport dependency
([ADR 0002](docs/adr/0002-tailcat-transport.md)); with the default
GOTOOLCHAIN=auto the toolchain is fetched as needed. SQLite is bundled in
the driver. From the repository root:

```sh
make check  # gofmt, tests and vet, with FTS5 enabled
make build  # bin/ohya
./bin/ohya
```

For individual Go commands use `-tags sqlite_fts5`, including tests and vet;
without it the SQLite build lacks FTS5. CI runs these checks on macOS.
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
