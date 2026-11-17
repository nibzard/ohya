# Brainstorm: a smaller, faster ohya memory store

Date: 2026-11-16. Status: brainstorm, nothing decided. Method: Innovation Lab
(frame, map, speculate, generate, pressure test, make real), applied to a
technical problem rather than a market.

## 1. Frame

**Challenge.** The hub on the VM is 16.3 GB. It mirrors 9.85 GB of agent
transcripts from two devices. Find designs that store and serve the same
memory with less disk, less I/O, and less CPU, over a 12-month horizon.

**Hard constraints** (from `MANIFESTO.md` and `AGENTS.md`):

- Keep every original record recoverable byte for byte. Lossy storage of
  originals is out.
- One Go binary, one hub-owned SQLite database, FTS5/BM25 search.
- Never require an LLM for ingestion or search.
- No embeddings, mandatory AI classification, extra infrastructure, or new
  dependencies without evidence.
- Ingestion stays idempotent and crash-safe.

**Anti-goals.** Do not build a general data lake. Do not trade recall
quality for size. Do not add a server process.

## 2. Map the territory

### Measured facts (2026-11-16, this hub)

| Fact | Value | How measured |
|---|---|---|
| Hub file | 16.3 GB, 0 free pages | `PRAGMA page_count`, `freelist_count` |
| Raw transcript bytes ingested | 9.85 GB (VM 5.38, MacBook 4.47) | `sum(sources.cursor_bytes)` |
| `events` table | 14.88 GB, of which 13.84 GB payload, 0.99 GB unused page space | `dbstat` |
| FTS5 index | 0.83 GB, external content (no second text copy) | `dbstat`, schema |
| All other indexes | under 0.6 GB | `dbstat` |
| Rows | 2.66 M events, 7,189 sessions, 8,559 sources | `count(*)` |
| Row anatomy | `raw_json` ≈ 3.5 KB, `text` ≈ 2.1 KB, `metadata` ≈ 0.3 KB on average | 20k random rows |
| Heavy kinds | Codex `response_item` 43%, Codex `event_msg` 20% of bytes | 40k random rows |
| Heavy tail | 0.5% of rows (> 64 KiB) hold 27% of bytes | 40k random rows |
| Opaque payload | Codex `encrypted_content` is 7.5% of `raw_json` | 5k random rows |

### Compression experiment on real data

Sample: 60 whole sessions, chosen with probability proportional to their
size, so the sample follows where the bytes are. 206,810 rows, 826 MB of
`raw_json`. Python `zstandard` 0.25, single core unless noted.

| Strategy | Ratio | Size | Time | Random access to one record |
|---|---|---|---|---|
| Per row, zstd-3, no dictionary | 2.40x | 345 MB | 3.3 s | yes |
| Per row, zstd-3, 110 KB trained dictionary | 3.08x | 269 MB | 4.2 s | yes |
| Per row, zstd-19, dictionary | 3.25x | n/a | slow | yes |
| Large-string dedup + per-item zstd-3 + dictionaries | 3.84x | 215 MB | n/a | yes, two lookups |
| ~64 KB blocks of consecutive rows, zstd-3 | 3.25x | 255 MB | 2.1 s | decode one 64 KB block |
| ~1 MB blocks of consecutive rows, zstd-3 | 4.10x | 202 MB | 1.8 s | decode one 1 MB block |
| Whole session, zstd-19 `--long=27` | 5.78x | 143 MB | 121 s (4 threads) | decode whole session |

Duplication inside the sample:

- String values of 256 bytes or more hold 78.7% of the raw bytes.
- 32.9% of the raw bytes are exact repeats of a large string seen earlier.
  Content-defined chunking (gear hash, ~2 KB chunks) finds 31.4%.
- Top repeated fields: Codex `event_msg.payload.item.aggregated_output`
  (4.6%), `summary_text` (3.5%), `formatted_output` (3.0%), Codex
  `response_item` output text (2.6%), Claude `attachment` tool schemas and
  descriptions (5.0% together), rendered attachments (2.2%).

**First-principles reading.** The data is not big; it is repetitive.
Agents re-log the same content: Codex writes most items twice
(`response_item` and `event_msg`), compaction snapshots re-copy history,
and every Claude session re-attaches the same tool schemas. Per-row storage
cannot see this redundancy. The gap between 2.4x (per row) and 5.8x (per
session) is the price of storing each record alone.

