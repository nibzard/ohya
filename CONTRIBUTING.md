# Contributing

Read MANIFESTO.md and AGENTS.md first. Go 1.25 or later, CGO and a C compiler
are required (on macOS, install Xcode Command Line Tools). SQLite is bundled in
the driver. From the repository root:

```sh
make check  # gofmt, tests and vet, with FTS5 enabled
make build  # bin/ohya
./bin/ohya
```

For individual Go commands use `-tags sqlite_fts5`, including tests and vet;
without it the SQLite build lacks FTS5. CI runs these checks on macOS. The
initial SQL migration is the schema source of truth; future migrations must not
rewrite applied migrations. Use synthetic, non-sensitive fixtures in `testdata/`.
Do not commit session transcripts or credentials. See BACKLOG.md for work order.
