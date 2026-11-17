# Offline analysis scripts

Read-only measurement programs for storage decisions. They open a hub
database with `mode=ro` and never write to it. Each program writes its
results to `perf/t102/` (gitignored, aggregate numbers only) and to
stdout. Scratch databases, sample files, and trained dictionaries stay
in a temporary directory that the program removes at exit.

Never commit an output file, a dictionary, or a sample file. A trained
dictionary contains fragments of transcript content, so it must not
leave the machine it was trained on.

Shared helpers live in `perf/hubdata`.

## FTS index share by event category

```sh
go run -tags "sqlite_fts5 sqlite_dbstat" ./perf/ftsshare \
  -db ~/.ohya/hub.db -out perf/t102/fts-share.txt
```

Samples every Nth event id (`-sample`, default 20) and inserts the
sampled text into one contentless FTS5 table per event category, plus a
tool-output partition and an all-docs table. Index bytes per category
are the `_data`, `_docsize`, and `_idx` pages after `optimize`,
measured with `dbstat`. The `sqlite_dbstat` build tag is required for
the driver; without it the program fails at the `dbstat` query.

## Duplication and compression ratios per device

```sh
go run -tags sqlite_fts5 ./perf/devratio \
  -db ~/.ohya/hub.db -out perf/t102/device-storage.txt
```

Phase A samples every Nth event id and measures per-row zstd level 3
ratios, the byte share of JSON string values of 256 bytes or more, and
exact-repeat shares per device. Phase B reads whole sessions picked
with probability proportional to their estimated size and measures
duplication over the session stream, 64 KB and 1 MB block ratios, and
per-row and block ratios with a dictionary trained by `zstd --train`
on one half of the picked sessions and measured on the other. The
dictionary is trained through the `zstd` CLI, which must be on `PATH`.

## zstd decode cost per block

```sh
go run -tags sqlite_fts5 ./perf/zstdbench \
  -db ~/.ohya/hub.db -out perf/t102/zstd-decode.txt
```

Builds 64 KB and 1 MB blocks from the largest hub sessions, compresses
them with `klauspost/compress` zstd level 3 with and without a trained
dictionary, verifies a byte-exact round trip, and times repeated
single-goroutine `DecodeAll` calls. Reports MB/s and microseconds per
block. The library version is the one linked into this module, the
same code the ohya binary carries through `tailscale.com`.

## Recall query deadline reproduction

```sh
go run -tags sqlite_fts5 ./perf/recallduration -mode build \
  -db <fixture>/hub.db -scale 2.8 -seed 7
go run -tags sqlite_fts5 ./perf/recallduration -mode measure -runs 40 \
  -db <fixture>/hub.db
go run -tags sqlite_fts5 ./perf/recallduration -mode index -runs 20 \
  -db <fixture>/hub.db
```

Reproduces the timed-out `events`-to-`sessions` join on `cwd` from plan
003 R2 on a synthetic hub-scale fixture: the compact three-agent `mix`
corpus at scale 2.8 (seed 7) matches the live hub's event count
(about 2.7 M events; the live hub held 2,691,545 events on
2026-11-17). Build writes the fixture database through the production
parse and ingest path (about 30 minutes on the reference VM; keep it
outside the repository). Measure runs the failing join, the settled
variant, and the covering-subquery recipe through the mcpserver engine
with a usage sink attached, one cold run (the database is evicted from
the page cache with `posix_fadvise`) plus N warm runs per shape, and
reports cold, p50, p95, timeouts, and result bytes per shape from the
usage records, then the `EXPLAIN QUERY PLAN` of each shape. Index
creates `events(time_ms DESC, id)` on the fixture, measures build
time, added pages, the join latency with the index present (including
a no-match cwd worst case), and drops the index again. The fixture is
synthetic; no transcript content exists in it or in any output.