### Who has a job here

| Stakeholder | Job | Push (pain now) | Pull | Anxiety | Habit |
|---|---|---|---|---|---|
| Developer running a hub | "Keep my agents' memory without filling my disk." | 16 GB and growing ~1.7x of transcripts; recovery copies double it | small footprint, fast backup | losing originals; corruption | default SQLite file |
| Agent using `sql()` | "Find the earlier decision fast, with a citation." | broad scans over wide rows read many pages | narrow, hot tables | answers that cite stale or partial data | `SELECT ... raw_json` |
| Remote device (MacBook) | "Upload cheaply on battery and Wi-Fi." | raw records sent verbatim | compressed batches | protocol breakage across versions | v1 protocol |
| Maintainer | "Change storage without breaking invariants." | migrations copy the whole DB | sealed, immutable segments | complex state machines | one table per concept |
| Non-consumer: user on a small laptop SSD | "Try ohya without giving it 20 GB." | first import of years of history | bounded size | silent data loss | does not install |

The non-consumer matters most. Today a heavy user's first import can take
tens of gigabytes. That size is a real reason not to install.

## 3. Speculate (12-month horizon)

**Probable.** Agent transcripts keep growing. Context windows grow and
agents log more tool output. A heavy user reaches 30–50 GB of raw
transcripts within a year. Agents keep deleting or rotating their own
files, so the hub stays the only durable copy.

**Plausible.** Format-aware compressors become normal tools (OpenZL, Vortex,
Parquet Variant). Small CPU embedding models become cheap enough to run on
every laptop. LLM-based compression stays too slow for live data.

**Preferable.** The hub costs about the size of the *unique information*
in the transcripts, not the size of the logs. Search stays instant.
Originals stay exact.

Scenarios ("In 2027, when X, stakeholder will need Y, but cannot because Z"):

1. In 2027, when a heavy user has 50 GB of transcripts, the developer will
   need the hub to fit beside their code on a 256 GB laptop. Per-row
   storage makes it ~80 GB.
2. In 2027, when agents re-attach 30 KB of tool schemas to every session,
   the hub will store the same schema 10,000 times, because rows are
   stored alone.
3. When the hub schema changes, the maintainer will need a migration that
   takes minutes. A full recovery copy of a 50 GB file on a slow disk takes
   a working day (8.5 h measured for 13 GB).
4. When the MacBook uploads a month of Codex history over hotel Wi-Fi, the
   device will need to send about one fifth of the bytes. Batches are
   uncompressed.
5. When an agent asks "what did we decide about X across all devices",
   `sql()` will need to scan only text, not 6 KB rows that carry raw JSON.
6. When a user wants a backup, they will want to copy only the parts that
   changed. One 16 GB mutable file defeats incremental backup.
