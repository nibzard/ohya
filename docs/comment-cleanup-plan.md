# Plan for removing code comments

Status: in progress on branch `comment-cleanup`. This document plans the cleanup; it does not authorize behavior changes.

## Objective

Remove explanatory comments from maintained code without losing useful knowledge or changing behavior.
Use clear names and simple structure to explain operations.
Keep contracts, assumptions, and design rationale in the relevant documentation.
Address unclear code directly. Do not introduce speculative abstractions to replace comments.

## Verified starting point

The initial search found 4,310 lines starting with `//` across 107 Go files.
Of these, 2,742 lines occur in source files and 1,568 occur in test files.
These are search counts, not a complete inventory of parsed comments.
They include at least one compiler directive and can miss inline comments.
Refresh the inventory before implementation because the working tree can change.

The following constraints affect the cleanup:

- `internal/store/migrate.go` uses `//go:embed migrations/*.sql` to include migrations in the binary.
- Migration checksums cover the original file bytes, including comments.
- `CONTRIBUTING.md` prohibits rewriting applied migrations.
- Tests in `internal/mcpserver` include SQL comments inside string literals.
- Protocol comments describe frame layouts, bounds, and acknowledgment rules.
- Watcher comments describe file replacement, hardlinks, checkpoints, and recovery.
- Go checks require `-tags sqlite_fts5`. The `make check` target supplies this tag.

## Scope and policy

Start with the Go implementation and its tests under `cmd/` and `internal/`.
Then review executable scripts, the Makefile, workflow configuration, and TLA+ specifications.
Do not remove Markdown explanations or rewrite transcript fixtures.

Before cleanup, update `AGENTS.md` and `CONTRIBUTING.md` with the precise policy:

> Never write explanatory comments in code. Use clear names and simple structure.
> Keep contracts and design rationale in documentation.
> Preserve required machine directives, legal notices, and immutable historical files.
> Preserve comment text when it is input data, including test fixtures.

This is a proposed clarification of the existing rule, not a change already applied.
Do not introduce general exceptions for difficult code or public declarations.
Record each necessary exception with its path and reason in `CONTRIBUTING.md`.
Keep existing SQL migrations byte-for-byte unchanged.
Do not change the migration checksum mechanism to accommodate this cleanup.

## Phase 1: Establish a reliable inventory

1. Record the current revision and working tree status. Preserve all unrelated edits.
2. Run `make check` and `make build` to establish the baseline. Record failures before editing code.
3. Use Go's standard parser with `parser.ParseComments` to inventory every tracked Go file.
4. Report comment locations, package names, test status, and directive candidates.
5. Review each comment group and assign one disposition from the table below.
6. Inventory other code formats with their own syntax. Check strings and executable directives manually.
7. Record counts and unresolved classifications in this plan before starting each package batch.

The inventory tool must distinguish comments from raw strings and quoted strings.
Do not use regular expressions to remove comments.
Use repository files as input, not dependency caches, session directories, or generated build output.
Treat generated files separately: change their source generator only when it belongs to this repository.

| Comment purpose | Action | Completion evidence |
|---|---|---|
| Repeats a name or operation | Remove it | Code remains clear without replacement |
| Explains unclear operations | Improve names or local structure, then remove it | Existing behavior tests pass |
| Defines a contract or invariant | Update authoritative documentation, then remove it | Documentation identifies the package and relevant behavior tests |
| Explains a design decision | Update the relevant architecture decision record or design section | The reason and tradeoff remain discoverable |
| Records a workaround | Verify its cause and status before editing | Keep valid behavior; track a separate fix if needed |
| Contains an unresolved task | Move an actionable item to the established backlog | The item has scope and acceptance criteria |
| Controls the compiler or another tool | Preserve the required directive | The directive remains effective and has a documented reason |
| Contains a legal notice | Preserve it when required | No required notice is lost |
| Belongs to an existing SQL migration | Preserve the entire file | File bytes and checksums remain unchanged |
| Appears inside test input | Preserve the input | Fixture bytes and test coverage remain unchanged |

## Phase 2: Preserve useful knowledge

Use existing documents first. Avoid creating one document per source file.
Add a focused document only when the current documents cannot hold the material clearly.

