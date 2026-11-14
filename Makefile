.PHONY: build check bench bench-save bench-diff memory-e2e tla

build:
	go build -tags sqlite_fts5 -o bin/ohya ./cmd/ohya

check:
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go test -tags sqlite_fts5 ./...
	go vet -tags sqlite_fts5 ./...

# Ingestion and retrieval benchmarks over deterministic synthetic corpora
# (one per agent format). OHYA_BENCH_MIB raises the corpus volume (default
# 32); the measured limits and the reproduction commands live in
# docs/performance.md.
bench:
	go test -tags sqlite_fts5 -run '^$$' -bench . -benchmem \
		./internal/events ./internal/mcpserver

# Repeated benchmark run saved for cross-commit comparison (benchstat).
# COUNT is the repetitions per benchmark — benchstat needs several runs per
# benchmark for statistics; 10 is the default CI stores. OUT names the output
# file and defaults to a per-commit name under perf/ (gitignored).
COUNT ?= 10
OUT = perf/bench-$(shell git rev-parse --short HEAD).txt

bench-save:
	mkdir -p $(dir $(OUT))
	go test -tags sqlite_fts5 -run '^$$' -bench . -benchmem -count $(COUNT) \
		./internal/events ./internal/mcpserver > $(OUT)
	@echo wrote $(OUT)

# benchstat table between two saved runs (docs/performance.md names the
# pinned benchstat version to install; it is a tool, not a dependency).
bench-diff:
	benchstat $(BASE) $(NEW)

# Memory verification gate with real agent clients: seed an isolated hub
# database with synthetic recorded decisions through the real ingest path,
# then a real Claude Code and a real Codex client must retrieve the evidence
# through the sql MCP tool and cite event IDs that resolve in the hub; a
# current-context case must be answered without querying memory at all.
# Needs both CLIs on PATH plus ambient API credentials (ANTHROPIC_* for
# Claude Code, OPENAI_API_KEY for Codex). Never reads ~/.claude or ~/.codex;
# every database, client state file, and trace stays under memory-e2e/
# (gitignored). Not part of make check.
memory-e2e: build
	go run ./cmd/memory-e2e -ohya bin/ohya

# Model-check the TLA+ protocol specifications with TLC (tla/README.md).
# Needs Java 11 or later; downloads the pinned TLC jar on first use.
tla:
	./tla/run-tlc.sh
