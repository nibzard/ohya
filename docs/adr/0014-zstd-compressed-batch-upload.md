# 0014: zstd-compressed batch upload frames

Status: accepted, 2026-11-19 (T145).

## Context

The first join of a device with real history is the one sync operation whose
duration is set by the network, not the disk. The measured case
(NOTES.md, 2026-11-16 Tailcat spike): a MacBook holding 4.46 GB of raw
records joined over the relay-only path (no direct UDP path through the
double NAT) at 3.1–3.3 Mbit/s, so the first backfill took about three hours.
On-record gzip shrinks a 20,000-record sample only 1.9 times. The offline
storage analysis on the deployed hub
([plan 004](../../plans/004-storage-efficiency.md), S3) measured zstd at
2.4x per row, 4.1x for 1 MB session blocks, and 5.8x for whole sessions —
and a 64 MiB batch of contiguous records from one source resembles a session
block, because transcripts repeat their JSON scaffolding record to record.

Operator decision (2026-11-19, plan 004 item O5): compress the device-to-hub
first backfill with zstd. The candidate codecs were zstd through
`github.com/klauspost/compress` (already carried in the module graph through
`tailscale.com`, promoted to a direct dependency by T102) and stdlib flate
(no new dependency, but the same 1.9x class as gzip — insufficient for a
three-hour backfill, and it saves a dependency the build already carries).

## Decision

**Compress device-to-hub batch frames with zstd at level 3, negotiated per
connection, with plain frames as the universal fallback.** Only the batch
payload is compressed in protocol version 1: registrations, cursor probes,
acks, pairing, and the remote query and annotate frames stay exactly as they
are.

### Framing

A compressed batch is a `Z` frame: the plain batch header (device, source,
byte range, record count) in the clear, then the uncompressed length of the
record section, the compressed length, and one zstd stream over exactly the
bytes a plain batch carries after its header — `count` times
`offset(8) len(4) json`. The declared uncompressed length is the decompressor's
allocation bound; see "Decompression discipline" below. The writer falls back
to the plain `B` frame whenever compression does not win (incompressible
payload, or a batch with no records, which is a cursor probe and has nothing
to compress), so a compressed frame on the wire always carries a real
reduction.

`github.com/klauspost/compress` v1.20.0 stays pinned as the direct dependency
T102 introduced; with this ADR the import is production-facing (ADR 0001
required its own ADR for exactly that step). The encoder is created lazily
per process at level 3 (`EncoderLevelFromZstd(3)`, the library's default
speed class) with the frame CRC enabled, so corruption is detected at decode;
the level is a starting point, and T146 tunes it against measurement.

### Negotiation, and why it is not a hello extension

`ProtocolVersion` checks are strict equality on every frame, and every
reader consumes exactly the bytes its frame declares. That rules out the two
obvious in-band tricks:

- Appending capability bytes to the existing `hello` frame: an old hub reads
  the credential, authenticates, and then misframes the trailing bytes as the
  first request frame — silent corruption instead of a clean refusal.
- A new auth-frame marker or a version bump on the hello: the old hub's
  refusal arrives as `CodeAuth` (all authenticate failures do), which is
  indistinguishable from a refused credential, so every rejected credential
  would cost a second connection to diagnose.

Instead, the capability indication rides **after** the unchanged hello, as
the connection's first request frame: a `V` frame carrying a 16-bit
capability mask, answered by a `v` welcome frame carrying the mask the hub
will honor (bit 0: zstd batch compression). An old hub answers `V` the way
it answers any unknown marker — `CodeMalformed` and close — which is
distinguishable from `CodeAuth`, so:

- a bad credential still costs exactly one round trip, exactly as before;
- a new device meeting an old hub spends one refused connection, marks the
  hub plain for the life of the process (the same lifetime rule as an
  unsupported workspace exchange), reconnects, and uploads plain frames;
- an old device never changes behavior at all: it sends no `V` frame, and a
  new hub answers its plain hello, registrations, probes, and `B` batches
  exactly as before. Every existing endpoint test is this case.

A new hub accepts a `Z` frame on any connection, not only a negotiated one:
the frame is self-describing and bounded per frame, so the read path keeps
no per-connection capability state. Negotiation exists for the device's
benefit (do not send what the hub refuses); the hub's defense is per-frame
bounds, not negotiation memory.

### The hub-first upgrade window

Because only a new hub can answer `V` and only a new device asks, compression
engages exactly when both ends run this change. The supported upgrade order
is therefore **hub first**: upgrade the hub, then devices at any pace. A new
device against an old hub falls back to plain frames after one refused
capability frame per process; an old device against a new hub never notices
the new frames. There is no flag, no configuration, and no version bump to
coordinate — and no mixed pair ever exchanges a compressed frame.

### Decompression discipline

The hub treats a compressed batch as untrusted input, exactly like a plain
one, and every limit is enforced before or at a capped allocation:

- The plain header validates the same fields a plain batch validates
  (non-negative range, end ≥ start, span ≤ 64 MiB, count ≤ 65,536) before
  anything is allocated.