| Code area | Knowledge to retain | Documentation destination |
|---|---|---|
| `internal/hubsync` | Frame layouts, field sizes, byte order, bounds, version handling, acknowledgments | Sync sections in `ARCHITECTURE.md`; add `docs/sync-protocol.md` if needed |
| `internal/watch`, `internal/durablefile`, `internal/uploader` | File identity, checkpoints, durable writes, retries, offline replay, recovery | `ARCHITECTURE.md` and the recovery decision record |
| `internal/store` | Transactions, migration checksums, deduplication, parser versions, annotations | `DB.md` and the relevant decision records |
| `internal/claudecode`, `internal/codex`, `internal/pi` | Format evidence, identity rules, usage accounting, malformed input behavior | Existing format documents under `testdata/` |
| `internal/events` | Derivation rules and event identity | Domain sections in `ARCHITECTURE.md` and `DB.md` |
| `internal/mcpserver`, `internal/registry`, `internal/control`, `internal/transport` | Authorization, query bounds, ownership, revocation, transport assumptions | `ARCHITECTURE.md` and the relevant decision records |
| `internal/device` | Identity ownership, credential storage, permissions | Identity and security sections in `ARCHITECTURE.md` |
| `cmd/ohya` | Command behavior, output contracts, exit codes, defaults | `README.md`, `CONTRIBUTING.md`, and the JSON output decision record |
| Fixture generation, benchmarks, and test commands | Synthetic data constraints and reproduction commands | `testdata/README.md`, `docs/performance.md`, and `CONTRIBUTING.md` |
| `tla/` | Model assumptions, invariants, action meanings, verification commands | `tla/README.md` |

Verify each statement against current code and tests before moving it.
Do not copy stale comments into documentation as facts.
Keep one authoritative explanation and link to it from other documents where needed.
Keep documentation and the related comment removal in the same commit.

## Phase 3: Clean up one area at a time

Use this order to expose simpler cases before touching critical state handling:

1. Fixture generators, benchmark helpers, and standalone developer commands.
2. Agent parsers and event derivation, including their tests.
3. Device identity, durable file operations, and watcher state.
4. Store operations, with all historical migration files excluded.
5. Query tools, registry, control, and transport adapters.
6. Sync frames, sessions, hub endpoints, and the uploader.
7. The main command, remaining integration tests, build files, scripts, and formal specifications.

Each area can require several commits. Keep each commit small enough for a reviewer to understand.
Inspect the applicable nested instructions before editing an area.

For each package batch:

1. Read the implementation, tests, comments, and applicable documentation together.
2. Remove redundant explanations and preserve important knowledge in documentation.
3. Rename unclear local variables or helpers where their purpose is established.
4. Extract a helper only when it expresses a coherent domain operation and reduces complexity.
5. Remove the reviewed comments and format changed Go files.
6. Run the applicable checks, inspect the full diff, and verify protected files and literals.
7. Commit only that coherent batch and attach a brief Git context note.

Do not rename exported identifiers solely to eliminate a documentation comment.
Do not create one-line wrappers solely to turn prose into function names.
Use descriptive test names and named cases when they clarify test intent.
Preserve assertions, negative cases, and failure-path coverage.
If review exposes a behavior bug, track it separately with a regression test and explicit scope.
Do not mix a protocol, schema, or authorization redesign into comment removal.

## Phase 4: Prevent recurrence

Add a small checker that uses the Go standard library and requires no new dependencies.
Reuse the inventory logic instead of maintaining a second parser.

The checker must:

- Parse all maintained Go files, including tests and files excluded by the current platform's build constraints.
- Report explanatory comments with their file and line.
- Accept only explicitly approved machine directives or required legal notices.
- Reject unreviewed directive prefixes; do not accept every `//go:` comment automatically.
- Ignore comment markers inside literals and leave test inputs unchanged.
- Return a failure when a file cannot be read or parsed.

Add meaningful tests for inline comments, block comments, raw strings, directives, and platform-specific file discovery.
The checker itself follows the clarified policy.
Develop it with the inventory, then enable enforcement after all Go batches are complete.
Add it to `make check` so existing continuous integration jobs run it on Linux and macOS.
Document its scope and exception procedure in `CONTRIBUTING.md`.
Review the remaining language formats manually; do not add a fragile universal regular expression gate.

