# 0001: SQLite driver and MCP SDK

Status: selected for bootstrap, 2026-11-13.

Use `github.com/mattn/go-sqlite3` v1.14.53 with Go 1.25 or later and
`sqlite_fts5`. Its bundled SQLite avoids a runtime SQLite installation; it
requires CGO and a C compiler, including Xcode Command Line Tools on macOS.
Build and test each target architecture with an appropriate C toolchain; a Go
cross-compile alone is insufficient. Shipping signed/notarized macOS binaries
and cross-architecture packaging remains unverified. See the driver's
[installation and build tags](https://github.com/mattn/go-sqlite3/tree/v1.14.53#installation)
and [FTS5 tag](https://github.com/mattn/go-sqlite3/blob/v1.14.53/sqlite3_opt_fts5.go).
The driver exposes a per-connection [authorizer](https://github.com/mattn/go-sqlite3/blob/v1.14.53/sqlite3.go)
and interrupts active statements on context cancellation in that same source.
Integration tests exercise FTS5, denial and cancellation with this pinned driver.
These capabilities alone do not implement the planned SQL security boundary;
connection configuration, allowed operations and limits still need testing.
CGO is the tradeoff for a mature SQLite binding with these capabilities.

Select the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
for the MCP implementation, initially targeting v1.8.0. It is maintained with
Google and supports servers and clients. Version 1.8.0 declares Go 1.25. Pin it in go.mod when implementation
imports it, after checking protocol compatibility; an unused
SDK is not a runtime dependency. Tailcat selection and packaging remain deferred
until its feasibility spike. No transport or MCP code is included in bootstrap.

Pinned 2026-11-13 (T008): `github.com/modelcontextprotocol/go-sdk v1.8.0` is a
direct dependency, imported by `internal/mcpserver`. Protocol compatibility
verified before pinning: the SDK supports versions 2024-11-05 through
2026-07-28, clients pinned to 2024-11-05, 2025-03-26 and 2025-06-18 (what
Claude Code negotiates) complete the handshake and call the tool
(`TestServerProtocolCompatibility`), and the full stdio path runs against a
real subprocess in `cmd/ohya`. The SDK pulls the transitive dependencies
listed in go.mod (jsonschema-go for tool schemas, oauth2 and jwt for its auth
package, and related support libraries); only `mcp` and `jsonrpc` are imported
by ohya code.

Pinned 2026-11-16 (T121): `github.com/tailscale/tailcat v0.7.0` (BSD-3-Clause)
is a direct dependency, imported by `internal/transport/address`
([ADR 0011](0011-tailcat-address-transport.md)). `tailscale.com` stays at
v1.104.4; the 2026-11-16 spike verified that Tailcat builds and its test suite
passes with that pin, and the dependency graph only reuses versions go.mod
already carried. Tailcat v0.x promises no API, command-line, or wire-format
stability, so ohya pins one exact version and upgrades it only in a release
that states the change; a Tailcat wire change must bump the sync
`ProtocolVersion`. The shared `tailscale.com` pin also covers the test-only
`derp/derpserver` import that runs a local relay without internet access.

Removed 2026-11-16 (T123): `github.com/juanfont/headscale` v0.29.7, the
embedded coordination server behind the superseded tsnet transport
([ADR 0011](0011-tailcat-address-transport.md), "What is removed"), and
with it every transitive dependency only it carried. Direct dependencies
shrank to five. The linux/amd64 release binary, built with the
`scripts/package.sh` flags, fell from 72,148,584 to 49,350,600 bytes
(see [docs/performance.md](../performance.md), "Release binary size").

Promoted 2026-11-17 (T102): `github.com/klauspost/compress` v1.20.0
moved from indirect (carried through `tailscale.com`) to direct. The
offline storage analysis programs under `perf/`
([plan 004](../../plans/004-storage-efficiency.md) S3) import its zstd
encoder and decoder to measure compression ratios and decode cost on a
real hub; no version changed and no release binary imports it outside
the existing module graph. A storage design that adopts zstd at rest
will make the import production-facing and needs its own ADR.

Production-facing since 2026-11-19 (T145): the sync protocol imports the
zstd encoder and decoder for compressed batch upload frames
([ADR 0014](0014-zstd-compressed-batch-upload.md)); the version stays
pinned at v1.20.0 and the approval, bounds, and mixed-version rules are
recorded there.