- The declared uncompressed length is refused unless it fits the maximum
  plain record section: 64 MiB of records plus 12 framing bytes per record,
  given the already-validated count. The compressed payload length is
  refused above the 64 MiB batch bound — an honest writer never approaches
  it, because it sends plain when compression does not shrink.
- Decompression allocates exactly the declared length and decodes with the
  decoder's cap limit enabled (`WithDecodeAllCapLimit`): a payload that
  expands past its declared length fails with a deterministic error, no
  matter how large its expansion would be. A zstd bomb therefore costs at
  most the declared bound — the same 64 MiB class a plain batch can already
  claim — never a growing buffer.
- The declared and decoded lengths must match, and the decoded record
  section is then parsed by the same reader as a plain batch: per-record
  32 MiB bound, 64 MiB total, exact consumption with no trailing bytes.
  A record that parses to anything but exactly one record at its framed
  boundaries is rejected, as for a plain batch.

Every failure above is a deterministic protocol error — `CodeTooLarge` for
the bound refusals and overlong expansion, `CodeMalformed` for corrupt
streams, length mismatch, or trailing bytes — and closes the connection. A
crash, an unbounded allocation, or a hang is not an accepted outcome, and
the frame reader is fuzzed with compressed batches as seeds.

### State machine

A capability flag changes no batch or acknowledgment behavior. A compressed
batch is the same `SendBatch` action with a different encoding: it still
starts at the durable checkpoint, one batch per source is still in flight,
only an acknowledgment moves the cursor, and a lost acknowledgment still
replays the same bytes (re-encoded, possibly differently). The negotiation
adds one request-reply pair at connection setup, before any batch exists,
and its failure modes degrade to the plain protocol rather than to a new
sync state. `tla/Sync.tla` therefore does not change, and
[tla/README.md](../../tla/README.md) records that frame encoding is
deliberately outside the model; the model must be revisited only if a future
capability changes the batch/ack state machine itself (for example, multiple
batches in flight).

## Alternatives rejected

- **Stdlib flate.** No dependency, but the on-record measurement class is
  1.9x; over a 3.1 Mbit/s relay that still leaves a multi-hour first
  backfill. The zstd dependency is already in the module graph and pinned.
- **Bumping `ProtocolVersion` for compressed frames.** Strict-equality
  version checks mean an old hub closes with no fallback path, and the
  protocol forks into two dialects. Capability negotiation at version 1
  keeps one wire format with a graceful degradation path.
- **Extending the hello frame** (trailing bytes or a new auth marker):
  rejected for the framing and error-code reasons in "Negotiation" above.
- **Per-record compression.** Loses the cross-record redundancy that makes
  session blocks compress 4.1x where rows compress 2.4x, and adds a length
  prefix per record for nothing.
- **A shared dictionary** (trained per agent): deferred with the plan 004
  D3 items; T146 evaluates it and adopts one only on a material measured
  gain, because a dictionary adds hub-side versioning and pairing logistics
  that plain zstd does not need.

## Tests

Round-trip and plain-fallback (`TestCompressedBatchRoundTrip`), every-prefix
truncation, corrupt and truncated zstd streams, declared-length mismatch,
expansion past the declared length, oversized declared and compressed
lengths, and trailing record-section bytes
(`TestReadFrameRejectsBrokenCompressedBatches`), capability and welcome
round-trips including the error frame and zero-mask refusals
(`TestCapabilityFrames`), the welcome intersection at the endpoint
(`TestCapabilityNegotiationAnswersWelcome`), compressed batches committing
byte-identically to plain ones through the real endpoint
(`TestCompressedBatchCommitsLikePlain`), the uploader negotiating compression
against a real hub with wire bytes below payload bytes
(`TestCompressedUploadNegotiated`), the mixed-version fallback against a
scripted old hub in both directions — new device falling back to plain
frames for the process (`TestOldHubFallsBackToPlainFrames`), old device on a
new hub being the existing plain endpoint tests — and the end-to-end serve,
join, and remote-MCP suites over real processes. `FuzzReadFrame` gained a
compressed-batch seed and a capability-frame seed.

## Consequences

The first backfill over a relay-only path drops by roughly the batch
compression ratio on real transcripts (the 4.1x class on session-shaped
blocks), which turns a three-hour join into well under an hour at the same
link speed; steady-state traffic compresses identically, and its cost is
level-3 encode on the device and one bounded decode per batch on the hub.
The protocol gains its first negotiated capability with a pattern later
features can reuse (offer in `V`, intersection in `v`, plain fallback), and
the dependency surface grows by zero modules. T146 measures the achieved
ratio and CPU cost on real corpora and decides the dictionary question.

T146 outcome (2026-11-20, numbers in
[docs/performance.md](../performance.md) "Compressed backfill"): level 3
stays — the higher speed classes buy 6.8% (`better`) and 11.6% (`best`,
an upper bound from per-session sections) byte-weighted wire reduction on
real records for 1.7–7x the encode cost, not material for a once-per-device
join — and the shared dictionary is rejected: 0.1–5.3% additional reduction
on real record distributions against the over-25% adoption threshold, not
worth the hub-side versioning and pairing logistics named below.