7. When a user asks for semantic recall ("the bug that looked like a race
   condition"), BM25 misses paraphrases. Embeddings add storage, and their
   value for this corpus is unproven.
8. When a parser improves, the maintainer will need to re-derive `text` for
   millions of rows. Derived columns stored next to originals make that a
   full rewrite.
9. When storage is cheap but I/O is not (VM disks at ~10 MB/s), the hub
   must avoid rewriting cold data. Today, idle passes still re-read every
   source's fingerprint.
10. When an agent produces a 100 KB tool output that it never looks at
    again, the hub still indexes every token of it in FTS.

Wildcards:

- **LLM compression becomes free.** Even then, decoding needs the exact
  same model and deterministic kernels forever. That conflicts with "never
  require LLMs" and with byte-exact archives.
- **Agents ship structured, deduplicated logs** (for example, Codex drops
  `event_msg` duplicates). Then format-specific dedup decays, and generic
  dedup keeps working.
- **Disk becomes the dominant cost again** (cloud VM, small SSDs). Then
  cold-tier archiving matters more than query speed.

## 4. Opportunities (clustered)

### O1. Sealed, compressed session segments (cold tier)

*What.* Keep recent and open sessions as today (hot). When a session has
been idle for N days, seal its `raw_json` into compressed blocks of about
1 MB, ordered by session and ordinal, and store them in a `segments` table.
`events.raw_json` becomes a reference: segment ID plus offset. A registered
SQL function (for example `ohya_raw(id)`) keeps `events_settled.raw_json`
working for agents.
*Evidence.* 4.10x on 1 MB blocks; 5.78x with long-range zstd-19 for
archive-grade seals. zstd is already in the binary through
`github.com/klauspost/compress` v1.20.0 (indirect today, through the
tailscale transport), so this needs no new module.
*Estimated hub size.* raw part 9.9 GB → about 2.4 GB; hub about 16.3 →
8.8 GB. With archive-grade seals, about 8.1 GB.
*Why it fits.* Sealed segments are immutable. That makes backup, sync, and
migration incremental: copy new segments only. It matches LSM and lakehouse
practice: hot rows, cold immutable files.
*Scenarios:* 1, 3, 6, 9.

### O2. Content-addressed large strings (dedup)

*What.* Split each record into a skeleton and its large string values
(≥ 256 bytes). Store each unique large string once, keyed by a 128-bit
hash, compressed with a trained dictionary. Rebuild the exact original
bytes from skeleton + blobs.
*Evidence.* 32.9% of raw bytes are exact repeats; with dictionaries the
combined ratio is 3.84x while each record stays individually addressable.
*Caveat.* Byte-exact rebuild needs the skeleton to keep the original
serialization (key order, escapes, number formats). Splitting by byte
ranges inside the original record, rather than by re-serializing, keeps
this exact. Content-defined chunking (FastCDC, VectorCDC) does this without
parsing at all: 31.4% duplicate chunks measured.
*Scenarios:* 2, 4, 10.

### O3. Per-row compression with trained dictionaries (warm tier)

*What.* Compress `raw_json` per row with zstd and a dictionary trained per
agent and record kind. This is what `sqlite-zstd` does, but in Go, inside
the existing transaction.
*Evidence.* 3.08x measured, and still a single-row decode. `sqlite-zstd`
reports up to 80% size reduction on suitable data.
*Estimated hub size.* raw part 9.9 → 3.2 GB; hub about 9.6 GB.
*Scenarios:* 1, 5.

### O4. Narrow hot table, wide cold table

*What.* Move `raw_json` out of `events` into its own table (or into O1
segments). Keep `events` narrow: IDs, time, kind, role, tool, `text`,
`metadata`. Today a scan over `events` reads raw JSON pages it does not
need, because SQLite stores whole rows, including overflow chains.
*Evidence.* 0.99 GB unused space inside `events` pages, from 4 KB pages
holding rows of 6 KB on average with overflow chains. Separating the wide
column also makes O1 and O3 local changes.
*Scenarios:* 5, 8.

### O5. Compressed sync batches

*What.* Compress upload batches with zstd and a shared dictionary,
negotiated in the protocol handshake.
*Evidence.* The same ratios as O3 apply to the wire. Devices on battery
and slow links gain the most.
*Scenarios:* 4.

### O6. Index less, index smarter

*What.* Keep originals, but stop indexing low-value bytes in FTS: tool
outputs beyond a size limit, base64 payloads, repeated tool schemas. Index
agent reasoning, user messages, and tool *inputs* fully.
*Evidence.* "The Complexity Trap" (JetBrains, NeurIPS 2025 DL4Code
workshop) found that masking old tool observations halves agent cost
without lowering solve rates. That suggests observations are low-value for
recall too. Measured 2026-11-17 (plan 004 S3): tool outputs hold 67.7% of
this hub's FTS index, so the size lever is real; whether their recall
value is low enough to drop is still a hypothesis for this corpus, to be
checked against the plan 003 R5 recall evaluation.
*Scenarios:* 10.

### O7. Optional semantic layer, measured first

*What.* An optional, off-by-default embedding index over `text` only, with
int8 or binary vectors. A 384-dimension binary vector is 48 bytes; for
2.66 M events that is about 128 MB, and int8 about 1 GB.
*Evidence against embeddings being needed.* `agent-memory-bakeoff` found
pure vector retrieval trailed BM25 by 0.16 MRR, and hybrid gained only
0.023. "Beyond Top-K" (arXiv 2608.06305) found grep and BM25 ahead of
embedding RAG on its task. One counterexample: `agentmemory` reports
BM25+vector 95.2% vs BM25 86.2% R@5 on LongMemEval-S.
*Scenarios:* 7.

### O8. LLM-based compression (rejected early, kept for the record)

*What.* Arithmetic coding with a language model (LLMZip, ts_zip, FineZip).
*Evidence.* 2–4x better than gzip in papers, but ts_zip runs at up to
1 MB/s on an RTX 4090, and LLMZip at about 280 B/s. "Waiting to
Decompress" (Kipf et al., CIDR 2026) finds LLM compression needs about 10
years of storage to pay back its compute against no compression, and about
120 years against classic compressors. Decoding also needs the exact model
and deterministic GPU kernels forever.

### O9. Columnar file formats (Vortex, Parquet Variant, OpenZL)

*What.* Export sealed segments as columnar files: Vortex (FSST strings,
100x faster random access than Parquet per its authors), Parquet with the
Variant type (shredded JSON), or OpenZL (format-aware, self-describing
frames).
*Evidence.* Strong for analytical scans over typed columns. Our payload is
mostly free text inside heterogeneous JSON; FSST gets 2–3x on strings,
below zstd blocks here. Each format is a new dependency, and most are Rust
or C++.

## 5. Pressure test

Criteria (1 = poor, 5 = strong):

- **Saves** — disk saved on this hub.
- **Fast** — query and ingest latency after the change.
- **Fits** — fits the manifesto: one binary, SQLite, no new dependency, no
  LLM, originals exact.
- **Simple** — small change, easy to model-check and to test.
- **Grows** — value increases as transcripts grow.

| Opportunity | Saves | Fast | Fits | Simple | Grows | Total |
|---|---|---|---|---|---|---|
| O1 Sealed session segments | 5 | 4 | 5 | 3 | 5 | **22** |
| O4 Narrow hot table | 2 | 5 | 5 | 4 | 4 | **20** |
| O3 Per-row dictionary zstd | 4 | 4 | 5 | 4 | 3 | **20** |
| O2 Content-addressed strings | 4 | 3 | 4 | 2 | 5 | 18 |
| O5 Compressed sync batches | 1 | 4 | 4 | 3 | 4 | 16 |
| O6 Index less | 2 | 4 | 3 | 4 | 3 | 16 |
| O7 Optional embeddings | 1 | 3 | 1 | 2 | 3 | 10 |
| O9 Columnar formats | 3 | 3 | 1 | 1 | 3 | 11 |
| O8 LLM compression | 4 | 1 | 1 | 1 | 2 | 9 |

### Kill round

**O1 Sealed segments.**
- *Why it fails:* "Idle for N days" is a guess. A resumed session appends
  to a sealed session; Claude Code and Codex both resume sessions. Sealing
  then needs an append path or a re-seal.
- *Wrong assumption:* that agents rarely read `raw_json`. If agents use
  `raw_json` in many queries, each read decodes a 1 MB block (about 1 ms
  each at typical zstd speeds), and wide scans become CPU-bound.
- *Who already does this:* LSM engines, lakehouse tables, and Git
  packfiles. This is old, proven practice; the risk is integration, not the
  idea.
- *Answer:* seal per segment, not per session. A resumed session adds new
  hot rows and a later new segment. Segments never change. Measure
  `raw_json` use from MCP query logs before choosing block size.

**O4 Narrow hot table.**
- *Why it fails:* it saves only about 1 GB by itself, and it is a schema
  migration on a 16 GB file. On a slow disk, a migration plus recovery copy
  already caused an outage (see `NOTES.md`).
- *Answer:* do it only together with O1 or O3, in one migration, and make
  the recovery point size-aware first.

**O3 Per-row dictionary zstd.**
- *Why it fails:* dictionaries are state. A lost or changed dictionary makes
  rows unreadable. Every dictionary must be immutable, versioned, stored in
  the same database, and referenced by each row.
- *Answer:* store dictionaries in a table keyed by ID; never update one.
  This is how `sqlite-zstd` works.

### JTBD check

- **Who pays:** the developer whose disk and backup hold the hub; and the
  non-consumer who will not install a tool that needs 20+ GB.
- **Their words:** "Keep everything my agents did, but don't eat my disk."
- **Painkiller or vitamin:** a painkiller on this VM (a migration copy
  took the machine down), a vitamin on a large SSD.

Survivors: **O1 + O4** as one design, with **O3** as the simpler fallback.
O2 and O5 are follow-ups that reuse the same compression code.

## 6. Make it real

### Sealed segments for original records

**One-liner.** Store cold original records in immutable, zstd-compressed,
~1 MB segments inside the hub, keep a narrow hot `events` table for search,
and rebuild exact originals on read.

**The job.** "Keep every record my agents wrote, byte for byte, at a
quarter of the size, without slowing recall."

**The wedge (first 90 days).**

1. Add an offline command, `ohya store estimate`, that measures what O1
   and O3 would save on a user's hub (read-only, like the experiment
   above).
