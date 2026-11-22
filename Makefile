.PHONY: build check bench bench-save bench-diff memory-e2e recall-bench tla

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

# Full recall exercise against the local production hub: plant canaries
# through the real agents, wait out the settle gate, run both arms, and
# write report.md under recall-runs/<timestamp>. Real API spend; run it
# deliberately, never from check.
recall-bench: build
	@d=$$(date -u +%Y%m%dT%H%MZ); mkdir -p recall-runs; \
	go run ./cmd/recall-bench plant -out recall-runs/$$d; \
	go run ./cmd/recall-bench run -out recall-runs/$$d \
		-questions questions/recall-bench-curated.json -ohya bin/ohya

tla:
	./tla/run-tlc.sh
