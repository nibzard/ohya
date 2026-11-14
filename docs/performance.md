# ohya — Measured performance limits

This document records what the hub costs to run, measured on real code paths
with a reproducible corpus. Operators use it to plan disk, memory and time
budgets; engineers use it as the baseline before any optimization. Every
number below was measured on 2026-11-14 on the reference machine in the next
section; absolute values do not transfer to other hardware, but the ratios
and the scaling behavior do.

## Method

All measurements run on synthetic corpora from `internal/fixgen`:
deterministic session generators for every supported agent format (the same
record kinds, field shapes and size distribution verified in each
`testdata/<agent>/README.md`). All three draw texts from one skewed word
distribution, so per-agent numbers differ in record envelope and parser, not
in text volume: mean record size is about 3.2 KB for Claude Code, 3.7 KB for
Codex rollouts and 4.7 KB for Pi sessions. No real transcripts are committed
or required. The same seed always produces the same bytes.

Reproduce with:

```sh
make bench                                   # ingestion + query benchmarks, 32 MiB corpus
OHYA_BENCH_MIB=128 make bench                # larger volume
go run ./cmd/genfixture -root /tmp/corpus    # on-disk Claude Code corpus
go run ./cmd/genfixture -agent codex -root /tmp/corpus-codex
go run ./cmd/genfixture -agent pi -root /tmp/corpus-pi
make build
/usr/bin/time -v ./bin/ohya ingest --once --claude-root /tmp/corpus \
    --codex-root '' --pi-root '' --db /tmp/hub.db --state /tmp/state
```

The Codex and Pi corpora ingest the same way with `--codex-root
/tmp/corpus-codex` (and the other two roots empty) or `--pi-root
/tmp/corpus-pi`.

Benchmarks write only to temporary directories; profiles and other generated
outputs belong under `perf/` (gitignored).

Reference machine: one shared Linux VM (kernel 6.8, ext4), 12th-generation
laptop CPU reporting 8 cores to Go, Go 1.27.3, CGO build with the SQLite
driver's bundled SQLite, WAL journaling and `synchronous = FULL` — the
shipped durability configuration. The disk serves other tenants, so
fsync-bound numbers vary between runs; every fsync-sensitive figure below
states its observed spread.

## Ingestion

Benchmarks: `internal/events` (`make bench`), end-to-end run of
`bin/ohya ingest --once` over 461 files (200 main sessions plus subagent
transcripts, 138.7 MiB, 43,626 events).

| Path | Result | Notes |
|---|---|---|
| First import, full path | 4–8 MB/s sustained | Store-level benchmark; single-run spread across disk states, 4.42 MB/s at 128 MiB, up to 19 MB/s with a hot page cache |
| First import, end to end | 56 s wall, 12 s CPU, 138.7 MiB | 2.5 MB/s wall on the contended reference disk; CPU is 21% of wall — the path waits on fsync |
| Parser alone (no database) | Claude Code 127–132 MB/s, Codex 173–178 MB/s, Pi 156–159 MB/s | Stable across runs; CPU-bound; `BenchmarkParseCorpus*` over per-agent corpora from one text distribution |
| Idempotent replay validation | 1.0–1.2 GB/s | A fully repeated 2 MiB batch costs about 2 ms: retry after a lost acknowledgment is cheap |
| One live append (8 records, one commit) | 5–33 ms | fsync at FULL synchronous dominates; spread measured across runs |
| No-op polling pass, 461 sources | 0.02 s | Discovery, stat and fingerprint checks per poll |
| Pass that ingests one 2-record turn | 0.13 s | Near-real-time within the default 2 s poll interval plus about 0.1 s of commit |

CPU profile of the ingest benchmark (5 runs, `perf/ingest-cpu.prof`): 67% of
CPU samples sit inside `runtime.cgocall` — SQLite executing inserts and
commits in C — and 8–15% in `encoding/json` decoding; `store.Ingest`
accumulates 64% cumulative. CPU samples cover only about a quarter of wall
time: the process waits on fsync, not on Go code. No ohya-level Go function
is hot, so no Go-side optimization is justified; the levers are durability
settings and batch size, which are deliberate design choices, not accidents.

Extrapolation: a history the size of the verified real corpus (about 2 GB)
imports in 4–13 minutes wall (about 2 minutes CPU) on comparable hardware.
Ingestion work is linear in bytes: the per-byte rate is flat from 4 MiB to
128 MiB corpora within measurement noise.

## Queries

Benchmarks: `internal/mcpserver` over the seeded hub, plus one probe through
the real `ohya mcp` stdio tool (JSON-RPC round trip included) against the
335 MiB end-to-end hub with 43,626 events. The sql tool's default limits
apply: 5 s deadline, 500 rows, 256 KiB output.

| Query pattern | 32 MiB hub | 128 MiB hub | 335 MiB hub, real stdio |
|---|---|---|---|
| FTS selective term (rare word, ranked, LIMIT 10) | 0.11 ms | 0.16 ms | 0.6 ms |
| FTS wide term (common word, ranked, LIMIT 10) | 4.9 ms | 20.9 ms | 25.3 ms |
| Session token totals (GROUP BY over all sessions) | 0.27 ms | 1.1 ms | 4.1 ms |
| Recent sessions (indexed ORDER BY, LIMIT 20) | 0.05 ms | 0.06 ms | 0.7 ms |
| One session's event range (LIMIT 500) | 1.5 ms | 1.2 ms | — |
| Annotation paging (NOT EXISTS, LIMIT 100) | 0.10 ms | 0.10 ms | — |

