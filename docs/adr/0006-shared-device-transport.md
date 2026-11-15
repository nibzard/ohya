# 0006: Share one transport node per device state

Status: accepted, 2026-11-15.

## Context

`join` and each `mcp --remote` process used to start their own Tailcat node.
They loaded the same enrolled identity from `<state>/tailcat`. Live tests
showed that remote queries worked while the uploader stopped making progress.
An append stayed pending for 90 seconds. Restarting the uploader recovered it.
Two network stacks cannot independently own the same transport identity.

## Decision

Use `tailcat.NewSharedDevice` for both commands. Keep transport sharing inside
`internal/transport/tailcat`; callers still receive ordinary `net.Conn` values.

The first dialing process acquires an exclusive, nonblocking `flock` on
`<state>/tailcat/owner.lock`. Only that process starts the enrolled node.
The lock file stays in place. Removing it would let processes lock different
files and start competing nodes.

After a successful tailnet dial, the owner starts tsnet's existing loopback
SOCKS5 proxy. It writes the address, proxy password, hostname, coordination
URL, and descriptor version to `proxy.json`. The atomic write creates a
file with mode 0600. No LocalAPI credential is shared. Other processes dial
through this proxy with username `tsnet` and its random password. The existing
`golang.org/x/net/proxy` package supplies the SOCKS5 client; its pinned module
becomes a direct dependency. No new module is added.

Readers bound the descriptor to 4096 bytes. They reject public file permissions,
unsupported versions, non-loopback addresses, invalid passwords, and settings
that differ from the owner. The hostname and coordination URL must agree.
Each caller still chooses its hub address and sends its own pairing credential.
Sharing transport does not authenticate a hub request or assign an annotation
writer. The hub retains its per-request authorization and revocation checks.

The owner publishes the proxy only after the node can reach the tailnet.
Borrowers wait for publication within the caller's context. Shutdown removes
the descriptor, closes the node, and then closes the lock file. A borrower
never closes another process's node or removes its descriptor.

A long-lived node holds transport state that outlives a hub process restart
(T070 workload 4): after the coordination plane inside `ohya serve` restarts,
the device node's control connection never re-polls and its cached peer
endpoints stop matching the restarted hub, so every dial stalls until the
caller's deadline and the device never uploads again. The owner therefore
treats "a dial failed while the coordination plane still answers" as proof of
stale node state: it closes the node, starts the same durable identity again,
republishes the proxy descriptor, and retries the dial once. The direct dial
carries its own deadline below the caller's, so this recovery fits inside one
caller budget. A dial failure with an unreachable plane is ordinary hub
downtime and only backs off; the node stays. An idle owner that never dials
is not rebuilt by its borrowers — the borrower contract stays "wait, retry,
or take over after the owner exits".

On process death, the operating system closes its sockets and releases its
lock. A borrower can acquire that lock on its next dial, discard the stale
descriptor, and start the same durable identity. Existing sync and remote
session retries reconnect through the new owner. Queries remain available
without a running uploader. Several MCP processes may share the same node.

## Alternatives

- Make `join` the only owner and require it for remote queries. This changes
  the standalone MCP contract and adds a separate local forwarding service.
- Enroll a different node for each MCP process. This requires new admission
  credentials and lifecycle rules for identities that do not represent Devices.
- Copy transport state into separate directories. Each copy still starts the
  same identity and retains the original conflict.

## Verification and limits

`TransportOwner.tla` checks exclusive ownership, publication, shutdown, crashes,
and multiple borrowers. Its assumptions are reliable local filesystem locks,
protected state, and complete node shutdown before lock release. It checks
safety; it does not prove connection availability or tsnet internals.

Go tests cover concurrent startup, owner death, ownership transfer, descriptor
permissions, settings mismatch, and bounded startup waits. The CLI regression
test appends records while two MCP clients are connected, kills the owner,
restarts the uploader, and checks remote queries and revocation again.
Another CLI regression test stops and restarts serve with an established
join and asserts the device re-uploads within a bounded window, and unit
tests pin the rebuild decision: a failing dial rebuilds only while the
coordination plane answers.

The lock requires a local filesystem with working `flock` semantics on the
supported Linux and macOS platforms. Do not share one device state directory
between machines. State already in use by an older binary has no ownership
lock. Stop old `join` and remote MCP processes before using this version.
