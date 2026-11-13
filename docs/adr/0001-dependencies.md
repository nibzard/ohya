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
