.PHONY: build check bench bench-save bench-diff memory-e2e tla

build:
	go build -tags sqlite_fts5 -o bin/ohya ./cmd/ohya

check:
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go test -tags sqlite_fts5 -timeout 20m ./...
	go vet -tags sqlite_fts5 ./...
	go run ./cmd/commentcheck

bench:
	go test -tags sqlite_fts5 -run '^$$' -bench . -benchmem \
		./internal/events ./internal/hubsync ./internal/mcpserver

COUNT ?= 10
OUT = perf/bench-$(shell git rev-parse --short HEAD).txt

bench-save:
	mkdir -p $(dir $(OUT))
	go test -tags sqlite_fts5 -run '^$$' -bench . -benchmem -count $(COUNT) \
		./internal/events ./internal/hubsync ./internal/mcpserver > $(OUT)
	@echo wrote $(OUT)

bench-diff:
	benchstat $(BASE) $(NEW)

memory-e2e: build
	go run ./cmd/memory-e2e -ohya bin/ohya

tla:
	./tla/run-tlc.sh
