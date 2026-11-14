# 0002: Embedded Tailcat transport

Status: selected for the sync layer, 2026-11-13, after the T014 feasibility
spike; the production control plane was decided 2026-11-14 (below). Feasibility
is validated for pairing and encrypted exchange on Linux against a
self-hosted control plane, and the self-hosted plane now ships embedded in
`ohya serve`. Forced-relay (DERP-only) behavior is validated 2026-11-14
between isolated network peers (T032, below).

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

### Control-plane decision (2026-11-14)

The production control plane is self-hosted coordination bundled with and
managed by the `ohya serve` command: the Headscale control server
(`github.com/juanfont/headscale` v0.29.7) runs in-process out of
`internal/control` — pinned as the tested pair with the tailscale client
library, which pins the exact same `tailscale.com v1.104.4` — with its own
SQLite database, a generated configuration, and the embedded DERP relay. No
external service runs in the control or data path, and there is no second
process or admin surface to operate: serve provisions the plane under the
state directory (`control/`) on first start and regenerates the
configuration from its flags on every start.

Device onboarding stays one join command plus one one-time code from the
T018 pairing registry: `ohya devices invite` mints the registry invitation
and a single-use coordinator preauth key together and prints them as one
join code; `ohya join` spends the preauth key to enroll its transport node
and the invitation to pair its identity over the tailnet. Each half is
consumable exactly once within the invitation's lifetime. A revocation in
the registry ends the device's sync sessions; deleting the coordinator node
is transport hygiene on top, not the authorization boundary.

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

### Forced-relay validation (T032, the PRODUCT.md relay release gate)

The relay-only path was measured between two isolated Docker networks:
hub and device containers on separate bridges with every packet between
them dropped, so a direct path (UDP or otherwise) is impossible by
construction. Both endpoints reported the peer as reachable only through
the embedded DERP relay for the whole run — the spike records the peer's
relay name and direct endpoint from the node's live status on each side.
The direct reference ran the same binary, flags, and control plane with
both containers on one bridge, where a direct path came up. Raw results
and the topology evidence live with the T032 run artifacts.

| Measurement (100 exchanges each, p50 / p95) | Forced relay | Direct (same day) |
|---|---|---|
| 1 KiB round trip | 260 / 378 µs | 124 / 185 µs |
| 64 KiB round trip | 1,244 / 1,884 µs | 676 / 1,031 µs |
| 1 MiB round trip | 14,215 / 16,789 µs | 3,561 / 7,217 µs |
| 1 MiB throughput | 69.5 MiB/s | 251.3 MiB/s |
| Dial after process start (warm) | 4.0 s | 4.6 s |

Degradation is 1.8–2.1× at the kilobyte sizes sync batches actually use
and about 4× at 1 MiB; the relay still moves 69.5 MiB/s, far inside the
sync contract's 8 MiB batch bound. Peer authentication, digest-verified
commits, and durable restarts behave identically on the relay path.

Two beliefs were corrected by the same runs. First, a plain-HTTP control
plane carries no relay traffic at all: the tsnet client upgrades its DERP
connection to TLS unconditionally unless the vendor debug switch
`TS_DEBUG_USE_DERP_HTTP` is set, and without a relay the isolated peers
never connected (the first attempt timed out after two minutes with no
DERP connection). The measured relay leg therefore ran over plain HTTP
behind that switch — steady-state cost is the extra network hop; TLS adds
record overhead and a per-connection handshake in production. Second, the
T014 single-host peers never used DERP to find each other: they discovered
endpoints through the control-plane map exchange and went straight to a
local direct path, so DERP had carried no data before this validation.

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
  `internal/transport/tailcat`. Since the serving and joining commands
  landed (T034), `cmd/ohya` links it: the binary that also embeds the
  coordination server (`github.com/juanfont/headscale` v0.29.7, imported
  only by `internal/control`) grows well past the spike's 33 MB, and each
  node process costs roughly 15 MiB RSS over a plain socket program.
- Both libraries are pure Go (userspace network stack; headscale's SQLite
  runs on the glebarez pure-Go driver); they add no CGO beyond the existing
  mattn SQLite driver.
- Adopting them forces `go 1.27.3` or later in go.mod — a hard gate; Go 1.25
  refuses to build the package. GOTOOLCHAIN=auto fetches a suitable
  toolchain, and CI reads the version from go.mod. go.sum grows by about
  240 lines for tailscale plus the headscale tree.
- One process per node, one node state directory per process. Concurrent
  processes sharing a `StateDir` silently share one node identity — the hub
  guards double starts through the coordination listener, as it already
  must for the database writer. The coordination database tolerates a
  second same-version process (`ohya devices invite`) through the driver's
  busy timeout.

## Operational limits and open items

- Connection setup after process start takes seconds (9–15 s cold in the
  T014 spike; warm restarts connected in about 4 s in both the T032 relay
  and direct runs). Path discovery dominates, and the T014 run had no
  published STUN, so peers discovered endpoints through the control-plane
  map exchange and connected directly over local addresses. A long-running
  hub and warm device nodes make this a startup cost, not a per-batch cost;
  steady-state round trips are sub-millisecond at small payloads.
- `ohya serve` binds the coordination plane, its STUN helper, and the
  embedded DERP to loopback by default, which serves the single-machine
  deployment. A hub reachable from other machines overrides
  `-control-listen` (and passes `-control-url` with the address devices
  use), `-stun-listen`, and `-tailnet-port`. TLS for the coordination
  endpoint is not wired into serve's flags yet; the plane speaks plain HTTP
  on the operator-provided address, so cross-machine deployments should
  front it accordingly until that gap closes. The T032 validation made the
  consequence concrete: over plain HTTP the tsnet client never connects to
  the embedded DERP (derphttp upgrades to TLS unconditionally), so a device
  with no direct path to the hub has no fallback at all. The validation
  itself needed the vendor debug switch `TS_DEBUG_USE_DERP_HTTP`, which is
  not an acceptable production posture; cross-machine relay support
  therefore rides on closing the TLS gap.
- Relay requirements are validated (T032, the PRODUCT.md release gate):
  the forced-relay path between isolated peers carries the full verified
  workload with 1.8–2.1× p50 degradation at kilobyte payloads and about 4×
  at 1 MiB (69.5 MiB/s), measured against a same-day direct reference on
  the same control plane. Numbers and method are recorded above; raw
  artifacts live with the T032 run artifacts.
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
