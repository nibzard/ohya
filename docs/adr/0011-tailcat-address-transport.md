# 0011: Tailcat address transport

Status: accepted, 2026-11-16. Supersedes [ADR 0002](0002-tailcat-transport.md)
and [ADR 0006](0006-shared-device-transport.md).

## Context

ADR 0002 named the transport "Tailcat", but ohya embedded a different stack:
the `tailscale.com/tsnet` node plus an in-process Headscale coordination
server. That stack has two requirements that it cannot satisfy itself:

- Every device must reach the hub's coordination URL over TCP.
- Every device must trust the TLS certificate of that URL, because the
  embedded relay always uses TLS.

The first cross-machine deployment (2026-11-15) had a hub VM behind NAT
(network address translation). The operator used the system Tailscale
client for reachability and `tailscale cert` for a trusted certificate.
The embedded node then ignored the Tailscale interface, and joining hung
until a code fix. The stack also caused these incidents (`NOTES.md`,
`docs/performance.md`): MagicDNS fallback to the system resolver,
self-signed certificate rejection, a hub watchdog panic, devices that never
reconnected after a hub restart, a Unix socket path limit, and a STUN bind
failure that exits the process.

ohya has a star topology. Devices connect only to the hub, and the device
registry is the only authorization boundary. The tailnet identity check
(`WhoIs`) is not used for authorization. So the coordination server,
MagicDNS, and the mesh add cost but no function.

The real Tailcat is `github.com/tailscale/tailcat` (BSD-3-Clause). It has no
control plane. A server prints an address that holds its WireGuard public
key, a separate path-discovery key, a 256-bit pre-shared key, and a relay
region. A client dials that address. Both peers meet through a DERP relay
and then try a direct UDP path. Traffic stays on the relay when NAT
traversal fails.

### Spike evidence (2026-11-16)

The spike embedded `tailcat.Server` in a small program on the hub VM and
used the `tailcat` command-line client on a MacBook with Tailscale quit.
The raw program is outside the repository (`/tmp/tcspike`); `NOTES.md`
records the results.

| Check | Result |
| --- | --- |
| Build with `tailscale.com` v1.104.4 | Builds; the Tailcat v0.7.0 test suite passes |
| Spike binary size | 30 MB (ohya with Headscale: 80 MB) |
| Address after restart | Same, when the program saves the node key, pre-shared key, and region |
| Same LAN (local area network) | Direct path after about 4 pings; 32 MiB at 8 to 12 Mbit/s |
| Phone hotspot | Relay only; 32 MiB at 3.1 to 3.3 Mbit/s; 65 to 265 ms round trip |
| Hub NAT behavior | `MappingVariesByDestIP: false` (not symmetric) |

## Decision

Use `github.com/tailscale/tailcat`, pinned at v0.7.0, as the device-to-hub
transport. Keep it behind the `internal/transport` interfaces (`Hub`,
`Device`). Callers still receive plain `net.Listener` and `net.Conn` values,
so the sync endpoint and the uploader do not change.

### Hub identity and address

- The hub generates its node private key and pre-shared key with
  `key.NewNode` and `tailcat.NewPresharedKey`. `Server.Start` does not
  write back keys that it generates, so ohya must not rely on it to do so.
- On the first start, the hub saves the node key, the pre-shared key, and
  the selected DERP region in an owner-only file under `tailcat-hub/` in
  the state directory. Later starts load that file, so the Tailcat address
  stays the same across restarts.
- The Tailcat address is a secret because it holds the pre-shared key.
  ohya never logs it. It appears only inside join codes
  ([ADR 0012](0012-two-step-multi-machine-setup.md)). Diagnostics show the
  hub's public node key only.
- The sync endpoint listens on TCP port 9027 inside the Tailcat network
  stack. The hub opens no listening port on the host.

### Devices

- Each device process uses a `tailcat.Client` with a new key for each
  process. The registry credential authenticates the device, so the client
  key carries no identity.
- `join` and each `ohya mcp --remote` process dial the hub independently.
  The shared-node owner lock and the loopback proxy from ADR 0006 are
  removed, together with `tla/TransportOwner.tla`.
- A paired device saves the hub's Tailcat address in its join state, next
  to `credential.json`.

### Admission and authorization

- Transport admission is the pre-shared key: only a holder of the address
  can complete a WireGuard handshake.
- Authorization stays in the device registry: invitation, credential, and
  revocation, as modeled in `tla/Auth.tla`. A revoked device still knows
  the address and can complete a handshake, but the registry rejects its
  hello. Revocation does not change the address.
- `Server.AllowedClients` is not used. Per-process client keys and pairing
  of unknown devices both need open admission. Revisit this only with
  evidence of transport-level abuse.

### Relay

- By default, Tailcat uses the DERP map at `https://tailcat.dev/derpmap.json`.
  Tailscale operates these relays. They need no account. Traffic is
  WireGuard-encrypted end to end with the pre-shared key, so a relay sees
  ciphertext, peer IP addresses, timing, and volume.
- A device needs outbound HTTPS (TCP 443) to the relay. A direct path also
  needs outbound UDP.
- A self-hosted relay (`derper`) and a fixed UDP port for port forwarding
  are deferred. Neither is necessary for correctness. They improve the
  speed of the first upload on networks where NAT traversal fails.

### Version policy

- Tailcat v0.x has no stability promise for its API, command-line tool, or
  wire format. ohya pins one exact version and upgrades it only in a
  release that states the change.
- The hub and its devices must run builds with the same pinned Tailcat
  version. The sync handshake keeps its existing `ProtocolVersion` check.
  A Tailcat wire change bumps `ProtocolVersion`, so a mismatched device
  fails with a version error and not a silent timeout.

### What is removed

- `internal/control` (embedded Headscale) and the `github.com/juanfont/headscale`
  dependency.
- The `serve` flags `--control-listen`, `--stun-listen`, `--control-url`,
  `--tls-cert`, and `--tls-key`, and the `--control` flag of `join` and
  `mcp --remote`.
- The state-directory certificate trust directory and the `SSL_CERT_DIR`
  change, the `net/netns` switch, the MagicDNS wait in `Node.Dial`, the
  shared-device lock, and `cmd/tailcat-spike`.

## Consequences

- A hub needs no reachable address, no certificate, and no open port.
  Two machines on any networks with outbound internet access can connect.
- The first upload can be slow when the path stays on the relay. In the
  spike, 4.46 GB of history needs about 3 hours at 3.2 Mbit/s. Daily sync
  volume is small, so the relay is enough after the first upload. ADR 0012
  makes the path and the upload progress visible.
- The default relay is a third-party service with no account. Operators who
  need no third party at all wait for the deferred self-hosted relay option.
- The binary loses the Headscale dependency tree. The spike suggests a
  binary of roughly 30 MB plus ohya's own code.
- Existing joined devices keep `device.json`, `credential.json`, and their
  upload checkpoints. They need the new hub address once (ADR 0012), and they
  do not pair again. The hub database and the registry do not change.
- The pairing, sync, and authorization state machines do not change, so
  `tla/Auth.tla` and `tla/Sync.tla` stay valid. `tla/TransportOwner.tla` is
  removed with the mechanism that it models.
