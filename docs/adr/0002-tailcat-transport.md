# 0002: Embedded Tailcat transport

Status: selected for the sync layer, 2026-11-13, after the T014 feasibility
spike. Feasibility is validated for pairing and encrypted exchange on Linux
against a self-hosted control plane. The production control-plane choice and
forced-relay behavior remain open and are recorded below.

## Decision

Use embedded Tailcat — the `tailscale.com/tsnet` node embedded in the
process, pinned at `tailscale.com v1.104.4` — as the device-to-hub
transport, isolated behind the `internal/transport` adapter interfaces
(`Hub`, `Device`). The spike harness is `cmd/tailcat-spike`; the raw results
and provenance live with the T014 run artifacts. The interfaces return
plain `net` values, so the sync layer (hub endpoint, uploader) never sees a
transport-specific type, and the implementation can be replaced without
touching synchronization code.

Tailcat needs a coordination server. The spike ran Headscale v0.29.7 as a
local Docker container with its embedded DERP relay, single-use preauth
keys and MagicDNS, which satisfies PRODUCT.md's local-first, no-cloud-account
position. The vendor control plane is the alternative; choosing it for
production is an operator decision (see Consequences).

## What the spike validated

Two separate OS processes — a hub node listening on tailnet port 9027 and a
device node dialing it — exchanged framed batch/acknowledgment messages
(sha256-verified payloads, advancing cursor) over WireGuard-encrypted
transit, 111,514,624 bytes in the recorded run, zero errors.

| Measurement (loopback, 100 exchanges each) | Tailcat | TCP baseline |
|---|---|---|
| Connect after process start | 9.0 s by IP; 14–15 s by hostname | < 1 ms |
| 1 KiB round trip, p50 / p95 | 179–186 µs / 355–384 µs | 23 µs / 31 µs |
| 64 KiB round trip, p50 | 677–752 µs (88 MiB/s) | 96 µs (543 MiB/s) |
| 1 MiB round trip, p50 | 3.6–4.0 ms (232–258 MiB/s) | 1.1 ms (900 MiB/s) |
| Node startup, cold / warm | 5.1 s / 3.7 s | — |
| Process RSS | 37–41 MiB | 24 MiB |
| Node state on disk | 2.4 KiB | — |

Pairing (a PRODUCT.md release requirement): Headscale single-use preauth
keys model the one-time expiring pairing invitations of ARCHITECTURE.md.
Enrollment with an already-consumed key is rejected in 82 ms with the
server's reason surfaced through the adapter. Peer authentication works:
the hub resolves each accepted connection to the caller's tailnet identity
(`WhoIs`), which is the transport-level device identity the authorization
model can build on. Ephemeral registration is unsuitable for ohya devices:
the coordination server expires an ephemeral node at disconnect, orphaning
its local state, so the next start needs a new pairing key. Nodes are
therefore durable; restarts reuse the enrolled identity and consume no key.

## Tradeoffs against alternatives

- Plain TLS over TCP: fastest and dependency-free, but solves neither
  identity, roaming, NAT traversal nor key distribution; every device would
  need a reachable hub address and manual credential management.
- Manually configured WireGuard: same encryption without coordination, but
  no pairing story and no relay fallback; operationally brittle for the
  intended "install once, join with a code" experience.
- libp2p: comparable feature set with a larger, less predictable dependency
  surface for this use case.
- Copied-roots ingestion (BACKLOG.md item 6, task T026): no network at all;
  remains the fallback if the control-plane requirement is rejected.

Tailcat's cost is the dependency tree and the coordination server. The
measured latency floor — roughly 7× TCP at small payloads, under 4× at
1 MiB, still over 200 MiB/s — is far inside the sync layer's needs; batches
are kilobytes to low megabytes and the sync contract already bounds them at
8 MiB.

## Packaging

- `tailscale.com v1.104.4` is a direct dependency, imported only by
  `internal/transport/tailcat`. `cmd/ohya` does not import it: the shipped
  binary is unchanged at 16.8 MB with zero tailscale modules linked. A
  binary that imports the transport grows to about 33 MB, and each node
  process costs roughly 15 MiB RSS over a plain socket program.
- The library is pure Go (userspace network stack); it adds no CGO beyond
  the existing SQLite driver.
- Adopting it forces `go 1.27.3` or later in go.mod — a hard gate; Go 1.25
  refuses to build the package. GOTOOLCHAIN=auto fetches a suitable
  toolchain, and CI reads the version from go.mod. go.sum grows by about
  240 lines.
- One process per node, one node state directory per process. Concurrent
  processes sharing a `StateDir` silently share one node identity — the hub
  must guard against double starts, as it already must for the database
  writer.

## Operational limits and open items

- Connection setup after process start takes seconds (9–15 s in the spike;
  path discovery dominates, and the run had no published STUN, so peers
  found each other through the embedded DERP before going direct). A
  long-running hub and warm device nodes make this a startup cost, not a
  per-batch cost; steady-state round trips are sub-millisecond at
  small payloads.
- Relay requirements are only partially validated: the embedded DERP
  server was reachable (probe returned 200) and bootstrapped discovery, but
  the spike could not force a relay-only path on a single host, so
  relay-path latency and degradation are unmeasured. Validating that needs
  two hosts with direct paths blocked.
- The macOS behavior of the pinned library (signing, notarization, network
  extensions) is unverified, as is all of T021's distribution work.
- Revocation semantics on the chosen control plane (node deletion while
  connections are open) belong to the pairing implementation (T018), which
  the TLA+ model (T016) precedes.

## Fallback

If the control-plane requirement is later rejected — for example because
self-hosting Headscale is out of scope and the vendor tailnet is
unacceptable — the recorded fallback is copied-roots ingestion (T026) for
multi-machine capture, with the transport adapter left in place for a
future decision. This ADR then records the rejection and the reasons, per
the task contract; no sync implementation precedes that record.