2. Write an ADR for the segment format: block size, dictionary handling,
   checksum per block, and the `ohya_raw(id)` SQL function.
3. Model the seal step in TLA+: sealing is idempotent, a crash mid-seal
   leaves the hot rows authoritative, and a segment becomes visible only in
   the same transaction that clears the hot `raw_json`.
4. Make setup recovery points size-aware before any migration of this size.
5. Implement seal for sessions idle for 30 days, behind a flag; ship
   `ohya_raw` and keep `events_settled.raw_json` source-compatible.

**Why now.** The hub just crossed 16 GB, a migration copy on a slow disk
caused an outage, and zstd is already in the binary.

**Why us.** ohya already owns the full write path (idempotent batches,
ordinals, dedup by content), so it can seal at commit boundaries that
external tools cannot see.

**Kill criteria (stop if, after the wedge):**

- `ohya store estimate` shows under 2x on three real hubs.
- Agents read `raw_json` in more than 20% of `sql()` calls, and p95 query
  latency rises by more than 20% with sealed segments.
- The TLA+ model needs more than one new state machine.

**Go criteria:**

- At least 3x saving on real hubs, and p95 `sql()` latency within 10%.
- A crash at any point in sealing passes the fault-injection tests.

**Strongest argument against.** It adds a second storage path for the
most important data. A bug in sealing is a bug in the only durable copy of
a user's history. Byte-exact round-trip tests, block checksums, and a
verify-after-seal step are mandatory, not optional.

