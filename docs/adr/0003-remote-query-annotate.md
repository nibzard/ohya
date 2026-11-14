# 0003: Remote query and annotate ride the sync connection

Status: accepted, 2026-11-13 (T036).

## Context

Paired devices upload their sources to the hub, but the shared memory was
one-way: `sql(query)` and `annotate(operations)` ran only over stdio
against a local hub database (`ohya mcp`). PRODUCT.md expects "a local
stdio MCP bridge connects coding agents to the remote hub through Tailcat",
and ARCHITECTURE.md fixes the MVP rule: all paired devices may read all
history and upload only their own sources. tla/Auth.tla anticipated remote
reads — "reads carry no ownership state to check" — and its per-request
`Authorized` guard was written to cover them.

Two surfaces had to be connected: the sync endpoint, which already owns
authenticated device connections, per-frame registry checks and revocation
teardown (T017, T018), and the query engine, which already owns the
read-only policy and every bound (T008, T009).

## Decision

Query and annotate are protocol frames on the existing sync connection,
served by the endpoint through a `RemoteEngine` interface that the
`internal/mcpserver` engine implements.

- A query frame carries one SQL statement (64 KiB wire bound, the sql
  tool's input limit). The reply frames the engine's bounded result —
  columns, rows, row count, explicit truncation — exactly as the stdio
  tool shapes it, as JSON within a 4 MiB wire bound.
- An annotate frame carries the tool's JSON arguments (1 MiB wire bound).
  The reply carries the committed applied/unchanged counts. The frame has
  no writer field: the hub derives `writer_id` from the authenticated
  device, the same rule the stdio tool applies with the local Device ID.
- The endpoint's existing per-frame `Authorize` check and revocation
  teardown gate both frames exactly as they gate registrations and batches;
  a revocation mid-query cancels the statement through the request context
  and closes the connection before any result is delivered.
- An endpoint constructed without a `RemoteEngine` stays upload-only and
  answers these frames with `CodeNoRemote`; the connection stays usable.
- `hubsync.RemoteClient` is the device side: one authenticated connection,
  sequential exchanges, mirroring the uploader's discipline.

The implementation is a library, built and tested ahead of deployment the
way `internal/hubsync` and `internal/uploader` preceded T033/T034. The
serving command attaches the engine when it wires the endpoint; the
device-side stdio MCP bridge over `RemoteClient` ships with the joining
command. Both ride T034.

## Rejected alternatives

- A separate MCP-over-Tailcat bridge service: a second listener, a second
  authentication path, and a second place where bounds could drift from the
  stdio tools. The endpoint already holds everything authorization needs.
- Proxying the MCP protocol itself over the transport: the wire would
  inherit the SDK's protocol versioning, and the hub would re-parse tool
  calls it can answer with two small frames.
- A dedicated query port: more configuration for operators, no isolation
  gain — the registry and the policy are identical either way.

## Consequences

`internal/hubsync` imports `internal/mcpserver` (the sync protocol now
carries the remote MCP surface, and the engine is the one authority on
bounds and policy — DRY); `mcpserver` stays free of sync concerns, and no
new external dependency is introduced. Deterministic annotate rejections
carry sentinels (`mcpserver.ErrAnnotateRejected`,
`store.ErrAnnotationRejected`) so the endpoint can classify them as
`CodeRejected` while transient write failures answer `CodeInternal`. A hub
engine configured past the 4 MiB result wire bound produces an explicit
internal error instead of an undeliverable frame; the engine's default
output bound (256 KiB) fits sixteen times over. Offline devices still have
no query replica: shared-history queries require a reachable hub
(PRODUCT.md).
