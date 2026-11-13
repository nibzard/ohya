.PHONY: build check tla

build:
	go build -tags sqlite_fts5 -o bin/ohya ./cmd/ohya

check:
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go test -tags sqlite_fts5 ./...
	go vet -tags sqlite_fts5 ./...

# Model-check the sync TLA+ specification with TLC (tla/README.md).
# Needs Java 11 or later; downloads the pinned TLC jar on first use.
tla:
	./tla/run-tlc.sh