**What it becomes.** One storage layer for originals that serves three
uses: compact local storage, incremental backup (copy new segments only),
and compressed sync batches (O5). Content-addressed dedup (O2) can then
run inside segments, where it is cheap.

## Negative list (explored and dismissed today)

- **LLM-based compression** — too slow to pay back and needs exact models
  forever (CIDR 2026). Revisit only for a cold archive tier, and only if
  decoding without an LLM stays possible.
- **Lossy summaries instead of originals** — violates "preserve original
  transcript records". Summaries may exist only as a derived layer.
- **Dropping Codex `event_msg` or `encrypted_content`** — saves up to ~3 GB
  but loses originals. Dedup (O2) recovers most of the duplicate part
  losslessly.
- **External columnar formats as the store** — new non-Go dependencies and
  a second engine; the payload is text-heavy JSON where zstd blocks already
  win.
- **VACUUM** — no free pages; it would not help.

## Open questions

Answered 2026-11-17 by the plan 004 S3 offline analyses (read-only
scripts under `perf/`, raw tables in `perf/t102/`, summary and method
notes in `NOTES.md`):

- **What share of the FTS index comes from tool outputs?** 67.7% on this
  hub (22,386 of 64,457 sampled indexed docs; 100.6 of 137.4 MB sampled
  text). Codex `response_item` across all roles holds 59.6% alone and
  Codex `event_msg` holds zero, because all its rows carry NULL text.
  O6 has real headroom — excluding tool outputs would remove about two
  thirds of the index — but the recall cost is unmeasured and needs the
  plan 003 R5 evaluation before cutting anything.