## Validation

For a Go batch, format its changed files with `gofmt` and run focused tests first.
Then run `make check` before committing the completed increment.
Run `make build` when a batch affects directives, embedded resources, or command wiring.

For a pure comment deletion, compare Go tokens before and after with comments excluded.
Require token kinds and literal text to match. Ignore source positions and redundant empty lines.
This check does not apply unchanged to batches that deliberately rename or extract helpers.
Review those structural diffs separately and verify behavior through existing tests.

Use these additional checks when their triggers apply:

| Trigger | Required evidence |
|---|---|
| Migration-related cleanup | Historical SQL files match the baseline bytes; migration drift tests still pass |
| Parser structure changes | Parser and adapter tests pass; use existing fuzz targets when parsing logic changes |
| Concurrent state handling changes | Focused tests pass under `-race` with `-tags sqlite_fts5` |
| Sync or authorization state machine changes | Review assumptions and run `make tla`; record all results |
| TLA+ comment removal | Preserve specification expressions and run `make tla` |
| Shell or workflow comment removal | Preserve interpreter directives, command text, quoting, and YAML block scalar contents |
| Performance-sensitive restructuring | Run the relevant existing benchmarks and investigate unexplained regressions |

Finish the complete cleanup with `make check`, `make build`, and a fresh comment inventory.
Record local platform limits. Existing continuous integration jobs provide Linux and macOS checks after publication.
Do not claim those jobs passed before they run.
The external memory verification gate is unnecessary unless client behavior changes.
Avoid such behavior changes in this work.

## Completion criteria

- No explanatory comments remain in maintained Go source or tests.
- The checker rejects a new explanatory comment and accepts the approved directives.
- Every remaining comment in other executable formats has a reviewed disposition.
- All required directives, notices, literal input, and historical migrations remain intact.
- Useful contracts and rationale are accurate and discoverable in the designated documentation.
- Names and structure explain operations without unnecessary helpers or new dependencies.
- Tests retain their behavioral assertions and failure cases.
- Required local checks pass, or an existing baseline failure is clearly recorded and remains unresolved.
- Commits contain only this work. No push, release, or publication occurs as part of the cleanup.

## Evidence to record during implementation

For each batch, record the paths, removed comment count, retained exceptions, and documentation destination.
Record the exact check commands, results, commit ID, and any open issue.
Use commit context notes for completed batches and `NOTES.md` for unresolved findings.
Do not keep a duplicate archive of deleted comments.

## Source references

This plan uses local repository evidence. Recheck it before implementation.

- [Agent instructions](../AGENTS.md)
- [Engineering manifesto](../MANIFESTO.md)
- [Contribution checks and migration policy](../CONTRIBUTING.md)
- [Build and check targets](../Makefile)
- [Migration loading and checksums](../internal/store/migrate.go)
- [Migration failure tests](../internal/store/migrate_test.go)
- [Sync protocol implementation](../internal/hubsync/protocol.go)
- [Watcher implementation](../internal/watch/watch.go)
- [SQL parser test inputs](../internal/mcpserver/statement_test.go)
- [Formal model documentation](../tla/README.md)

## Implementation record

Baseline: revision `c0079bf`, recorded 2026-11-14.
The work runs in a separate worktree on branch `comment-cleanup`.
The main worktree had unrelated uncommitted edits to the Codex repository derivation; this branch does not include them.
`make check` and `make build` passed at the baseline.
The first attempt failed with "no space left on device"; clearing the Go build cache fixed it.

`go run ./cmd/commentcheck -inventory` found 4,537 comment tokens in 110 Go files at the baseline:

- 2,893 explanatory comments in source files.
- 1,643 explanatory comments in test files.
- 1 approved directive: `//go:embed migrations/*.sql` in `internal/store/migrate.go`.
- No cgo preambles, build constraints, or other directive candidates.

The inventory counts each line of a `//` comment group separately.
Comment markers inside raw and quoted strings are not comments and are not counted.

| Batch | Paths | Comments removed | Documentation destination |
|---|---|---|---|
| 1 | `cmd/genfixture`, `internal/fixgen` | 201 | `docs/performance.md`, "Corpus shape" |