Every measured pattern answers at least two orders of magnitude inside the
5 s deadline. Selective search, indexed listings and paging are flat in
corpus volume. Wide full-text matches scale with the number of matching
postings — about 0.5–1 ms per 1,000 matching events at this scale — so a
10x larger hub keeps wide matches in the hundreds of milliseconds.

## Database size

The end-to-end hub holds 138.7 MiB of source records as 351.6 MB (2.42x
input). The multiplier is structural: every record stores its original JSON
verbatim, the normalized searchable text, compact metadata, and the FTS5
index over that text. Plan about 2.5x the transcript volume for the hub
database, plus the same again temporarily for a `VACUUM INTO` backup.

During sustained import the write-ahead log stays bounded by SQLite's
automatic checkpoint: about 4–6 MiB observed, cycling as checkpoints keep
up. A clean process stop checkpoints and removes the WAL.

## Memory

The import streams bounded batches, so memory does not grow with corpus
size: peak resident set size of the full 138.7 MiB end-to-end import was
27.7 MB (about 28 MB including the SQLite page cache). The in-process bound
is one 8 MiB batch plus at most one record at the 1 MiB record limit.
Benchmark allocations through parse plus commit run about 7.3 bytes of
garbage per input byte. Query memory per call is bounded by the row and
byte limits (500 rows, 256 KiB); observed allocations per call are 9–320 KB
depending on row width.

## Regression guards

Two tests fail `make check` on order-of-magnitude regressions; their
ceilings sit far above the measured times so machine-speed drift does not
flake them:

- `TestIngestPerformanceFloor` (`internal/events`): a 24 MiB import through
  the full path must finish inside 30 s (measured about 3 s; a superlinear
  write path blows past the ceiling).
- `TestRetrievalPerformanceCeiling` (`internal/mcpserver`): wide FTS search,
  token totals and annotation paging over a seeded hub must each answer
  inside 2 s (measured under 10 ms; the engine's 5 s deadline is the hard
  bound agents experience).

## Tracking across commits

A single `make bench` run cannot be compared by eye: the fsync-bound
numbers swing several-fold between runs (see above), so any regression
smaller than that spread is invisible. Cross-commit comparison is routine
instead:

```sh
git checkout main && make bench-save          # baseline, COUNT=10 runs per benchmark
git checkout mybranch && make bench-save      # candidate
make bench-diff BASE=perf/bench-<main>.txt NEW=perf/bench-<branch>.txt
```

- `make bench-save` repeats every benchmark `COUNT` times and writes the
  raw run to `OUT`, which defaults to `perf/bench-<commit>.txt`. The
  default of 10 leaves margin over benchstat's minimums (6 samples for a
  confidence interval, 4 to test a difference). Generated output stays
  under `perf/` (gitignored), like profiles.
- `make bench-diff` prints the benchstat table: per-benchmark medians,
  the delta between the two runs, and whether that delta is statistically
  distinguishable from run-to-run noise. Install benchstat as a pinned
  tool, not a project dependency:

  ```sh
  go install golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e
  ```

CI automates this (`.github/workflows/ci.yml`, `bench` job): every push
and pull request stores its raw run as the `bench-<commit>` artifact,
restores the baseline the main branch last saved, and appends the
benchstat table to the job summary. The job reports; it does not fail on
a regression. Only same-hardware comparisons are meaningful — GitHub's
runners differ from the reference machine, and even between CI runs the
fsync-bound rows (`BenchmarkIngestCorpus`, `BenchmarkIngestAppend`) swing
with the shared runners' disk contention, while the parse and query rows
are stable. Treat a CI delta as a signal to reproduce locally with
`make bench-save` on both commits, not as a verdict.

## Limits and open items

- All fsync-sensitive numbers (import rate, append latency) vary several-fold
  with disk contention; treat the ranges, not the midpoints, as the limit.
- Measurements cover one machine. End-to-end ingestion is benchmarked on the
  Claude Code format (the largest verified corpus); Codex and Pi are
  benchmarked at the parser (`BenchmarkParseCorpusCodex`,
  `BenchmarkParseCorpusPi`) over corpora drawn from the same text
  distribution: 173–178 and 156–159 MB/s against Claude Code's 127–132 —
  the same order, JSON decode dominating. The write path behind the parser
  is shared and fsync-bound, so the full-path numbers above apply to every
  agent format.
- Query latency is measured warm (page cache hot). A cold hub on a slow disk
  pays storage reads on first access; the 5 s deadline absorbs that at these
  volumes.
- Benchmark comparison across commits is automated only where CI reaches:
  each run is compared against the baseline the main branch last saved, on
  GitHub's shared runners. Comparisons across machines (CI versus the
  reference VM) stay manual: `make bench-save` on both commits, then
  `make bench-diff`.