- **Does the measured 33% duplication hold on the MacBook data?** Yes.
  Session-stream duplicates are 34.8% on the MacBook against 30.4% on
  the VM (whole-session streams; a row-uniform sample undercounts at
  18–20% because it breaks intra-session repeats). Large strings
  (≥ 256 B) carry 86.7% (MacBook) and 76.5% (VM) of raw bytes.
  Cross-device large-string repeats are ~0%, so content addressing (O2)
  has no cross-device win on this hub and per-device dictionaries lose
  nothing. Other users' hubs stay unmeasured until `ohya store
  estimate` exists (D1). Per-device ratios: VM per-row 2.26x, 64 KB
  blocks 3.01x, 1 MB blocks 3.76x; MacBook 2.20x / 2.82x / 3.28x. The
  per-row dictionary ratio measured on disjoint data is 2.47–2.61x, not
  the 3.08x this document measured on the dictionary's own training
  sample.
- **What is the decode cost of `klauspost/compress` zstd per 64 KB and
  per 1 MB block?** On this VM (i9-12900HK, single goroutine, library
  v1.20.0): 64 KB block 123 µs without a dictionary (759 MB/s), 158 µs
  with one (590 MB/s); 1 MB block 1.33 ms without (874 MB/s), 1.72 ms
  with (676 MB/s). A dictionary costs about 25% of decode speed. The
  "about 1 ms per 1 MB block" assumption behind O1 was right.

Still open:

- How often do agents read `raw_json` through `sql()`? This sets the
  block size and whether O1 or O3 wins. Blocked on the plan 002
  acceptance gate, not on offline analysis.
- Can the segment format double as the sync batch format without a
  protocol version bump?

## Sources

Checked 2026-11-16.

- [phiresky/sqlite-zstd: transparent dictionary-based row-level compression for SQLite](https://github.com/phiresky/sqlite-zstd) and the [design post](https://phiresky.github.io/blog/2022/sqlite-zstd/)
- [mlin/sqlite_zstd_vfs: page-level zstd VFS for SQLite](https://github.com/mlin/sqlite_zstd_vfs)
- [SQLite FTS5: external content, contentless, `detail` option](https://sqlite.org/fts5.html)
- [Kipf et al., "Waiting to Decompress: The Economics of LLM-Based Compression", CIDR 2026](https://www.vldb.org/cidrdb/papers/2026/p34-kipf.pdf)
- [Delétang et al., "Language Modeling Is Compression", ICLR 2024](https://arxiv.org/abs/2309.10668)
- [Bellard, ts_zip](https://bellard.org/ts_zip/)
- [FineZip, arXiv 2409.17141](https://arxiv.org/pdf/2409.17141)
- [Meta, "Introducing OpenZL", Oct 2025](https://engineering.fb.com/2025/10/06/developer-tools/openzl-open-source-format-aware-compression-framework/) and [OpenZL paper, arXiv 2605.09928](https://arxiv.org/pdf/2605.09928)
- [DuckDB, "Announcing Vortex Support in DuckDB", Jan 2026](https://duckdb.org/2026/01/23/duckdb-vortex-extension) and [Vortex BtrBlocks compressor](https://vortex.dev/blog/btrblocks-compressor)
- [DuckDB VARIANT type](https://duckdb.org/docs/2.0/sql/data_types/variant) and [Iceberg v3 Variant shredding](https://datalakehousehub.com/blog/iceberg-v3-variant-shredding-s3-tables/)
- [VectorCDC, USENIX FAST 2025](https://www.usenix.org/system/files/fast25-udayashankar.pdf), [SeqCDC, Middleware 2024](https://cs.uwaterloo.ca/~alkiswan/papers/SeqCDC_Middleware24.pdf), [CDC survey, arXiv 2409.06066](https://arxiv.org/pdf/2409.06066)
- [Lindenbauer et al., "The Complexity Trap", arXiv 2508.21433](https://arxiv.org/html/2508.21433) and [JetBrains Research blog](https://blog.jetbrains.com/research/2025/12/efficient-context-management/)
- [agent-memory-bakeoff: BM25 vs vector vs hybrid](https://github.com/JaysonRawlins/agent-memory-bakeoff)
- ["Beyond Top-K", arXiv 2608.06305](https://arxiv.org/pdf/2608.06305)
- [agentmemory LongMemEval-S comparison](https://github.com/rohitg00/agentmemory/blob/main/benchmark/COMPARISON.md)
- [Vespa, "Embedding Tradeoffs, Quantified"](https://blog.vespa.ai/embedding-tradeoffs-quantified/) and [HAKARI-Bench, arXiv 2606.22778](https://arxiv.org/pdf/2606.22778)
